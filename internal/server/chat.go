package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mahdi/dnsmessenger/internal/protocol"
)

// ChatServer replaces TelegramReader. It stores chat messages in RAM and
// rebuilds DNS blocks on every new message — no external services needed.
// Implements MessageSender (for dns.go) and channelRefresher.
type ChatServer struct {
	feed     *Feed
	mu       sync.RWMutex
	rooms    []string                   // ordered room names (matches feed channel numbers)
	messages map[int][]protocol.Message // 1-based channel index → messages
	msgLimit int                        // max messages kept per room
	idGen    atomic.Uint32              // monotonic message-ID counter
}

// NewChatServer creates a ChatServer with the given rooms pre-loaded.
// The Feed must already be initialized with the same room names.
func NewChatServer(feed *Feed, rooms []string, msgLimit int) *ChatServer {
	if msgLimit <= 0 {
		msgLimit = 50
	}
	cs := &ChatServer{
		feed:     feed,
		rooms:    append([]string{}, rooms...),
		messages: make(map[int][]protocol.Message),
		msgLimit: msgLimit,
	}
	// Mark server as ready (reuses the TelegramLoggedIn flag in metadata).
	feed.SetTelegramLoggedIn(true)
	for i, name := range rooms {
		log.Printf("[chat] room %d: %s", i+1, name)
	}
	return cs
}

// SendMessage is called by the DNS server when it receives a complete upstream
// send payload. Payload format produced by the client: "username\nmessage text".
// channelNum is 1-based and must correspond to a configured room.
func (cs *ChatServer) SendMessage(_ context.Context, channelNum int, payload string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if channelNum < 1 || channelNum > len(cs.rooms) {
		return fmt.Errorf("chat: channel %d out of range (have %d rooms)", channelNum, len(cs.rooms))
	}

	// Parse "username\nbody" produced by the chat client.
	sender := "anon"
	body := payload
	if idx := strings.IndexByte(payload, '\n'); idx >= 0 {
		if s := strings.TrimSpace(payload[:idx]); s != "" {
			sender = s
		}
		body = strings.TrimSpace(payload[idx+1:])
	}
	if body == "" {
		return fmt.Errorf("chat: empty message body")
	}

	id := cs.idGen.Add(1)
	msg := protocol.Message{
		ID:        id,
		Timestamp: uint32(time.Now().Unix()),
		// Embed sender in Text so all protocol-compatible clients display it.
		Text: "[" + sender + "] " + body,
	}

	msgs := append(cs.messages[channelNum], msg)
	if len(msgs) > cs.msgLimit {
		msgs = msgs[len(msgs)-cs.msgLimit:]
	}
	cs.messages[channelNum] = msgs

	// UpdateChannel handles serialize → compress → split → meta rebuild.
	cs.feed.UpdateChannel(channelNum, msgs)
	log.Printf("[chat] ch=%d(%s) from=%s: %s", channelNum, cs.rooms[channelNum-1], sender, body)
	return nil
}

// UpdateChannels replaces the room list (called by admin add/remove via dns.go).
func (cs *ChatServer) UpdateChannels(rooms []string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.rooms = append([]string{}, rooms...)
	cs.feed.SetChannels(rooms)
}

// RequestRefresh rebuilds all channel blocks from current in-memory messages.
func (cs *ChatServer) RequestRefresh() {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	for i := range cs.rooms {
		ch := i + 1
		cs.feed.UpdateChannel(ch, cs.messages[ch])
	}
	log.Printf("[chat] refresh: rebuilt %d rooms", len(cs.rooms))
}

// Rooms returns a snapshot of current room names.
func (cs *ChatServer) Rooms() []string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return append([]string{}, cs.rooms...)
}

// Messages returns a snapshot of messages for channelNum (1-based).
func (cs *ChatServer) Messages(channelNum int) []protocol.Message {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	src := cs.messages[channelNum]
	if len(src) == 0 {
		return nil
	}
	out := make([]protocol.Message, len(src))
	copy(out, src)
	return out
}
