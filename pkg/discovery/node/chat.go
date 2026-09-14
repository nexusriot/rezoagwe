package node

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

func (n *Node) appendChat(e pb.ChatEntry) {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	n.Model.AppendChat(e)
	n.ev.ChatChanged()
}

// system records a local-only notice (joins, renames, command output). System
// lines are part of the chat log — they are what makes the pane readable — but
// they carry no sender.
func (n *Node) system(format string, args ...interface{}) {
	n.appendChat(pb.ChatEntry{Text: fmt.Sprintf(format, args...), Kind: pb.ChatSystem})
}

func (n *Node) announceJoin(addr, nick string) {
	if nick == "" {
		n.system("%s joined", addr)
		return
	}
	n.system("%s (%s) joined", nick, addr)
}

func (n *Node) announceLeave(addr, nick string) {
	if nick == "" {
		n.system("%s left", addr)
		return
	}
	n.system("%s (%s) left", nick, addr)
}

// SendChat broadcasts a message to every peer and records it locally.
func (n *Node) SendChat(text string) {
	n.sendChatMessage(text, false)
}

// SendAction broadcasts a /me emote.
func (n *Node) SendAction(text string) {
	n.sendChatMessage(text, true)
}

func (n *Node) sendChatMessage(text string, action bool) {
	m := pb.ChatMessage{
		Sender: n.cfg.AdvertiseAddr,
		Nick:   n.Model.Nick(),
		Text:   text,
		TS:     time.Now().Unix(),
		Action: action,
	}
	kind := pb.ChatMsg
	if action {
		kind = pb.ChatAction
	}
	n.appendChat(pb.ChatEntry{TS: m.TS, Sender: m.Sender, Nick: m.Nick, Text: text, Kind: kind})
	n.broadcast(pb.KindChat, m)
}

// SendDM delivers a message to one peer only, addressed by nickname or
// address.
func (n *Node) SendDM(target, text string) error {
	addr := n.resolvePeer(target)
	if addr == "" {
		return fmt.Errorf("no such peer: %s", target)
	}
	m := pb.ChatMessage{
		Sender: n.cfg.AdvertiseAddr,
		Nick:   n.Model.Nick(),
		Text:   text,
		TS:     time.Now().Unix(),
		To:     addr,
	}
	n.send(addr, pb.KindDirectMessage, m)
	n.appendChat(pb.ChatEntry{
		TS:     m.TS,
		Sender: n.cfg.AdvertiseAddr,
		Nick:   m.Nick,
		Text:   text,
		Kind:   pb.ChatDirect,
		To:     addr,
	})
	return nil
}

// resolvePeer maps a nickname or an address to a known peer address.
func (n *Node) resolvePeer(target string) string {
	if n.Model.HasPeer(target) {
		return target
	}
	for _, addr := range n.Model.GetNodes() {
		if strings.EqualFold(n.Model.NickOf(addr), target) {
			return addr
		}
	}
	return ""
}

// SetNick renames this node and tells the cluster.
func (n *Node) SetNick(nick string) {
	prev := n.Model.SetOwnNick(nick)
	n.system("%s is now known as %s", prev, nick)
	n.helloAllPeers()
	n.SendChat(fmt.Sprintf("(was %s)", prev))
}

// Submit handles one line of chat input, including slash commands. Commands
// live here rather than in a front end so the TUI, the HTTP gateway and the
// Android app all understand the same vocabulary.
func (n *Node) Submit(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if !strings.HasPrefix(text, "/") {
		n.SendChat(text)
		return
	}
	cmd, rest := split2(text[1:])
	switch strings.ToLower(cmd) {
	case "help", "?":
		n.system("commands: /nick <name>  /me <text>  /msg <peer> <text>  /peers  " +
			"/keys  /get <key>  /set <key> <value>  /setttl <key> <seconds> <value>  /del <key>")
	case "nick":
		if rest == "" {
			n.system("usage: /nick <name>")
			return
		}
		n.SetNick(rest)
	case "me":
		if rest == "" {
			n.system("usage: /me <text>")
			return
		}
		n.SendAction(rest)
	case "msg", "dm":
		target, body := split2(rest)
		if target == "" || body == "" {
			n.system("usage: /msg <peer> <text>")
			return
		}
		if err := n.SendDM(target, body); err != nil {
			n.system("%s", err)
		}
	case "peers":
		peers := n.Model.GetNodes()
		if len(peers) == 0 {
			n.system("no peers known")
			return
		}
		sort.Strings(peers)
		var b strings.Builder
		for i, p := range peers {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(n.peerName(p))
			if nick := n.Model.NickOf(p); nick != "" {
				fmt.Fprintf(&b, " (%s)", p)
			}
		}
		n.system("peers: %s", b.String())
	case "keys":
		entries := n.Model.Store.Entries()
		if len(entries) == 0 {
			n.system("store is empty")
			return
		}
		keys := make([]string, 0, len(entries))
		for _, e := range entries {
			keys = append(keys, e.Key)
		}
		n.system("keys: %s", strings.Join(keys, ", "))
	case "get":
		if rest == "" {
			n.system("usage: /get <key>")
			return
		}
		if v, ok := n.Model.Store.Get(rest); ok {
			n.system("%s = %s", rest, v)
		} else {
			n.system("%s is not set", rest)
		}
	case "set":
		key, value := split2(rest)
		if key == "" {
			n.system("usage: /set <key> <value>")
			return
		}
		n.Set(key, value, 0)
		n.system("set %s", key)
	case "setttl":
		key, remainder := split2(rest)
		secs, value := split2(remainder)
		ttl, err := strconv.Atoi(secs)
		if key == "" || err != nil || ttl <= 0 {
			n.system("usage: /setttl <key> <seconds> <value>")
			return
		}
		n.Set(key, value, time.Duration(ttl)*time.Second)
		n.system("set %s (expires in %ds)", key, ttl)
	case "del", "delete":
		if rest == "" {
			n.system("usage: /del <key>")
			return
		}
		n.Delete(rest)
		n.system("deleted %s", rest)
	default:
		n.system("unknown command: /%s (try /help)", cmd)
	}
}

// split2 splits off the first whitespace-delimited word.
func split2(s string) (string, string) {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i+1:])
}
