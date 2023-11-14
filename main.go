package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
)

const (
	Idle     int = 0
	Hello    int = 1
	Offer    int = 2
	Answer   int = 3
	Ready    int = 4
	Finished int = 5
)

var proxyConn *ProxyConnection
var frameResultwriter *FrameResultWriter
var virtualWallFilterIp string

func main() {
	virtualWallIp := flag.String("v", "", "Use virtual wall ip filter")
	proxyPort := flag.String("p", ":0", "Use as a proxy with specified port")
	resultDirectory := flag.String("m", "", "Result directory")
	flag.Parse()
	frameResultwriter = NewFrameResultWriter(*resultDirectory, 5)
	fileCont, _ := os.OpenFile(*resultDirectory+"_cont.csv", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	fileCont.WriteString("timestamp;bitrate\n")

	useProxy := false
	if *proxyPort != ":0" {
		proxyConn = NewProxyConnection()
		fmt.Println(*proxyPort)
		proxyConn.SetupConnection(*proxyPort)
		useProxy = true
	}

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetSCTPMaxReceiveBufferSize(16 * 1024 * 1024)
	if *virtualWallIp != "" {
		virtualWallFilterIp = *virtualWallIp
		settingEngine.SetIPFilter(VirtualWallFilter)
	}
	i := &interceptor.Registry{}
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}
	videoRTCPFeedback := []webrtc.RTCPFeedback{
		{Type: "goog-remb", Parameter: ""},
		{Type: "ccm", Parameter: "fir"},
		{Type: "nack", Parameter: ""},
		{Type: "nack", Parameter: "pli"},
	}

	codecCapability := webrtc.RTPCodecCapability{
		MimeType:     "video/pcm",
		ClockRate:    90000,
		Channels:     0,
		SDPFmtpLine:  "",
		RTCPFeedback: videoRTCPFeedback,
	}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: codecCapability,
		PayloadType:        5,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeVideo)
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)

	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, i); err != nil {
		panic(err)
	}

	responder, _ := nack.NewResponderInterceptor()
	i.Add(responder)

	// Client side

	m.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC}, webrtc.RTPCodecTypeVideo)
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	m.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC}, webrtc.RTPCodecTypeAudio)
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}

	generator, err := twcc.NewSenderInterceptor(twcc.SendInterval(10 * time.Millisecond))
	if err != nil {
		panic(err)
	}

	i.Add(generator)

	nackGenerator, _ := nack.NewGeneratorInterceptor()
	i.Add(nackGenerator)

	var candidatesMux sync.Mutex
	pendingCandidates := make([]*webrtc.ICECandidate, 0)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine), webrtc.WithInterceptorRegistry(i), webrtc.WithMediaEngine(m))
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{
		PeerIdentity: "test",
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	})

	if err != nil {
		panic(err)
	}

	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("Cannot close peer connection: %v\n", cErr)
		}
	}()

	wsHandler := NewWSHandler("127.0.0.1:5678")

	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidatesMux.Lock()
		desc := peerConnection.RemoteDescription()
		if desc == nil {
			pendingCandidates = append(pendingCandidates, c)
		} else {
			payload := []byte(c.ToJSON().Candidate)
			wsHandler.SendMessage(WebsocketPacket{1, 4, string(payload)})
		}
		candidatesMux.Unlock()
	})

	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer connection state has changed: %s\n", s.String())
		if s == webrtc.PeerConnectionStateFailed {
			fmt.Println("Peer connection has gone to failed exiting")
			os.Exit(0)
		}
		if s == webrtc.PeerConnectionStateConnected {
			proxyConn.SetWsHandler(wsHandler)
		}
	})

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		println("OnTrack has been called")
		println("MIME type:", track.Codec().MimeType)
		println("Payload type:", track.PayloadType())

		codecName := strings.Split(track.Codec().RTPCodecCapability.MimeType, "/")
		fmt.Printf("Track of type %d has started: %s \n", track.PayloadType(), codecName)

		// Create buffer to receive incoming track data, using 1300 bytes - header bytes
		buf := make([]byte, 1220)

		// Allows to check if frames are received completely
		// Frame number and corresponding length
		frames := make(map[uint32]uint32)
		oldEpochMilliseconds := time.Now().UnixNano() / int64(time.Millisecond)
		msCounter := int(0)
		bw := 0
		for {
			_, _, readErr := track.Read(buf)
			if readErr != nil {
				panic(err)
			}
			if useProxy {
				// TODO: Use bufBinary and make plugin buffer size as parameter
				proxyConn.SendFramePacket(buf, 20)
			}
			// Create a buffer from the byte array, skipping the first 20 WebRTC bytes
			// TODO: mention WebRTC header content explicitly
			bufBinary := bytes.NewBuffer(buf[20:])
			// Read the fields from the buffer into a struct
			var p FramePacket
			err := binary.Read(bufBinary, binary.LittleEndian, &p)
			if err != nil {
				panic(err)
			}
			frames[p.FrameNr] += p.SeqLen
			if frames[p.FrameNr] == p.SeqLen {
				frameResultwriter.CreateRecord(p.FrameNr, time.Now().UnixNano()/int64(time.Millisecond), false)
				frameResultwriter.SetSizeInBytes(p.FrameNr, p.FrameLen, false)
			}
			if frames[p.FrameNr] == p.FrameLen {
				frameResultwriter.SetProcessingCompleteTimestamp(p.FrameNr, time.Now().UnixNano()/int64(time.Millisecond), false)
				frameResultwriter.SaveRecord(p.FrameNr, false)
			}
			if frames[p.FrameNr] == p.FrameLen && p.FrameNr%50 == 0 {
				println("FRAME COMPLETE ", p.FrameNr, p.FrameLen)
			}

			epochMilliseconds := time.Now().UnixNano() / int64(time.Millisecond)
			msCounter += int(epochMilliseconds - oldEpochMilliseconds)
			bw += int(p.SeqLen + 20)
			if uint64(msCounter/1000) > 0 {
				timestamp := time.Now().UnixNano() / int64(time.Millisecond)
				data := fmt.Sprintf("%d;%.2f\n", timestamp, float64(8*bw/1000000))
				fileCont.WriteString(data)
				msCounter = msCounter - 1000
				bw = 0
				// lossTargetBitrate
				// averageLoss
				// delayTargetBitrate
			}
			oldEpochMilliseconds = epochMilliseconds

		}
	})

	var state = Idle
	println("Current state:", state)
	// CLIENT CB
	var handleMessageCallback = func(wsPacket WebsocketPacket) {
		switch wsPacket.MessageType {
		case 2: // offer
			println("Received offer")
			offer := webrtc.SessionDescription{}
			err := json.Unmarshal([]byte(wsPacket.Message), &offer)
			if err != nil {
				panic(err)
			}
			peerConnection.SetRemoteDescription(offer)
			answer, err := peerConnection.CreateAnswer(nil)
			if err != nil {
				panic(err)
			}
			if err = peerConnection.SetLocalDescription(answer); err != nil {
				panic(err)
			}
			payload, err := json.Marshal(answer)
			if err != nil {
				panic(err)
			}
			wsHandler.SendMessage(WebsocketPacket{1, 3, string(payload)})
			state = Offer
			println("Current state:", state)
		case 4: // candidate
			println("Received candidate")
			candidate := wsPacket.Message
			if candidateErr := peerConnection.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate}); candidateErr != nil {
				panic(candidateErr)
			}
		default:
			println(fmt.Sprintf("Received non-compliant message type %d", wsPacket.MessageType))
		}
	}

	wsHandler.StartListening(handleMessageCallback)
	// CLIENT
	// CREATE PEER CONNECTION
	// CREATE OFFER
	// SEND OFFER VIA WS
	/*offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}
	if err = peerConnection.SetLocalDescription(offer); err != nil {
		panic(err)
	}
	payload, err := json.Marshal(offer)
	if err != nil {
		panic(err)
	}*/
	wsHandler.SendMessage(WebsocketPacket{1, 1, "Hello"})

	//wsHandler.SendMessage(WebsocketPacket{1, 1, "Hello"})

	// Block forever
	select {}
}

func VirtualWallFilter(addr net.IP) bool {
	if addr.String() == virtualWallFilterIp {
		return true
	}
	return false
}
