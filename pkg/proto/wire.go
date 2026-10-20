package proto

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
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
	KindKV               MessageKind = 0
	KindChat             MessageKind = 1
	KindStateRequest     MessageKind = 2
	KindStateResponse    MessageKind = 3
	KindPeerGossip       MessageKind = 4
	KindHello            MessageKind = 5
	KindGoodbye          MessageKind = 6
	KindDigest           MessageKind = 7
	KindPullRequest      MessageKind = 8
	KindKVBatch          MessageKind = 9
	KindDirectMessage    MessageKind = 10
	KindFingerprint      MessageKind = 11
	KindFingerprintReply MessageKind = 12
	KindChatPull         MessageKind = 13
	KindChatBacklog      MessageKind = 14

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
	case KindFingerprint:
		return "fingerprint"
	case KindFingerprintReply:
		return "fingerprint_reply"
	case KindChatPull:
		return "chat_pull"
	case KindChatBacklog:
		return "chat_backlog"
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
	// Seq counts this sender's own broadcasts, from 1, and survives a restart
	// with the state file. It is what lets a receiver notice that a line never
	// arrived: chat is one datagram per peer with nothing re-sending it, so
	// without a sequence a dropped message is indistinguishable from one that
	// was never sent, at both ends.
	//
	// Zero means unsequenced — a peer older than this field, or a direct
	// message — and is never gap-checked.
	Seq uint64 `json:"seq,omitempty"`
	// Node is the author's persistent node id, which is what Seq counts
	// against. Keying on the id rather than the address matches how a KV
	// version already identifies a writer: a node that moves ports is still
	// the same author, and a node that restarts with no state file gets a new
	// id and is correctly read as a new one — rather than appearing to rewind
	// its whole conversation and having every line after it ignored as a
	// duplicate.
	Node string `json:"node,omitempty"`
	// Hops counts how many peers have forwarded this line. A broadcast only
	// ever reached the peers its *author* knew, and peer tables are routinely
	// asymmetric — topology already reports links only one end claims — so a
	// node the author had not yet learned about never heard the line at all.
	// Unlike the store, chat has no digest to notice.
	//
	// Zero is an original broadcast, and a peer that predates this field
	// simply does not forward. Deduplication by (author, Seq) is what stops a
	// relayed line circulating; MaxChatHops is the backstop for when it
	// cannot, not the primary bound.
	Hops uint8 `json:"hops,omitempty"`
}

// MaxChatHops bounds forwarding. Three is past the diameter of any peer graph
// gossip keeps connected: a line that has not reached a node in three
// forwards is not going to, and something is wrong with membership instead.
const MaxChatHops = 3

// ChatPull asks a sender to re-send its own broadcasts by sequence number.
//
// Addressed to the author, not to whoever happens to be nearby: only the
// author can be sure what its own Seq 7 was, and asking a third party would
// propagate whatever that peer had instead.
type ChatPull struct {
	From string `json:"from"`
	// Seqs are the sender's sequence numbers this node is missing, bounded by
	// MaxChatPull so one request stays one datagram.
	Seqs []uint64 `json:"seqs"`
}

// ChatBacklog answers a ChatPull.
type ChatBacklog struct {
	From string `json:"from"`
	// Messages are the requested lines the author still holds.
	Messages []ChatMessage `json:"messages,omitempty"`
	// Unavailable are the ones it no longer holds, because the chat ring has
	// moved past them. Saying so is the point: it turns "still waiting" into
	// "gone", which is the difference between a gap that will heal and one
	// that will not, and a receiver that is not told keeps asking forever.
	Unavailable []uint64 `json:"unavailable,omitempty"`
}

// MaxChatPull bounds how many sequence numbers one request may name.
const MaxChatPull = 64

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
	// Seq is the author's own sequence number, carried into the log so the
	// author can answer a ChatPull for it and so a re-delivery is recognised
	// as one rather than appended twice. Zero on system lines, direct
	// messages, and anything from a peer older than the field.
	Seq uint64 `json:"seq,omitempty"`
	// Node is the author's persistent id, recorded for the same reason a KV
	// entry records its writer: Seq counts against the id, so a log that kept
	// only the address could not tell the tracker which author's numbering a
	// stored line belongs to — and the baseline would not survive a restart.
	Node string `json:"node,omitempty"`
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
	// ChatSeq is how many lines this node has broadcast, carried on the
	// heartbeat so a peer learns where our numbering stands when we *meet*
	// rather than when we first happen to speak. Without it the baseline for a
	// quiet author is set by their first audible line, and everything they
	// said before that — including while this node was listening but
	// unreachable — is neither delivered nor reported.
	ChatSeq uint64 `json:"chat_seq,omitempty"`
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
	// ValueHash is sha256 of the value, hex, and empty for a tombstone or an
	// empty value. It is what lets the checker tell apart two replicas that
	// agree on the version but hold different data — the one divergence
	// last-write-wins cannot repair, because neither side's version is newer.
	// Absent from a peer too old to send it, which reads as "cannot compare"
	// rather than as a difference.
	ValueHash string `json:"vhash,omitempty"`
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

// Fingerprint asks a peer to summarise its whole store, so two replicas can be
// compared without shipping either of them.
type Fingerprint struct {
	From string `json:"from"`
	// Buckets, when set, also asks for the (key, version) pairs held in those
	// buckets. A checker fills it on a second request, once the digests have
	// said which buckets disagree — which turns "you differ somewhere in this
	// sixteenth of the keyspace" into the names of the keys.
	//
	// Asking for nothing is the old request exactly, and a peer that does not
	// understand the field answers with no Entries, so a report against an
	// older node degrades to bucket indices rather than failing.
	Buckets []int `json:"buckets,omitempty"`
}

