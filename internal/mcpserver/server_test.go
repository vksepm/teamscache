package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stn1slv/teamscache/internal/feedback"
	"github.com/stn1slv/teamscache/internal/idbtest"
	"github.com/stn1slv/teamscache/internal/service"
)

func connect(t *testing.T, account string) *mcp.ClientSession {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sample.leveldb")
	require.NoError(t, idbtest.WriteTeamsSample(dir, time.Now()))
	return connectCache(t, account, dir)
}

func connectCache(t *testing.T, account, dir string) *mcp.ClientSession {
	t.Helper()
	return connectWith(t, account, dir, nil, "test")
}

// connectWith connects a client called clientName to a server that has the
// feedback store, when it is not nil.
func connectWith(t *testing.T, account, dir string, store *feedback.Store, clientName string) *mcp.ClientSession {
	t.Helper()
	cache := service.NewCache(dir, time.Minute, slog.New(slog.DiscardHandler))
	server := New(cache, service.Options{MaxContent: 6, Now: time.Now}, "test", account, store)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "1.2"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestTools(t *testing.T) {
	t.Parallel()
	session := connect(t, "")

	res, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
		assert.True(t, tool.Annotations.ReadOnlyHint, tool.Name)
	}
	for _, name := range []string{
		"list_accounts", "list_conversations", "read_conversation", "search_messages",
		"recent_messages", "mentions", "unread_messages", "overview",
	} {
		assert.True(t, names[name], name)
	}
	schema, err := json.Marshal(res.Tools[0].InputSchema)
	require.NoError(t, err)
	assert.Contains(t, string(schema), `"type":"object"`)
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) []map[string]any {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var out struct {
		Result          []map[string]any `json:"result"`
		Partial         bool             `json:"partial"`
		SkippedRecords  int              `json:"skipped_records"`
		DamagedSections json.RawMessage  `json:"damaged_sections"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.False(t, out.Partial)
	assert.Zero(t, out.SkippedRecords)
	assert.JSONEq(t, "[]", string(out.DamagedSections))
	return out.Result
}

func TestToolResults(t *testing.T) {
	t.Parallel()
	session := connect(t, "")

	accounts := callTool(t, session, "list_accounts", nil)
	require.Len(t, accounts, 1)
	assert.Equal(t, idbtest.SampleAccount, accounts[0]["key"])

	found := callTool(t, session, "search_messages", map[string]any{"query": "budget", "account": idbtest.SampleAccount})
	require.Len(t, found, 1)
	assert.Equal(t, "Budget…", found[0]["content"], "content is cut to the maximum length")
	assert.Equal(t, "c1", found[0]["conversation_id"])

	assert.Len(t, callTool(t, session, "recent_messages", map[string]any{"days": 7}), 2)
	assert.Len(t, callTool(t, session, "read_conversation", map[string]any{"conversation_id": "c1", "limit": 1}), 1)
	assert.Len(t, callTool(t, session, "unread_messages", nil), 1)
	assert.Equal(t, "Project", callTool(t, session, "overview", nil)[0]["conversation"])
}

func TestInvalidArgumentsAreRejected(t *testing.T) {
	t.Parallel()
	session := connect(t, "")

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_messages", Arguments: map[string]any{"days": 1}})

	assert.True(t, err != nil || res.IsError, "a call without the required query fails")
}

func TestServerBoundToOneAccount(t *testing.T) {
	t.Parallel()
	session := connect(t, idbtest.SampleAccount)

	assert.Len(t, callTool(t, session, "list_accounts", nil), 1)
	assert.Len(t, callTool(t, session, "recent_messages", map[string]any{"days": 7}), 2, "the bound account is used without an account argument")

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "recent_messages", Arguments: map[string]any{"account": "other:account"},
	})
	require.NoError(t, err)
	assert.True(t, res.IsError, "another account is refused")
}

func TestServerBoundToAnotherAccountSeesNothing(t *testing.T) {
	t.Parallel()
	session := connect(t, "other:account")

	assert.Empty(t, callTool(t, session, "list_accounts", nil))
	assert.Empty(t, callTool(t, session, "recent_messages", map[string]any{"days": 7}))
}

func TestNegativeDaysAreRejected(t *testing.T) {
	t.Parallel()
	session := connect(t, "")

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "overview", Arguments: map[string]any{"days": -1}})

	require.NoError(t, err)
	assert.True(t, res.IsError)
}

func TestNegativeLimitIsRejected(t *testing.T) {
	t.Parallel()
	session := connect(t, "")

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"recent_messages", map[string]any{"limit": -1}},
		{"overview", map[string]any{"limit": -1}},
	} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		require.NoError(t, err)
		assert.True(t, res.IsError, tc.name)
	}
}

func TestEmptyRequiredArgumentsAreRejected(t *testing.T) {
	t.Parallel()
	session := connect(t, "")

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"read_conversation", map[string]any{"conversation_id": "   "}},
		{"search_messages", map[string]any{"query": "   "}},
	} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		require.NoError(t, err)
		assert.True(t, res.IsError, tc.name)
	}
}

func TestOverviewRespectsLimit(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "cache.leveldb")
	p := func(k string, v any) idbtest.Prop { return idbtest.Prop{Key: k, Value: v} }
	nowMs := float64(time.Now().UnixMilli())
	require.NoError(t, idbtest.Write(dir, "", []idbtest.Database{{
		Number: 1, Name: "Teams:replychain-manager:react-web-client:t:u:en-us",
		Stores: []idbtest.Store{{Name: "replychains", Records: []idbtest.Record{
			{Key: "r1", Value: idbtest.Obj{p("conversationId", "c1"), p("messageMap", idbtest.Obj{p("m1", idbtest.Obj{p("id", "m1"), p("content", "one"), p("originalArrivalTime", nowMs)})})}},
			{Key: "r2", Value: idbtest.Obj{p("conversationId", "c2"), p("messageMap", idbtest.Obj{p("m2", idbtest.Obj{p("id", "m2"), p("content", "two"), p("originalArrivalTime", nowMs-1000)})})}},
		}}},
	}}))
	session := connectCache(t, "", dir)

	rows := callTool(t, session, "overview", map[string]any{"limit": 1})

	require.Len(t, rows, 1)
	assert.Equal(t, "c1", rows[0]["conversation_id"])
}

func TestEveryToolReportsPartialResults(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "cache.leveldb")
	require.NoError(t, idbtest.Write(dir, "", []idbtest.Database{{
		Number: 1, Name: "Teams:replychain-manager:react-web-client:t:u:en-us",
		Stores: []idbtest.Store{{Name: "replychains", Records: []idbtest.Record{
			{Key: "bad", Value: "bad", Wrapping: idbtest.Corrupt},
		}}},
	}}))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "999999.ldb"), []byte("corrupt"), 0o600))
	session := connectCache(t, "", dir)
	for _, name := range []string{
		"list_accounts", "list_conversations", "read_conversation", "search_messages",
		"recent_messages", "mentions", "unread_messages", "overview",
	} {
		args := map[string]any{}
		if name == "read_conversation" {
			args["conversation_id"] = "c"
		}
		if name == "search_messages" {
			args["query"] = "x"
		}
		response, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		require.False(t, response.IsError, name)
		raw, err := json.Marshal(response.StructuredContent)
		require.NoError(t, err)
		var diagnostics struct {
			Partial         bool             `json:"partial"`
			SkippedRecords  int              `json:"skipped_records"`
			DamagedSections []map[string]any `json:"damaged_sections"`
		}
		require.NoError(t, json.Unmarshal(raw, &diagnostics))
		assert.True(t, diagnostics.Partial, name)
		assert.Equal(t, 1, diagnostics.SkippedRecords, name)
		require.Len(t, diagnostics.DamagedSections, 1, name)
		assert.Equal(t, "999999.ldb", diagnostics.DamagedSections[0]["file"], name)
	}
}

func TestEveryToolReportsCompleteResults(t *testing.T) {
	t.Parallel()
	session := connect(t, "")
	for _, name := range []string{
		"list_accounts", "list_conversations", "read_conversation", "search_messages",
		"recent_messages", "mentions", "unread_messages", "overview",
	} {
		args := map[string]any{}
		if name == "read_conversation" {
			args["conversation_id"] = "c1"
		}
		if name == "search_messages" {
			args["query"] = "budget"
		}
		callTool(t, session, name, args)
	}
}
