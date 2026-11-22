package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stn1slv/teamscache/internal/feedback"
)

// feedbackSession connects a client to a server whose Teams cache does not
// exist, to show that report_feedback does not need it.
func feedbackSession(t *testing.T, store *feedback.Store, clientName string) *mcp.ClientSession {
	t.Helper()
	return connectWith(t, "", filepath.Join(t.TempDir(), "missing.leveldb"), store, clientName)
}

func newStore(t *testing.T, limit int) (*feedback.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feedback.jsonl")
	store, err := feedback.NewStore(path, limit, time.Hour, nil)
	require.NoError(t, err)
	return store, path
}

func report(t *testing.T, session *mcp.ClientSession, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "report_feedback", Arguments: args})
	require.NoError(t, err)
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return text.Text
}

func TestReportFeedbackIsListedWithoutAlarmingHints(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t, 5)
	session := feedbackSession(t, store, "test")

	res, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	var found *mcp.Tool
	for _, tool := range res.Tools {
		if tool.Name == "report_feedback" {
			found = tool
			continue
		}
		assert.True(t, tool.Annotations.ReadOnlyHint, tool.Name)
	}
	require.NotNil(t, found)
	assert.Len(t, res.Tools, 9)
	assert.False(t, found.Annotations.ReadOnlyHint)
	assert.False(t, found.Annotations.IdempotentHint)
	require.NotNil(t, found.Annotations.DestructiveHint)
	assert.False(t, *found.Annotations.DestructiveHint)
	require.NotNil(t, found.Annotations.OpenWorldHint)
	assert.False(t, *found.Annotations.OpenWorldHint)
	assert.Contains(t, found.Description, "NEVER include message text")

	schema, err := json.Marshal(found.OutputSchema)
	require.NoError(t, err)
	assert.Contains(t, string(schema), `"type":"object"`)
	assert.Contains(t, string(schema), "request_id")
}

func TestReportFeedbackIsAbsentWithoutAStore(t *testing.T) {
	t.Parallel()
	session := connect(t, "")

	res, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	for _, tool := range res.Tools {
		assert.NotEqual(t, "report_feedback", tool.Name)
	}
	assert.Len(t, res.Tools, 8)
}

func TestReportFeedbackStoresTheReport(t *testing.T) {
	t.Parallel()
	store, path := newStore(t, 5)
	session := feedbackSession(t, store, "claude-code")

	res := report(t, session, map[string]any{
		"category": "search_miss", "summary": "search found nothing", "details": "read_conversation has it",
		"related_tool": "search_messages", "proposed_fix": "also search titles",
	})

	require.False(t, res.IsError, resultText(t, res))
	var out FeedbackResult
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Len(t, out.RequestID, 16)
	assert.Equal(t, feedbackReport{Category: "search_miss", Received: true}, out.Report)
	assert.Contains(t, resultText(t, res), out.RequestID, "clients that ignore structured content get the same data as text")

	entries, err := feedback.List(path, time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, out.RequestID, entries[0].ID)
	assert.Equal(t, feedback.Client{Name: "claude-code", Version: "1.2"}, entries[0].Client)
	assert.Equal(t, "search_messages", entries[0].RelatedTool)
	assert.Equal(t, "also search titles", entries[0].ProposedFix)
}

func TestReportFeedbackDeduplicates(t *testing.T) {
	t.Parallel()
	store, path := newStore(t, 5)
	session := feedbackSession(t, store, "test")
	args := map[string]any{"category": "bug", "summary": "same problem"}

	first := report(t, session, args)
	second := report(t, session, args)

	require.False(t, second.IsError)
	assert.Contains(t, resultText(t, second), `"deduplicated":true`)
	assert.NotContains(t, resultText(t, first), `"deduplicated":true`)
	entries, err := feedback.List(path, time.Time{}, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestReportFeedbackRejectsInvalidReports(t *testing.T) {
	t.Parallel()
	store, path := newStore(t, 5)
	session := feedbackSession(t, store, "test")

	for name, args := range map[string]map[string]any{
		"unknown category": {"category": "praise", "summary": "x"},
		"empty summary":    {"category": "bug", "summary": " "},
		"long summary":     {"category": "bug", "summary": strings.Repeat("x", feedback.MaxSummary+1)},
	} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "report_feedback", Arguments: args})
		assert.True(t, err != nil || res.IsError, name)
	}
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "report_feedback", Arguments: map[string]any{"summary": "no category"}})
	assert.True(t, err != nil || res.IsError, "a call without the required category fails")

	entries, err := feedback.List(path, time.Time{}, 0)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestReportFeedbackRateLimitIsAToolErrorPerClient(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t, 2)
	session := feedbackSession(t, store, "agent-a")

	for _, summary := range []string{"one", "two"} {
		res := report(t, session, map[string]any{"category": "bug", "summary": summary})
		require.False(t, res.IsError, resultText(t, res))
	}
	limited := report(t, session, map[string]any{"category": "bug", "summary": "three"})

	require.True(t, limited.IsError, "the refusal is a tool error, so the agent returns to its task")
	assert.Contains(t, resultText(t, limited), "retry_after_seconds=")

	other := feedbackSession(t, store, "agent-b")
	res := report(t, other, map[string]any{"category": "bug", "summary": "three"})
	assert.False(t, res.IsError, "another client has its own quota")
}
