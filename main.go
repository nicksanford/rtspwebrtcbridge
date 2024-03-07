package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/aler9/gortsplib/pkg/rtpcodecs/rtph265"
	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/gorilla/websocket"
	"github.com/nicksanford/rtspwebrtcbridge/formatprocessor"
	"github.com/nicksanford/rtspwebrtcbridge/unit"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

const homeHTML = `<!DOCTYPE html>
<html lang="en">
	<head>
		<title>synced-playback</title>
	</head>
	<body id="body">
		<video id="video1" autoplay playsinline></video>

		<div>
		  <input type="number" id="seekTime" value="30">
		  <button type="button" onClick="seekClick()">Seek</button>
		  <button type="button" onClick="playClick()">Play</button>
		  <button type="button" onClick="pauseClick()">Pause</button>
		</div>

		<script>
			let conn = new WebSocket('ws://' + window.location.host + '/ws')
			let pc = new RTCPeerConnection()

			console.log("before on track register")
			pc.ontrack = function (event) {
				console.log("on track", event);
			  if (event.track.kind === 'audio') {
				return
			  }
			  var el = document.getElementById('video1')
			  el.srcObject = event.streams[0]
			  el.autoplay = true
			  el.controls = true
			}

	// needed for safari to work
			const x = pc.addTransceiver('video');
			console.log("before on open register")
			conn.onopen = () => {
				console.log("open");
				pc.createOffer({offerToReceiveVideo: true, offerToReceiveAudio: true}).then(offer => {
					console.log("got offer", offer.sdp);
					pc.setLocalDescription(offer)
					conn.send(JSON.stringify({event: 'offer', data: JSON.stringify(offer)}))
				})
			}
			console.log("before on close register")
			conn.onclose = evt => {
				console.log("close");
				console.log('Connection closed')
			}
			console.log("before on message register")
			conn.onmessage = evt => {
				console.log("message"), evt;
				let msg = JSON.parse(evt.data)
				if (!msg) {
					return console.log('failed to parse msg')
				}

				switch (msg.event) {
				case 'answer':
					answer = JSON.parse(msg.data)
					if (!answer) {
						return console.log('failed to parse answer')
					}
					pc.setRemoteDescription(answer)
					return console.log('processed answer')
				}
			}
			window.conn = conn
		</script>
	</body>
</html>
`

var mimeLookup = map[string]string{"4": webrtc.MimeTypeH264, "5": webrtc.MimeTypeH265}

var (
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	peerConnectionConfig = webrtc.Configuration{}
	videoTrackRTP        = &webrtc.TrackLocalStaticRTP{}
)

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

func main() {
	if len(os.Args) != 3 {
		log.Fatalf("usage %s <rtsp server url> <4|5>", os.Args[0])
	}

	if os.Args[2] != "4" && os.Args[2] != "5" {
		log.Fatalf("usage %s <rtsp server url> <4|5>", os.Args[0])
	}

	httpListenAddress := ""
	flag.StringVar(&httpListenAddress, "http-listen-address", ":8080", "address for HTTP server to listen on")
	flag.Parse()

	mimeType := mimeLookup[os.Args[2]]
	var err error
	videoTrackRTP, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: mimeType}, "synced-video", "synced-video")
	if err != nil {
		log.Fatal(err)
	}
	log.Println(videoTrackRTP.Codec())

	c := gortsplib.Client{}

	// parse URL
	u, err := base.ParseURL(os.Args[1])
	if err != nil {
		panic(err)
	}

	// connect to the server
	err = c.Start(u.Scheme, u.Host)
	if err != nil {
		panic(err)
	}

	defer c.Close()

	go stream(&c, u, mimeType)

	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", serveWs)

	fmt.Printf("streaming on '%s', have fun! \n", httpListenAddress)
	log.Fatal(http.ListenAndServe(httpListenAddress, nil))
}

