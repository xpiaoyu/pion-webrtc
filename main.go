package main

import (
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v2"
	"io"
	"log"
	"strconv"
	"strings"
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
	h264Fmtp        = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"
)

var respChan = make(chan string)

var restart = false

var sdpChan chan string

var broadcastVideoName string

var receiverTacks []*webrtc.Track

func main() {
	sdpChan = HTTPSDPServer()
	for {
		restart = false
		main2()
		respChan <- "restarted"
		log.Println("[!] Restart")
	}
}

func MyRTPH264Codec(payloadType uint8, clockRate uint32) *webrtc.RTPCodec {
	c := webrtc.NewRTPCodec(webrtc.RTPCodecTypeVideo,
		"H264",
		clockRate,
		0,
		h264Fmtp,
		payloadType,
		&codecs.H264Payloader{})
	return c
}

// DefaultPayloadTypeVP8  = 96
// DefaultPayloadTypeVP9  = 98
// DefaultPayloadTypeH264 = 102
func getMediaNameByPayloadType(payloadType uint8) string {
	switch payloadType {
	case 96:
		return "VP8"
	case 98:
		return "VP9"
	case 102:
		return "H264"
	default:
		return "VP8"
	}
}

func main2() {
	// Everything below is the pion-WebRTC API, thanks for using it ❤️.
	// Create a MediaEngine object to configure the supported codec
	m := webrtc.MediaEngine{}

	// Setup the codecs you want to use.
	// Only support VP8, this makes our proxying code simpler
	//m.RegisterCodec(MyRTPH264Codec(102, 90000))
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
	log.Println("[!] Client offer:\r\n", offer)

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}
	log.Print("[!] Got peerConnection:", peerConnection)

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
		log.Println("[!] OnTrack remoteTrack:", remoteTrack)
		log.Printf("[!] Remote track codec name:[%s] payloadType:[%d]", remoteTrack.Codec().Name, remoteTrack.Codec().PayloadType)

		if remoteTrack.PayloadType() == webrtc.DefaultPayloadTypeVP8 || remoteTrack.PayloadType() == webrtc.DefaultPayloadTypeVP9 ||
			remoteTrack.PayloadType() == webrtc.DefaultPayloadTypeH264 {
			broadcastVideoName = remoteTrack.Codec().Name
			log.Printf("[!] Video codec name:[%s]", broadcastVideoName)
			go func() {
				ticker := time.NewTicker(rtcpPLIInterval)
				for range ticker.C {
					if rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: remoteTrack.SSRC()}}); rtcpSendErr != nil {
						log.Println("[ERROR]", rtcpSendErr)
					}
				}
			}()

			// Create a local track, all our SFU clients will be fed via this track remoteTrack.PayloadType()
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
				packet := &rtp.Packet{}
				err = packet.Unmarshal(rtpBuf[:i])
				if err != nil {
					panic(err)
				}
				for _, track := range receiverTacks {
					packet.Header.PayloadType = track.PayloadType()
					if err := track.WriteRTP(packet); err != nil {
						if err == io.ErrClosedPipe {
							log.Printf("[!] ErrClosedPipe track ID:[%s]", track.ID())
						} else {
							// Unexpected error, panic for test
							panic(err)
						}
					}
				}

				// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
				//if _, err = localTrack.Write(rtpBuf[:i]); err != nil && err != io.ErrClosedPipe {
				//	panic(err)
				//}
			}
		} else {
			localTrack, newTrackErr := peerConnection.NewTrack(remoteTrack.PayloadType(), remoteTrack.SSRC(), "audio", "pion")
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
		}
	})

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		log.Println(err)
	}
	log.Println("[!] Set remote description")

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		log.Println(err)
	}
	//answer.SDP = strings.Replace(answer.SDP, "42001f", "42e01f", -1)
	log.Println("[!] Created server answer:\r\n", answer)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		log.Println(err)
	}
	log.Println("[!] Set local description")

	// Get the LocalDescription and take it to base64 so we can paste in browser
	serverLocalDesc := Encode(answer)
	log.Println(serverLocalDesc)
	respChan <- serverLocalDesc

	// N broadcasters will create 2*N tracks
	// each one create one audio track and one video track
	localTrack1 := <-localTrackChan
	log.Println("[!] Got track 1 waiting track 2")
	localTrack2 := <-localTrackChan
	log.Println("[!] Got track 2")

	for !restart {
		log.Println("Curl an base64 SDP to start send-only peer connection")
		log.Println("Waiting next receiver...")

		recvOnlyOffer := webrtc.SessionDescription{}
		output := "nil"
		for {
			select {
			case output = <-sdpChan:
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
		log.Println("[DEBUG] Offer:\r\n", recvOnlyOffer)

		//createWebrtcApiWithOffer(&recvOnlyOffer)
		recvApi, payloadType := createWebrtcApiWithOffer(&recvOnlyOffer, broadcastVideoName)

		// Create a new PeerConnection
		peerConnection, err := recvApi.NewPeerConnection(peerConnectionConfig)
		if err != nil {
			log.Println(err)
			continue
		}

		var videoTrack, audioTrack *webrtc.Track

		if localTrack1.Kind() == webrtc.RTPCodecTypeVideo {
			audioTrack = localTrack2
			videoTrack = localTrack1
		} else if localTrack2.Kind() == webrtc.RTPCodecTypeVideo {
			audioTrack = localTrack1
			videoTrack = localTrack2
		}

		newTrack, err := peerConnection.NewTrack(uint8(payloadType), videoTrack.SSRC(), videoTrack.ID(), videoTrack.Label())
		if err != nil {
			log.Printf("[ERROR] create new track error:%s", err.Error())
			continue
		}
		receiverTacks = append(receiverTacks, newTrack)
		log.Printf("[+] New client connected, current tracks num: %d", len(receiverTacks))

		_, err = peerConnection.AddTrack(audioTrack)
		if err != nil {
			log.Println(err)
			continue
		}
		log.Println("[+] Added audioTrack")

		_, err = peerConnection.AddTrack(newTrack)
		if err != nil {
			log.Println(err)
			continue
		}
		log.Println("[+] Added videoTrack")

		peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
			log.Printf("ICEConnectionState changed:[%s]", state.String())
			if state.String() == "disconnected" {
				log.Printf("[-] Client disconnected removing track ID:[%s]", newTrack.ID())
				for k, v := range receiverTacks {
					if v == newTrack {
						receiverTacks = append(receiverTacks[:k], receiverTacks[k+1:]...)
					}
				}
				log.Printf("[-] Track removed, current track num: %d", len(receiverTacks))
			}
		})

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
		log.Println("[DEBUG] Answer:\r\n", answer)

		log.Println("[!] Audio track codec:", audioTrack.Codec())
		log.Println("[!] Video track codec:", videoTrack.Codec())
		log.Println("[!] New   track codec:", newTrack.Codec())
		//log.Println("[+] Local track 1:", localTrack1.Codec())
		//log.Println("[+] Local track 2:", localTrack2.Codec())

		// Get the LocalDescription and take it to base64 so we can paste in browser
		localDesc := Encode(answer)
		log.Println(localDesc)
		respChan <- localDesc
	}
}

