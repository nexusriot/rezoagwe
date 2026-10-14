// Package mdns is a minimal multicast DNS / DNS-SD implementation, enough to
// advertise a rezoagwe node on a LAN and to find the others.
//
// It is written here rather than taken from a library because the whole of
// what this needs is one service type, four record types and no caching, while
// every mDNS library carries a resolver, a cache and a conflict-resolution
// protocol. The rest of this repository depends on four modules; a LAN
// discovery convenience is not the place to add a fifth.
package mdns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Record types, as used by DNS-SD.
const (
	typeA    uint16 = 1
	typePTR  uint16 = 12
	typeTXT  uint16 = 16
	typeSRV  uint16 = 33
	typeAny  uint16 = 255
	classIN  uint16 = 1
	classAny uint16 = 255

	// cacheFlush is the top bit of the class field in a response, telling
	// receivers to replace what they hold rather than add to it.
	cacheFlush uint16 = 0x8000
	// unicastResponse is the top bit of the class field in a *question*,
	// asking for the answer by unicast. Same bit, opposite direction — which
	// is exactly the kind of thing a hand-rolled parser gets wrong, so the two
	// are named apart.
	unicastResponse uint16 = 0x8000
)

var errMalformed = errors.New("malformed DNS message")

// maxNamePointers bounds name-compression following. A message can point a
// name at itself, and a parser that trusts the pointers loops forever on one
// datagram from anywhere on the network.
const maxNamePointers = 16

// message is a parsed DNS message, carrying only what DNS-SD needs.
type message struct {
	id        uint16
	response  bool
	questions []question
	answers   []record
}

type question struct {
	name    string
	qtype   uint16
	qclass  uint16
	unicast bool
}

// record is one resource record. Only the fields of the types this package
// handles are decoded; the rest keep their raw data.
type record struct {
	name  string
	rtype uint16
	class uint16
	ttl   uint32

	// ptr is the target of a PTR record.
	ptr string
	// target and port come from an SRV record.
	target string
	port   uint16
	// txt holds the strings of a TXT record.
	txt []string
	// ip is the address in an A record.
	ip net.IP
}

// encodeName writes a domain name in uncompressed label form.
//
// Compression is only ever read here, never written. Every message this
// package produces is small enough that compressing it saves nothing worth the
// chance of writing a bad pointer, and a receiver is required to accept the
// uncompressed form.
func encodeName(b []byte, name string) ([]byte, error) {
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if len(label) == 0 {
			continue
		}
		if len(label) > 63 {
			return nil, fmt.Errorf("label %q is longer than 63 bytes", label)
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0), nil
}

// decodeName reads a possibly-compressed name, returning it and the offset
// just past it in the *message* (not past whatever a pointer led to).
func decodeName(msg []byte, off int) (string, int, error) {
	var labels []string
	pointers := 0
	next := -1
	for {
		if off >= len(msg) {
			return "", 0, errMalformed
		}
		length := int(msg[off])
		switch {
		case length == 0:
			off++
			if next == -1 {
				next = off
			}
			return strings.Join(labels, ".") + ".", next, nil
		case length&0xc0 == 0xc0:
			if off+1 >= len(msg) {
				return "", 0, errMalformed
			}
			pointers++
			if pointers > maxNamePointers {
				return "", 0, errMalformed
			}
			target := int(binary.BigEndian.Uint16(msg[off:])) & 0x3fff
			if next == -1 {
				next = off + 2
			}
			// A pointer must go backwards. Forward pointers are what a loop
			// is built from, and nothing legitimate emits one.
			if target >= off {
				return "", 0, errMalformed
			}
			off = target
		case length > 63:
			return "", 0, errMalformed
		default:
			off++
			if off+length > len(msg) {
				return "", 0, errMalformed
			}
			labels = append(labels, string(msg[off:off+length]))
			off += length
		}
	}
}

