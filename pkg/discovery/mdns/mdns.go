package mdns

import (
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Service is the DNS-SD service type rezoagwe nodes advertise.
const Service = "_rezoagwe._udp.local."

// defaultTTL is how long a receiver may cache our records. Two gossip
// intervals: long enough that a browse between announcements still finds us,
// short enough that a node which leaves stops being offered for long.
const defaultTTL = 120

// multicast addresses and the port, fixed by the mDNS specification.
var (
	ipv4Group = net.IPv4(224, 0, 0, 251)
	mdnsAddr  = &net.UDPAddr{IP: ipv4Group, Port: 5353}
)

// Entry is a discovered node.
type Entry struct {
	// Addr is host:port, ready to hand to the peer table.
	Addr string
	// Cluster is the cluster the node said it belongs to, so a browse can
	// filter before anything tries to talk to it.
	Cluster string
	// Nick is what it calls itself, for the log line.
	Nick string
	// Instance is the DNS-SD instance name, which is unique per node and is
	// what tells two announcements from the same node apart from two nodes.
	Instance string
}

// Config describes what to advertise.
type Config struct {
	// Instance is a name unique to this node within the service.
	Instance string
	// Port is the port peers should connect to.
	Port int
	// IP is the address to advertise. Empty means "work it out", which is
	// right on a machine with one interface and wrong on a machine with
	// several — the same problem -advertise exists for.
	IP net.IP
	// Cluster and Nick are published as TXT records.
	Cluster string
	Nick    string
}

// Server advertises this node and finds others.
//
// It answers queries for the rezoagwe service type only. Anything else on the
// network — and a LAN carries a great deal of mDNS — is read far enough to
// know it is not ours and then dropped.
type Server struct {
	cfg  Config
	conn *net.UDPConn
	// send is a separate unicast socket. Replying on the multicast socket
	// works on Linux and does not on every platform, and a responder that
	// answers nobody is worse than one that is a socket more expensive.
	send *net.UDPConn

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup

	mu      sync.Mutex
	entries map[string]Entry

	rndMu sync.Mutex
	rnd   *rand.Rand
}

// New opens the multicast sockets and starts answering queries.
func New(cfg Config) (*Server, error) {
	if cfg.Instance == "" {
		return nil, fmt.Errorf("mdns: an instance name is required")
	}
	if cfg.IP == nil {
		cfg.IP = localIPv4()
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, mdnsAddr)
	if err != nil {
		return nil, fmt.Errorf("mdns: listen: %w", err)
	}
	send, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mdns: send socket: %w", err)
	}
	s := &Server{
		cfg:     cfg,
		conn:    conn,
		send:    send,
		stop:    make(chan struct{}),
		entries: make(map[string]Entry),
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.serve()
	}()
	return s, nil
}

// Close stops serving and releases the sockets.
func (s *Server) Close() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.conn.Close()
		s.send.Close()
		s.wg.Wait()
	})
}

// instanceName is this node's fully-qualified DNS-SD instance name.
func (s *Server) instanceName() string {
	return escapeInstance(s.cfg.Instance) + "." + Service
}

// hostName is the name the SRV record points at.
func (s *Server) hostName() string {
	return escapeInstance(s.cfg.Instance) + ".local."
}

// escapeInstance makes a nickname safe to use as a DNS label. A label may not
// contain a dot, and a nickname very well may.
func escapeInstance(s string) string {
	s = strings.NewReplacer(".", "-", " ", "-").Replace(s)
	if len(s) > 63 {
		s = s[:63]
	}
	if s == "" {
		s = "node"
	}
	return s
}

// Announce sends an unsolicited response, which is how a node that has just
// started is found before anybody asks.
func (s *Server) Announce() {
	msg := &message{response: true, answers: s.records()}
	s.writeTo(msg, mdnsAddr)
}

