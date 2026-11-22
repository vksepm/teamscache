# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`teamscache` is a Go CLI and read-only MCP server that reads the new Microsoft Teams (v2) desktop client's on-disk message cache. It uses no network, Graph API or OAuth. The cache is Chromium IndexedDB, stored in LevelDB with V8-serialized values. All decoding is pure Go, with no LevelDB engine, so builds use `CGO_ENABLED=0`. The decoders are a port of parts of `ccl_chromium_reader`. User-facing usage, flags and MCP tools are documented in `README.md`.

## Commands

```bash
make setup      # go mod download
make test       # go test -race ./...
make lint       # golangci-lint + gofumpt -extra check (both pinned as `go tool` deps in go.mod)
make format     # gofumpt -extra -w .
make build      # CGO_ENABLED=0 build -> bin/teamscache
make run ARGS="overview --days 7"
make vulncheck  # govulncheck

go test -race ./internal/teams -run TestOpenReadsASyntheticCache   # single test
```

CI details to keep in mind:
- CI also runs `go vet` and golangci-lint with `GOOS=windows` and `GOOS=darwin`. Cache discovery branches on `runtime.GOOS`, so check cross-platform changes with `GOOS=windows go vet ./...`.
- Total coverage must stay at or above 80%, excluding `internal/idbtest`.
- Tests also run on Windows without `-race`. Keep tests portable, for example avoid relying on Unix file-permission behavior.
- Go version is pinned in `go.mod` (`go 1.27.x`).

## Architecture

The layers are strictly stacked, and each depends only on those below it:

```
cmd/teamscache/main.go → internal/cli (cobra) ┐
                                              ├→ internal/service → internal/teams → internal/indexeddb → internal/leveldb
                         internal/mcpserver   ┘                                    └→ internal/v8 (Blink + V8 value decoding)
```

- **`internal/leveldb`**: Reads `.ldb` tables and `.log` files directly, including Snappy blocks. It tolerates corruption by recording `Damage` sections (file, offset, reason) and continuing, instead of failing. `ContextReader` makes reads cancelable.
- **`internal/indexeddb`**: Decodes Chromium's IndexedDB key and metadata encoding on top of LevelDB, and loads external blob files for large values. It yields every record version, with its sequence number and live/deleted state.
- **`internal/v8`**: Blink envelope plus V8 `ValueDeserializer`, producing a generic value tree.
- **`internal/teams`**: Maps the Teams schema onto those records.
  - `discover.go` finds caches per OS: WebView2 profiles under `EBWebView/*/IndexedDB/https_teams.microsoft.com_0.indexeddb.leveldb`.
  - `reader.go` `Open`/`OpenFor` first **copies the LevelDB data files and referenced blobs into a temp dir**, because the running client locks them. It retries the copy up to 3 times if the file set changes mid-copy. `Close()` deletes the copy.
  - A `Scope` bitmask limits which stores' blobs are copied, so CLI commands only pay for what they need. The messages, conversations and mentions stores are `replychains` (plus numbered copies like `replychains-2`), `conversations` and `mentions-metadata-items`. There is one database set per tenant and user.
  - **Newest-version-wins**: LevelDB keeps stale versions until compaction. The reader picks the highest-sequence version of each record, and a message found in several stores is returned once. An undecodable newest version deliberately hides older versions of that chain.
- **`internal/service`**: `Snapshot` is the parsed cache (messages, conversations, mentions, accounts, skipped count, damage). It holds the query logic shared by the CLI and MCP: `Overview`, `ListConversations`, `ReadConversation`, search, mentions, and account labelling. `LoadFor` builds a snapshot for a scope.
- **`internal/cli`**: Cobra commands. A generic `run[T]` helper resolves the cache path, loads a snapshot with the command's scope, runs a query, writes JSON to stdout and a `# N result(s), N record(s) skipped, N damaged section(s)` summary to stderr. `--profile` and `--leveldb` are mutually exclusive, and each has a `TEAMSCACHE_*` env default.
- **`internal/feedback`**: Append-only JSONL store behind the MCP `report_feedback` tool (default `~/.teamscache/feedback.jsonl`, files `0600`). It validates reports, drops same-client duplicates inside the window, and rate limits per MCP client name (default 10 per 10 minutes). The count is read from the file, not kept in memory, so it survives restarts and is shared by server processes. It has no MCP or CLI imports and no redaction. The `teamscache feedback` command reads the same file.
- **`internal/mcpserver`**: Stdio MCP server exposing the same queries as tools. It is read-only for Teams data. `report_feedback` is registered only when a `*feedback.Store` is passed to `New`, never loads the Teams cache, and is the only tool without `ReadOnlyHint`. The Teams tools share a parsed snapshot cached in memory (TTL `--cache-ttl`, default 180s) and re-parse on expiry or when Teams writes new data. Every response includes `skipped_records`, `damaged_sections` and `partial`.

Records that fail to decode are counted as "skipped", and unreadable file sections or missing blobs are counted as "damaged". Both are reported, never fatal. Preserve that partial-result behavior when changing readers.

## Testing

- **`internal/idbtest`** (`idbtest.go`, `teams.go`) builds synthetic LevelDB/IndexedDB/Teams caches in code. The `teams`, `service`, `cli` and `mcpserver` tests use it, so they need no real Teams data. Tests use synthetic data only, and real message content must never be committed.
- **Golden and corpus fixtures** under `internal/*/testdata/` (`internal/v8/testdata/corpus/*.bin|.json`, `internal/indexeddb/testdata/golden.json`) are expected outputs from `ccl_chromium_reader` for the same bytes. The generators are `internal/v8/testdata/gen.mjs` (Node `v8.serialize`), `internal/indexeddb/testdata/gen/{main.go,golden.py}` and `internal/teams/testdata/gen/main.go` (an edge-case cache for manual end-to-end checks). Regenerate only when the decoding behavior intentionally changes.

## Lint conventions

`.golangci.yml` enables `gosec` (G115 and G304 excluded because the binary parsers bounds-check before converting and read user-given paths), `revive` (exported symbols need doc comments, plus a package comment), `errorlint`, `exhaustive`, `forcetypeassert`, `goconst` and `testifylint` (all rules). Code must also be `gofumpt -extra` clean.