// FingerprintReply summarises a whole store.
//
// Anti-entropy repairs divergence but never reports it, so a cluster can sit
// split for as long as nobody looks. Buckets are the smallest thing that turns
// "these two disagree" into "these two disagree about this part of the
// keyspace": each is an order-independent fold of the (key, version) pairs
// that hash into it, so two replicas holding the same versions produce the
// same bucket whatever order they learned them in.
type FingerprintReply struct {
	From       string `json:"from"`
	Nick       string `json:"nick,omitempty"`
	Keys       int    `json:"keys"`
	Tombstones int    `json:"tombstones"`
	Clock      uint64 `json:"clock"`
	// Buckets is FingerprintBuckets hex digests, indexed by bucket.
	Buckets []string `json:"buckets"`
	// Entries is every (key, version) the sender holds in the buckets the
	// request named, sorted by key, including its tombstones — a key deleted
	// on one replica and live on the other is exactly the kind of divergence
	// worth naming. Empty unless Fingerprint.Buckets asked.
	Entries []KeyVersion `json:"entries,omitempty"`
}

// MaxFrameBytes is the largest frame a stream will carry, and so the hard
// ceiling on anything that has to replicate. transport.MaxFrameSize is the
// same number at the layer that enforces it; a test pins the two together.
const MaxFrameBytes = 64 << 20

// frameReserve leaves room for everything in a KV frame that is not the value:
// the key, the version, the expiry, the JSON around them, and the kind, nonce,
// timestamp and authentication tag the codec wraps it all in.
const frameReserve = 64 << 10

// MaxReplicableValueBytes is the largest a value may be, measured as it will
// be written on the wire rather than as it sits in memory, and still fit in a
// frame. Plain text costs its own length, so this is also the plain answer to
// "how big can a value be"; anything the encoder has to escape costs more.
//
// A value past this is not merely large, it is unreplicable: the datagram path
// refuses it on size, the stream path refuses the frame, and a state sync
// builds that same frame — so it would be accepted locally, reported as
// written, and then sit on one replica forever with nothing able to repair it.
// Refusing the write is the only honest answer, and it is a property of the
// protocol rather than a tuning knob, which is why it is here and not in
// Limits.
const MaxReplicableValueBytes = MaxFrameBytes - frameReserve

// asciiEscapeLen is what each ASCII byte costs inside a JSON string. Derived
// from the encoder rather than from memory — it uses the short form for seven
// characters, spells the rest of the C0 controls out as \u00xx, and escapes
// the three characters that would end an HTML element or entity. A test
// re-derives the whole table and fails if a toolchain ever disagrees.
var asciiEscapeLen = func() (t [utf8.RuneSelf]uint8) {
	for c := 0; c < utf8.RuneSelf; c++ {
		switch {
		case c == '\b', c == '\t', c == '\n', c == '\f', c == '\r', c == '"', c == '\\':
			t[c] = 2
		case c < 0x20, c == '<', c == '>', c == '&':
			t[c] = 6
		default:
			t[c] = 1
		}
	}
	return t
}()

// invalidByteCost is how many bytes the encoder writes for one byte it cannot
// decode as UTF-8.
//
// Measured, not written down — which is the whole lesson here. This was the
// constant 6, because Go wrote the six-byte escape `\ufffd`; since Go 1.25 it
// writes the three-byte replacement character itself, so the constant became
// wrong under a toolchain upgrade with nothing in the code to say so. The
// escape table above is already re-derived from the encoder by its own test;
// this was the one cost that was copied instead, and it is the one that drifted.
//
// A size calculation that mirrors another package's output has to come from
// that package.
var invalidByteCost = func() int {
	b, err := json.Marshal("\xff")
	if err != nil || len(b) < 3 {
		// Unreachable for a string, but the two failure modes are not
		// symmetric: over-counting only refuses a value that would have fit,
		// while under-counting admits one that can never replicate. Fall back
		// to the larger of the costs Go has ever charged.
		return 6
	}
	return len(b) - 2 // less the surrounding quotes
}()

// EscapedLen is how many bytes s occupies once encoded as a JSON string,
// counted without building it — the point is to measure values far too big to
// want a second copy of.
//
// Printable text costs its own length. Control bytes cost up to six times it,
// and bytes that are not valid UTF-8 cost [invalidByteCost] each, the encoder
// having replaced them with U+FFFD. A value arriving through the HTTP gateway
// is whatever bytes the client sent, so a blob really can multiply on the wire
// — which is why this is measured rather than assumed.
func EscapedLen(s string) int {
	n := 2 // the surrounding quotes
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			n += int(asciiEscapeLen[c])
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			n += invalidByteCost // one per byte the encoder could not decode
			i++
		case r == ' ', r == ' ':
			n += 6
			i += size
		default:
			n += size
			i += size
		}
	}
	return n
}

// FingerprintBuckets is how finely a store is summarised. Sixteen is enough to
// point at a region of the keyspace and small enough that the reply fits in a
// datagram alongside everything else.
const FingerprintBuckets = 16

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
	// SeenAt is the source address the rendezvous service observed this
	// request arriving from — the asker's own address, as the network sees it
	// rather than as the asker believes it.
	//
	// It is what a node cannot work out for itself. A node bound to a wildcard
	// knows every address its machine has and not which one its packets leave
	// by, so "you are advertising something nobody can reach" has until now
	// been a local guess about loopback. Bootstrap is the one component that
	// sees the truth, and it was throwing it away.
	//
	// Only the host is dependable. A datagram arrives from the node's own
	// bound socket, so the port is right too; a stream is dialled with an
	// ephemeral source port, so the port there means nothing. Compare hosts.
	SeenAt string `json:"seen_at,omitempty"`
}
