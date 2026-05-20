package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mahdi/dnsmessenger/internal/client"
	"github.com/mahdi/dnsmessenger/internal/protocol"
)

// savedConfig is what gets persisted to config.json.
type savedConfig struct {
	Domain     string   `json:"domain"`
	Passphrase string   `json:"passphrase"`
	Username   string   `json:"username"`
	Resolvers  []string `json:"resolvers"`
}

// app holds all runtime state and owns the fetcher lifecycle.
type app struct {
	mu      sync.RWMutex
	cfg     savedConfig
	fetcher *client.Fetcher
	checker *client.ResolverChecker
	scanner *client.ResolverScanner
	// fetchCancel cancels the context for the currently running fetcher/checker.
	fetchCancel context.CancelFunc

	dataDir string
	debug   bool
	rootCtx context.Context
}

// configured reports whether a server is currently set up.
func (a *app) configured() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.fetcher != nil
}

// connect initialises (or re-initialises) the fetcher with new credentials,
// saves the config to disk, and starts background goroutines.
func (a *app) connect(cfg savedConfig) error {
	if cfg.Domain == "" || cfg.Passphrase == "" {
		return fmt.Errorf("domain and passphrase are required")
	}
	if len(cfg.Resolvers) == 0 {
		cfg.Resolvers = []string{"8.8.8.8:53", "1.1.1.1:53", "9.9.9.9:53", "208.67.222.222:53"}
	}
	if cfg.Username == "" {
		cfg.Username = "anon"
	}

	newFetcher, err := client.NewFetcher(cfg.Domain, cfg.Passphrase, cfg.Resolvers)
	if err != nil {
		return fmt.Errorf("init fetcher: %w", err)
	}
	if a.debug {
		newFetcher.SetDebug(true)
	}
	newFetcher.SetLogFunc(func(msg string) { log.Println("[dns]", msg) })

	newChecker := client.NewResolverChecker(newFetcher, 15*time.Second)
	newChecker.SetLogFunc(func(msg string) { log.Println("[checker]", msg) })
	newChecker.SetAutoScan(true)

	fetchCtx, cancel := context.WithCancel(a.rootCtx)

	a.mu.Lock()
	// Stop the previous fetcher/checker if running.
	if a.fetchCancel != nil {
		a.fetchCancel()
	}
	a.cfg = cfg
	a.fetcher = newFetcher
	a.checker = newChecker
	a.fetchCancel = cancel
	a.mu.Unlock()

	newFetcher.Start(fetchCtx)
	newChecker.Start(fetchCtx)
	go newChecker.CheckNow(fetchCtx)

	if err := a.saveConfig(cfg); err != nil {
		log.Printf("[config] save failed: %v", err)
	}
	log.Printf("[app] connected to %s as %s", cfg.Domain, cfg.Username)
	return nil
}

// disconnect stops the fetcher and removes the saved config.
func (a *app) disconnect() {
	a.mu.Lock()
	if a.fetchCancel != nil {
		a.fetchCancel()
		a.fetchCancel = nil
	}
	a.fetcher = nil
	a.checker = nil
	a.cfg = savedConfig{}
	a.mu.Unlock()

	if err := os.Remove(a.configPath()); err != nil && !os.IsNotExist(err) {
		log.Printf("[config] remove failed: %v", err)
	}
	log.Println("[app] disconnected")
}

func (a *app) configPath() string {
	return filepath.Join(a.dataDir, "config.json")
}

