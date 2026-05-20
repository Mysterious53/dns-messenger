package server

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mahdi/dnsmessenger/internal/protocol"
)

// GitHubRelayConfig configures the optional GitHub fast-relay.
type GitHubRelayConfig struct {
	Enabled    bool
	Token      string
	Repo       string
	Branch     string
	StatePath  string
	MaxBytes   int64
	TTLMinutes int
}

func (g GitHubRelayConfig) Active() bool {
	return g.Enabled && g.Token != "" && g.Repo != ""
}

// Config holds DNS Messenger server configuration.
type Config struct {
	ListenAddr  string
	Domain      string
	Passphrase  string
	RoomsFile   string
	MaxPadding  int
	MsgLimit    int           // max messages per room in memory (0 = default 50)
	AllowManage bool          // if true, DNS send/admin commands are accepted
	FetchInterval time.Duration // unused; kept for future use
	Debug       bool
}

// Server orchestrates the DNS server and chat store.
type Server struct {
	cfg   Config
	feed  *Feed
	chat  *ChatServer
	rooms []string
}

// New creates a new Server. Returns error if no rooms are configured.
func New(cfg Config) (*Server, error) {
	rooms, err := loadRooms(cfg.RoomsFile)
	if err != nil {
		return nil, fmt.Errorf("load rooms: %w", err)
	}
	if len(rooms) == 0 {
		return nil, fmt.Errorf("no rooms configured in %s", cfg.RoomsFile)
	}
	log.Printf("[server] loaded %d rooms from %s", len(rooms), cfg.RoomsFile)

	feed := NewFeed(rooms)
	return &Server{cfg: cfg, feed: feed, rooms: rooms}, nil
}

// Run starts the DNS server (blocking). Cancel ctx to stop.
func (s *Server) Run(ctx context.Context) error {
	queryKey, responseKey, err := protocol.DeriveKeys(s.cfg.Passphrase)
	if err != nil {
		return fmt.Errorf("derive keys: %w", err)
	}

	msgLimit := s.cfg.MsgLimit
	if msgLimit <= 0 {
		msgLimit = 50
	}

	chat := NewChatServer(s.feed, s.rooms, msgLimit)
	s.chat = chat

	maxPad := s.cfg.MaxPadding
	if maxPad == 0 {
		maxPad = protocol.DefaultMaxPadding
	}

	go startLatestVersionTracker(ctx, s.feed)

	dnsServer := NewDNSServer(
		s.cfg.ListenAddr,
		s.cfg.Domain,
		s.feed,
		queryKey, responseKey,
		maxPad,
		chat,
		s.cfg.AllowManage,
		s.cfg.RoomsFile,
		s.cfg.Debug,
	)
	dnsServer.SetChannelRefresher(chat)

	return dnsServer.ListenAndServe(ctx)
}

// Chat returns the ChatServer (nil before Run is called).
func (s *Server) Chat() *ChatServer {
	return s.chat
}

func loadRooms(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var rooms []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rooms = append(rooms, strings.TrimPrefix(line, "@"))
	}
	return rooms, sc.Err()
}
