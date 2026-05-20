package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/mahdi/dnsmessenger/internal/protocol"
)

const reportChannelBuffer = 4096
const topResolverLimit = 20

// MessageSender is implemented by ChatServer; dns.go calls it to deliver
// incoming upstream-send payloads into the in-memory chat store.
type MessageSender interface {
	SendMessage(ctx context.Context, channelNum int, text string) error
}

// DNSServer serves chat data over DNS TXT queries.
type DNSServer struct {
	domain       string
	feed         *Feed
	sender       MessageSender // receives upstream send payloads (chat messages)
	channelCtl   channelRefresher
	refreshers   []channelRefresher
	queryKey     [protocol.KeySize]byte
	responseKey  [protocol.KeySize]byte
	listenAddr   string
	maxPadding   int
	allowManage  bool   // if true, admin/send commands are accepted
	channelsFile string // path to rooms.txt for admin commands

	sessionsMu sync.Mutex
	sessions   map[uint16]*uploadSession

	reportCh chan reportEvent
	debug    bool
}

type channelFetchStats struct {
	Queries int64 `json:"queries"`
}

type reportEvent struct {
	channel  uint16
	resolver string
	invalid  bool
}

type hourlyFetchReport struct {
	windowStart     time.Time
	totalQueries    int64
	metadataQueries int64
	versionQueries  int64
	mediaQueries    int64
	invalidQueries  int64
	perChannel      map[uint16]*channelFetchStats
	perResolver     map[string]int64
}

type uploadSession struct {
	kind          protocol.UpstreamKind
	targetChannel uint8
	totalBlocks   uint8
	blocks        [][]byte
	received      []bool
	expiresAt     time.Time
}

type channelRefresher interface {
	UpdateChannels(channels []string)
	RequestRefresh()
}

// NewDNSServer creates a DNS server for the given domain.
func NewDNSServer(listenAddr, domain string, feed *Feed, queryKey, responseKey [protocol.KeySize]byte, maxPadding int, sender MessageSender, allowManage bool, channelsFile string, debug bool) *DNSServer {
	s := &DNSServer{
		domain:       strings.TrimSuffix(domain, "."),
		feed:         feed,
		sender:       sender,
		channelCtl:   nil,
		refreshers:   nil,
		queryKey:     queryKey,
		responseKey:  responseKey,
		listenAddr:   listenAddr,
		maxPadding:   maxPadding,
		allowManage:  allowManage,
		channelsFile: channelsFile,
		sessions:     make(map[uint16]*uploadSession),
		reportCh:     make(chan reportEvent, reportChannelBuffer),
		debug:        debug,
	}
	return s
}

// SetChannelRefresher wires the chat server as a refresher for admin operations.
func (s *DNSServer) SetChannelRefresher(channelCtl channelRefresher) {
	s.channelCtl = channelCtl
	if channelCtl != nil {
		s.refreshers = append(s.refreshers, channelCtl)
	}
}

// AddRefresher adds an additional source refresher for admin refresh.
func (s *DNSServer) AddRefresher(r channelRefresher) {
	if r != nil {
		s.refreshers = append(s.refreshers, r)
	}
}

// ListenAndServe starts the DNS server on UDP, shutting down when ctx is cancelled.
func (s *DNSServer) ListenAndServe(ctx context.Context) error {
	mux := dns.NewServeMux()
	mux.HandleFunc(s.domain+".", s.handleQuery)

	server := &dns.Server{
		Addr:    s.listenAddr,
		Net:     "udp",
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		log.Println("[dns] shutting down...")
		server.Shutdown()
	}()

	go s.runHourlyReports(ctx)

	log.Printf("[dns] listening on %s (domain: %s)", s.listenAddr, s.domain)
	return server.ListenAndServe()
}

