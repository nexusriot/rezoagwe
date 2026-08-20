package proto

import (
	"encoding/json"
	"strings"
)

// Wire format for every rezoagwe packet — node-to-node *and* node-to-bootstrap:
//
//	byte[0]     = MessageKind
//	byte[1..8]  = nonce
//	byte[9..16] = unix timestamp (big endian)
//	byte[17..48]= HMAC-SHA256 tag
//	byte[49..]  = body (JSON)
//
// Framing, authentication and replay rejection live in codec.go. This file
// only describes the message bodies.
//
// Wire version 2 dropped protobuf entirely: bootstrap used to speak raw
// protobuf on its own unauthenticated path, which meant two framings, two
// codecs, and no way to authenticate a REGISTER.

type MessageKind byte

const (
	KindKV            MessageKind = 0
	KindChat          MessageKind = 1
	KindStateRequest  MessageKind = 2
	KindStateResponse MessageKind = 3
	KindPeerGossip    MessageKind = 4
	KindHello         MessageKind = 5
	KindGoodbye       MessageKind = 6
	KindDigest        MessageKind = 7
	KindPullRequest   MessageKind = 8
	KindKVBatch       MessageKind = 9
	KindDirectMessage MessageKind = 10

	KindBootstrapRegister MessageKind = 20
	KindBootstrapDiscover MessageKind = 21
	KindBootstrapRoster   MessageKind = 22
)

// String names the kind for logs and metrics labels.
func (k MessageKind) String() string {
	switch k {
	case KindKV:
		return "kv"
	case KindChat:
		return "chat"
	case KindStateRequest:
		return "state_request"
	case KindStateResponse:
		return "state_response"
	case KindPeerGossip:
		return "peer_gossip"
	case KindHello:
		return "hello"
	case KindGoodbye:
		return "goodbye"
	case KindDigest:
		return "digest"
	case KindPullRequest:
		return "pull_request"
	case KindKVBatch:
		return "kv_batch"
	case KindDirectMessage:
		return "direct_message"
	case KindBootstrapRegister:
		return "bootstrap_register"
	case KindBootstrapDiscover:
		return "bootstrap_discover"
	case KindBootstrapRoster:
		return "bootstrap_roster"
	default:
		return "unknown"
	}
}

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

// Zero reports whether v is the unset version, which compare-and-swap reads as
// "this key must not exist".
func (v Version) Zero() bool {
	return v.Counter == 0 && v.Node == ""
}

func (v Version) Equal(other Version) bool {
	return v.Counter == other.Counter && v.Node == other.Node
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
	// ExpiresAt is a unix timestamp (seconds); 0 means the key never expires.
	// Expiry is deterministic — every replica hides and tombstones the entry at
	// the same instant, so no expiry message has to be replicated.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// DeletedAt records when a tombstone was created, so tombstone GC can age
	// them out consistently instead of guessing from the Lamport counter.
	DeletedAt int64 `json:"deleted_at,omitempty"`
}

// KVBatch carries several updates in one packet — used by anti-entropy, which
// would otherwise emit a packet per reconciled key.
type KVBatch struct {
	Updates []KVUpdate `json:"updates"`
}

type ChatMessage struct {
	Sender string `json:"sender"`
	Nick   string `json:"nick"`
	Text   string `json:"text"`
	TS     int64  `json:"ts"`
	// Action marks a /me emote so the receiver renders it in the third person.
	Action bool `json:"action,omitempty"`
	// To is set on a direct message (KindDirectMessage) and names the intended
	// recipient's address, so the receiver can label it.
	To string `json:"to,omitempty"`
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
	// context instead of an empty pane.
	Chat ChatLog `json:"chat,omitempty"`
}

// ChatKind labels how an entry should be rendered.
type ChatKind string

const (
	ChatMsg    ChatKind = ""       // an ordinary message
	ChatSystem ChatKind = "system" // joins, leaves, renames
	ChatAction ChatKind = "action" // /me
	ChatDirect ChatKind = "dm"     // a direct message, in either direction
)

