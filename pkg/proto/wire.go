package proto

import (
	"encoding/json"
	"errors"
)

// Wire format for discovery node-to-node UDP packets:
//   byte[0]    = MessageKind
//   byte[1..]  = body
//
// For KindKV the body is a protobuf-marshalled Payload (existing format).
// For all other kinds the body is JSON. Bootstrap traffic does not use this
// envelope — bootstrap still speaks raw protobuf BootstrapMessage.

type MessageKind byte

const (
	KindKV            MessageKind = 0
	KindChat          MessageKind = 1
	KindStateRequest  MessageKind = 2
	KindStateResponse MessageKind = 3
	KindPeerGossip    MessageKind = 4
	KindHello         MessageKind = 5
)

type ChatMessage struct {
	Sender string `json:"sender"`
	Nick   string `json:"nick"`
	Text   string `json:"text"`
	TS     int64  `json:"ts"`
}

type StateRequest struct {
	From string `json:"from"`
}

type StateResponse struct {
	Store map[string]string `json:"store"`
}

type PeerGossip struct {
	From  string   `json:"from"`
	Nick  string   `json:"nick,omitempty"`
	Peers []string `json:"peers"`
}

type Hello struct {
	From string `json:"from"`
	Nick string `json:"nick,omitempty"`
}

func EncodeJSON(kind MessageKind, v interface{}) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+len(body))
	out[0] = byte(kind)
	copy(out[1:], body)
	return out, nil
}

func EncodeKV(pbBytes []byte) []byte {
	out := make([]byte, 1+len(pbBytes))
	out[0] = byte(KindKV)
	copy(out[1:], pbBytes)
	return out
}

func SplitKind(packet []byte) (MessageKind, []byte, error) {
	if len(packet) < 1 {
		return 0, nil, errors.New("empty packet")
	}
	return MessageKind(packet[0]), packet[1:], nil
}
