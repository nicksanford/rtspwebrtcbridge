// heavily copied from https://github.com/bluenviron/mediamtx/blob/main/internal/formatprocessor/h264.go & the rest of that package
package formatprocessor

import (
	"log"

	"bytes"
	"errors"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/rtptime"

	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph265"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h265"
	"github.com/pion/rtp"

	"github.com/nicksanford/rtspwebrtcbridge/unit"
)

func New(
	udpMaxPayloadSize int,
	forma format.Format,
	generateRTPPackets bool,
) (unit.Processor, error) {
	switch forma := forma.(type) {
	case *format.H264:
		return newH264(udpMaxPayloadSize, forma, generateRTPPackets)

	case *format.H265:
		return newH265(udpMaxPayloadSize, forma, generateRTPPackets)

	default:
		return nil, errors.New("Unsupported formatprocessor")
	}
}

// extract SPS and PPS without decoding RTP packets
func rtpH264ExtractParams(payload []byte) ([]byte, []byte) {
	if len(payload) < 1 {
		return nil, nil
	}

	typ := h264.NALUType(payload[0] & 0x1F)

	switch typ {
	case h264.NALUTypeSPS:
		return payload, nil

	case h264.NALUTypePPS:
		return nil, payload

	case h264.NALUTypeSTAPA:
		payload := payload[1:]
		var sps []byte
		var pps []byte

		for len(payload) > 0 {
			if len(payload) < 2 {
				break
			}

			size := uint16(payload[0])<<8 | uint16(payload[1])
			payload = payload[2:]

			if size == 0 {
				break
			}

			if int(size) > len(payload) {
				return nil, nil
			}

			nalu := payload[:size]
			payload = payload[size:]

			typ = h264.NALUType(nalu[0] & 0x1F)

			switch typ {
			case h264.NALUTypeSPS:
				sps = nalu

			case h264.NALUTypePPS:
				pps = nalu
			}
		}

		return sps, pps

	default:
		return nil, nil
	}
}

type formatProcessorH264 struct {
	udpMaxPayloadSize int
	format            *format.H264

	encoder *rtph264.Encoder
	decoder *rtph264.Decoder
}

func newH264(
	udpMaxPayloadSize int,
	forma *format.H264,
	generateRTPPackets bool,
) (*formatProcessorH264, error) {
	t := &formatProcessorH264{
		udpMaxPayloadSize: udpMaxPayloadSize,
		format:            forma,
	}

	if generateRTPPackets {
		err := t.createEncoder(nil, nil)
		if err != nil {
			return nil, err
		}
	}
	// log.Printf("NICK: newH264: %#v", t)

	return t, nil
}

func (t *formatProcessorH264) createEncoder(
	ssrc *uint32,
	initialSequenceNumber *uint16,
) error {
	t.encoder = &rtph264.Encoder{
		PayloadMaxSize:        t.udpMaxPayloadSize - 12,
		PayloadType:           t.format.PayloadTyp,
		SSRC:                  ssrc,
		InitialSequenceNumber: initialSequenceNumber,
		PacketizationMode:     t.format.PacketizationMode,
	}
	return t.encoder.Init()
}

func (t *formatProcessorH264) updateTrackParametersFromRTPPacket(payload []byte) {
	sps, pps := rtpH264ExtractParams(payload)

	if (sps != nil && !bytes.Equal(sps, t.format.SPS)) ||
		(pps != nil && !bytes.Equal(pps, t.format.PPS)) {
		if sps == nil {
			sps = t.format.SPS
		}
		if pps == nil {
			pps = t.format.PPS
		}
		t.format.SafeSetParams(sps, pps)
	}
}

