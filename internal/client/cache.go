package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mahdi/dnsmessenger/internal/protocol"
)

const (
	maxCachedMessages = 200
	cacheTTL          = 7 * 24 * time.Hour
)

// Gap represents a range of missing messages detected between two consecutive
// cached messages (IDs gap > 1, capped at 500 to exclude natural Telegram gaps).
type Gap struct {
	AfterID  uint32 `json:"after_id"`
	BeforeID uint32 `json:"before_id"`
	Count    int    `json:"count"`
}

// MessagesResult is the wire type for /api/messages/<n>.
// It carries the full merged history plus any detected gaps.
type MessagesResult struct {
	Messages []protocol.Message `json:"messages"`
	Gaps     []Gap              `json:"gaps"`
}

// NewMessagesResult wraps a raw message slice with gap detection.
// Used as a fallback when the on-disk cache is unavailable.
func NewMessagesResult(msgs []protocol.Message) *MessagesResult {
	if msgs == nil {
		msgs = []protocol.Message{}
	}
	sorted := make([]protocol.Message, len(msgs))
	copy(sorted, msgs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return &MessagesResult{Messages: sorted, Gaps: detectGaps(sorted)}
}

// Cache stores channel and metadata snapshots on disk, keyed by channel name.
// Channel files not updated for 7 days are automatically removed by Cleanup.
type Cache struct {
	dir string
	mu  sync.Mutex
}

type cachedChannel struct {
	Name        string             `json:"name,omitempty"`
	Messages    []protocol.Message `json:"messages"`
	FetchedAt   int64              `json:"fetched_at"`
	DisplayName string             `json:"display_name,omitempty"`
}

type cachedMeta struct {
	Metadata  *protocol.Metadata `json:"metadata"`
	FetchedAt int64              `json:"fetched_at"`
}

// NewCache creates a file cache in the given directory.
func NewCache(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &Cache{dir: dir}, nil
}

// GetMessages reads the cached message history for a channel by name.
// Returns nil if the file is missing or has not been updated within 7 days.
func (c *Cache) GetMessages(channelName string) *MessagesResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	path := c.channelPath(channelName)
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if time.Since(info.ModTime()) > cacheTTL {
		_ = os.Remove(path)
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cc cachedChannel
	if err := json.Unmarshal(data, &cc); err != nil {
		return nil
	}
	return &MessagesResult{Messages: cc.Messages, Gaps: detectGaps(cc.Messages)}
}

// MergeAndPut merges fresh messages with the on-disk history, enforces the
// 200-message cap, detects gaps, persists the result, and returns it.
// Existing history is always included regardless of age to preserve context.
func (c *Cache) MergeAndPut(channelName string, fresh []protocol.Message) (*MessagesResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Load existing history without TTL check — we want to keep old messages.
	var existing []protocol.Message
	existingDisplayName := ""
	if data, err := os.ReadFile(c.channelPath(channelName)); err == nil {
		var cc cachedChannel
		if json.Unmarshal(data, &cc) == nil {
			existing = cc.Messages
			if cc.DisplayName != "" {
				existingDisplayName = cc.DisplayName
			}
		}
	}

	// Merge by ID (fresh wins on conflict).
	byID := make(map[uint32]protocol.Message, len(existing)+len(fresh))
	for _, m := range existing {
		byID[m.ID] = m
	}
	for _, m := range fresh {
		byID[m.ID] = m
	}
	merged := make([]protocol.Message, 0, len(byID))
	for _, m := range byID {
		merged = append(merged, m)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })

	// Keep the newest 200.
	if len(merged) > maxCachedMessages {
		merged = merged[len(merged)-maxCachedMessages:]
	}

	cc := cachedChannel{Messages: merged, FetchedAt: time.Now().Unix(), Name: channelName, DisplayName: existingDisplayName}
	data, err := json.Marshal(cc)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.channelPath(channelName), data, 0600); err != nil {
		return nil, err
	}
	return &MessagesResult{Messages: merged, Gaps: detectGaps(merged)}, nil
}

// PutMetadata stores metadata.
func (c *Cache) PutMetadata(meta *protocol.Metadata) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	cached := cachedMeta{
		Metadata:  meta,
		FetchedAt: time.Now().Unix(),
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, "metadata.json"), data, 0600)
}

