// Package mcpserver exposes the Teams cache over the Model Context Protocol,
// read-only. Its one write is report_feedback, which appends to a local file.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stn1slv/teamscache/internal/feedback"
	"github.com/stn1slv/teamscache/internal/leveldb"
	"github.com/stn1slv/teamscache/internal/service"
	"github.com/stn1slv/teamscache/internal/teams"
)

// Result wraps a list, so that structured content is a JSON object as older
// MCP clients expect.
type Result[T any] struct {
	Result          []T              `json:"result"`
	SkippedRecords  int              `json:"skipped_records"`
	DamagedSections []leveldb.Damage `json:"damaged_sections"`
	Partial         bool             `json:"partial"`
}

func result[T any](snap *service.Snapshot, rows []T) Result[T] {
	return Result[T]{
		Result: rows, SkippedRecords: snap.Skipped, DamagedSections: snap.Damage,
		Partial: snap.Skipped > 0 || len(snap.Damage) > 0,
	}
}

type accountArg struct {
	Account string `json:"account,omitempty" jsonschema:"account key <tenantId>:<userId> from list_accounts; empty for all accounts"`
}

type listConversationsArgs struct {
	accountArg
	Limit *int `json:"limit,omitempty" jsonschema:"maximum number of results (default 100)"`
}

type readConversationArgs struct {
	accountArg
	ConversationID string `json:"conversation_id" jsonschema:"conversation id from list_conversations, overview or mentions"`
	Limit          *int   `json:"limit,omitempty" jsonschema:"maximum number of messages (default 50)"`
}

type searchArgs struct {
	accountArg
	Query string `json:"query" jsonschema:"text to find in message content or sender name, ignoring case"`
	Days  *int   `json:"days,omitempty" jsonschema:"only messages of the last N days; 0 for all"`
	Limit *int   `json:"limit,omitempty" jsonschema:"maximum number of results (default 50)"`
}

type recentArgs struct {
	accountArg
	Days  *int `json:"days,omitempty" jsonschema:"messages of the last N days (default 7)"`
	Limit *int `json:"limit,omitempty" jsonschema:"maximum number of results (default 200)"`
}

type mentionsArgs struct {
	accountArg
	Days       *int  `json:"days,omitempty" jsonschema:"only mentions of the last N days; 0 for all"`
	UnreadOnly *bool `json:"unread_only,omitempty" jsonschema:"only mentions not read yet"`
	Limit      *int  `json:"limit,omitempty" jsonschema:"maximum number of results (default 100)"`
}

type unreadArgs struct {
	accountArg
	Days  *int `json:"days,omitempty" jsonschema:"only mentions of the last N days (default 30)"`
	Limit *int `json:"limit,omitempty" jsonschema:"maximum number of results (default 100)"`
}

type overviewArgs struct {
	accountArg
	Days  *int `json:"days,omitempty" jsonschema:"activity of the last N days (default 7)"`
	Limit *int `json:"limit,omitempty" jsonschema:"maximum number of results (default 100)"`
}

type feedbackArgs struct {
	Category    string `json:"category" jsonschema:"one of: bug, unexpected_output, missing_capability, tool_description, search_miss, other"`
	Summary     string `json:"summary" jsonschema:"one sentence describing the problem, at most 200 characters"`
	Details     string `json:"details,omitempty" jsonschema:"what you called, what you expected and what happened, at most 2000 characters"`
	RelatedTool string `json:"related_tool,omitempty" jsonschema:"name of the tool this is about, for example search_messages"`
	ProposedFix string `json:"proposed_fix,omitempty" jsonschema:"a change to the tool, its description or its schema that would help, at most 2000 characters"`
}

type feedbackReport struct {
	Category     string `json:"category"`
	Received     bool   `json:"received"`
	Deduplicated bool   `json:"deduplicated"`
}