func (t *formatProcessorH264) updateTrackParametersFromAU(au [][]byte) {
	// log.Printf("NICK: updateTrackParametersFromAU: %p, len(au): %d", au, len(au))
	sps := t.format.SPS
	pps := t.format.PPS
	update := false

	for _, nalu := range au {
		typ := h264.NALUType(nalu[0] & 0x1F)

		switch typ {
		case h264.NALUTypeSPS:
			if !bytes.Equal(nalu, sps) {
				sps = nalu
				update = true
			}

		case h264.NALUTypePPS:
			if !bytes.Equal(nalu, pps) {
				pps = nalu
				update = true
			}
		}
	}

	if update {
		t.format.SafeSetParams(sps, pps)
	}
}

func (t *formatProcessorH264) remuxAccessUnit(au [][]byte) [][]byte {
	// log.Printf("NICK: remuxAccessUnit: %p, len(au): %d", au, len(au))
	isKeyFrame := false
	n := 0

	for _, nalu := range au {
		typ := h264.NALUType(nalu[0] & 0x1F)

		switch typ {
		case h264.NALUTypeSPS, h264.NALUTypePPS: // parameters: remove
			continue

		case h264.NALUTypeAccessUnitDelimiter: // AUD: remove
			continue

		case h264.NALUTypeIDR: // key frame
			if !isKeyFrame {
				isKeyFrame = true

				// prepend parameters
				if t.format.SPS != nil && t.format.PPS != nil {
					n += 2
				}
			}
		}
		n++
	}

	if n == 0 {
		return nil
	}

	filteredNALUs := make([][]byte, n)
	i := 0

	if isKeyFrame && t.format.SPS != nil && t.format.PPS != nil {
		filteredNALUs[0] = t.format.SPS
		filteredNALUs[1] = t.format.PPS
		i = 2
	}

	for _, nalu := range au {
		typ := h264.NALUType(nalu[0] & 0x1F)

		switch typ {
		case h264.NALUTypeSPS, h264.NALUTypePPS:
			continue

		case h264.NALUTypeAccessUnitDelimiter:
			continue
		}

		filteredNALUs[i] = nalu
		i++
	}

	return filteredNALUs
}

func (t *formatProcessorH264) ProcessUnit(uu unit.Unit) error {
	// log.Printf("NICK: ProcessUnit: %p, %#v", uu, uu)
	u := uu.(*unit.H264)
	// log.Printf("NICK: BEFORE ProcessUnit: %p, len(u.AU): %d, %#v", u, len(u.AU), u)

	t.updateTrackParametersFromAU(u.AU)
	u.AU = t.remuxAccessUnit(u.AU)
	// log.Printf("NICK: post remuxAccessUnit: %p, %#v", uu, uu)

	if u.AU != nil {
		pkts, err := t.encoder.Encode(u.AU)
		if err != nil {
			log.Printf("NICK: Encoder Err: %s", err.Error())
			return err
		}
		u.RTPPackets = pkts

		ts := uint32(multiplyAndDivide(u.PTS, time.Duration(t.format.ClockRate()), time.Second))
		for _, pkt := range u.RTPPackets {
			pkt.Timestamp += ts
		}
	}
	// log.Printf("NICK: AFTER ProcessUnit: %p, len(u.AU): %d, %#v", u, len(u.AU), u)

	return nil
}

