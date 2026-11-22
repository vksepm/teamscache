package feedback

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type clock struct{ t atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.t.Store(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano())
	return c
}

func (c *clock) Now() time.Time          { return time.Unix(0, c.t.Load()).UTC() }
func (c *clock) Advance(d time.Duration) { c.t.Add(int64(d)) }

func newTestStore(t *testing.T, limit int, window time.Duration) (*Store, *clock, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "feedback.jsonl")
	clk := newClock()
	s, err := NewStore(path, limit, window, clk.Now)
	require.NoError(t, err)
	return s, clk, path
}

func report(n int) Input {
	return Input{Category: "bug", Summary: fmt.Sprintf("problem %d", n)}
}

var agent = Client{Name: "claude-code", Version: "1.0"}

func TestSubmitAppendsEntries(t *testing.T) {
	t.Parallel()
	s, clk, path := newTestStore(t, 5, time.Hour)

	first, err := s.Submit(agent, Input{
		Category: " Search_Miss ", Summary: " search found nothing ", Details: "d", RelatedTool: "search_messages", ProposedFix: "fix",
	})
	require.NoError(t, err)
	assert.Len(t, first.ID, 16)
	assert.Equal(t, "search_miss", first.Category)
	assert.False(t, first.Deduplicated)

	clk.Advance(time.Second)
	second, err := s.Submit(agent, report(2))
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(raw), "\n"))

	entries, err := List(path, time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, second.ID, entries[0].ID, "newest first")
	assert.Equal(t, Entry{
		ID: first.ID, Time: clk.Now().Add(-time.Second), Client: agent, Category: "search_miss", Summary: "search found nothing",
		Details: "d", RelatedTool: "search_messages", ProposedFix: "fix", Hash: entries[1].Hash,
	}, entries[1])
	assert.Equal(t, path, s.Path())
}

func TestSubmitCreatesPrivateFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	s, _, path := newTestStore(t, 5, time.Hour)

	_, err := s.Submit(agent, report(1))
	require.NoError(t, err)

	fileInfo, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())
	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestRateLimitSlidingWindow(t *testing.T) {
	t.Parallel()
	s, clk, _ := newTestStore(t, 3, 10*time.Minute)

	for i := range 3 {
		_, err := s.Submit(agent, report(i))
		require.NoError(t, err)
		clk.Advance(time.Minute)
	}
	// Now 3 minutes after the first report.
	_, err := s.Submit(agent, report(99))
	var limited *RateLimitError
	require.ErrorAs(t, err, &limited)
	assert.Equal(t, 3, limited.Limit)
	assert.Equal(t, 7*time.Minute, limited.RetryAfter, "the first report leaves the window in 7 minutes")
	assert.Contains(t, err.Error(), "retry_after_seconds=420")

	clk.Advance(7*time.Minute + time.Nanosecond)
	_, err = s.Submit(agent, report(100))
	require.NoError(t, err, "the first report left the window")
	// The window now holds the reports from minutes 1, 2 and 10.
	_, err = s.Submit(agent, report(101))
	require.ErrorAs(t, err, &limited)
	assert.Equal(t, time.Minute-time.Nanosecond, limited.RetryAfter, "the report from minute 1 leaves next")

	clk.Advance(time.Minute)
	_, err = s.Submit(agent, report(102))
	require.NoError(t, err, "the report from minute 1 left the window")
}

func TestRateLimitIsPerClient(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, 1, time.Hour)

	_, err := s.Submit(agent, report(1))
	require.NoError(t, err)
	_, err = s.Submit(agent, report(2))
	var limited *RateLimitError
	require.ErrorAs(t, err, &limited)

	_, err = s.Submit(Client{Name: "other"}, report(2))
	require.NoError(t, err)
	_, err = s.Submit(Client{}, report(3))
	require.NoError(t, err, "a client without a name is counted as unknown")
	_, err = s.Submit(Client{Name: "  "}, report(4))
	require.ErrorAs(t, err, &limited)
}

func TestRateLimitSurvivesRestartAndIsSharedByStores(t *testing.T) {
	t.Parallel()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "feedback.jsonl")
	open := func() *Store {
		s, err := NewStore(path, 2, time.Hour, clk.Now)
		require.NoError(t, err)
		return s
	}

	_, err := open().Submit(agent, report(1))
	require.NoError(t, err)
	_, err = open().Submit(agent, report(2))
	require.NoError(t, err)

	_, err = open().Submit(agent, report(3))
	var limited *RateLimitError
	require.ErrorAs(t, err, &limited)
}

