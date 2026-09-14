package proto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Framing constants. Every packet is authenticated: there is no unauthenticated
// path to keep tested. When no pre-shared key is configured the key is derived
// from the cluster name alone, which still separates two clusters sharing a
// LAN but — the name being public — provides no secrecy. Pass a -psk for that.
const (
	nonceLen  = 8
	tsLen     = 8
	macLen    = sha256.Size
	headerLen = 1 + nonceLen + tsLen + macLen

	// keyDomain separates this key derivation from any other use of the psk.
	keyDomain = "rezoagwe/wire/v2"

	// DefaultCluster labels a cluster that did not pick a name.
	DefaultCluster = "rezoagwe"

	// DefaultSkew bounds how far a packet's timestamp may sit from local time.
	// It has to cover real clock drift between nodes and the time a packet
	// spends in flight, while staying short enough that the replay window is
	// small.
	DefaultSkew = 30 * time.Second
)

var (
	ErrShortFrame = errors.New("frame shorter than header")
	ErrBadMAC     = errors.New("authentication failed")
	ErrClockSkew  = errors.New("timestamp outside accepted skew")
	ErrReplay     = errors.New("nonce already seen")
)

// DeriveKey turns (psk, cluster) into the per-cluster framing key. An empty psk
// is allowed and yields a cluster-separation-only key.
func DeriveKey(psk, cluster string) []byte {
	if cluster == "" {
		cluster = DefaultCluster
	}
	mac := hmac.New(sha256.New, []byte(psk))
	mac.Write([]byte(keyDomain))
	mac.Write([]byte{0})
	mac.Write([]byte(cluster))
	return mac.Sum(nil)
}

// Codec frames, authenticates and replay-checks packets.
//
// Decode is safe for concurrent use; the replay cache is the only shared state.
type Codec struct {
	key  []byte
	skew time.Duration

	// now is overridable so tests can drive clock-skew and replay-expiry paths
	// without sleeping.
	now func() time.Time

	mu        sync.Mutex
	seen      map[[nonceLen]byte]time.Time
	lastPrune time.Time
}

func NewCodec(psk, cluster string) *Codec {
	return &Codec{
		key:  DeriveKey(psk, cluster),
		skew: DefaultSkew,
		now:  time.Now,
		seen: make(map[[nonceLen]byte]time.Time),
	}
}

// KeyFingerprint is a short, non-secret label for the derived framing key.
//
// Two nodes that cannot talk usually differ in the psk or the cluster name, and
// neither is printable — one is a secret, the other looks right until you
// compare them character by character. A fingerprint is comparable at a glance
// and gives nothing away.
func (c *Codec) KeyFingerprint() string {
	sum := sha256.Sum256(c.key)
	return hex.EncodeToString(sum[:4])
}

// Encode marshals v as the body of a framed, authenticated packet.
func (c *Codec) Encode(kind MessageKind, v interface{}) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := make([]byte, headerLen+len(body))
	out[0] = byte(kind)
	if _, err := rand.Read(out[1 : 1+nonceLen]); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint64(out[1+nonceLen:], uint64(c.now().Unix()))
	copy(out[headerLen:], body)
	tag := c.tag(out[0], out[1:1+nonceLen], out[1+nonceLen:1+nonceLen+tsLen], body)
	copy(out[1+nonceLen+tsLen:], tag)
	return out, nil
}

// Decode authenticates a frame and returns its kind and body. Frames that fail
// authentication, sit outside the accepted clock skew, or reuse a nonce are
// rejected — the caller drops the packet and counts it.
func (c *Codec) Decode(frame []byte) (MessageKind, []byte, error) {
	if len(frame) < headerLen {
		return 0, nil, ErrShortFrame
	}
	kind := MessageKind(frame[0])
	nonce := frame[1 : 1+nonceLen]
	tsBytes := frame[1+nonceLen : 1+nonceLen+tsLen]
	mac := frame[1+nonceLen+tsLen : headerLen]
	body := frame[headerLen:]

	if !hmac.Equal(mac, c.tag(frame[0], nonce, tsBytes, body)) {
		return kind, nil, ErrBadMAC
	}

	ts := time.Unix(int64(binary.BigEndian.Uint64(tsBytes)), 0)
	now := c.now()
	if delta := now.Sub(ts); delta > c.skew || delta < -c.skew {
		return kind, nil, ErrClockSkew
	}

	var key [nonceLen]byte
	copy(key[:], nonce)
	if !c.remember(key, now) {
		return kind, nil, ErrReplay
	}
	return kind, body, nil
}

// DecodeInto decodes a frame and unmarshals its body in one step.
func (c *Codec) DecodeInto(frame []byte, v interface{}) (MessageKind, error) {
	kind, body, err := c.Decode(frame)
	if err != nil {
		return kind, err
	}
	return kind, json.Unmarshal(body, v)
}

func (c *Codec) tag(kind byte, nonce, ts, body []byte) []byte {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte{kind})
	mac.Write(nonce)
	mac.Write(ts)
	mac.Write(body)
	return mac.Sum(nil)
}

// remember records a nonce and reports whether it was new. Entries older than
// twice the skew can never be accepted again on timestamp grounds, so they are
// pruned rather than kept forever.
func (c *Codec) remember(nonce [nonceLen]byte, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.lastPrune) > c.skew {
		cutoff := now.Add(-2 * c.skew)
		for k, seenAt := range c.seen {
			if seenAt.Before(cutoff) {
				delete(c.seen, k)
			}
		}
		c.lastPrune = now
	}
	if _, dup := c.seen[nonce]; dup {
		return false
	}
	c.seen[nonce] = now
	return true
}