func (t *formatProcessorH264) ProcessRTPPacket( //nolint:dupl
	pkt *rtp.Packet,
	ntp time.Time,
	pts time.Duration,
	hasNonRTSPReaders bool,
) (unit.Unit, error) {
	// log.Printf("NICK: ProcessRTPPacket called with %s", pkt)
	u := &unit.H264{
		Base: unit.Base{
			RTPPackets: []*rtp.Packet{pkt},
			NTP:        ntp,
			PTS:        pts,
		},
	}

	t.updateTrackParametersFromRTPPacket(pkt.Payload)

	if t.encoder == nil {
		// log.Printf("NICK: ProcessRTPPacket t.encoder == nil")
		// remove padding
		pkt.Header.Padding = false
		pkt.PaddingSize = 0

		// RTP packets exceed maximum size: start re-encoding them
		if pkt.MarshalSize() > t.udpMaxPayloadSize {
			// log.Printf("NICK: ProcessRTPPacket pkt.MarshalSize(): %d > t.udpMaxPayloadSize: %d",
			// pkt.MarshalSize(), t.udpMaxPayloadSize)
			v1 := pkt.SSRC
			v2 := pkt.SequenceNumber
			err := t.createEncoder(&v1, &v2)
			if err != nil {
				log.Printf("NICK: ProcessRTPPacket createEncoder, Err: %s", err.Error())
				return nil, err
			}
		}
	}

	// decode from RTP
	if hasNonRTSPReaders || t.decoder != nil || t.encoder != nil {
		// log.Printf("NICK: ProcessRTPPacket hasNonRTSPReaders: %t || t.decoder != nil: %t || t.encoder != nil: %t",
		// hasNonRTSPReaders, t.decoder != nil, t.encoder != nil)
		if t.decoder == nil {
			// log.Printf("NICK: ProcessRTPPacket t.decoder == nil")
			var err error
			t.decoder, err = t.format.CreateDecoder()
			if err != nil {
				log.Printf("NICK: ProcessRTPPacket CreateDecoder Err: %s", err.Error())
				return nil, err
			}
		}

		au, err := t.decoder.Decode(pkt)
		var auByteSize int
		for _, a := range au {
			auByteSize += len(a)
		}
		// log.Printf("NICK: ProcessRTPPacket Decode len(au): %d, auByteSize: %d, Err: %s", len(au), auByteSize, err)

		if t.encoder != nil {
			// log.Printf("NICK: ProcessRTPPacket t.encoder != nil, u.RTPPackets len before %d", len(u.RTPPackets))
			u.RTPPackets = nil
		}

		if err != nil {
			if errors.Is(err, rtph264.ErrNonStartingPacketAndNoPrevious) ||
				errors.Is(err, rtph264.ErrMorePacketsNeeded) {
				return u, nil
			}
			return nil, err
		}

		u.AU = t.remuxAccessUnit(au)
		auByteSize = 0
		for _, a := range au {
			auByteSize += len(a)
		}
		// log.Printf("NICK: ProcessRTPPacket post remuxAccessUnit len(au): %d, auByteSize: %d", len(u.AU), auByteSize)
	}

	// route packet as is
	if t.encoder == nil {
		// log.Printf("NICK: ProcessRTPPacket t.encoder == nil")
		return u, nil
	}

	// encode into RTP
	if len(u.AU) != 0 {
		// log.Printf("NICK: len(u.AU) != 0")
		pkts, err := t.encoder.Encode(u.AU)
		if err != nil {
			log.Printf("NICK: ProcessRTPPacket: Encode Err: %s", err.Error())
			return nil, err
		}
		u.RTPPackets = pkts

		for _, newPKT := range u.RTPPackets {
			newPKT.Timestamp = pkt.Timestamp
		}
		var pktStr string
		for _, pkt := range u.RTPPackets {
			pktStr += pkt.String()
			pktStr += "\n"
		}
		// log.Printf("NICK: ProcessRTPPacket: u.RTPPackets: \n%s", pktStr)
	}

	return u, nil
}

// avoid an int64 overflow and preserve resolution by splitting division into two parts:
// first add the integer part, then the decimal part.
func multiplyAndDivide(v, m, d time.Duration) time.Duration {
	secs := v / d
	dec := v % d
	return (secs*m + dec*m/d)
}

