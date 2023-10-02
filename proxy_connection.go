package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
)

const (
	FramePacketType   uint32 = 0
	ControlPacketType uint32 = 1
)

type RemoteInputPacketHeader struct {
	Framenr     uint32
	Framelen    uint32
	Frameoffset uint32
	Packetlen   uint32
}

type RemoteFrame struct {
	currentLen uint32
	frameLen   uint32
	frameData  []byte
}

type ProxyConnection struct {
	// General
	addr *net.UDPAddr
	conn *net.UDPConn

	// Receiving
	m                 sync.RWMutex
	incomplete_frames map[uint32]RemoteFrame
	complete_frames   []RemoteFrame
	frameCounter      uint32
}

func NewProxyConnection() *ProxyConnection {
	return &ProxyConnection{nil, nil, sync.RWMutex{}, make(map[uint32]RemoteFrame), make([]RemoteFrame, 0), 0}
}

func (pc *ProxyConnection) sendPacket(b []byte, offset uint32, packet_type uint32) {
	buffProxy := make([]byte, 1300)
	binary.LittleEndian.PutUint32(buffProxy[0:], packet_type)
	copy(buffProxy[4:], b[offset:])
	_, err := pc.conn.WriteToUDP(buffProxy, pc.addr)
	if err != nil {
		fmt.Println("Error sending response:", err)
		panic(err)
	}
}

func (pc *ProxyConnection) SetupConnection(port string) {
	address, err := net.ResolveUDPAddr("udp", port)
	if err != nil {
		fmt.Println("Error resolving address:", err)
		return
	}

	// Create a UDP connection
	pc.conn, err = net.ListenUDP("udp", address)
	if err != nil {
		fmt.Println("Error listening:", err)
		return
	}

	// Create a buffer to read incoming messages
	buffer := make([]byte, 1500)

	// Wait for incoming messages
	fmt.Println("Waiting for a message...")
	_, pc.addr, err = pc.conn.ReadFromUDP(buffer)
	if err != nil {
		fmt.Println("Error reading:", err)
		return
	}
	fmt.Println("Connected to proxy")
}

func (pc *ProxyConnection) StartListening() {
	println("listen")
	str := "Hello!"
	byteArray := make([]byte, 1500)
	copy(byteArray[:], str)
	//byteArray[len(str)] = 0
	_, err := pc.conn.WriteToUDP(byteArray, pc.addr)
	if err != nil {
		fmt.Println("Error sending response:", err)
		return
	}
	go func() {
		for {
			buffer := make([]byte, 1500)
			_, _, _ = pc.conn.ReadFromUDP(buffer)
			bufBinary := bytes.NewBuffer(buffer[4:20])
			// Read the fields from the buffer into a struct
			var p RemoteInputPacketHeader
			err := binary.Read(bufBinary, binary.LittleEndian, &p)
			if err != nil {
				fmt.Println("Error:", err)
				return
			}
			pc.m.Lock()
			_, exists := pc.incomplete_frames[p.Framenr]
			if !exists {
				r := RemoteFrame{
					0,
					p.Framelen,
					make([]byte, p.Framelen),
				}
				pc.incomplete_frames[p.Framenr] = r
			}
			value := pc.incomplete_frames[p.Framenr]

			copy(value.frameData[p.Frameoffset:p.Frameoffset+p.Packetlen], buffer[20:20+p.Packetlen])
			value.currentLen = value.currentLen + p.Packetlen
			
			pc.incomplete_frames[p.Framenr] = value
			if value.currentLen == value.frameLen {
				println("FRAME ", p.Framenr, " COMPLETE")
				pc.complete_frames = append(pc.complete_frames, value)
				delete(pc.incomplete_frames, p.Framenr)
			}
			//println(p.Frameoffset, p.Framenr, value.currentLen, p.Framelen)
			pc.m.Unlock()

		}
	}()
}
func (pc *ProxyConnection) SendFramePacket(b []byte, offset uint32) {
	pc.sendPacket(b, offset, FramePacketType)
}
func (pc *ProxyConnection) SendControlPacket(b []byte) {
	pc.sendPacket(b, 0, ControlPacketType)
}

func (pc *ProxyConnection) NextFrame() []byte {
	isNextFrameReady := false
	for !isNextFrameReady {
		pc.m.Lock()
		if len(pc.complete_frames) > 0 {
			isNextFrameReady = true
		}
		pc.m.Unlock()
	}
	pc.m.Lock()
	data := pc.complete_frames[0].frameData
	if pc.frameCounter%100 == 0 {
		println("SENDING FRAME ", pc.frameCounter)
	}
	pc.complete_frames = pc.complete_frames[1:]
	pc.frameCounter = pc.frameCounter + 1
	pc.m.Unlock()
	return data
}
