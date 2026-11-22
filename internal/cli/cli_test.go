package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stn1slv/teamscache/internal/feedback"
	"github.com/stn1slv/teamscache/internal/idbtest"
)

func sampleCache(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sample.leveldb")
	require.NoError(t, idbtest.WriteTeamsSample(dir, time.Now()))
	return dir
}

func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewRootCommand("test", &out, &errOut)
	root.SetArgs(args)
	err = root.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func TestCommands(t *testing.T) {
	t.Parallel()
	cache := sampleCache(t)
	tests := []struct {
		args      []string
		wantCount int
		wantFirst map[string]any
	}{
		{[]string{"accounts"}, 1, map[string]any{"key": idbtest.SampleAccount, "label": "Org"}},
		{[]string{"conversations"}, 1, map[string]any{"id": "c1", "title": "Project"}},
		{[]string{"search", "BUDGET"}, 1, map[string]any{"message_id": "m1", "content": "Budget <draft> is ready", "reply_chain_id": "r1", "content_type": "RichText/Html"}},
		{[]string{"search", "budget", "--account", "other:account"}, 0, nil},
		{[]string{"read", "c1"}, 2, map[string]any{"message_id": "m0"}},
		{[]string{"recent", "--days", "7"}, 2, map[string]any{"message_id": "m1"}},
		{[]string{"recent", "--days", "7", "--limit", "1"}, 1, nil},
		{[]string{"mentions", "--unread"}, 1, map[string]any{"conversation": "Project", "sender": "Ann Lee (Org)", "is_read": false}},
		{[]string{"overview"}, 1, map[string]any{"conversation": "Project", "count": 2.0}},
	}
	for _, tt := range tests {
		t.Run(tt.args[0], func(t *testing.T) {
			t.Parallel()
			stdout, stderr, err := runCLI(t, append(tt.args, "--leveldb", cache)...)
			require.NoError(t, err)

			var rows []map[string]any
			require.NoError(t, json.Unmarshal([]byte(stdout), &rows), stdout)
			require.Len(t, rows, tt.wantCount)
			for k, v := range tt.wantFirst {
				assert.Equal(t, v, rows[0][k], k)
			}
			assert.Contains(t, stderr, "record(s) skipped")
		})
	}
}

func TestOutputKeepsHTMLCharacters(t *testing.T) {
	t.Parallel()
	stdout, _, err := runCLI(t, "search", "budget", "--leveldb", sampleCache(t))
	require.NoError(t, err)
	assert.Contains(t, stdout, "Budget <draft> is ready")
	assert.NotContains(t, stdout, `\u003c`, "JSON output does not escape < as \\u003c")
}