func stream(c *gortsplib.Client, u *base.URL, mimeType string) {
	// find available medias
	desc, _, err := c.Describe(u)
	if err != nil {
		panic(err)
	}

	var forma format.Format
	var medi *description.Media
	if mimeType == webrtc.MimeTypeH264 {
		var f *format.H264
		medi = desc.FindFormat(&f)
		if medi == nil {
			panic("media not found")
		}
		forma = f
	} else if mimeType == webrtc.MimeTypeH265 {
		var f *format.H265
		medi = desc.FindFormat(&f)
		if medi == nil {
			panic("media not found")
		}
		forma = f
	} else {
		panic("provide media type")
	}

	// setup a single media
	_, err = c.Setup(desc.BaseURL, medi, 0, 0)
	if err != nil {
		panic(err)
	}

	fp, err := formatprocessor.New(1472, forma, true)
	if err != nil {
		log.Fatal(err)
	}

	firstReceived := false
	var lastPTS time.Duration
	webrtcPayloadMaxSize := 1188 // 1200 - 12 (RTP header)

	h264Encoder := &rtph264.Encoder{
		PayloadType:    96,
		PayloadMaxSize: webrtcPayloadMaxSize,
	}

	if err := h264Encoder.Init(); err != nil {
		log.Fatal(err)
	}

	h265Encoder := &rtph265.Encoder{
		PayloadType:    96,
		PayloadMaxSize: webrtcPayloadMaxSize,
	}

	h265Encoder.Init()
	// if mimeType == "video/h264" {
	// } else if mimeType == "video/h265" {
	// } else {
	// 	panic("provide media type")
	// }

	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		pts, ok := c.PacketPTS(medi, pkt)
		if !ok {
			return
		}
		ntp := time.Now()
		u, err := fp.ProcessRTPPacket(pkt, ntp, pts, false)
		if err != nil {
			log.Println(err.Error())
			return
		}

		// NOTE: In mediamtx there is a ring buffer between the goroutine which receives RTP packets from RSTP & the WebRTC publisher
		// at this point
		// This might be a place to improve performance by adding a similar ring buffer

		switch forma.(type) {
		case *format.H264:
			tunit, ok := u.(*unit.H264)
			if !ok {
				log.Println("u.(*unit.H264) type conversion error")
				return
			}

			if tunit.AU == nil {
				return
			}

			if !firstReceived {
				firstReceived = true
			} else if tunit.PTS < lastPTS {
				log.Fatal("WebRTC doesn't support H264 streams with B-frames")
			}
			lastPTS = tunit.PTS
			packets, err := h264Encoder.Encode(tunit.AU)
			if err != nil {
				panic(err.Error())
			}
			for _, pkt := range packets {
				pkt.Timestamp += tunit.RTPPackets[0].Timestamp
				if err := videoTrackRTP.WriteRTP(pkt); err != nil {
					log.Printf("WriteRTP err: %s", err.Error())
				}
			}

		case *format.H265:
			tunit, ok := u.(*unit.H265)
			if !ok {
				log.Println("u.(*unit.H265) type conversion error")
				return
			}

			if tunit.AU == nil {
				return
			}

			packets, err := h265Encoder.Encode(tunit.AU, pts)
			if err != nil {
				panic(err.Error())
			}

			for _, pkt := range packets {
				pkt.Timestamp += tunit.RTPPackets[0].Timestamp
				if err := videoTrackRTP.WriteRTP(pkt); err != nil {
					log.Printf("WriteRTP err: %s", err.Error())
				}
			}

		default:
			panic("unsupported type")
		}

	})

	// start playing
	_, err = c.Play(nil)
	if err != nil {
		panic(err)
	}

	// wait until a fatal error
	panic(c.Wait())
}

func handleWebsocketMessage(pc *webrtc.PeerConnection, ws *websocket.Conn, message *websocketMessage) error {
	switch message.Event {
	case "offer":
		offer := webrtc.SessionDescription{}
		if err := json.Unmarshal([]byte(message.Data), &offer); err != nil {
			panic(err)
		}
		fmt.Println(message.Data)
		fmt.Println(offer)

		if err := pc.SetRemoteDescription(offer); err != nil {
			panic(err)
		}

		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			panic(err)
		}

		gatherComplete := webrtc.GatheringCompletePromise(pc)

		if err := pc.SetLocalDescription(answer); err != nil {
			panic(err)
		}

		<-gatherComplete

		answerString, err := json.Marshal(pc.LocalDescription())
		if err != nil {
			panic(err)
		}

		if err = ws.WriteJSON(&websocketMessage{
			Event: "answer",
			Data:  string(answerString),
		}); err != nil {
			return err
		}
	default:

	}
	return nil
}

func serveWs(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		if _, ok := err.(websocket.HandshakeError); !ok {
			panic(err)
		}
	}

	peerConnection, err := webrtc.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}

	if _, err = peerConnection.AddTrack(videoTrackRTP); err != nil {
		panic(err)
	}

	defer func() {
		if err := peerConnection.Close(); err != nil {
			panic(err)
		}
	}()

	message := &websocketMessage{}
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			break
		} else if err := json.Unmarshal(msg, &message); err != nil {
			panic(err)
		}

		if err := handleWebsocketMessage(peerConnection, ws, message); err != nil {
			panic(err)
		}
	}
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, homeHTML)
}