// FeedbackResult is the output of report_feedback.
type FeedbackResult struct {
	RequestID string         `json:"request_id"`
	Report    feedbackReport `json:"report"`
}

const reportFeedbackDescription = "Report a problem with these Teams tools so that the maintainer can fix it: a tool error that looks wrong, " +
	"an output that contradicts what the user sees in Teams, a search that missed something you found another way, " +
	"a misleading tool description, or a capability the user asked for that does not exist. " +
	"Do not use it for ordinary empty results or for the user's own mistakes. " +
	"The report is saved in a local file and is not sent anywhere. Rate limited; if it is refused, go back to the user's task and do not retry. " +
	"NEVER include message text, people's names, email addresses, or tenant, user and conversation ids. " +
	"Describe the problem in general terms, for example 'search_messages returned no rows for a term that read_conversation shows'."

// ErrInvalidArgument reports a tool argument the server does not accept.
var ErrInvalidArgument = errors.New("invalid argument")

func orDefault[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

// days returns the days argument or its default. Negative values are errors.
func days(p *int, def int) (int, error) {
	d := orDefault(p, def)
	if d < 0 {
		return 0, fmt.Errorf("%w: days must be 0 or more, got %d", ErrInvalidArgument, d)
	}
	return d, nil
}

// server holds what the tool handlers share.
type server struct {
	cache *service.Cache
	opts  service.Options
	// account, when set, is the only account the tools serve.
	account string
}

func (s *server) messageResult(ctx context.Context, snap *service.Snapshot, msgs []teams.Message) (*mcp.CallToolResult, Result[service.MessageRow], error) {
	rows, err := service.Rows(ctx, msgs, s.opts.MaxContent)
	if err != nil {
		return nil, Result[service.MessageRow]{}, err
	}
	return nil, result(snap, rows), nil
}

// scope returns the account filter of a tool call. A server bound to one
// account always uses it and rejects requests for another account.
func (s *server) scope(requested string) (string, error) {
	if s.account == "" {
		return requested, nil
	}
	if requested != "" && requested != s.account {
		return "", fmt.Errorf("%w: this server only serves account %s", ErrInvalidArgument, s.account)
	}
	return s.account, nil
}

// prepare checks the account and limit arguments and returns the parsed
// cache.
func (s *server) prepare(ctx context.Context, requested string, limit *int) (*service.Snapshot, string, error) {
	if limit != nil && *limit < 0 {
		return nil, "", fmt.Errorf("%w: limit must be 0 or more, got %d", ErrInvalidArgument, *limit)
	}
	account, err := s.scope(requested)
	if err != nil {
		return nil, "", err
	}
	snap, err := s.cache.Snapshot(ctx)
	if err != nil {
		return nil, "", err
	}
	return snap, account, nil
}

// New returns an MCP server with the read-only tools. When account is not
// empty, every tool serves only that account. When store is not nil, it also
// has report_feedback, which appends to the store's file and nothing else.
func New(cache *service.Cache, opts service.Options, version, account string, store *feedback.Store) *mcp.Server {
	srv := &server{cache: cache, opts: opts, account: account}
	s := mcp.NewServer(&mcp.Implementation{Name: "teamscache", Version: version}, nil)
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	tool := func(name, description string) *mcp.Tool {
		return &mcp.Tool{Name: name, Description: description, Annotations: readOnly}
	}

	mcp.AddTool(s, tool("list_accounts",
		"List the Teams accounts (tenant and user contexts) in the local cache. Each has a key (use it as the account filter of the other tools), tenant_id, user_id, and a label from the user's profile (tenant_name) or organization names in messages, plus optional display_name and email."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, Result[service.Account], error) {
			snap, account, err := srv.prepare(ctx, "", nil)
			if err != nil {
				return nil, Result[service.Account]{}, err
			}
			out := []service.Account{}
			for _, a := range snap.Accounts {
				if err := ctx.Err(); err != nil {
					return nil, Result[service.Account]{}, err
				}
				if account == "" || a.Key == account {
					out = append(out, a)
				}
			}
			return nil, result(snap, out), ctx.Err()
		})

	mcp.AddTool(s, tool("list_conversations",
		"List chats and channels (id, title, type), optionally for one account."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in listConversationsArgs) (*mcp.CallToolResult, Result[teams.Conversation], error) {
			snap, account, err := srv.prepare(ctx, in.Account, in.Limit)
			if err != nil {
				return nil, Result[teams.Conversation]{}, err
			}
			convs, err := snap.ListConversations(ctx, account, orDefault(in.Limit, 100))
			if err != nil {
				return nil, Result[teams.Conversation]{}, err
			}
			for i := range convs {
				if err := ctx.Err(); err != nil {
					return nil, Result[teams.Conversation]{}, err
				}
				convs[i].LastMessage = service.Truncate(convs[i].LastMessage, srv.opts.MaxContent)
			}
			return nil, result(snap, convs), ctx.Err()
		})

	mcp.AddTool(s, tool("read_conversation",
		"Return the most recent messages of a conversation, oldest first."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in readConversationArgs) (*mcp.CallToolResult, Result[service.MessageRow], error) {
			if strings.TrimSpace(in.ConversationID) == "" {
				return nil, Result[service.MessageRow]{}, fmt.Errorf("%w: conversation_id is required", ErrInvalidArgument)
			}
			snap, account, err := srv.prepare(ctx, in.Account, in.Limit)
			if err != nil {
				return nil, Result[service.MessageRow]{}, err
			}
			msgs, err := snap.ReadConversation(ctx, in.ConversationID, account, orDefault(in.Limit, 50))
			if err != nil {
				return nil, Result[service.MessageRow]{}, err
			}
			return srv.messageResult(ctx, snap, msgs)
		})

	mcp.AddTool(s, tool("search_messages",
		"Search cached messages for text in the content or sender name, ignoring case. Optional days keeps only messages of the last N days."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchArgs) (*mcp.CallToolResult, Result[service.MessageRow], error) {
			if strings.TrimSpace(in.Query) == "" {
				return nil, Result[service.MessageRow]{}, fmt.Errorf("%w: query is required", ErrInvalidArgument)
			}
			d, err := days(in.Days, 0)
			if err != nil {
				return nil, Result[service.MessageRow]{}, err
			}
			snap, account, err := srv.prepare(ctx, in.Account, in.Limit)
			if err != nil {
				return nil, Result[service.MessageRow]{}, err
			}
			msgs, err := snap.Search(ctx, in.Query, account, d, orDefault(in.Limit, 50), opts)
			if err != nil {
				return nil, Result[service.MessageRow]{}, err
			}
			return srv.messageResult(ctx, snap, msgs)
		})

	mcp.AddTool(s, tool("recent_messages",
		"Messages received in the last N days, newest first, in one call. Use it for 'what did I miss'. Filter by account (see list_accounts) to keep one tenant."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in recentArgs) (*mcp.CallToolResult, Result[service.MessageRow], error) {
			d, err := days(in.Days, 7)
			if err != nil {
				return nil, Result[service.MessageRow]{}, err
			}
			snap, account, err := srv.prepare(ctx, in.Account, in.Limit)
			if err != nil {
				return nil, Result[service.MessageRow]{}, err
			}
			msgs, err := snap.Recent(ctx, d, account, orDefault(in.Limit, 200), opts)
			if err != nil {
				return nil, Result[service.MessageRow]{}, err
			}
			return srv.messageResult(ctx, snap, msgs)
		})

	mcp.AddTool(s, tool("mentions",
		"@-mentions of you, newest first, with content. unread_only keeps mentions not read yet; days limits how old they can be. This is the only reliable read or unread signal in the local cache."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mentionsArgs) (*mcp.CallToolResult, Result[service.MentionRow], error) {
			d, err := days(in.Days, 0)
			if err != nil {
				return nil, Result[service.MentionRow]{}, err
			}
			snap, account, err := srv.prepare(ctx, in.Account, in.Limit)
			if err != nil {
				return nil, Result[service.MentionRow]{}, err
			}
			rows, err := snap.MentionsQuery(ctx, d, orDefault(in.UnreadOnly, false), account, orDefault(in.Limit, 100), opts)
			if err != nil {
				return nil, Result[service.MentionRow]{}, err
			}
			return nil, result(snap, rows), nil
		})

	mcp.AddTool(s, tool("unread_messages",
		"Unread items that need attention: your unread @-mentions. The local cache has no general read marker, so all unread messages cannot be found. For everything that arrived while you were away, use recent_messages."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in unreadArgs) (*mcp.CallToolResult, Result[service.MentionRow], error) {
			d, err := days(in.Days, 30)
			if err != nil {
				return nil, Result[service.MentionRow]{}, err
			}
			snap, account, err := srv.prepare(ctx, in.Account, in.Limit)
			if err != nil {
				return nil, Result[service.MentionRow]{}, err
			}
			rows, err := snap.MentionsQuery(ctx, d, true, account, orDefault(in.Limit, 100), opts)
			if err != nil {
				return nil, Result[service.MentionRow]{}, err
			}
			return nil, result(snap, rows), nil
		})

	mcp.AddTool(s, tool("overview",
		"Who messaged you: recent activity grouped by conversation, newest first. Per conversation: title, conversation_id (for read_conversation), message count, senders, last message and its time. A good first call."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in overviewArgs) (*mcp.CallToolResult, Result[service.OverviewRow], error) {
			d, err := days(in.Days, 7)
			if err != nil {
				return nil, Result[service.OverviewRow]{}, err
			}
			snap, account, err := srv.prepare(ctx, in.Account, in.Limit)
			if err != nil {
				return nil, Result[service.OverviewRow]{}, err
			}
			rows, err := snap.Overview(ctx, d, account, opts)
			if err != nil {
				return nil, Result[service.OverviewRow]{}, err
			}
			limit := orDefault(in.Limit, 100)
			if limit > 0 && len(rows) > limit {
				rows = rows[:limit]
			}
			return nil, result(snap, rows), nil
		})

	if store != nil {
		addReportFeedback(s, store)
	}
	return s
}

// addReportFeedback registers report_feedback. It does not read the Teams
// cache, so it works when the cache is missing or damaged.
func addReportFeedback(s *mcp.Server, store *feedback.Store) {
	// Unset hints default to destructive and open-world, which makes strict
	// hosts warn about a tool that only appends to a local file.
	no := false
	mcp.AddTool(s, &mcp.Tool{
		Name:        "report_feedback",
		Description: reportFeedbackDescription,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: false, OpenWorldHint: &no},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in feedbackArgs) (*mcp.CallToolResult, FeedbackResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, FeedbackResult{}, err
		}
		receipt, err := store.Submit(clientOf(req), feedback.Input{
			Category: in.Category, Summary: in.Summary, Details: in.Details,
			RelatedTool: in.RelatedTool, ProposedFix: in.ProposedFix,
		})
		if err != nil {
			return nil, FeedbackResult{}, err
		}
		return nil, FeedbackResult{
			RequestID: receipt.ID,
			Report:    feedbackReport{Category: receipt.Category, Received: true, Deduplicated: receipt.Deduplicated},
		}, nil
	})
}

// clientOf returns the MCP client that sent req, from its initialize request.
func clientOf(req *mcp.CallToolRequest) feedback.Client {
	if req == nil || req.Session == nil {
		return feedback.Client{}
	}
	params := req.Session.InitializeParams()
	if params == nil || params.ClientInfo == nil {
		return feedback.Client{}
	}
	return feedback.Client{Name: params.ClientInfo.Name, Version: params.ClientInfo.Version}
}