// ChatEntry is one line of conversation. Entries are stored, persisted and
// synced structurally rather than pre-rendered, so each front end — the TUI,
// the HTTP gateway, the Android app — formats them its own way.
type ChatEntry struct {
	TS     int64    `json:"ts"`
	Sender string   `json:"sender,omitempty"` // address; empty for system lines
	Nick   string   `json:"nick,omitempty"`
	Text   string   `json:"text"`
	Kind   ChatKind `json:"kind,omitempty"`
	// To is the counterpart address of a direct message.
	To string `json:"to,omitempty"`
}

// ChatLog is a slice of entries that also accepts the wire-v1 form, where chat
// history was a list of pre-rendered strings. Without this, upgrading a node
// would fail to parse its own data file and silently discard the KV store
// along with the chat.
type ChatLog []ChatEntry

func (c *ChatLog) UnmarshalJSON(data []byte) error {
	var entries []ChatEntry
	if err := json.Unmarshal(data, &entries); err == nil {
		*c = entries
		return nil
	}
	var legacy []string
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	out := make([]ChatEntry, 0, len(legacy))
	for _, line := range legacy {
		out = append(out, ChatEntry{Text: stripMarkup(line), Kind: ChatSystem})
	}
	*c = out
	return nil
}

// stripMarkup removes tview colour tags from a legacy pre-rendered chat line.
func stripMarkup(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '[':
			depth++
		case r == ']' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

type PeerGossip struct {
	From  string   `json:"from"`
	Nick  string   `json:"nick,omitempty"`
	ID    string   `json:"id,omitempty"`
	Peers []string `json:"peers"`
}

type Hello struct {
	From string `json:"from"`
	Nick string `json:"nick,omitempty"`
	// ID is the sender's stable node id — the same string it stamps into
	// Version.Node — so peers can attribute a write to a nickname even after
	// the node moves to a different address.
	ID string `json:"id,omitempty"`
}

// Goodbye is sent to every known peer when a node shuts down cleanly, so
// peers drop it immediately instead of waiting for the stale-eviction timeout.
type Goodbye struct {
	From string `json:"from"`
	Nick string `json:"nick,omitempty"`
}

// KeyVersion is one entry of an anti-entropy digest: what the sender holds for
// a key, without the value.
type KeyVersion struct {
	Key     string  `json:"key"`
	Version Version `json:"version"`
	Deleted bool    `json:"deleted,omitempty"`
}

// Digest advertises what the sender holds for a *contiguous slice* of the
// sorted keyspace, [Lo, Hi). Bounding the range is what lets the receiver
// detect keys the sender is missing entirely: anything the receiver holds
// inside the range but that is absent from Entries has to be pushed. Hi == ""
// means the range runs to the end of the keyspace.
type Digest struct {
	From    string       `json:"from"`
	Lo      string       `json:"lo,omitempty"`
	Hi      string       `json:"hi,omitempty"`
	Entries []KeyVersion `json:"entries"`
}

// PullRequest asks the peer to (re)send the named keys, which the receiver
// answers with a KVBatch.
type PullRequest struct {
	From string   `json:"from"`
	Keys []string `json:"keys"`
}

// BootstrapRegister announces a node to the rendezvous service.
type BootstrapRegister struct {
	From string `json:"from"`
	Nick string `json:"nick,omitempty"`
}

// BootstrapDiscover asks the rendezvous service for the current roster.
type BootstrapDiscover struct {
	From string `json:"from"`
}

// BootstrapPeer is one roster entry. The nickname lets a joining node label
// peers before it has exchanged a single Hello.
type BootstrapPeer struct {
	Addr     string `json:"addr"`
	Nick     string `json:"nick,omitempty"`
	LastSeen int64  `json:"last_seen,omitempty"`
}

type BootstrapRoster struct {
	Peers []BootstrapPeer `json:"peers"`
}
