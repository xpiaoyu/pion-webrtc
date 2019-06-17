package main

import (
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v2"
	"io"
	"log"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v2"
)

// 52.68.64.213
// mv.piaoyu.org
var peerConnectionConfig = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{
		{
			URLs:       []string{"turn:mv.piaoyu.org:3478"},
			Username:   "test",
			Credential: "test",
		},
	},
	SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
}

const (
	rtcpPLIInterval = time.Second * 3
)

var respChan = make(chan string)

var restart = false

var sdpChan chan string

func main() {
	sdpChan = HTTPSDPServer()
	for {
		restart = false
		main2()
		respChan <- "restarted"
		log.Println("[!] Restart")
	}
}

func MyRTPH264Codec(payloadType uint8, clockrate uint32) *webrtc.RTPCodec {
	c := webrtc.NewRTPCodec(webrtc.RTPCodecTypeVideo,
		"H264",
		clockrate,
		0,
		"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		payloadType,
		&codecs.H264Payloader{})
	return c
}

func main2() {
	// Everything below is the pion-WebRTC API, thanks for using it ❤️.
	// Create a MediaEngine object to configure the supported codec
	m := webrtc.MediaEngine{}

	// Setup the codecs you want to use.
	// Only support VP8, this makes our proxying code simpler
	//m.RegisterCodec(MyRTPH264Codec(96, 90000))
	m.RegisterCodec(webrtc.NewRTPVP8Codec(webrtc.DefaultPayloadTypeVP8, 90000))
	//m.RegisterCodec(webrtc.NewRTPVP8Codec(100, 90000))
	m.RegisterCodec(webrtc.NewRTPOpusCodec(webrtc.DefaultPayloadTypeOpus, 48000))
	//m.RegisterCodec(webrtc.NewRTPG722Codec(webrtc.DefaultPayloadTypeG722, 8000))
	//m.RegisterDefaultCodecs()

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	offer := webrtc.SessionDescription{}
	output := "nil"
	for {
		select {
		case output = <-sdpChan:
			// 如果chan1成功读到数据，则进行该case处理语句
		default:
		}
		if restart {
			return
		}
		if output != "nil" {
			break
		} else {
			time.Sleep(1 * time.Second)
			log.Println("empty loop")
		}
	}
	Decode(output, &offer)
	log.Println("[+] Client offer:\r\n", offer)

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}
	log.Print("[+] Got peerConnection:", peerConnection)

	// Allow us to receive 1 audio track
	if _, err = peerConnection.AddTransceiver(webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}
	log.Print("[+] Added transceiver audio")
	// Allow us to receive 1 video track
	if _, err = peerConnection.AddTransceiver(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}
	log.Print("[+] Added transceiver video")

	localTrackChan := make(chan *webrtc.Track)
	// Set a handler for when a new remote track starts, this just distributes all our packets
	// to connected peers
	peerConnection.OnTrack(func(remoteTrack *webrtc.Track, receiver *webrtc.RTPReceiver) {
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		// This can be less wasteful by processing incoming RTCP events, then we would emit a NACK/PLI when a viewer requests it
		log.Print("[+] OnTrack remoteTrack:", remoteTrack, "payloadType:", remoteTrack.PayloadType())
		go func() {
			ticker := time.NewTicker(rtcpPLIInterval)
			for range ticker.C {
				if rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: remoteTrack.SSRC()}}); rtcpSendErr != nil {
					log.Println(rtcpSendErr)
				}
			}
		}()

		// Create a local track, all our SFU clients will be fed via this track
		localTrack, newTrackErr := peerConnection.NewTrack(remoteTrack.PayloadType(), remoteTrack.SSRC(), "video", "pion")
		if newTrackErr != nil {
			panic(newTrackErr)
		}
		log.Print("[+] Created new local track:", localTrack)
		localTrackChan <- localTrack

		rtpBuf := make([]byte, 1400)
		for {
			i, readErr := remoteTrack.Read(rtpBuf)
			if readErr != nil {
				panic(readErr)
			}

			// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
			if _, err = localTrack.Write(rtpBuf[:i]); err != nil && err != io.ErrClosedPipe {
				panic(err)
			}
		}
	})

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		log.Println(err)
	}
	log.Println("[+] Set remote description")

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		log.Println(err)
	}
	//answer.SDP = strings.Replace(answer.SDP, "42001f", "42e01f", -1)
	log.Println("[+] Created server answer:\r\n", answer)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		log.Println(err)
	}
	log.Println("[+] Set local description")

	// Get the LocalDescription and take it to base64 so we can paste in browser
	serverLocalDesc := Encode(answer)
	log.Println(serverLocalDesc)
	respChan <- serverLocalDesc

	// N broadcasters will create 2*N tracks
	// each one create one audio track and one video track
	localTrack1 := <-localTrackChan
	log.Println("[+] Got track 1 waiting track 2")
	localTrack2 := <-localTrackChan
	log.Println("[+] Got track 2")

	for !restart {
		log.Println("Curl an base64 SDP to start send-only peer connection")
		log.Println("Waiting next receiver...")

		recvOnlyOffer := webrtc.SessionDescription{}
		output := "nil"
		for {
			select {
			case output = <-sdpChan:
				// 如果chan1成功读到数据，则进行该case处理语句
			default:
			}
			if restart {
				return
			}
			if output != "nil" {
				break
			} else {
				time.Sleep(1 * time.Second)
			}
		}
		Decode(output, &recvOnlyOffer)
		log.Println("[+] Offer:\r\n", recvOnlyOffer)

		parsed := sdp.SessionDescription{}
		if err := parsed.Unmarshal([]byte(recvOnlyOffer.SDP)); err != nil {
			log.Println("[ERROR]", err)
			continue
		}

		// Create a new PeerConnection
		peerConnection, err := api.NewPeerConnection(peerConnectionConfig)
		if err != nil {
			log.Println(err)
			continue
		}

		//config := peerConnection.GetConfiguration()
		//config.SDPSemantics = webrtc.SDPSemanticsPlanB
		//log.Println(config)
		//peerConnection.SetConfiguration(config)
		//peerConnection.GetConfiguration()

		_, err = peerConnection.AddTrack(localTrack1)
		if err != nil {
			log.Println(err)
			continue
		}
		log.Println("[+] Added localTrack1")

		_, err = peerConnection.AddTrack(localTrack2)
		if err != nil {
			log.Println(err)
			continue
		}
		log.Println("[+] Added localTrack2")

		// Set the remote SessionDescription
		err = peerConnection.SetRemoteDescription(recvOnlyOffer)
		if err != nil {
			log.Println(err)
			continue
		}

		// Create answer
		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			log.Println("[ERROR]", err)
			continue
		}

		// Sets the LocalDescription, and starts our UDP listeners
		err = peerConnection.SetLocalDescription(answer)
		if err != nil {
			log.Println(err)
			continue
		}
		log.Println("[+] Answer:\r\n", answer)

		mediaDescriptionLen := len(parsed.MediaDescriptions)
		log.Println("[+] parsed MD length:", mediaDescriptionLen)
		log.Println("[+] MD[0]:", parsed.MediaDescriptions[0].MediaName.Media)

		// Get the LocalDescription and take it to base64 so we can paste in browser
		localDesc := Encode(answer)
		log.Println(localDesc)
		respChan <- localDesc
	}
}
