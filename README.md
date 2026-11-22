# teamscache

`teamscache` reads the message cache of the new Microsoft Teams (v2) desktop client on your computer. It prints chats, messages and @-mentions as JSON, and it can also run as a read-only MCP server for AI assistants.

It does not use Microsoft Graph, OAuth, an Azure app registration or the network. It reads only what the signed-in Teams client already keeps on your disk.

## Why

To read Teams messages through Microsoft Graph, you need permissions such as `Chat.Read`, and many tenants allow them only after admin consent. As a guest in another company's tenant, you often cannot get that consent. The desktop client still keeps your recent conversations on your disk, and `teamscache` reads them from there.

## Install

With Homebrew on macOS or Linux:

```bash
brew install stn1slv/tap/teamscache
```

Binaries for macOS, Linux and Windows are on the [releases page](https://github.com/stn1slv/teamscache/releases).

With Go 1.27 or later:

```bash
go install github.com/stn1slv/teamscache/cmd/teamscache@latest
```

Or build from source with `make build`; the binary is written to `bin/teamscache`.

You need a new Teams client that has been signed in at least once. Auto-discovery of the cache works on macOS and Windows. On Linux it is best effort.

## Usage

### Common workflows

1. Read all cached messages in one chat (oldest first):

   ```bash
   teamscache conversations --limit 0       # find the conversation "id"
   teamscache read "19:abc@thread.v2" --limit 0
   ```

2. Filter by account or WebView2 profile when you have multiple tenants or profiles:

   ```bash
   teamscache accounts                      # get "<tenantId>:<userId>"
   teamscache overview --account "<tenantId>:<userId>"

   teamscache profiles                      # get profile folder name
   teamscache overview --profile "Default"
   ```

3. Save JSON output to a file (JSON goes to stdout, summary goes to stderr):

   ```bash
   teamscache read "19:abc@thread.v2" --limit 0 > chat.json
   ```

`teamscache` reads only messages stored in the local Teams desktop cache, not older history that Teams has not cached on disk. `read` returns messages oldest first; `recent`, `search`, `mentions`, and `overview` return newest first.

### Commands

```bash
teamscache accounts                      # tenant and user contexts in the cache
teamscache profiles                      # discovered WebView2 cache profiles
teamscache overview --days 7             # recent activity grouped by conversation
teamscache recent --days 7               # messages of the last 7 days, newest first
teamscache mentions --unread             # @-mentions of you that are not read yet
teamscache search "quarterly budget"     # text in message content or sender name, newest first
teamscache conversations                 # chats and channels (id, title, type, last message)
teamscache read "19:abc@thread.v2"       # last 50 messages of one conversation, oldest first
teamscache dump                          # accounts, conversations, messages and mentions in one JSON object
teamscache feedback                      # feedback that agents reported through the MCP server, newest first
```

All commands print JSON to stdout and a short summary to stderr, for example `# 12 result(s), 0 record(s) skipped, 0 damaged section(s)`. Skipped records could not be decoded. Damaged sections are unreadable file sections or missing referenced blob files, and are counted separately.

### Flags and defaults

| Command | Flags and defaults |
|---------|--------------------|
| `conversations` | `--limit 50` |
| `search QUERY` | `--limit 50`, `--days 0` |
| `read CONVERSATION_ID` | `--limit 50` |
| `recent` | `--limit 200`, `--days 7` |
| `mentions` | `--limit 100`, `--days 0`, `--unread` (`false`) |
| `overview` | `--days 7` |
| `dump` | `--days 0` |
| `feedback` | `--limit 50`, `--days 0` |
| `mcp` | `--cache-ttl 180s` (Go duration, such as `180s` or `3m`), `--feedback-rate-limit 10`, `--feedback-rate-window 10m` |

In those command flags, `--limit N` sets the maximum number of results (`0` means no limit). `--days N` keeps only items from the last N days (`0` means all dates, and `--limit` still applies). `--unread` keeps only unread @-mentions.

Global flags:

| Flag | Meaning |
|------|---------|
| `--account <tenantId>:<userId>` | Only one account. The keys come from `teamscache accounts`. With `mcp`, every tool serves only this account. |
| `--leveldb PATH` | Read this cache folder instead of the auto-discovered one. Cannot be combined with `--profile`. |
| `--profile NAME` | Choose a discovered WebView2 profile by folder name (see `teamscache profiles`). Cannot be combined with `--leveldb`. |
| `--max-content N` | Characters of message content in CLI `mentions`, `overview`, `dump`, and all MCP results (`0` means no limit, default `600`). CLI `conversations`, `read`, `recent`, and `search` print full content. |
| `--feedback-file PATH` | JSONL file for agent feedback (default `~/.teamscache/feedback.jsonl`). Used by `mcp` and `feedback`. |
| `-v`, `--verbose` | Log details, such as skipped records, to stderr. |

Environment variables:

| Variable | Meaning |
|----------|---------|
| `TEAMSCACHE_LEVELDB` | Default for `--leveldb` (cannot be set with `TEAMSCACHE_PROFILE`). |
| `TEAMSCACHE_PROFILE` | Default for `--profile` (cannot be set with `TEAMSCACHE_LEVELDB`). |
| `TEAMSCACHE_MAX_CONTENT` | Default for `--max-content`. |
| `TEAMSCACHE_CACHE_TTL` | Integer number of seconds the MCP server reuses a parsed cache (default `180`). |
| `TEAMSCACHE_FEEDBACK_FILE` | Default for `--feedback-file`. |
| `TEAMSCACHE_FEEDBACK_RATE_LIMIT` | Default for `--feedback-rate-limit`. |
| `TEAMSCACHE_FEEDBACK_RATE_WINDOW` | Integer number of seconds for the default of `--feedback-rate-window` (default `600`). |

The cache has no general read or unread marker. The read state of @-mentions is the only read state it keeps, so `mentions --unread` is the reliable way to find what needs your attention.

## MCP server

`teamscache mcp` runs a read-only MCP server on stdin and stdout. Example configuration for Claude Desktop or Claude Code:

```json
{
  "mcpServers": {
    "teams": { "command": "teamscache", "args": ["mcp"] }
  }
}
```

Pass `--max-content 0` for full message content in MCP tool results, or `--account <tenantId>:<userId>` to serve only one account:

```json
{
  "mcpServers": {
    "teams": {
      "command": "teamscache",
      "args": ["mcp", "--max-content", "0", "--account", "<tenantId>:<userId>"]
    }
  }
}
```

| Tool | Purpose | Key arguments and defaults |
|------|---------|----------------------------|
| `overview` | Who messaged you: recent activity grouped by conversation. A good first call. | `days` (`7`), `limit` (`100`), `account` |
| `recent_messages` | Messages of the last N days, newest first. | `days` (`7`), `limit` (`200`), `account` |
| `mentions` | @-mentions of you, with content and read state. | `days` (`0`), `unread_only` (`false`), `limit` (`100`), `account` |
| `unread_messages` | Your unread @-mentions. | `days` (`30`), `limit` (`100`), `account` |
| `search_messages` | Text search in content and sender names. | `query` (required), `days` (`0`), `limit` (`50`), `account` |
| `list_conversations` | Chats and channels with their titles and IDs. | `limit` (`100`), `account` |
| `read_conversation` | Last messages of one conversation, oldest first. Pass `limit: 0` for all cached messages. | `conversation_id` (required), `limit` (`50`), `account` |
| `list_accounts` | Accounts in the cache; use `key` as the `account` filter. | none |
| `report_feedback` | The agent reports a problem with these tools. Appended to the feedback file, never sent anywhere. | `category` and `summary` (required), `details`, `related_tool`, `proposed_fix` |

The server parses the cache on the first tool call and keeps the result in memory. It parses again when the TTL ends or when Teams writes new data.

Every tool response keeps its rows in `result` and includes `skipped_records`, `damaged_sections`, and `partial`. When `partial` is true, some cache data could not be read, so an empty result does not confirm that no matching messages exist.

### Agent feedback

`report_feedback` lets the calling agent say what went wrong: a tool error that looks wrong, an output that contradicts what you see in Teams, a search that missed a message the agent found another way, a misleading tool description, or a capability that does not exist. `category` is one of `bug`, `unexpected_output`, `missing_capability`, `tool_description`, `search_miss` and `other`. It returns `{"request_id": "...", "report": {"category", "received", "deduplicated"}}`.

Each report is one line in `~/.teamscache/feedback.jsonl` (change it with `--feedback-file`). The file is only ever appended to, and it is created with owner-only permissions. Nothing is sent over the network. Review the reports with `teamscache feedback`, then decide yourself whether to open a GitHub issue.

- **Rate limit:** each MCP client (the `clientInfo.name` from the handshake, such as `claude-code`) can send 10 reports per 10 minutes. Change this with `--feedback-rate-limit` and `--feedback-rate-window`. The count is read from the file, so it survives a restart and is shared by every server process that uses the file. A refused call is a tool error with `retry_after_seconds`.
- **Duplicates:** an identical report from the same client inside the window is not stored again, answers `"deduplicated": true` and does not use up the limit.
- **Limits on text:** the summary is at most 200 characters, `details` and `proposed_fix` at most 2000.
- **No redaction:** the tool description tells the agent not to include message text, names, email addresses or tenant, user and conversation ids, but the server stores what the agent sends. Check the file before you share it.

## How it works

The new Teams client (`com.microsoft.teams2` on macOS, the `MSTeams` package on Windows) is an Edge WebView2 app. It stores recent conversations in an IndexedDB database, which Chromium keeps in LevelDB with values in the V8 serialization format. `teamscache`:

1. finds the LevelDB folder, or uses `--leveldb`,
2. copies its data files and referenced blob files to a temporary folder, because the running client locks the files, and removes the copy when it is done,
3. reads the LevelDB files directly (tables and logs, including Snappy-compressed blocks), without a LevelDB engine,
4. decodes the IndexedDB keys and the Blink and V8 values,
5. maps the Teams schema: messages in `replychains` and its numbered copies such as `replychains-2`, chats in `conversations`, mentions in `mentions-metadata-items`, one database set per tenant and user.

LevelDB can keep older versions of a record until it compacts its files. `teamscache` uses only the newest version of each record, so an edited message shows its current text and the read state of a mention is the current one.

The LevelDB, IndexedDB and V8 decoding is a Go port of parts of [ccl_chromium_reader](https://github.com/cclgroupltd/ccl_chromium_reader). See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Privacy

The tool never changes the Teams cache and makes no network calls. The one thing it writes is the feedback file that `report_feedback` appends to, described above. The temporary copy of the cache is created with owner-only permissions and removed after each run. Output can contain message content from all tenants in the cache, so treat it as confidential.

## Development

```bash
make setup    # download dependencies
make test     # unit tests with the race detector
make lint     # golangci-lint and gofumpt
make format   # format the code
make build    # build bin/teamscache
make upgrade-deps # update dependencies and pinned Go tools
```

Tests use synthetic data only. The test fixtures and their generators are in `testdata` folders:

- `internal/v8/testdata`: values written by Node.js `v8.serialize` (`gen.mjs`) and the result of `ccl_chromium_reader` for the same bytes (`golden.py`).
- `internal/indexeddb/testdata`: a synthetic IndexedDB written by `gen/main.go`, and the records `ccl_chromium_reader` reads from it (`gen/golden.py`).
- `internal/teams/testdata/gen`: a synthetic Teams cache with edge cases, for manual end-to-end checks.

## License

MIT. See [LICENSE](LICENSE) and [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
