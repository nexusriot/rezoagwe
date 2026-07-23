package proto

import (
	"encoding/json"
	"errors"
)

// Wire format for discovery node-to-node UDP packets:
//   byte[0]    = MessageKind
//   byte[1..]  = body (JSON)
//
// Every discovery kind carries a JSON body. Bootstrap traffic does not use
// this envelope — bootstrap still speaks raw protobuf BootstrapMessage.

type MessageKind byte

const (
	KindKV            MessageKind = 0
	KindChat          MessageKind = 1
	KindStateRequest  MessageKind = 2
	KindStateResponse MessageKind = 3
	KindPeerGossip    MessageKind = 4
	KindHello         MessageKind = 5
	KindGoodbye       MessageKind = 6
)

// Version is a per-key logical clock for last-write-wins replication: a
// Lamport counter with the originating node's id as a deterministic
// tiebreaker for concurrent writes at the same counter.
type Version struct {
	Counter uint64 `json:"counter"`
	Node    string `json:"node"`
}

// Newer reports whether v should win over other: higher counter wins, ties
// broken by node id. Not-newer (including exactly equal) means "don't apply".
func (v Version) Newer(other Version) bool {
	if v.Counter != other.Counter {
		return v.Counter > other.Counter
	}
	return v.Node > other.Node
}

type KVAction string

const (
	KVSet    KVAction = "set"
	KVDelete KVAction = "delete"
)

// KVUpdate is a single versioned key mutation. It is both the replication
// message (KindKV) and the unit of a state-sync snapshot. A delete is carried
// as a versioned tombstone (Action=KVDelete) so a stale set cannot resurrect
// the key.
type KVUpdate struct {
	Action  KVAction `json:"action"`
	Key     string   `json:"key"`
	Value   string   `json:"value,omitempty"`
	Version Version  `json:"version"`
}

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
	// KV is the responder's full store as versioned entries (including
	// tombstones), which the joiner merges under last-write-wins rather than
	// overwriting blindly.
	KV []KVUpdate `json:"kv"`
	// Chat carries the responder's recent chat history so a joining node has
	// context instead of an empty pane. Capped by the sender to bound packet
	// size (the whole response still travels in one UDP datagram).
	Chat []string `json:"chat,omitempty"`
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

// Goodbye is sent to every known peer when a node shuts down cleanly, so
// peers drop it immediately instead of waiting for the stale-eviction timeout.
type Goodbye struct {
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

func SplitKind(packet []byte) (MessageKind, []byte, error) {
	if len(packet) < 1 {
		return 0, nil, errors.New("empty packet")
	}
	return MessageKind(packet[0]), packet[1:], nil
}
