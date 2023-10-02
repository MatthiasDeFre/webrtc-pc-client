package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

type WebsocketPacket struct {
	ClientID    uint64
	MessageType uint64
	Message     string
}

type WebsocketCallback func(WebsocketPacket)

type WebsocketHandler struct {
	conn *websocket.Conn
}

func NewWSHandler(addr string) *WebsocketHandler {
	u := url.URL{Scheme: "ws", Host: addr, Path: "/"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		println("Cannot connect to signalling server")
		os.Exit(200)
	}
	return &WebsocketHandler{conn}
}

func (w *WebsocketHandler) StartListening(cb WebsocketCallback) {
	go func() {
		for {
			_, message, err := w.conn.ReadMessage()
			if err != nil {
				panic(err)
			}
			v := strings.Split(string(message), "@")
			clientID, _ := strconv.ParseUint(v[0], 10, 64)
			messageType, _ := strconv.ParseUint(v[1], 10, 64)
			wsPacket := WebsocketPacket{clientID, messageType, v[2]}
			cb(wsPacket)
		}
	}()
}

func (w *WebsocketHandler) SendMessage(wsPacket WebsocketPacket) {
	s := fmt.Sprintf("%d@%d@%s", wsPacket.ClientID, wsPacket.MessageType, wsPacket.Message)
	err := w.conn.WriteMessage(websocket.TextMessage, []byte(s))
	if err != nil {
		panic(err)
	}
}
