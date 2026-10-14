package proto

import (
	"fmt"
	"testing"
)

// BenchmarkEncode and BenchmarkDecode measure the per-packet cost of framing
// and authentication, which every message in the system pays twice.
func BenchmarkEncode(b *testing.B) {
	c := NewCodec("psk", "cluster")
	u := KVUpdate{Action: KVSet, Key: "key/00000001", Value: "a value of ordinary size",
		Version: Version{Counter: 42, Node: "node-a"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := c.Encode(KindKV, u); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	c := NewCodec("psk", "cluster")
	u := KVUpdate{Action: KVSet, Key: "key/00000001", Value: "a value of ordinary size",
		Version: Version{Counter: 42, Node: "node-a"}}
	pkt, err := c.Encode(KindKV, u)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A fresh codec each time: reusing one would reject every iteration
		// after the first as a replay and measure the rejection path instead.
		dec := NewCodec("psk", "cluster")
		if _, _, err := dec.Decode(pkt); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEscapedLen is paid on every write, local or remote, before anything
// is stored — so its cost is admission control's cost.
func BenchmarkEscapedLen(b *testing.B) {
	for _, size := range []int{64, 4096, 1 << 20} {
		value := make([]byte, size)
		for i := range value {
			value[i] = byte('a' + i%26)
		}
		s := string(value)
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				EscapedLen(s)
			}
		})
	}
}