// h265
// extract VPS, SPS and PPS without decoding RTP packets
func rtpH265ExtractParams(payload []byte) ([]byte, []byte, []byte) {
	if len(payload) < 2 {
		return nil, nil, nil
	}

	typ := h265.NALUType((payload[0] >> 1) & 0b111111)

	switch typ {
	case h265.NALUType_VPS_NUT:
		return payload, nil, nil

	case h265.NALUType_SPS_NUT:
		return nil, payload, nil

	case h265.NALUType_PPS_NUT:
		return nil, nil, payload

	case h265.NALUType_AggregationUnit:
		payload := payload[2:]
		var vps []byte
		var sps []byte
		var pps []byte

		for len(payload) > 0 {
			if len(payload) < 2 {
				break
			}

			size := uint16(payload[0])<<8 | uint16(payload[1])
			payload = payload[2:]

			if size == 0 {
				break
			}

			if int(size) > len(payload) {
				return nil, nil, nil
			}

			nalu := payload[:size]
			payload = payload[size:]

			typ = h265.NALUType((nalu[0] >> 1) & 0b111111)

			switch typ {
			case h265.NALUType_VPS_NUT:
				vps = nalu

			case h265.NALUType_SPS_NUT:
				sps = nalu

			case h265.NALUType_PPS_NUT:
				pps = nalu
			}
		}

		return vps, sps, pps

	default:
		return nil, nil, nil
	}
}

type formatProcessorH265 struct {
	udpMaxPayloadSize int
	format            *format.H265
	timeEncoder       *rtptime.Encoder
	encoder           *rtph265.Encoder
	decoder           *rtph265.Decoder
}

func newH265(
	udpMaxPayloadSize int,
	forma *format.H265,
	generateRTPPackets bool,
) (*formatProcessorH265, error) {
	t := &formatProcessorH265{
		udpMaxPayloadSize: udpMaxPayloadSize,
		format:            forma,
	}

	if generateRTPPackets {
		err := t.createEncoder(nil, nil)
		if err != nil {
			return nil, err
		}

		t.timeEncoder = &rtptime.Encoder{
			ClockRate: forma.ClockRate(),
		}
		err = t.timeEncoder.Initialize()
		if err != nil {
			return nil, err
		}
	}

	return t, nil
}

func (t *formatProcessorH265) createEncoder(
	ssrc *uint32,
	initialSequenceNumber *uint16,
) error {
	t.encoder = &rtph265.Encoder{
		PayloadMaxSize:        t.udpMaxPayloadSize - 12,
		PayloadType:           t.format.PayloadTyp,
		SSRC:                  ssrc,
		InitialSequenceNumber: initialSequenceNumber,
		MaxDONDiff:            t.format.MaxDONDiff,
	}
	return t.encoder.Init()
}

func (t *formatProcessorH265) updateTrackParametersFromRTPPacket(payload []byte) {
	vps, sps, pps := rtpH265ExtractParams(payload)

	if (vps != nil && !bytes.Equal(vps, t.format.VPS)) ||
		(sps != nil && !bytes.Equal(sps, t.format.SPS)) ||
		(pps != nil && !bytes.Equal(pps, t.format.PPS)) {
		if vps == nil {
			vps = t.format.VPS
		}
		if sps == nil {
			sps = t.format.SPS
		}
		if pps == nil {
			pps = t.format.PPS
		}
		t.format.SafeSetParams(vps, sps, pps)
	}
}

func (t *formatProcessorH265) updateTrackParametersFromAU(au [][]byte) {
	vps := t.format.VPS
	sps := t.format.SPS
	pps := t.format.PPS
	update := false

	for _, nalu := range au {
		typ := h265.NALUType((nalu[0] >> 1) & 0b111111)

		switch typ {
		case h265.NALUType_VPS_NUT:
			if !bytes.Equal(nalu, t.format.VPS) {
				vps = nalu
				update = true
			}

		case h265.NALUType_SPS_NUT:
			if !bytes.Equal(nalu, t.format.SPS) {
				sps = nalu
				update = true
			}

		case h265.NALUType_PPS_NUT:
			if !bytes.Equal(nalu, t.format.PPS) {
				pps = nalu
				update = true
			}
		}
	}

	if update {
		t.format.SafeSetParams(vps, sps, pps)
	}
}