func TestDuplicatesAreNotStoredOrCounted(t *testing.T) {
	t.Parallel()
	s, clk, path := newTestStore(t, 2, time.Hour)

	first, err := s.Submit(agent, report(1))
	require.NoError(t, err)
	for range 5 {
		clk.Advance(time.Second)
		dup, err := s.Submit(agent, report(1))
		require.NoError(t, err)
		assert.True(t, dup.Deduplicated)
		assert.Equal(t, first.ID, dup.ID)
	}
	_, err = s.Submit(agent, report(2))
	require.NoError(t, err, "duplicates did not use the quota")

	// A duplicate is still answered when the client is over the limit.
	dup, err := s.Submit(agent, report(2))
	require.NoError(t, err)
	assert.True(t, dup.Deduplicated)

	entries, err := List(path, time.Time{}, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 2)

	// Another client or a later window stores the same report again.
	other, err := s.Submit(Client{Name: "other"}, report(1))
	require.NoError(t, err)
	assert.False(t, other.Deduplicated)
	clk.Advance(2 * time.Hour)
	again, err := s.Submit(agent, report(1))
	require.NoError(t, err)
	assert.False(t, again.Deduplicated)
}

func TestMalformedLinesAreSkipped(t *testing.T) {
	t.Parallel()
	s, _, path := newTestStore(t, 1, time.Hour)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	junk := "not json\n{\"id\":\"x\"}\n\n{\"id\":\"cut\",\"time\":\"2026-01-0"
	require.NoError(t, os.WriteFile(path, []byte(junk), 0o600))

	_, err := s.Submit(agent, report(1))
	require.NoError(t, err)

	entries, err := List(path, time.Time{}, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestValidation(t *testing.T) {
	t.Parallel()
	s, _, path := newTestStore(t, 5, time.Hour)
	long := func(n int) string { return strings.Repeat("é", n) }

	for name, in := range map[string]Input{
		"unknown category":   {Category: "praise", Summary: "x"},
		"empty category":     {Summary: "x"},
		"empty summary":      {Category: "bug", Summary: "   "},
		"summary too long":   {Category: "bug", Summary: long(MaxSummary + 1)},
		"details too long":   {Category: "bug", Summary: "x", Details: long(MaxDetails + 1)},
		"fix too long":       {Category: "bug", Summary: "x", ProposedFix: long(MaxProposedFix + 1)},
		"related tool long":  {Category: "bug", Summary: "x", RelatedTool: long(MaxRelatedTool + 1)},
		"category not exact": {Category: "bug!", Summary: "x"},
	} {
		_, err := s.Submit(agent, in)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "rejected reports write nothing")

	_, err = s.Submit(agent, Input{Category: "bug", Summary: long(MaxSummary), Details: long(MaxDetails)})
	require.NoError(t, err, "limits count characters, not bytes")
}

func TestLongClientNamesAreClipped(t *testing.T) {
	t.Parallel()
	s, _, path := newTestStore(t, 5, time.Hour)

	_, err := s.Submit(Client{Name: strings.Repeat("n", 500), Version: strings.Repeat("v", 500)}, report(1))
	require.NoError(t, err)

	entries, err := List(path, time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Len(t, entries[0].Client.Name, maxClientName)
	assert.Len(t, entries[0].Client.Version, maxClientName)
}

func TestConcurrentSubmitsRespectTheLimit(t *testing.T) {
	t.Parallel()
	s, _, path := newTestStore(t, 5, time.Hour)

	var wg sync.WaitGroup
	var accepted, limited atomic.Int32
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Submit(agent, report(i))
			var rl *RateLimitError
			switch {
			case err == nil:
				accepted.Add(1)
			case errors.As(err, &rl):
				limited.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(5), accepted.Load())
	assert.Equal(t, int32(15), limited.Load())
	entries, err := List(path, time.Time{}, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 5)
}

func TestList(t *testing.T) {
	t.Parallel()
	s, clk, path := newTestStore(t, 10, time.Hour)

	entries, err := List(path, time.Time{}, 0)
	require.NoError(t, err)
	assert.Empty(t, entries, "a missing file is an empty list")

	start := clk.Now()
	for i := range 4 {
		_, err := s.Submit(agent, report(i))
		require.NoError(t, err)
		clk.Advance(time.Minute)
	}

	entries, err = List(path, time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, entries, 4)
	assert.Equal(t, "problem 3", entries[0].Summary)
	assert.Equal(t, "problem 0", entries[3].Summary)

	entries, err = List(path, time.Time{}, 2)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "problem 2", entries[1].Summary)

	entries, err = List(path, start.Add(2*time.Minute), 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "problem 2", entries[1].Summary)
}

func TestReadErrorIsReported(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewStore(dir, 1, time.Hour, nil) // a folder, not a file
	require.NoError(t, err)

	_, err = s.Submit(agent, report(1))
	require.Error(t, err)
	_, err = List(dir, time.Time{}, 0)
	require.Error(t, err)
}

func TestNewStoreRejectsBadSettings(t *testing.T) {
	t.Parallel()
	_, err := NewStore("", 1, time.Hour, nil)
	require.Error(t, err)
	_, err = NewStore("f", 0, time.Hour, nil)
	require.Error(t, err)
	_, err = NewStore("f", 1, 0, nil)
	require.Error(t, err)
	_, err = NewStore("f", 1, time.Hour, nil)
	require.NoError(t, err)
}

func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	assert.Equal(t, filepath.Join(home, ".teamscache", "feedback.jsonl"), DefaultPath())
}
