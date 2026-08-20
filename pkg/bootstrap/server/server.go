// Package server is the rezoagwe rendezvous service: it knows the set of node
// addresses and nothing else — no KV data, no chat.
package server

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/rezoagwe/pkg/bootstrap/model"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// streamTimeout bounds one request/response exchange on a stream.
const streamTimeout = 10 * time.Second

type Config struct {
	Port        int
	NodeTimeout time.Duration
	DataPath    string
	PSK         string
	Cluster     string

	// Transport is injectable for tests; nil means UDP + TCP on :Port.
	Transport transport.Transport
}

// Server answers REGISTER and DISCOVER.
type Server struct {
	Model *model.Model

	codec *pb.Codec
	tr    transport.Transport

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup

	changeMu sync.Mutex
	onChange func()
}

func New(cfg Config) (*Server, error) {
	if cfg.NodeTimeout == 0 {
		cfg.NodeTimeout = 30 * time.Second
	}
	if cfg.Cluster == "" {
		cfg.Cluster = pb.DefaultCluster
	}
	tr := cfg.Transport
	if tr == nil {
		udp, err := transport.ListenUDP(fmt.Sprintf(":%d", cfg.Port))
		if err != nil {
			return nil, err
		}
		tr = udp
	}
	return &Server{
		Model: model.NewModel(cfg.Port, cfg.NodeTimeout, cfg.DataPath),
		codec: pb.NewCodec(cfg.PSK, cfg.Cluster),
		tr:    tr,
		stop:  make(chan struct{}),
	}, nil
}

// OnChange registers a callback fired whenever the roster changes.
func (s *Server) OnChange(f func()) {
	s.changeMu.Lock()
	s.onChange = f
	s.changeMu.Unlock()
}

func (s *Server) notify() {
	s.changeMu.Lock()
	f := s.onChange
	s.changeMu.Unlock()
	if f != nil {
		f()
	}
}

func (s *Server) Start() {
	s.loop(s.packetLoop)
	s.loop(s.streamLoop)
	s.loop(s.sweepLoop)
}

func (s *Server) loop(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.tr.Close()
		s.wg.Wait()
	})
}

func (s *Server) packetLoop() {
	packets := s.tr.Packets()
	for {
		select {
		case <-s.stop:
			return
		case pkt, ok := <-packets:
			if !ok {
				return
			}
			kind, body, err := s.codec.Decode(pkt.Data)
			if err != nil {
				// An unauthenticated packet is not a protocol event worth
				// reporting: on an open port it is background noise.
				log.Debugf("drop %s packet from %s: %s", kind, pkt.From, err)
				continue
			}
			s.handle(kind, body, func(reply []byte) {
				s.tr.Send(replyAddr(pkt.From, body, kind), reply)
			})
		}
	}
}

// replyAddr decides where a datagram answer goes. The address a node advertises
// is authoritative — a node bound to a wildcard address is reachable there,
// while the source port of its datagram may be ephemeral.
func replyAddr(from string, body []byte, kind pb.MessageKind) string {
	if kind == pb.KindBootstrapDiscover {
		var req pb.BootstrapDiscover
		if err := json.Unmarshal(body, &req); err == nil && req.From != "" {
			return req.From
		}
	}
	return from
}

func (s *Server) streamLoop() {
	streams := s.tr.Streams()
	for {
		select {
		case <-s.stop:
			return
		case conn, ok := <-streams:
			if !ok {
				return
			}
			go s.serveStream(conn)
		}
	}
}

func (s *Server) serveStream(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(streamTimeout))

	frame, err := transport.ReadFrame(conn)
	if err != nil {
		log.Debugf("read stream frame: %s", err)
		return
	}
	kind, body, err := s.codec.Decode(frame)
	if err != nil {
		log.Debugf("drop stream frame: %s", err)
		return
	}
	s.handle(kind, body, func(reply []byte) {
		if err := transport.WriteFrame(conn, reply); err != nil {
			log.Debugf("write stream reply: %s", err)
		}
	})
}

// handle processes one authenticated message, calling reply with an encoded
// response when there is one.
func (s *Server) handle(kind pb.MessageKind, body []byte, reply func([]byte)) {
	switch kind {
	case pb.KindBootstrapRegister:
		var reg pb.BootstrapRegister
		if err := json.Unmarshal(body, &reg); err != nil {
			return
		}
		if reg.From == "" {
			return
		}
		if s.Model.RegisterNode(reg.From, reg.Nick) {
			log.Debugf("registered node %s (%s)", reg.From, reg.Nick)
			s.notify()
		}
	case pb.KindBootstrapDiscover:
		var req pb.BootstrapDiscover
		if err := json.Unmarshal(body, &req); err != nil {
			return
		}
		// A node that asks for the roster is alive: record it, so discovery
		// alone is enough to join even if the REGISTER was lost.
		if req.From != "" && s.Model.RegisterNode(req.From, "") {
			s.notify()
		}
		pkt, err := s.codec.Encode(pb.KindBootstrapRoster, s.Model.Roster(req.From))
		if err != nil {
			log.Errorf("encode roster: %s", err)
			return
		}
		reply(pkt)
	default:
		log.Debugf("unexpected kind on bootstrap: %s", kind)
	}
}

func (s *Server) sweepLoop() {
	interval := s.Model.NodeTimeout / 2
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if removed := s.Model.RemoveStaleNodes(); len(removed) > 0 {
				for _, n := range removed {
					log.Debugf("dropped stale node %s", n.Addr)
				}
				s.notify()
			}
		}
	}
}