func (s *DNSServer) handleQuery(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	if len(r.Question) == 0 {
		w.WriteMsg(m)
		return
	}

	q := r.Question[0]
	if q.Qtype != dns.TypeTXT {
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	channel, block, err := protocol.DecodeQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode query: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	switch channel {
	case protocol.UpstreamInitChannel:
		s.handleUpstreamInitQuery(w, m, q)
		return
	case protocol.UpstreamDataChannel:
		s.handleUpstreamDataQuery(w, m, q)
		return
	}

	if channel == protocol.SendChannel {
		s.handleSendQuery(w, m, q)
		return
	}

	if channel == protocol.AdminChannel {
		s.handleAdminQuery(w, m, q)
		return
	}

	s.trackRequestStart(channel, resolverHost(w.RemoteAddr()))
	if s.debug {
		log.Printf("[dns] query ch=%d blk=%d from=%s name=%s", channel, block, resolverHost(w.RemoteAddr()), q.Name)
	}

	data, err := s.feed.GetBlock(int(channel), int(block))
	if err != nil {
		s.trackInvalidQuery()
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	encoded, err := protocol.EncodeResponse(s.responseKey, data, s.maxPadding)
	if err != nil {
		log.Printf("[dns] encode response: %v", err)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	m.Answer = append(m.Answer, &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   q.Name,
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET,
			Ttl:    defaultResponseTTL,
		},
		Txt: splitTXT(encoded),
	})

	w.WriteMsg(m)
}

// defaultResponseTTL blends with real-world DNS; random-suffix queries stay
// uncacheable by uniqueness, deterministic ones benefit from this window.
const defaultResponseTTL uint32 = 60

func splitTXT(s string) []string {
	var parts []string
	for len(s) > 255 {
		parts = append(parts, s[:255])
		s = s[255:]
	}
	if len(s) > 0 {
		parts = append(parts, s)
	}
	return parts
}

func (s *DNSServer) writeEncodedResponse(w dns.ResponseWriter, m *dns.Msg, name string, data []byte) {
	encoded, err := protocol.EncodeResponse(s.responseKey, data, s.maxPadding)
	if err != nil {
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}
	m.Answer = append(m.Answer, &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   name,
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET,
			Ttl:    defaultResponseTTL,
		},
		Txt: splitTXT(encoded),
	})
	w.WriteMsg(m)
}

func (s *DNSServer) cleanupExpiredSessions(now time.Time) {
	for id, sess := range s.sessions {
		if now.After(sess.expiresAt) {
			delete(s.sessions, id)
		}
	}
}