func (a *app) saveConfig(cfg savedConfig) error {
	if err := os.MkdirAll(a.dataDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(a.configPath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(cfg)
}

func (a *app) loadConfig() (savedConfig, bool) {
	data, err := os.ReadFile(a.configPath())
	if err != nil {
		return savedConfig{}, false
	}
	var cfg savedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return savedConfig{}, false
	}
	return cfg, cfg.Domain != "" && cfg.Passphrase != ""
}

func main() {
	var (
		domain     = flag.String("domain", "", "DNS domain (overrides saved config)")
		passphrase = flag.String("passphrase", "", "Shared passphrase (overrides saved config)")
		username   = flag.String("username", "", "Display name (overrides saved config)")
		resolvers  = flag.String("resolvers", "", "Comma-separated DNS resolvers (overrides saved config)")
		port       = flag.Int("port", 7742, "Local web UI port")
		host       = flag.String("host", "127.0.0.1", "Local web UI bind address")
		dataDir    = flag.String("data-dir", defaultDataDir(), "Directory for config and cache")
		noBrowser  = flag.Bool("no-browser", false, "Do not open browser automatically")
		debug      = flag.Bool("debug", false, "Enable verbose DNS debug logging")
	)
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a := &app{
		dataDir: *dataDir,
		debug:   *debug,
		rootCtx: ctx,
		scanner: client.NewResolverScanner(),
	}

	// Load saved config, then overlay any flags that were explicitly provided.
	saved, hasSaved := a.loadConfig()
	if *domain != "" {
		saved.Domain = *domain
	}
	if *passphrase != "" {
		saved.Passphrase = *passphrase
	}
	if *username != "" {
		saved.Username = *username
	}
	if *resolvers != "" {
		saved.Resolvers = parseResolvers(*resolvers)
	}

	if saved.Domain != "" && saved.Passphrase != "" {
		if err := a.connect(saved); err != nil {
			log.Printf("[app] auto-connect failed: %v", err)
		}
	} else if hasSaved {
		log.Println("[app] saved config incomplete, starting in setup mode")
	} else {
		log.Println("[app] no config found, starting in setup mode")
	}

	addr := fmt.Sprintf("%s:%d", *host, *port)
	srv := &http.Server{Addr: addr, Handler: buildMux(a)}

	go func() {
		log.Printf("[client] web UI at http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[client] http error: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	if !*noBrowser {
		time.AfterFunc(400*time.Millisecond, func() {
			openBrowser("http://" + addr)
		})
	}

	log.Printf("[client] DNS Messenger — addr=%s data=%s", addr, *dataDir)
	<-ctx.Done()
	log.Println("[client] shutdown")
}

// buildMux wires all HTTP endpoints onto the app.
func buildMux(a *app) *http.ServeMux {
	mux := http.NewServeMux()

	staticDir := resolveStaticDir()
	if staticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(staticDir)))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "static files not found", http.StatusNotFound)
		})
	}

	// GET /api/server — current server info (no passphrase)
	// POST /api/server — connect to a server (saves config, re-inits fetcher)
	// DELETE /api/server — disconnect and wipe config
	mux.HandleFunc("/api/server", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			a.mu.RLock()
			cfg := a.cfg
			configured := a.fetcher != nil
			a.mu.RUnlock()
			jsonOK(w, map[string]any{
				"configured": configured,
				"domain":     cfg.Domain,
				"username":   cfg.Username,
				"resolvers":  cfg.Resolvers,
			})

		case http.MethodPost:
			var body struct {
				Domain     string   `json:"domain"`
				Passphrase string   `json:"passphrase"`
				Username   string   `json:"username"`
				Resolvers  []string `json:"resolvers"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			cfg := savedConfig{
				Domain:     strings.TrimSpace(body.Domain),
				Passphrase: body.Passphrase,
				Username:   strings.TrimSpace(body.Username),
				Resolvers:  body.Resolvers,
			}
			if err := a.connect(cfg); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			jsonOK(w, map[string]string{"status": "ok"})

		case http.MethodDelete:
			a.disconnect()
			jsonOK(w, map[string]string{"status": "ok"})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// GET /api/status — resolver health
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.mu.RLock()
		f := a.fetcher
		cfg := a.cfg
		a.mu.RUnlock()
		if f == nil {
			jsonOK(w, map[string]any{"configured": false})
			return
		}
		board := f.ResolverScoreboard()
		type entry struct {
			Addr    string  `json:"addr"`
			Score   float64 `json:"score"`
			Success int64   `json:"success"`
			Failure int64   `json:"failure"`
			AvgMs   float64 `json:"avg_ms"`
		}
		entries := make([]entry, len(board))
		for i, r := range board {
			entries[i] = entry{r.Addr, r.Score, r.Success, r.Failure, r.AvgMs}
		}
		jsonOK(w, map[string]any{
			"configured": true,
			"domain":     cfg.Domain,
			"username":   cfg.Username,
			"resolvers":  entries,
			"active":     len(f.Resolvers()),
			"all":        len(f.AllResolvers()),
		})
	})

	// GET /api/rooms
	mux.HandleFunc("/api/rooms", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.mu.RLock()
		f := a.fetcher
		a.mu.RUnlock()
		if f == nil {
			http.Error(w, "not connected", http.StatusServiceUnavailable)
			return
		}
		meta, err := f.FetchMetadata(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("fetch metadata: %v", err), http.StatusBadGateway)
			return
		}
		type roomEntry struct {
			ID         int    `json:"id"`
			Name       string `json:"name"`
			IsDM       bool   `json:"is_dm"`
			BlockCount int    `json:"block_count"`
		}
		rooms := make([]roomEntry, len(meta.Channels))
		for i, ch := range meta.Channels {
			rooms[i] = roomEntry{i + 1, ch.Name, strings.HasPrefix(ch.Name, "dm:"), int(ch.Blocks)}
		}
		jsonOK(w, rooms)
	})

	// GET /api/messages?room=N[&blocks=K]
	mux.HandleFunc("/api/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.mu.RLock()
		f := a.fetcher
		a.mu.RUnlock()
		if f == nil {
			http.Error(w, "not connected", http.StatusServiceUnavailable)
			return
		}
		roomID, err := strconv.Atoi(r.URL.Query().Get("room"))
		if err != nil || roomID < 1 {
			http.Error(w, "invalid room", http.StatusBadRequest)
			return
		}
		blockCount, _ := strconv.Atoi(r.URL.Query().Get("blocks"))
		if blockCount <= 0 {
			meta, err := f.FetchMetadata(r.Context())
			if err != nil {
				http.Error(w, fmt.Sprintf("fetch metadata: %v", err), http.StatusBadGateway)
				return
			}
			if roomID >= 1 && roomID <= len(meta.Channels) {
				blockCount = int(meta.Channels[roomID-1].Blocks)
			}
		}
		if blockCount <= 0 {
			jsonOK(w, []any{})
			return
		}
		msgs, err := f.FetchChannel(r.Context(), roomID, blockCount)
		if err != nil {
			http.Error(w, fmt.Sprintf("fetch channel: %v", err), http.StatusBadGateway)
			return
		}
		type msgEntry struct {
			ID        uint32 `json:"id"`
			Timestamp int64  `json:"ts"`
			Sender    string `json:"sender"`
			Text      string `json:"text"`
		}
		out := make([]msgEntry, len(msgs))
		for i, m := range msgs {
			sender, text := parseMsgText(m.Text)
			out[i] = msgEntry{m.ID, int64(m.Timestamp), sender, text}
		}
		jsonOK(w, out)
	})

	// POST /api/send — {"room": N, "text": "..."}
	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.mu.RLock()
		f := a.fetcher
		username := a.cfg.Username
		a.mu.RUnlock()
		if f == nil {
			http.Error(w, "not connected", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Room int    `json:"room"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Room < 1 || strings.TrimSpace(body.Text) == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		payload := username + "\n" + strings.TrimSpace(body.Text)
		if err := f.SendMessage(r.Context(), body.Room, payload); err != nil {
			http.Error(w, fmt.Sprintf("send failed: %v", err), http.StatusBadGateway)
			return
		}
		jsonOK(w, map[string]string{"status": "ok"})
	})

	// POST /api/admin — {"cmd": "add|remove|list|refresh", "arg": "..."}
	mux.HandleFunc("/api/admin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.mu.RLock()
		f := a.fetcher
		a.mu.RUnlock()
		if f == nil {
			http.Error(w, "not connected", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Cmd string `json:"cmd"`
			Arg string `json:"arg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var cmd protocol.AdminCmd
		switch strings.ToLower(body.Cmd) {
		case "add":
			cmd = protocol.AdminCmdAddChannel
		case "remove":
			cmd = protocol.AdminCmdRemoveChannel
		case "list":
			cmd = protocol.AdminCmdListChannels
		case "refresh":
			cmd = protocol.AdminCmdRefresh
		default:
			http.Error(w, "unknown command", http.StatusBadRequest)
			return
		}
		result, err := f.SendAdminCommand(r.Context(), cmd, body.Arg)
		if err != nil {
			http.Error(w, fmt.Sprintf("admin failed: %v", err), http.StatusBadGateway)
			return
		}
		jsonOK(w, map[string]string{"result": result})
	})

	// GET /api/scan — trigger health re-check
	mux.HandleFunc("/api/scan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.mu.RLock()
		ch := a.checker
		a.mu.RUnlock()
		if ch == nil {
			http.Error(w, "not connected", http.StatusServiceUnavailable)
			return
		}
		go ch.CheckNow(context.Background())
		jsonOK(w, map[string]string{"status": "scanning"})
	})

	// ── Scanner API ───────────────────────────────────────────────────────────

	// GET /api/scanner/presets
	mux.HandleFunc("/api/scanner/presets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jsonOK(w, map[string]any{
			"presets": []map[string]any{
				{"name": "public", "label": "Public DNS", "count": parseScannerPresetCount()},
			},
		})
	})

	// POST /api/scanner/start
	mux.HandleFunc("/api/scanner/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Targets      []string `json:"targets"`
			Preset       string   `json:"preset"`
			MaxIPs       int      `json:"maxIPs"`
			RateLimit    int      `json:"rateLimit"`
			Timeout      float64  `json:"timeout"`
			ExpandSubnet bool     `json:"expandSubnet"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.Preset == "public" && len(req.Targets) == 0 {
			req.Targets = parseScannerPresetLines()
		}
		if len(req.Targets) == 0 {
			http.Error(w, "targets required", http.StatusBadRequest)
			return
		}
		a.mu.RLock()
		cfg := a.cfg
		checker := a.checker
		a.mu.RUnlock()
		if cfg.Domain == "" || cfg.Passphrase == "" {
			http.Error(w, "not connected", http.StatusServiceUnavailable)
			return
		}
		// Cancel ongoing health check to free bandwidth.
		if checker != nil {
			checker.CancelCurrentScan()
		}
		scanCfg := client.ScannerConfig{
			Targets:      req.Targets,
			MaxIPs:       req.MaxIPs,
			RateLimit:    req.RateLimit,
			Timeout:      req.Timeout,
			ExpandSubnet: req.ExpandSubnet,
			Domain:       cfg.Domain,
			Passphrase:   cfg.Passphrase,
		}
		if err := a.scanner.Start(scanCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonOK(w, map[string]any{"ok": true})
	})

	// POST /api/scanner/stop
	mux.HandleFunc("/api/scanner/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.scanner.Stop()
		jsonOK(w, map[string]any{"ok": true})
	})

	// POST /api/scanner/pause
	mux.HandleFunc("/api/scanner/pause", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.scanner.Pause()
		jsonOK(w, map[string]any{"ok": true})
	})

	// POST /api/scanner/resume
	mux.HandleFunc("/api/scanner/resume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.scanner.Resume()
		jsonOK(w, map[string]any{"ok": true})
	})

	// GET /api/scanner/progress
	mux.HandleFunc("/api/scanner/progress", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jsonOK(w, a.scanner.Progress())
	})

	// POST /api/scanner/apply — set scanned resolvers as active
	mux.HandleFunc("/api/scanner/apply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Resolvers []string `json:"resolvers"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		resolvers := req.Resolvers
		if len(resolvers) == 0 {
			prog := a.scanner.Progress()
			for _, res := range prog.Results {
				resolvers = append(resolvers, res.IP)
			}
		}
		if len(resolvers) == 0 {
			http.Error(w, "no resolvers to apply", http.StatusBadRequest)
			return
		}
		// Ensure :53 suffix.
		for i, res := range resolvers {
			if !strings.Contains(res, ":") {
				resolvers[i] = res + ":53"
			}
		}

		a.mu.Lock()
		if a.fetcher != nil {
			a.fetcher.SetActiveResolvers(resolvers)
		}
		if a.cfg.Domain != "" {
			a.cfg.Resolvers = resolvers
			a.saveConfig(a.cfg)
		}
		a.mu.Unlock()

		jsonOK(w, map[string]any{"ok": true, "count": len(resolvers)})
	})

	return mux
}

func parseMsgText(text string) (sender, body string) {
	if strings.HasPrefix(text, "[") {
		if end := strings.Index(text, "] "); end > 1 {
			return text[1:end], text[end+2:]
		}
	}
	return "anon", text
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func parseResolvers(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(r); err != nil {
			r += ":53"
		}
		out = append(out, r)
	}
	return out
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dnsmessenger"
	}
	return filepath.Join(home, ".dnsmessenger")
}

func resolveStaticDir() string {
	if exe, err := os.Executable(); err == nil {
		if c := filepath.Join(filepath.Dir(exe), "static"); dirExists(c) {
			return c
		}
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		root := filepath.Join(filepath.Dir(file), "..", "..")
		if c := filepath.Join(root, "internal", "web", "static"); dirExists(c) {
			return c
		}
	}
	if dirExists("static") {
		return "static"
	}
	if dirExists("internal/web/static") {
		return "internal/web/static"
	}
	return ""
}

func dirExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd, args = "cmd", []string{"/c", "start", url}
	case "darwin":
		cmd, args = "open", []string{url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	if err := exec.Command(cmd, args...).Start(); err != nil {
		log.Printf("[client] open browser: %v", err)
	}
}