type rtpmap struct {
	rtpmap string
	fmtp   string
}

// Create an API
func createWebrtcApiWithOffer(sd *webrtc.SessionDescription, name string) (*webrtc.API, int) {
	parsed := sdp.SessionDescription{}
	if err := parsed.Unmarshal([]byte(sd.SDP)); err != nil {
		log.Println("[ERROR]", err)
	}

	mediaDescriptionLen := len(parsed.MediaDescriptions)
	log.Println("[!] parsed MD length:", mediaDescriptionLen)

	m := webrtc.MediaEngine{}

	recvMap := make(map[int]*rtpmap)
	setRecvRtpmap(parsed.MediaDescriptions, recvMap)

	name, payloadType, fmtp, ok := getAvailableVideoTypeByName(recvMap, name)
	if !ok {
		log.Printf("[WARN] Cannot find available codec, fallback to default codecs")
		m.RegisterDefaultCodecs()
		payloadType = 96
	} else {
		log.Printf("[!] Available video name:[%s] payload type:[%d] fmtp:[%s]", name, payloadType, fmtp)
		m.RegisterCodec(webrtc.NewRTPCodec(webrtc.RTPCodecTypeVideo, name, 90000,
			0, fmtp, uint8(payloadType), getPayloader(name)))
		//m.RegisterCodec(webrtc.NewRTPVP8Codec(96, 90000))
		//m.RegisterCodec(MyRTPH264Codec(98, 90000))
		m.RegisterCodec(webrtc.NewRTPOpusCodec(webrtc.DefaultPayloadTypeOpus, 48000))
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m)), payloadType
}

func getPayloader(name string) rtp.Payloader {
	switch name {
	case "VP8":
		return &codecs.VP8Payloader{}
	case "H264":
		return &codecs.H264Payloader{}
	}
	return nil
}

func setRecvRtpmap(mds []*sdp.MediaDescription, recvMap map[int]*rtpmap) {
	for i, md := range mds {
		log.Println("[!] MediaDescription index:", i, "MediaName.Media:", md.MediaName.Media)
		if md.MediaName.Media == "video" {
			log.Printf("[!] Video attributes:\r\n")
			for i, attr := range md.Attributes {
				if attr.Key == "rtpmap" || attr.Key == "fmtp" {
					log.Printf("[%d] Key:[%s] Value:[%s]", i, attr.Key, attr.Value)
					if payloadType, rest, ok := splitAttributeValue(attr.Value); ok {
						if attr.Key == "rtpmap" {
							v, ok := recvMap[payloadType]
							if ok {
								v.rtpmap = rest
							} else {
								recvMap[payloadType] = &rtpmap{rtpmap: rest, fmtp: ""}
							}
						} else if attr.Key == "fmtp" {
							v, ok := recvMap[payloadType]
							if ok {
								log.Printf("[!] Rest:[%s]", rest)
								v.fmtp = rest
							} else {
								recvMap[payloadType] = &rtpmap{fmtp: rest}
							}
						}
					} else {
						log.Printf("[ERROR]")
					}
				}
			}
		}
	}
}

func getAvailableVideoTypeByName(recvMap map[int]*rtpmap, name string) (RTPName string, payloadType int, fmtp string, ok bool) {
	for k, v := range recvMap {
		if strings.HasPrefix(v.rtpmap, name) {
			return name, k, v.fmtp, true
		}
	}
	return "", 0, "", false
}

func splitAttributeValue(v string) (payloadType int, rest string, ok bool) {
	ok = false
	index := strings.IndexByte(v, ' ')
	if index < 0 {
		return
	}
	payloadTypeStr := v[:index]
	rest = v[index+1:]
	if pt, err := strconv.Atoi(payloadTypeStr); err != nil {
		return
	} else {
		payloadType = pt
		ok = true
	}
	return
}