func (s *DNSServer) handleUpstreamInitQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question) {
	if !s.allowManage {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	init, err := protocol.DecodeUpstreamInitQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode upstream init: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	if init.Kind == protocol.UpstreamKindSend && s.sender == nil {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	now := time.Now()
	s.sessionsMu.Lock()
	s.cleanupExpiredSessions(now)
	s.sessions[init.SessionID] = &uploadSession{
		kind:          init.Kind,
		targetChannel: init.TargetChannel,
		totalBlocks:   init.TotalBlocks,
		blocks:        make([][]byte, init.TotalBlocks),
		received:      make([]bool, init.TotalBlocks),
		expiresAt:     now.Add(5 * time.Minute),
	}
	s.sessionsMu.Unlock()

	s.writeEncodedResponse(w, m, q.Name, []byte("READY"))
}

func (s *DNSServer) handleUpstreamDataQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question) {
	if !s.allowManage {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	sessionID, index, chunk, err := protocol.DecodeUpstreamBlockQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode upstream block: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	now := time.Now()
	s.sessionsMu.Lock()
	s.cleanupExpiredSessions(now)
	sess, ok := s.sessions[sessionID]
	if !ok || now.After(sess.expiresAt) {
		if ok {
			delete(s.sessions, sessionID)
		}
		s.sessionsMu.Unlock()
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}
	if int(index) >= len(sess.blocks) {
		s.sessionsMu.Unlock()
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}
	if !sess.received[index] {
		copied := make([]byte, len(chunk))
		copy(copied, chunk)
		sess.blocks[index] = copied
		sess.received[index] = true
	}
	sess.expiresAt = now.Add(5 * time.Minute)
	complete := true
	for _, received := range sess.received {
		if !received {
			complete = false
			break
		}
	}
	if !complete {
		s.sessionsMu.Unlock()
		s.writeEncodedResponse(w, m, q.Name, []byte("CONTINUE"))
		return
	}

	payload := make([]byte, 0)
	for _, block := range sess.blocks {
		payload = append(payload, block...)
	}
	delete(s.sessions, sessionID)
	s.sessionsMu.Unlock()

	result, err := s.executeUpstreamPayload(sess, payload)
	if err != nil {
		log.Printf("[dns] upstream execute: %v", err)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	s.writeEncodedResponse(w, m, q.Name, result)
}

func (s *DNSServer) executeUpstreamPayload(sess *uploadSession, payload []byte) ([]byte, error) {
	switch sess.kind {
	case protocol.UpstreamKindSend:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.sender.SendMessage(ctx, int(sess.targetChannel), string(payload)); err != nil {
			return nil, err
		}
		return []byte("OK"), nil
	case protocol.UpstreamKindAdmin:
		if len(payload) == 0 {
			return nil, fmt.Errorf("empty admin payload")
		}
		cmd := protocol.AdminCmd(payload[0])
		arg := ""
		if len(payload) > 1 {
			arg = string(payload[1:])
		}

		var result string
		var err error
		switch cmd {
		case protocol.AdminCmdAddChannel:
			result, err = s.adminAddChannel(arg)
		case protocol.AdminCmdRemoveChannel:
			result, err = s.adminRemoveChannel(arg)
		case protocol.AdminCmdListChannels:
			result, err = s.adminListChannels()
		case protocol.AdminCmdRefresh:
			result, err = s.adminRefresh()
		default:
			err = fmt.Errorf("unknown command: %d", cmd)
		}
		if err != nil {
			return nil, err
		}
		return []byte(result), nil
	default:
		return nil, fmt.Errorf("unknown upstream kind: %d", sess.kind)
	}
}

func (s *DNSServer) handleSendQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question) {
	if !s.allowManage {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	if s.sender == nil {
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	targetCh, message, err := protocol.DecodeSendQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode send query: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := s.sender.SendMessage(ctx, int(targetCh), string(message)); err != nil {
		log.Printf("[dns] send message to ch %d: %v", targetCh, err)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	s.writeEncodedResponse(w, m, q.Name, []byte("OK"))
}

func (s *DNSServer) handleAdminQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question) {
	if !s.allowManage {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	cmd, arg, err := protocol.DecodeAdminQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode admin query: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	var result string
	switch cmd {
	case protocol.AdminCmdAddChannel:
		result, err = s.adminAddChannel(string(arg))
	case protocol.AdminCmdRemoveChannel:
		result, err = s.adminRemoveChannel(string(arg))
	case protocol.AdminCmdListChannels:
		result, err = s.adminListChannels()
	case protocol.AdminCmdRefresh:
		result, err = s.adminRefresh()
	default:
		err = fmt.Errorf("unknown command: %d", cmd)
	}

	if err != nil {
		log.Printf("[dns] admin cmd=%d: %v", cmd, err)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	s.writeEncodedResponse(w, m, q.Name, []byte(result))
}

func (s *DNSServer) adminAddChannel(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "@")
	if name == "" {
		return "", fmt.Errorf("empty room name")
	}

	existing, err := loadChannelsFromFile(s.channelsFile)
	if err != nil {
		return "", fmt.Errorf("read rooms: %w", err)
	}
	for _, ch := range existing {
		if strings.EqualFold(ch, name) {
			return "already exists", nil
		}
	}

	f, err := os.OpenFile(s.channelsFile, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return "", fmt.Errorf("open rooms file: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s\n", name); err != nil {
		return "", fmt.Errorf("write room: %w", err)
	}

	log.Printf("[admin] added room %s", name)

	all, err := loadChannelsFromFile(s.channelsFile)
	if err == nil {
		s.feed.SetChannels(all)
		if s.channelCtl != nil {
			s.channelCtl.UpdateChannels(all)
			s.channelCtl.RequestRefresh()
		}
	}
	return "OK", nil
}

func (s *DNSServer) adminRemoveChannel(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "@")
	if name == "" {
		return "", fmt.Errorf("empty room name")
	}

	existing, err := loadChannelsFromFile(s.channelsFile)
	if err != nil {
		return "", fmt.Errorf("read rooms: %w", err)
	}

	found := false
	var remaining []string
	for _, ch := range existing {
		if strings.EqualFold(ch, name) {
			found = true
			continue
		}
		remaining = append(remaining, ch)
	}
	if !found {
		return "not found", nil
	}

	content := "# DNS Messenger rooms (one per line)\n"
	for _, ch := range remaining {
		content += ch + "\n"
	}
	if err := os.WriteFile(s.channelsFile, []byte(content), 0600); err != nil {
		return "", fmt.Errorf("write rooms: %w", err)
	}

	log.Printf("[admin] removed room %s", name)

	s.feed.SetChannels(remaining)
	if s.channelCtl != nil {
		s.channelCtl.UpdateChannels(remaining)
		s.channelCtl.RequestRefresh()
	}
	return "OK", nil
}

func (s *DNSServer) adminListChannels() (string, error) {
	channels, err := loadChannelsFromFile(s.channelsFile)
	if err != nil {
		return "", err
	}
	return strings.Join(channels, "\n"), nil
}

func (s *DNSServer) adminRefresh() (string, error) {
	if len(s.refreshers) == 0 {
		return "", fmt.Errorf("no active refresher")
	}
	for _, r := range s.refreshers {
		r.RequestRefresh()
	}
	log.Printf("[admin] hard refresh requested")
	return "OK", nil
}

func loadChannelsFromFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var channels []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		channels = append(channels, strings.TrimPrefix(line, "@"))
	}
	return channels, scanner.Err()
}

func resolverHost(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err == nil {
		return host
	}
	return addr.String()
}

func (s *DNSServer) trackRequestStart(channel uint16, resolver string) {
	if channel == protocol.UpstreamInitChannel || channel == protocol.UpstreamDataChannel ||
		channel == protocol.SendChannel || channel == protocol.AdminChannel {
		return
	}
	s.reportCh <- reportEvent{channel: channel, resolver: resolver}
}

func (s *DNSServer) trackInvalidQuery() {
	select {
	case s.reportCh <- reportEvent{invalid: true}:
	default:
	}
}

func (s *DNSServer) runHourlyReports(ctx context.Context) {
	rep := newHourlyFetchReport(time.Now())
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.drainReportEvents(rep)
			s.emitHourlyReport(rep, true)
			return
		case event := <-s.reportCh:
			recordReportQuery(rep, event)
		case <-ticker.C:
			s.emitHourlyReport(rep, false)
			rep = newHourlyFetchReport(time.Now())
		}
	}
}

func newHourlyFetchReport(start time.Time) *hourlyFetchReport {
	return &hourlyFetchReport{
		windowStart: start,
		perChannel:  make(map[uint16]*channelFetchStats),
		perResolver: make(map[string]int64),
	}
}

func recordReportQuery(rep *hourlyFetchReport, event reportEvent) {
	if event.invalid {
		rep.invalidQueries++
		return
	}
	rep.totalQueries++
	if event.resolver != "" {
		rep.perResolver[event.resolver]++
	}
	channel := event.channel
	if channel == protocol.MetadataChannel {
		rep.metadataQueries++
		return
	}
	if channel == protocol.VersionChannel {
		rep.versionQueries++
		return
	}
	if protocol.IsMediaChannel(channel) {
		rep.mediaQueries++
		return
	}
	stats := rep.perChannel[channel]
	if stats == nil {
		stats = &channelFetchStats{}
		rep.perChannel[channel] = stats
	}
	stats.Queries++
}

func (s *DNSServer) drainReportEvents(rep *hourlyFetchReport) {
	for {
		select {
		case event := <-s.reportCh:
			recordReportQuery(rep, event)
		default:
			return
		}
	}
}

func (s *DNSServer) emitHourlyReport(rep *hourlyFetchReport, final bool) {
	names := s.feed.ChannelNames()
	chs := make([]uint16, 0, len(rep.perChannel))
	for ch := range rep.perChannel {
		chs = append(chs, ch)
	}
	sort.Slice(chs, func(i, j int) bool { return chs[i] < chs[j] })

	type channelEntry struct {
		Channel uint16 `json:"channel"`
		Name    string `json:"name,omitempty"`
		Queries int64  `json:"queries"`
	}
	entries := make([]channelEntry, 0, len(chs))
	for _, ch := range chs {
		st := rep.perChannel[ch]
		name := ""
		if int(ch) >= 1 && int(ch) <= len(names) {
			name = names[int(ch)-1]
		}
		entries = append(entries, channelEntry{Channel: ch, Name: name, Queries: st.Queries})
	}

	type resolverEntry struct {
		Resolver string `json:"resolver"`
		Queries  int64  `json:"queries"`
	}
	resolvers := make([]resolverEntry, 0, len(rep.perResolver))
	for resolver, queries := range rep.perResolver {
		resolvers = append(resolvers, resolverEntry{Resolver: resolver, Queries: queries})
	}
	sort.Slice(resolvers, func(i, j int) bool {
		if resolvers[i].Queries == resolvers[j].Queries {
			return resolvers[i].Resolver < resolvers[j].Resolver
		}
		return resolvers[i].Queries > resolvers[j].Queries
	})
	if len(resolvers) > topResolverLimit {
		resolvers = resolvers[:topResolverLimit]
	}

	payload := map[string]any{
		"type":                 "dns_hourly_report",
		"from":                 rep.windowStart.UTC().Format(time.RFC3339),
		"to":                   time.Now().UTC().Format(time.RFC3339),
		"totalDnsQueries":      rep.totalQueries,
		"totalMetadataQueries": rep.metadataQueries,
		"totalVersionQueries":  rep.versionQueries,
		"totalMediaQueries":    rep.mediaQueries,
		"totalInvalidQueries":  rep.invalidQueries,
		"channels":             entries,
		"topResolvers":         resolvers,
		"finalFlush":           final,
	}
	if mediaCache := s.feed.MediaCache(); mediaCache != nil {
		payload["mediaCache"] = mediaCache.Stats()
	}
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[dns_hourly] marshal error: %v", err)
		return
	}
	log.Printf("[dns_hourly] %s", string(b))
}
