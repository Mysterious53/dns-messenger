package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPHandler returns an http.Handler that exposes the chat REST API.
// All mutating endpoints require the correct passphrase in the request body.
func (s *Server) HTTPHandler(staticDir string) http.Handler {
	mux := http.NewServeMux()

	// Static web UI
	if staticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(staticDir)))
	}

	mux.HandleFunc("/api/rooms", s.handleRooms)
	mux.HandleFunc("/api/rooms/", s.handleRoomMessages)
	mux.HandleFunc("/api/messages", s.handleSendMessage)
	mux.HandleFunc("/api/auth", s.handleAuth)

	return mux
}

type apiRoom struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	IsDM     bool   `json:"is_dm"`
	MsgCount int    `json:"msg_count"`
}

type apiMessage struct {
	ID        uint32 `json:"id"`
	Timestamp int64  `json:"ts"`
	Sender    string `json:"sender"`
	Text      string `json:"text"`
}

func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.checkPassphrase(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.chat == nil {
		jsonOK(w, []apiRoom{})
		return
	}
	rooms := s.chat.Rooms()
	out := make([]apiRoom, len(rooms))
	for i, name := range rooms {
		ch := i + 1
		msgs := s.chat.Messages(ch)
		out[i] = apiRoom{
			ID:       ch,
			Name:     name,
			IsDM:     strings.HasPrefix(name, "dm:"),
			MsgCount: len(msgs),
		}
	}
	jsonOK(w, out)
}

func (s *Server) handleRoomMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.checkPassphrase(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Path: /api/rooms/{id}/messages
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/rooms/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "missing room id", http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(parts[0])
	if err != nil || id < 1 {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}
	if s.chat == nil {
		jsonOK(w, []apiMessage{})
		return
	}
	msgs := s.chat.Messages(id)
	out := make([]apiMessage, len(msgs))
	for i, m := range msgs {
		sender, text := parseMsgText(m.Text)
		out[i] = apiMessage{
			ID:        m.ID,
			Timestamp: int64(m.Timestamp),
			Sender:    sender,
			Text:      text,
		}
	}
	jsonOK(w, out)
}

// handleSendMessage handles POST /api/messages
// Body JSON: {"channel_id": 1, "sender": "alice", "text": "hello", "passphrase": "secret"}
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ChannelID  int    `json:"channel_id"`
		Sender     string `json:"sender"`
		Text       string `json:"text"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Passphrase != s.cfg.Passphrase {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if body.Sender == "" {
		body.Sender = "anon"
	}
	if body.Text == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	if s.chat == nil {
		http.Error(w, "server not ready", http.StatusServiceUnavailable)
		return
	}
	payload := body.Sender + "\n" + body.Text
	if err := s.chat.SendMessage(r.Context(), body.ChannelID, payload); err != nil {
		http.Error(w, fmt.Sprintf("send failed: %v", err), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// handleAuth handles GET /api/auth?passphrase=secret
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	pp := r.URL.Query().Get("passphrase")
	if pp == "" {
		pp = r.Header.Get("X-Passphrase")
	}
	if pp == s.cfg.Passphrase {
		jsonOK(w, map[string]any{"ok": true, "rooms": s.chat.Rooms()})
	} else {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (s *Server) checkPassphrase(r *http.Request) bool {
	pp := r.URL.Query().Get("passphrase")
	if pp == "" {
		pp = r.Header.Get("X-Passphrase")
	}
	return pp == s.cfg.Passphrase
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// parseMsgText extracts sender and text from "[sender] text" format.
func parseMsgText(text string) (sender, body string) {
	if strings.HasPrefix(text, "[") {
		end := strings.Index(text, "] ")
		if end > 1 {
			return text[1:end], text[end+2:]
		}
	}
	return "anon", text
}

// unused but keeps the time import satisfied
var _ = time.Second
