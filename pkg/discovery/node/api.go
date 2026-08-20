package node

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// Join runs the bootstrap handshake and the initial state pull. It dials
// bootstrap and a peer, either of which can block on an unreachable host, so a
// front end should call it on its own goroutine.
func (n *Node) Join() { n.join() }

// Set writes a key and replicates it. A non-zero ttl expires the key on every
// replica at the same instant.
func (n *Node) Set(key, value string, ttl time.Duration) pb.KVUpdate {
	u, _ := n.write(key, value, ttl, nil)
	return u
}

// CompareAndSet writes only if the key currently holds expect, and reports
// whether it did. A zero expect requires the key to be absent, which is what
// turns this store into something you can build a lock or a leader election on.
func (n *Node) CompareAndSet(key, value string, ttl time.Duration, expect pb.Version) (pb.KVUpdate, bool) {
	return n.write(key, value, ttl, &expect)
}

func (n *Node) write(key, value string, ttl time.Duration, expect *pb.Version) (pb.KVUpdate, bool) {
	opt := model.WriteOptions{Expect: expect}
	if ttl > 0 {
		opt.ExpiresAt = time.Now().Add(ttl).Unix()
	}
	u, ok := n.Model.Store.Write(key, value, opt)
	if !ok {
		n.Metrics.KVCASFailures.Add(1)
		return u, false
	}
	n.Metrics.KVLocalWrites.Add(1)
	n.broadcast(pb.KindKV, u)
	n.ev.KVChanged()
	return u, true
}

// Delete tombstones a key and replicates the tombstone.
func (n *Node) Delete(key string) pb.KVUpdate {
	u, _ := n.remove(key, nil)
	return u
}

// CompareAndDelete deletes only if the key currently holds expect.
func (n *Node) CompareAndDelete(key string, expect pb.Version) (pb.KVUpdate, bool) {
	return n.remove(key, &expect)
}

func (n *Node) remove(key string, expect *pb.Version) (pb.KVUpdate, bool) {
	u, ok := n.Model.Store.Remove(key, model.WriteOptions{Expect: expect})
	if !ok {
		n.Metrics.KVCASFailures.Add(1)
		return u, false
	}
	n.Metrics.KVLocalWrites.Add(1)
	n.broadcast(pb.KindKV, u)
	n.ev.KVChanged()
	return u, true
}

func (n *Node) Get(key string) (string, bool) { return n.Model.Store.Get(key) }

func (n *Node) Entry(key string) (model.Entry, bool) { return n.Model.Store.GetEntry(key) }

func (n *Node) Entries() []model.Entry { return n.Model.Store.Entries() }

func (n *Node) History(key string) []model.HistoryEntry { return n.Model.Store.History(key) }

// PeerInfo is a peer as this node currently sees it.
type PeerInfo struct {
	Addr     string `json:"addr"`
	Nick     string `json:"nick,omitempty"`
	LastSeen int64  `json:"last_seen,omitempty"`
	AgeSec   int64  `json:"age_sec"`
}

// Peers lists known peers, sorted by address.
func (n *Node) Peers() []PeerInfo {
	addrs := n.Model.GetNodes()
	out := make([]PeerInfo, 0, len(addrs))
	for _, a := range addrs {
		info := PeerInfo{Addr: a, Nick: n.Model.NickOf(a)}
		if ts, ok := n.Model.LastSeen(a); ok {
			info.LastSeen = ts.Unix()
			info.AgeSec = int64(time.Since(ts).Seconds())
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// Chat returns the chat log, oldest first.
func (n *Node) Chat() []pb.ChatEntry { return n.Model.ChatLog() }

// ExportFile is the on-disk interchange format. It carries versions, so an
// import can merge into a live cluster without pretending the data is new.
type ExportFile struct {
	Node       string        `json:"node"`
	Cluster    string        `json:"cluster"`
	ExportedAt int64         `json:"exported_at"`
	Entries    []pb.KVUpdate `json:"entries"`
}

// Export serialises the whole store, tombstones included.
func (n *Node) Export() ([]byte, error) {
	return json.MarshalIndent(ExportFile{
		Node:       n.cfg.NodeAddr,
		Cluster:    n.cfg.Cluster,
		ExportedAt: time.Now().Unix(),
		Entries:    n.Model.Store.Updates(),
	}, "", "  ")
}

// Import loads an export.
//
// asLocalWrites decides how the data joins the cluster: preserving the exported
// versions merges it under the usual last-write-wins rule (so an entry older
// than what the cluster holds is correctly ignored), while re-stamping every
// entry as a fresh local write forces it to win. The first is a restore, the
// second is a seed.
func (n *Node) Import(data []byte, asLocalWrites bool) (int, error) {
	var f ExportFile
	if err := json.Unmarshal(data, &f); err != nil {
		return 0, err
	}
	applied := 0
	for _, u := range f.Entries {
		if asLocalWrites {
			if u.Action == pb.KVDelete {
				n.Delete(u.Key)
			} else {
				var ttl time.Duration
				if u.ExpiresAt > 0 {
					ttl = time.Until(time.Unix(u.ExpiresAt, 0))
					if ttl <= 0 {
						continue // already expired; nothing to seed
					}
				}
				n.Set(u.Key, u.Value, ttl)
			}
			applied++
			continue
		}
		if n.Model.Store.Apply(u) {
			applied++
			n.broadcast(pb.KindKV, u)
		}
	}
	if applied > 0 {
		n.logActivity("imported %d entr%s", applied, plural(applied))
		n.ev.KVChanged()
	}
	return applied, nil
}

// Status is a snapshot of node health for status bars and /health.
type Status struct {
	Addr       string `json:"addr"`
	Nick       string `json:"nick"`
	NodeID     string `json:"node_id"`
	Cluster    string `json:"cluster"`
	Peers      int    `json:"peers"`
	Keys       int    `json:"keys"`
	Tombstones int    `json:"tombstones"`
	UptimeSec  int64  `json:"uptime_sec"`
	Connected  bool   `json:"connected"`
}

func (n *Node) Status() Status {
	peers := len(n.Model.GetNodes())
	return Status{
		Addr:       n.cfg.NodeAddr,
		Nick:       n.Model.Nick(),
		NodeID:     n.Model.NodeID,
		Cluster:    n.cfg.Cluster,
		Peers:      peers,
		Keys:       n.Model.Store.Len(),
		Tombstones: n.Model.Store.Tombstones(),
		UptimeSec:  int64(n.Uptime().Seconds()),
		Connected:  peers > 0,
	}
}
