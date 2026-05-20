package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/mahdi/dnsmessenger/internal/server"
)

func main() {
	var (
		domain      = flag.String("domain", "", "DNS domain (e.g. chat.example.com) [required]")
		passphrase  = flag.String("passphrase", "", "Shared secret passphrase [required]")
		dnsAddr     = flag.String("dns-addr", ":53", "DNS server listen address (UDP)")
		httpAddr    = flag.String("http-addr", ":8080", "HTTP web UI listen address")
		roomsFile   = flag.String("rooms", "rooms.txt", "Path to rooms.txt")
		maxMessages = flag.Int("max-messages", 50, "Max messages kept per room in memory")
		allowManage = flag.Bool("allow-manage", true, "Allow DNS upstream send/admin commands")
		debug       = flag.Bool("debug", false, "Enable verbose DNS query logging")
	)
	flag.Parse()

	if *domain == "" {
		log.Fatal("--domain is required (e.g. chat.example.com)")
	}
	if *passphrase == "" {
		log.Fatal("--passphrase is required")
	}

	srv, err := server.New(server.Config{
		ListenAddr:  *dnsAddr,
		Domain:      *domain,
		Passphrase:  *passphrase,
		RoomsFile:   *roomsFile,
		MsgLimit:    *maxMessages,
		AllowManage: *allowManage,
		Debug:       *debug,
	})
	if err != nil {
		log.Fatalf("server init: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Start HTTP server for web UI + REST API.
	staticDir := resolveStaticDir()
	httpHandler := srv.HTTPHandler(staticDir)
	httpSrv := &http.Server{Addr: *httpAddr, Handler: httpHandler}
	go func() {
		log.Printf("[http] listening on %s (static: %s)", *httpAddr, staticDir)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[http] server error: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		httpSrv.Close()
	}()

	log.Printf("[server] DNS Messenger starting — domain=%s dns=%s http=%s", *domain, *dnsAddr, *httpAddr)

	// Start DNS server (blocking).
	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("server: %v", err)
	}
	log.Println("[server] shutdown complete")
}

// resolveStaticDir finds the web/static directory relative to the binary or
// the source tree (for `go run`). Returns empty string if not found.
func resolveStaticDir() string {
	// Try alongside the binary first.
	exe, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "static")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// When running with `go run`, walk up from the source file.
	_, file, _, ok := runtime.Caller(0)
	if ok {
		// file = .../cmd/server/main.go → project root is ../..
		root := filepath.Join(filepath.Dir(file), "..", "..")
		candidate := filepath.Join(root, "internal", "web", "static")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Fallback: current directory / static
	if _, err := os.Stat("static"); err == nil {
		return "static"
	}
	if _, err := os.Stat("internal/web/static"); err == nil {
		return "internal/web/static"
	}
	return ""
}