func (t *formatProcessorH265) remuxAccessUnit(au [][]byte) [][]byte {
	isKeyFrame := false
	n := 0

	for _, nalu := range au {
		typ := h265.NALUType((nalu[0] >> 1) & 0b111111)

		switch typ {
		case h265.NALUType_VPS_NUT, h265.NALUType_SPS_NUT, h265.NALUType_PPS_NUT: // parameters: remove
			continue

		case h265.NALUType_AUD_NUT: // AUD: remove
			continue

		case h265.NALUType_IDR_W_RADL, h265.NALUType_IDR_N_LP, h265.NALUType_CRA_NUT: // key frame
			if !isKeyFrame {
				isKeyFrame = true

				// prepend parameters
				if t.format.VPS != nil && t.format.SPS != nil && t.format.PPS != nil {
					n += 3
				}
			}
		}
		n++
	}

	if n == 0 {
		return nil
	}

	filteredNALUs := make([][]byte, n)
	i := 0

	if isKeyFrame && t.format.VPS != nil && t.format.SPS != nil && t.format.PPS != nil {
		filteredNALUs[0] = t.format.VPS
		filteredNALUs[1] = t.format.SPS
		filteredNALUs[2] = t.format.PPS
		i = 3
	}

	for _, nalu := range au {
		typ := h265.NALUType((nalu[0] >> 1) & 0b111111)

		switch typ {
		case h265.NALUType_VPS_NUT, h265.NALUType_SPS_NUT, h265.NALUType_PPS_NUT:
			continue

		case h265.NALUType_AUD_NUT:
			continue
		}

		filteredNALUs[i] = nalu
		i++
	}

	return filteredNALUs
}

func (t *formatProcessorH265) ProcessUnit(uu unit.Unit) error { //nolint:dupl
	u := uu.(*unit.H265)

	t.updateTrackParametersFromAU(u.AU)
	u.AU = t.remuxAccessUnit(u.AU)

	if u.AU != nil {
		pkts, err := t.encoder.Encode(u.AU)
		if err != nil {
			return err
		}
		u.RTPPackets = pkts

		ts := t.timeEncoder.Encode(u.PTS)
		for _, pkt := range u.RTPPackets {
			pkt.Timestamp += ts
		}
	}

	return nil
}

func (t *formatProcessorH265) ProcessRTPPacket( //nolint:dupl
	pkt *rtp.Packet,
	ntp time.Time,
	pts time.Duration,
	hasNonRTSPReaders bool,
) (unit.Unit, error) {
	u := &unit.H265{
		Base: unit.Base{
			RTPPackets: []*rtp.Packet{pkt},
			NTP:        ntp,
			PTS:        pts,
		},
	}

	t.updateTrackParametersFromRTPPacket(pkt.Payload)

	if t.encoder == nil {
		// remove padding
		pkt.Header.Padding = false
		pkt.PaddingSize = 0

		// RTP packets exceed maximum size: start re-encoding them
		if pkt.MarshalSize() > t.udpMaxPayloadSize {
			v1 := pkt.SSRC
			v2 := pkt.SequenceNumber
			err := t.createEncoder(&v1, &v2)
			if err != nil {
				return nil, err
			}
		}
	}

	// decode from RTP
	if hasNonRTSPReaders || t.decoder != nil || t.encoder != nil {
		if t.decoder == nil {
			var err error
			t.decoder, err = t.format.CreateDecoder()
			if err != nil {
				return nil, err
			}
		}

		au, err := t.decoder.Decode(pkt)

		if t.encoder != nil {
			u.RTPPackets = nil
		}

		if err != nil {
			if errors.Is(err, rtph265.ErrNonStartingPacketAndNoPrevious) ||
				errors.Is(err, rtph265.ErrMorePacketsNeeded) {
				return u, nil
			}
			return nil, err
		}

		u.AU = t.remuxAccessUnit(au)
	}

	// route packet as is
	if t.encoder == nil {
		return u, nil
	}

	// encode into RTP
	if len(u.AU) != 0 {
		pkts, err := t.encoder.Encode(u.AU)
		if err != nil {
			return nil, err
		}
		u.RTPPackets = pkts

		for _, newPKT := range u.RTPPackets {
			newPKT.Timestamp = pkt.Timestamp
		}
	}

	return u, nil
}