// GetAllTitles reads the display name from every ch_*.json cache file and returns
// a map of original channel name → display name. Files without a stored name or
// display name are skipped silently.
func (c *Cache) GetAllTitles() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil
	}
	titles := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "ch_") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.dir, e.Name()))
		if err != nil {
			continue
		}
		var cc cachedChannel
		if json.Unmarshal(data, &cc) != nil || cc.Name == "" || cc.DisplayName == "" {
			continue
		}
		titles[cc.Name] = cc.DisplayName
	}
	return titles
}

// PutTitle persists a display name for a channel into its cache file.
// If the file already exists it is updated in-place so that stored messages are preserved.
func (c *Cache) PutTitle(channelName, title string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	path := c.channelPath(channelName)
	var cc cachedChannel
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cc)
	}
	cc.Name = channelName
	cc.DisplayName = title
	data, err := json.Marshal(cc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// Cleanup removes channel cache files (ch_*.json) not modified in 7 days.
func (c *Cache) Cleanup() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "ch_") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > cacheTTL {
			_ = os.Remove(filepath.Join(c.dir, e.Name()))
		}
	}
	return nil
}

// detectGaps finds runs of missing IDs between consecutive messages. Album-
// merged canonicals cover a contiguous span of sibling IDs (counted via
// albumSpan), so absorbed siblings don't show up as fake gaps. Diffs > 500
// are ignored (natural Telegram numbering jumps); under 10 messages we don't
// have enough history to judge.
func detectGaps(msgs []protocol.Message) []Gap {
	if len(msgs) < 10 {
		return nil
	}
	var gaps []Gap
	for i := 1; i < len(msgs); i++ {
		prev, cur := msgs[i-1], msgs[i]
		span := uint32(albumSpan(prev.Text))
		if span == 0 {
			span = 1
		}
		expectedNext := prev.ID + span
		if cur.ID <= expectedNext {
			continue
		}
		diff := cur.ID - expectedNext
		if diff > 500 {
			continue
		}
		gaps = append(gaps, Gap{
			AfterID:  expectedNext - 1,
			BeforeID: cur.ID,
			Count:    int(diff),
		})
	}
	return gaps
}

// mediaHeaderTags are the leading [TAG] markers extractMessages may stack
// at the start of a canonical message body — one per absorbed album item.
var mediaHeaderTags = []string{
	protocol.MediaImage,
	protocol.MediaVideo,
	protocol.MediaFile,
	protocol.MediaAudio,
	protocol.MediaSticker,
	protocol.MediaGIF,
	protocol.MediaLocation,
	protocol.MediaContact,
}

// albumSpan counts the leading media-header lines in a canonical body — 0
// for plain text, 1 for a single media item, N for an N-item album. A
// leading [REPLY]... line is skipped first.
func albumSpan(text string) int {
	if strings.HasPrefix(text, protocol.MediaReply) {
		nl := strings.IndexByte(text, '\n')
		if nl < 0 {
			return 0
		}
		text = text[nl+1:]
	}
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if !isMediaHeaderLine(line) {
			break
		}
		n++
	}
	return n
}

// isMediaHeaderLine matches both the bare [TAG] form and the downloadable
// "[TAG]<digit>..." form. Caption text that happens to start with "[IMAGE]"
// is rejected because rest[0] won't be a digit.
func isMediaHeaderLine(line string) bool {
	for _, tag := range mediaHeaderTags {
		if line == tag {
			return true
		}
		if !strings.HasPrefix(line, tag) {
			continue
		}
		rest := line[len(tag):]
		if rest == "" {
			return true
		}
		if rest[0] >= '0' && rest[0] <= '9' {
			return true
		}
	}
	return false
}

// channelPath returns the file path for a channel's cache, keyed by sanitised name.
// Only letters, digits, hyphens, and underscores are kept; everything else becomes _.
func (c *Cache) channelPath(channelName string) string {
	safe := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, channelName)
	if safe == "" {
		safe = "unknown"
	}
	return filepath.Join(c.dir, "ch_"+safe+".json")
}
