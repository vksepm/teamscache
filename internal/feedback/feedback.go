// Package feedback keeps the feedback that an AI agent reports about the MCP
// tools. Reports are appended to a local JSONL file and rate limited per
// client. Nothing leaves the computer.
package feedback

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Limits on the text an agent can submit.
const (
	MaxSummary     = 200
	MaxDetails     = 2000
	MaxProposedFix = 2000
	MaxRelatedTool = 64
	maxClientName  = 100
)

// Defaults of the rate limit.
const (
	DefaultLimit  = 10
	DefaultWindow = 10 * time.Minute
)

const unknownClient = "unknown"

// ErrInvalid reports feedback the store does not accept.
var ErrInvalid = errors.New("invalid feedback")

// Categories lists the accepted values of Input.Category.
func Categories() []string {
	return []string{"bug", "unexpected_output", "missing_capability", "tool_description", "search_miss", "other"}
}

// Client identifies the MCP client that submits feedback.
type Client struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Input is the feedback an agent submits.
type Input struct {
	Category    string
	Summary     string
	Details     string
	RelatedTool string
	ProposedFix string
}

// Entry is one line of the feedback file.
type Entry struct {
	ID          string    `json:"id"`
	Time        time.Time `json:"time"`
	Client      Client    `json:"client"`
	Category    string    `json:"category"`
	Summary     string    `json:"summary"`
	Details     string    `json:"details,omitempty"`
	RelatedTool string    `json:"related_tool,omitempty"`
	ProposedFix string    `json:"proposed_fix,omitempty"`
	Hash        string    `json:"hash"`
}

// Receipt tells what happened to a submission. For a duplicate, ID is the ID
// of the entry that was stored first.
type Receipt struct {
	ID           string
	Category     string
	Deduplicated bool
}

// RateLimitError reports that a client sent too many reports in the window.
type RateLimitError struct {
	Limit      int
	Window     time.Duration
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit reached: at most %d feedback reports per %s; retry_after_seconds=%d",
		e.Limit, e.Window, int64((e.RetryAfter+time.Second-1)/time.Second))
}

// DefaultPath returns ~/.teamscache/feedback.jsonl, or "" when the home folder
// is unknown.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".teamscache", "feedback.jsonl")
}

// Store appends feedback to a JSONL file and applies the rate limit. The limit
// is counted from the file, so it also holds after a restart and between
// several server processes that share the file.
type Store struct {
	path   string
	limit  int
	window time.Duration
	now    func() time.Time

	mu sync.Mutex
}

// NewStore returns a store for the file at path. At most limit entries per
// client are accepted in any window. A nil now means time.Now.
func NewStore(path string, limit int, window time.Duration, now func() time.Time) (*Store, error) {
	switch {
	case path == "":
		return nil, errors.New("feedback file path is empty")
	case limit < 1:
		return nil, fmt.Errorf("feedback rate limit must be 1 or more, got %d", limit)
	case window <= 0:
		return nil, fmt.Errorf("feedback rate window must be positive, got %s", window)
	}
	if now == nil {
		now = time.Now
	}
	return &Store{path: path, limit: limit, window: window, now: now}, nil
}

// Path is the feedback file.
func (s *Store) Path() string { return s.path }

// Submit validates in and appends it to the file. An identical report of the
// same client inside the window is not stored again and does not count against
// the limit. It returns a *RateLimitError when the client is over the limit.
func (s *Store) Submit(client Client, in Input) (Receipt, error) {
	in, err := normalize(in)
	if err != nil {
		return Receipt{}, err
	}
	client.Name = clip(strings.TrimSpace(client.Name), maxClientName)
	client.Version = clip(strings.TrimSpace(client.Version), maxClientName)
	if client.Name == "" {
		client.Name = unknownClient
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	hash := hashOf(in)
	existing, err := readEntries(s.path)
	if err != nil {
		return Receipt{}, err
	}
	cutoff := now.Add(-s.window)
	var count int
	var oldest time.Time
	for _, e := range existing {
		if e.Client.Name != client.Name || !e.Time.After(cutoff) {
			continue
		}
		if e.Hash == hash {
			return Receipt{ID: e.ID, Category: e.Category, Deduplicated: true}, nil
		}
		count++
		if oldest.IsZero() || e.Time.Before(oldest) {
			oldest = e.Time
		}
	}
	if count >= s.limit {
		return Receipt{}, &RateLimitError{Limit: s.limit, Window: s.window, RetryAfter: oldest.Add(s.window).Sub(now)}
	}

	id, err := newID()
	if err != nil {
		return Receipt{}, err
	}
	entry := Entry{
		ID: id, Time: now, Client: client, Category: in.Category, Summary: in.Summary,
		Details: in.Details, RelatedTool: in.RelatedTool, ProposedFix: in.ProposedFix, Hash: hash,
	}
	if err := s.append(entry); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: id, Category: in.Category}, nil
}

// append writes entry as one line with a single write, so that processes that
// share the file do not interleave their lines.
func (s *Store) append(e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode feedback: %w", err)
	}
	line = append(line, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create feedback folder: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open feedback file: %w", err)
	}
	// A crash can leave a last line without its newline. Start a new line, so
	// that this entry is not glued to the broken one.
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("write feedback: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write feedback: %w", err)
	}
	return nil
}

// List returns the entries at or after since (all when since is zero), newest
// first. A limit of 0 or less means no limit. A missing file is an empty list.
func List(path string, since time.Time, limit int) ([]Entry, error) {
	all, err := readEntries(path)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(all))
	for _, e := range all {
		if since.IsZero() || !e.Time.Before(since) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// readEntries returns the valid entries of the file in file order. Lines that
// are not valid entries, such as a line cut short by a crash, are skipped.
func readEntries(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read feedback file: %w", err)
	}
	var out []Entry
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil || e.ID == "" || e.Time.IsZero() {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func normalize(in Input) (Input, error) {
	in.Category = strings.ToLower(strings.TrimSpace(in.Category))
	in.Summary = strings.TrimSpace(in.Summary)
	in.Details = strings.TrimSpace(in.Details)
	in.RelatedTool = strings.TrimSpace(in.RelatedTool)
	in.ProposedFix = strings.TrimSpace(in.ProposedFix)

	valid := false
	for _, c := range Categories() {
		if in.Category == c {
			valid = true
			break
		}
	}
	if !valid {
		return in, fmt.Errorf("%w: category must be one of %s, got %q", ErrInvalid, strings.Join(Categories(), ", "), in.Category)
	}
	if in.Summary == "" {
		return in, fmt.Errorf("%w: summary is required", ErrInvalid)
	}
	for _, f := range []struct {
		name  string
		value string
		max   int
	}{
		{"summary", in.Summary, MaxSummary},
		{"details", in.Details, MaxDetails},
		{"proposed_fix", in.ProposedFix, MaxProposedFix},
		{"related_tool", in.RelatedTool, MaxRelatedTool},
	} {
		if n := utf8.RuneCountInString(f.value); n > f.max {
			return in, fmt.Errorf("%w: %s is %d characters, the limit is %d", ErrInvalid, f.name, n, f.max)
		}
	}
	return in, nil
}

func hashOf(in Input) string {
	h := sha256.New()
	for _, part := range []string{in.Category, in.Summary, in.Details, in.RelatedTool, in.ProposedFix} {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("create feedback id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func clip(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes])
}
