package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
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

			window.seekClick = () => {
				conn.send(JSON.stringify({event: 'seek', data: document.getElementById('seekTime').value}))
			}
			window.playClick = () => {
				conn.send(JSON.stringify({event: 'play', data: ''}))
			}
			window.pauseClick = () => {
				conn.send(JSON.stringify({event: 'pause', data: ''}))
			}

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

			conn.onopen = () => {
	console.log("open");
				pc.createOffer({offerToReceiveVideo: true, offerToReceiveAudio: true}).then(offer => {
					pc.setLocalDescription(offer)
					conn.send(JSON.stringify({event: 'offer', data: JSON.stringify(offer)}))
				})
			}
			conn.onclose = evt => {
	console.log("close");
				console.log('Connection closed')
			}
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
	if len(os.Args) != 2 {
		log.Fatalf("usage %s <rtsp server url>", os.Args[0])
	}
	httpListenAddress := ""
	flag.StringVar(&httpListenAddress, "http-listen-address", ":8080", "address for HTTP server to listen on")
	flag.Parse()

	var err error
	videoTrackRTP, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: "video/h264"}, "synced-video", "synced-video")
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

	go stream(&c, u)

	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", serveWs)

	fmt.Printf("streaming on '%s', have fun! \n", httpListenAddress)
	log.Fatal(http.ListenAndServe(httpListenAddress, nil))
}

func stream(c *gortsplib.Client, u *base.URL) {
	// find available medias
	desc, _, err := c.Describe(u)
	if err != nil {
		panic(err)
	}

	// find the H264 media and format
	var forma *format.H264
	medi := desc.FindFormat(&forma)
	if medi == nil {
		panic("media not found")
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
	encoder := &rtph264.Encoder{
		PayloadType:    96,
		PayloadMaxSize: webrtcPayloadMaxSize,
	}

	if err := encoder.Init(); err != nil {
		log.Fatal(err)
	}

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

		packets, err := encoder.Encode(tunit.AU)
		if err != nil {
			log.Printf("NICK: Encode err: %s", err.Error())
			return
		}
		for _, pkt := range packets {
			pkt.Timestamp += tunit.RTPPackets[0].Timestamp
			if err := videoTrackRTP.WriteRTP(pkt); err != nil {
				log.Printf("WriteRTP err: %s", err.Error())
			}
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
			return err
		}

		if err := pc.SetRemoteDescription(offer); err != nil {
			return err
		}

		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			return err
		}

		gatherComplete := webrtc.GatheringCompletePromise(pc)

		if err := pc.SetLocalDescription(answer); err != nil {
			return err
		}

		<-gatherComplete

		answerString, err := json.Marshal(pc.LocalDescription())
		if err != nil {
			return err
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
			log.Println(err)
		}
		return
	}

	peerConnection, err := webrtc.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		log.Println(err)
		return
	}

	if _, err = peerConnection.AddTrack(videoTrackRTP); err != nil {
		log.Println(err)
		return
	}

	defer func() {
		if err := peerConnection.Close(); err != nil {
			log.Println(err)
		}
	}()

	message := &websocketMessage{}
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			break
		} else if err := json.Unmarshal(msg, &message); err != nil {
			log.Println(err)
			return
		}

		if err := handleWebsocketMessage(peerConnection, ws, message); err != nil {
			log.Println(err)
		}
	}
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, homeHTML)
}