func TestMissingCacheIsAnError(t *testing.T) {
	t.Parallel()
	_, _, err := runCLI(t, "accounts", "--leveldb", filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}

func TestNegativeValuesAreRejected(t *testing.T) {
	t.Parallel()
	cache := sampleCache(t)
	for _, args := range [][]string{
		{"mentions", "--max-content", "-1"},
		{"recent", "--days", "-1"},
		{"overview", "--days", "-3"},
		{"search", "x", "--limit", "-1"},
		{"mcp", "--cache-ttl", "-5s"},
	} {
		_, _, err := runCLI(t, append(args, "--leveldb", cache)...)
		require.ErrorIs(t, err, ErrInvalidFlag, args)
	}
}

func TestAccountsHonorsTheAccountFlag(t *testing.T) {
	t.Parallel()
	cache := sampleCache(t)

	stdout, _, err := runCLI(t, "accounts", "--account", "other:account", "--leveldb", cache)

	require.NoError(t, err)
	assert.JSONEq(t, "[]", stdout)
}

func TestDumpCommand(t *testing.T) {
	t.Parallel()
	cache := sampleCache(t)

	stdout, stderr, err := runCLI(t, "dump", "--leveldb", cache, "--max-content", "0")

	require.NoError(t, err)
	assert.Contains(t, stderr, "result(s)")
	var got struct {
		Accounts      []map[string]any `json:"accounts"`
		Conversations []map[string]any `json:"conversations"`
		Messages      []map[string]any `json:"messages"`
		Mentions      []map[string]any `json:"mentions"`
		Partial       bool             `json:"partial"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Len(t, got.Accounts, 1)
	assert.Len(t, got.Conversations, 1)
	assert.Len(t, got.Messages, 2)
	assert.Len(t, got.Mentions, 1)
	assert.False(t, got.Partial)
}

func TestAccountsCommandReadsProfiles(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "profiles.leveldb")
	p := func(k string, v any) idbtest.Prop { return idbtest.Prop{Key: k, Value: v} }
	require.NoError(t, idbtest.Write(dir, "", []idbtest.Database{
		{
			Number: 1, Name: "Teams:replychain-manager:react-web-client:t1:u1:en-us",
			Stores: []idbtest.Store{{Name: "replychains"}},
		},
		{
			Number: 2, Name: "Teams:profiles:react-web-client:t1:u1:en-us",
			Stores: []idbtest.Store{{Name: "profiles", Records: []idbtest.Record{
				{Key: "u1", Value: idbtest.Obj{
					p("objectId", "u1"), p("tenantName", "Acme Corp"), p("displayName", "Ann Lee"), p("userPrincipalName", "ann@acme.test"),
				}},
			}}},
		},
	}))

	stdout, _, err := runCLI(t, "accounts", "--leveldb", dir)

	require.NoError(t, err)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "Acme Corp", rows[0]["label"])
	assert.Equal(t, "Acme Corp", rows[0]["tenant_name"])
	assert.Equal(t, "Ann Lee", rows[0]["display_name"])
	assert.Equal(t, "ann@acme.test", rows[0]["email"])
}

func TestProfilesCommandAndProfileFlag(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runCLI(t, "profiles")

	require.NoError(t, err)
	assert.Contains(t, stderr, "profile(s)")
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &rows))

	_, _, err = runCLI(t, "accounts", "--leveldb", sampleCache(t), "--profile", "Default")
	require.ErrorIs(t, err, ErrInvalidFlag)

	_, _, err = runCLI(t, "accounts", "--profile", "no-such-profile-xyz")
	require.Error(t, err)
}

func TestHelpAndEnvVarPrecedence(t *testing.T) {
	cache := sampleCache(t)
	t.Setenv(envProfile, "no-such-profile-xyz")

	_, _, err := runCLI(t, "help")
	require.NoError(t, err, "help does not resolve profiles")

	stdout, _, err := runCLI(t, "accounts", "--leveldb", cache)
	require.NoError(t, err, "explicit --leveldb overrides TEAMSCACHE_PROFILE")
	assert.Contains(t, stdout, idbtest.SampleAccount)

	t.Setenv(envLevelDB, cache)
	_, _, err = runCLI(t, "accounts")
	require.ErrorIs(t, err, ErrInvalidFlag, "both env vars set without CLI override fails")
}

func TestDumpCommandFiltersAccountAndDays(t *testing.T) {
	t.Parallel()
	cache := sampleCache(t)

	stdout, _, err := runCLI(t, "dump", "--leveldb", cache, "--account", idbtest.SampleAccount, "--days", "7")

	require.NoError(t, err)
	var got struct {
		Accounts      []map[string]any `json:"accounts"`
		Conversations []map[string]any `json:"conversations"`
		Messages      []map[string]any `json:"messages"`
		Mentions      []map[string]any `json:"mentions"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Len(t, got.Accounts, 1)
	assert.Len(t, got.Conversations, 1)
	assert.Len(t, got.Messages, 2)
	assert.Len(t, got.Mentions, 1)
}

// seedFeedback writes n reports, one minute apart ending now, and returns the file.
func seedFeedback(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feedback.jsonl")
	now := time.Now().Add(-time.Duration(n) * time.Minute)
	store, err := feedback.NewStore(path, n, time.Hour, func() time.Time { now = now.Add(time.Minute); return now })
	require.NoError(t, err)
	for i := range n {
		_, err := store.Submit(feedback.Client{Name: "agent"}, feedback.Input{Category: "bug", Summary: fmt.Sprintf("problem %d", i)})
		require.NoError(t, err)
	}
	return path
}

func TestFeedbackCommand(t *testing.T) {
	t.Parallel()
	path := seedFeedback(t, 3)

	stdout, stderr, err := runCLI(t, "feedback", "--feedback-file", path)
	require.NoError(t, err)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &rows), stdout)
	require.Len(t, rows, 3)
	assert.Equal(t, "problem 2", rows[0]["summary"], "newest first")
	assert.Equal(t, "# 3 result(s)\n", stderr)

	stdout, _, err = runCLI(t, "feedback", "--feedback-file", path, "--limit", "1")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(stdout), &rows))
	assert.Len(t, rows, 1)

	stdout, _, err = runCLI(t, "feedback", "--feedback-file", path, "--days", "1")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(stdout), &rows))
	assert.Len(t, rows, 3)
}

func TestFeedbackCommandWithoutAFileIsEmpty(t *testing.T) {
	t.Parallel()

	stdout, stderr, err := runCLI(t, "feedback", "--feedback-file", filepath.Join(t.TempDir(), "none.jsonl"))

	require.NoError(t, err)
	assert.JSONEq(t, "[]", stdout)
	assert.Equal(t, "# 0 result(s)\n", stderr)
}

func TestFeedbackCommandNeedsAFile(t *testing.T) {
	t.Parallel()

	_, _, err := runCLI(t, "feedback", "--feedback-file", "")

	require.ErrorIs(t, err, ErrInvalidFlag)
}

func TestFeedbackFileEnvDefault(t *testing.T) {
	path := seedFeedback(t, 2)
	t.Setenv(envFeedbackFile, path)

	stdout, _, err := runCLI(t, "feedback")

	require.NoError(t, err)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &rows))
	assert.Len(t, rows, 2)
}

func TestMCPFeedbackRateFlagsAreValidated(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "feedback.jsonl")

	for _, args := range [][]string{
		{"mcp", "--feedback-file", file, "--feedback-rate-limit", "-1"},
		{"mcp", "--feedback-file", file, "--feedback-rate-window", "-1s"},
		{"mcp", "--feedback-file", file, "--feedback-rate-limit", "0"},
		{"mcp", "--feedback-file", file, "--feedback-rate-window", "0s"},
	} {
		_, _, err := runCLI(t, args...)
		require.ErrorIs(t, err, ErrInvalidFlag, args)
	}
}

func TestFeedbackStoreIsOptionalWithoutAFile(t *testing.T) {
	t.Parallel()
	g := &globals{stderr: &bytes.Buffer{}}

	store, err := g.feedbackStore(10, time.Minute)

	require.NoError(t, err)
	assert.Nil(t, store)
}