// Browse asks who else is there and waits for answers.
//
// It returns everything heard, including entries from other clusters: filtering
// is the caller's, so a node that finds only strangers can say so rather than
// reporting an empty LAN.
func (s *Server) Browse(wait time.Duration) []Entry {
	s.mu.Lock()
	s.entries = make(map[string]Entry)
	s.mu.Unlock()

	q := &message{
		id: uint16(s.random(1 << 16)),
		questions: []question{{
			name:   Service,
			qtype:  typePTR,
			qclass: classIN,
		}},
	}
	s.writeTo(q, mdnsAddr)

	select {
	case <-time.After(wait):
	case <-s.stop:
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	return out
}

func (s *Server) random(n int) int {
	s.rndMu.Lock()
	defer s.rndMu.Unlock()
	return s.rnd.Intn(n)
}

// records builds the four records that describe this node.
func (s *Server) records() []record {
	instance := s.instanceName()
	host := s.hostName()
	out := []record{
		{name: Service, rtype: typePTR, class: classIN, ttl: defaultTTL, ptr: instance},
		{name: instance, rtype: typeSRV, class: classIN | cacheFlush, ttl: defaultTTL,
			target: host, port: uint16(s.cfg.Port)},
		{name: instance, rtype: typeTXT, class: classIN | cacheFlush, ttl: defaultTTL,
			txt: []string{"cluster=" + s.cfg.Cluster, "nick=" + s.cfg.Nick}},
	}
	if ip := s.cfg.IP.To4(); ip != nil {
		out = append(out, record{name: host, rtype: typeA, class: classIN | cacheFlush,
			ttl: defaultTTL, ip: ip})
	}
	return out
}

func (s *Server) writeTo(m *message, addr *net.UDPAddr) {
	buf, err := m.encode()
	if err != nil {
		log.Debugf("mdns: encode: %s", err)
		return
	}
	if _, err := s.send.WriteToUDP(buf, addr); err != nil {
		log.Debugf("mdns: write to %s: %s", addr, err)
	}
}

func (s *Server) serve() {
	buf := make([]byte, 9000)
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.stop:
				return
			default:
			}
			log.Debugf("mdns: read: %s", err)
			continue
		}
		msg, err := decode(buf[:n])
		if err != nil {
			// A LAN carries mDNS from everything; one unparseable datagram is
			// not an event.
			continue
		}
		if msg.response {
			s.collect(msg)
			continue
		}
		s.answer(msg, from)
	}
}

// answer responds to a query for our service type.
func (s *Server) answer(msg *message, from *net.UDPAddr) {
	wanted := false
	unicast := false
	for _, q := range msg.questions {
		if !strings.EqualFold(q.name, Service) && !strings.EqualFold(q.name, s.instanceName()) {
			continue
		}
		if q.qtype == typePTR || q.qtype == typeAny || q.qtype == typeSRV || q.qtype == typeTXT {
			wanted = true
			unicast = unicast || q.unicast
		}
	}
	if !wanted {
		return
	}
	reply := &message{id: msg.id, response: true, answers: s.records()}
	if unicast {
		s.writeTo(reply, from)
		return
	}
	// A multicast answer, delayed a little. Every node on the LAN heard the
	// same question, and answering in the same instant is how a browse turns
	// into a collision.
	time.Sleep(time.Duration(s.random(100)) * time.Millisecond)
	s.writeTo(reply, mdnsAddr)
}

// collect folds a response into the discovered set.
//
// The records that matter are spread across a response: the PTR names the
// instance, the SRV gives its port and host, the A gives that host's address
// and the TXT says which cluster it is in. They are assembled by name, because
// nothing guarantees they arrive in one message or in any order.
func (s *Server) collect(msg *message) {
	srv := map[string]record{}
	txt := map[string][]string{}
	hosts := map[string]net.IP{}
	instances := map[string]bool{}

	for _, r := range msg.answers {
		switch r.rtype {
		case typePTR:
			if strings.EqualFold(r.name, Service) {
				instances[r.ptr] = true
			}
		case typeSRV:
			// Only instances under our own service type. A LAN carries SRV
			// records for printers, phones and media players, and taking any
			// of them put a stranger's address into the peer table — observed
			// on a real network, where a phone advertising a nearby-presence
			// service was collected as a rezoagwe node.
			if ownInstance(r.name) {
				srv[r.name] = r
				instances[r.name] = true
			}
		case typeTXT:
			if ownInstance(r.name) {
				txt[r.name] = r.txt
			}
		case typeA:
			// Kept unconditionally: an A record is named for a host, not for a
			// service, so there is nothing in its name to match on. It is only
			// ever read through an SRV target that already passed the check.
			hosts[r.name] = r.ip
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for instance := range instances {
		if !ownInstance(instance) {
			continue
		}
		if strings.EqualFold(instance, s.instanceName()) {
			continue // our own announcement, echoed back by the group
		}
		rec, ok := srv[instance]
		if !ok {
			// A PTR with no SRV names a node without saying where it is. The
			// browse that follows will ask again; there is nothing to record.
			continue
		}
		ip, ok := hosts[rec.target]
		if !ok {
			continue
		}
		e := Entry{
			Instance: instance,
			Addr:     net.JoinHostPort(ip.String(), strconv.Itoa(int(rec.port))),
		}
		for _, kv := range txt[instance] {
			key, value, found := strings.Cut(kv, "=")
			if !found {
				continue
			}
			switch key {
			case "cluster":
				e.Cluster = value
			case "nick":
				e.Nick = value
			}
		}
		s.entries[instance] = e
	}
}

// ownInstance reports whether a DNS-SD instance name sits under our service
// type, which is the only thing that makes it ours.
func ownInstance(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), "."+strings.ToLower(Service))
}

// localIPv4 is this machine's first routable IPv4 address.
func localIPv4() net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4
		}
	}
	return nil
}