// encode serialises a message.
func (m *message) encode() ([]byte, error) {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:], m.id)
	if m.response {
		// QR | AA: an authoritative answer, which is what every mDNS response
		// is — there is no server to be non-authoritative about.
		binary.BigEndian.PutUint16(b[2:], 0x8400)
	}
	binary.BigEndian.PutUint16(b[4:], uint16(len(m.questions)))
	binary.BigEndian.PutUint16(b[6:], uint16(len(m.answers)))

	var err error
	for _, q := range m.questions {
		if b, err = encodeName(b, q.name); err != nil {
			return nil, err
		}
		class := q.qclass
		if q.unicast {
			class |= unicastResponse
		}
		b = binary.BigEndian.AppendUint16(b, q.qtype)
		b = binary.BigEndian.AppendUint16(b, class)
	}
	for _, r := range m.answers {
		if b, err = encodeName(b, r.name); err != nil {
			return nil, err
		}
		b = binary.BigEndian.AppendUint16(b, r.rtype)
		b = binary.BigEndian.AppendUint16(b, r.class)
		b = binary.BigEndian.AppendUint32(b, r.ttl)

		var data []byte
		switch r.rtype {
		case typePTR:
			if data, err = encodeName(nil, r.ptr); err != nil {
				return nil, err
			}
		case typeSRV:
			data = binary.BigEndian.AppendUint16(data, 0) // priority
			data = binary.BigEndian.AppendUint16(data, 0) // weight
			data = binary.BigEndian.AppendUint16(data, r.port)
			if data, err = encodeName(data, r.target); err != nil {
				return nil, err
			}
		case typeTXT:
			for _, s := range r.txt {
				if len(s) > 255 {
					return nil, fmt.Errorf("TXT string longer than 255 bytes")
				}
				data = append(data, byte(len(s)))
				data = append(data, s...)
			}
			if len(data) == 0 {
				data = []byte{0} // an empty TXT is one empty string, not no data
			}
		case typeA:
			v4 := r.ip.To4()
			if v4 == nil {
				return nil, fmt.Errorf("A record needs an IPv4 address, got %v", r.ip)
			}
			data = append(data, v4...)
		default:
			return nil, fmt.Errorf("cannot encode record type %d", r.rtype)
		}
		b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
		b = append(b, data...)
	}
	return b, nil
}

// decode parses a message. Records whose type this package does not handle are
// kept with their name and type and nothing else, rather than failing the
// whole message: a LAN carries mDNS for printers and phones too.
func decode(buf []byte) (*message, error) {
	if len(buf) < 12 {
		return nil, errMalformed
	}
	m := &message{
		id:       binary.BigEndian.Uint16(buf[0:]),
		response: buf[2]&0x80 != 0,
	}
	qdCount := int(binary.BigEndian.Uint16(buf[4:]))
	// Answers, authority and additional are all read the same way, and DNS-SD
	// responders routinely put the SRV and A records a client needs in the
	// additional section rather than the answer section.
	rrCount := int(binary.BigEndian.Uint16(buf[6:])) +
		int(binary.BigEndian.Uint16(buf[8:])) +
		int(binary.BigEndian.Uint16(buf[10:]))

	off := 12
	for i := 0; i < qdCount; i++ {
		name, next, err := decodeName(buf, off)
		if err != nil {
			return nil, err
		}
		off = next
		if off+4 > len(buf) {
			return nil, errMalformed
		}
		class := binary.BigEndian.Uint16(buf[off+2:])
		m.questions = append(m.questions, question{
			name:    name,
			qtype:   binary.BigEndian.Uint16(buf[off:]),
			qclass:  class &^ unicastResponse,
			unicast: class&unicastResponse != 0,
		})
		off += 4
	}

	for i := 0; i < rrCount && off < len(buf); i++ {
		name, next, err := decodeName(buf, off)
		if err != nil {
			return nil, err
		}
		off = next
		if off+10 > len(buf) {
			return nil, errMalformed
		}
		r := record{
			name:  name,
			rtype: binary.BigEndian.Uint16(buf[off:]),
			class: binary.BigEndian.Uint16(buf[off+2:]) &^ cacheFlush,
			ttl:   binary.BigEndian.Uint32(buf[off+4:]),
		}
		dataLen := int(binary.BigEndian.Uint16(buf[off+8:]))
		off += 10
		if off+dataLen > len(buf) {
			return nil, errMalformed
		}
		data := buf[off : off+dataLen]

		switch r.rtype {
		case typePTR:
			// Names inside record data may point back into the message, so the
			// whole buffer is passed rather than just this record's data.
			if r.ptr, _, err = decodeName(buf, off); err != nil {
				return nil, err
			}
		case typeSRV:
			if dataLen < 6 {
				return nil, errMalformed
			}
			r.port = binary.BigEndian.Uint16(data[4:])
			if r.target, _, err = decodeName(buf, off+6); err != nil {
				return nil, err
			}
		case typeTXT:
			for i := 0; i < len(data); {
				length := int(data[i])
				if i+1+length > len(data) {
					return nil, errMalformed
				}
				r.txt = append(r.txt, string(data[i+1:i+1+length]))
				i += 1 + length
			}
		case typeA:
			if dataLen != 4 {
				return nil, errMalformed
			}
			r.ip = net.IP(append([]byte(nil), data...))
		}
		off += dataLen
		m.answers = append(m.answers, r)
	}
	return m, nil
}
