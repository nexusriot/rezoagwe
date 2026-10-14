package mdns

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestNameRoundTrip(t *testing.T) {
	for _, name := range []string{
		"local.",
		Service,
		"abc._rezoagwe._udp.local.",
		"a.b.c.d.e.local.",
	} {
		buf, err := encodeName(nil, name)
		if err != nil {
			t.Fatalf("encode %q: %v", name, err)
		}
		got, off, err := decodeName(buf, 0)
		if err != nil {
			t.Fatalf("decode %q: %v", name, err)
		}
		if got != name {
			t.Errorf("round trip = %q, want %q", got, name)
		}
		if off != len(buf) {
			t.Errorf("offset = %d, want %d", off, len(buf))
		}
	}
}

func TestMessageRoundTrip(t *testing.T) {
	m := &message{
		id:       0x1234,
		response: true,
		answers: []record{
			{name: Service, rtype: typePTR, class: classIN, ttl: 120, ptr: "node-a." + Service},
			{name: "node-a." + Service, rtype: typeSRV, class: classIN, ttl: 120,
				target: "node-a.local.", port: 3137},
			{name: "node-a." + Service, rtype: typeTXT, class: classIN, ttl: 120,
				txt: []string{"cluster=rezoagwe", "nick=alice"}},
			{name: "node-a.local.", rtype: typeA, class: classIN, ttl: 120,
				ip: net.IPv4(10, 0, 0, 4)},
		},
	}
	buf, err := m.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decode(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.response || got.id != 0x1234 {
		t.Errorf("header lost: response=%v id=%#x", got.response, got.id)
	}
	if len(got.answers) != 4 {
		t.Fatalf("decoded %d answers, want 4", len(got.answers))
	}
	if got.answers[0].ptr != "node-a."+Service {
		t.Errorf("PTR = %q", got.answers[0].ptr)
	}
	if got.answers[1].port != 3137 || got.answers[1].target != "node-a.local." {
		t.Errorf("SRV = %q:%d", got.answers[1].target, got.answers[1].port)
	}
	if len(got.answers[2].txt) != 2 || got.answers[2].txt[0] != "cluster=rezoagwe" {
		t.Errorf("TXT = %v", got.answers[2].txt)
	}
	if !got.answers[3].ip.Equal(net.IPv4(10, 0, 0, 4)) {
		t.Errorf("A = %v", got.answers[3].ip)
	}
}

func TestQuestionRoundTrip(t *testing.T) {
	m := &message{id: 7, questions: []question{{name: Service, qtype: typePTR, qclass: classIN, unicast: true}}}
	buf, err := m.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decode(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.questions) != 1 {
		t.Fatalf("decoded %d questions, want 1", len(got.questions))
	}
	q := got.questions[0]
	if q.name != Service || q.qtype != typePTR {
		t.Errorf("question = %q/%d", q.name, q.qtype)
	}
	// The unicast-response bit shares the class field with cache-flush, and
	// confusing the two is how a responder ends up answering the wrong way.
	if !q.unicast {
		t.Error("unicast-response bit lost")
	}
	if q.qclass != classIN {
		t.Errorf("qclass = %#x, want the bit stripped", q.qclass)
	}
}

// Compression is never written here but has to be read: other responders use
// it, and a name that points back into the message is the normal case.
func TestDecodeFollowsCompressionPointers(t *testing.T) {
	var buf []byte
	buf = append(buf, make([]byte, 12)...)
	binary.BigEndian.PutUint16(buf[6:], 1) // one answer

	// A PTR record whose data is a pointer to the name at the start of it.
	nameOff := len(buf)
	buf, _ = encodeName(buf, Service)
	buf = binary.BigEndian.AppendUint16(buf, typePTR)
	buf = binary.BigEndian.AppendUint16(buf, classIN)
	buf = binary.BigEndian.AppendUint32(buf, 120)
	buf = binary.BigEndian.AppendUint16(buf, 2) // rdlength: just the pointer
	buf = binary.BigEndian.AppendUint16(buf, 0xc000|uint16(nameOff))

	got, err := decode(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.answers) != 1 || got.answers[0].ptr != Service {
		t.Fatalf("compressed PTR = %+v, want %q", got.answers, Service)
	}
}

// A message can point a name at itself. A parser that trusts the pointer never
// returns, on one datagram from anywhere on the network.
func TestDecodeRefusesPointerLoops(t *testing.T) {
	var buf []byte
	buf = append(buf, make([]byte, 12)...)
	binary.BigEndian.PutUint16(buf[4:], 1) // one question
	// A pointer at offset 12 pointing at offset 12.
	buf = binary.BigEndian.AppendUint16(buf, 0xc00c)
	buf = binary.BigEndian.AppendUint16(buf, typePTR)
	buf = binary.BigEndian.AppendUint16(buf, classIN)

	if _, err := decode(buf); err == nil {
		t.Error("a self-referential name was accepted")
	}
}

// Every byte here arrives from whatever else is on the LAN.
func TestDecodeRejectsTruncatedMessages(t *testing.T) {
	m := &message{response: true, answers: []record{
		{name: Service, rtype: typePTR, class: classIN, ttl: 120, ptr: "node-a." + Service},
	}}
	full, err := m.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 0; i < len(full); i++ {
		// Must not panic. Most prefixes are errors; a few may parse as a
		// message with no records, which is also fine.
		_, _ = decode(full[:i])
	}
}

func TestEscapeInstanceMakesALegalLabel(t *testing.T) {
	cases := map[string]string{
		"alice":                   "alice",
		"alice.bob":               "alice-bob",
		"two words":               "two-words",
		"":                        "node",
		string(make([]byte, 100)): "",
	}
	for in, want := range cases {
		got := escapeInstance(in)
		if len(got) > 63 {
			t.Errorf("escapeInstance(%q) is %d bytes, over the 63-byte label limit", in, len(got))
		}
		if want != "" && got != want {
			t.Errorf("escapeInstance(%q) = %q, want %q", in, got, want)
		}
		if _, err := encodeName(nil, got+"."+Service); err != nil {
			t.Errorf("escapeInstance(%q) produced an unencodable name: %v", in, err)
		}
	}
}
