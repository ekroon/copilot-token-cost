# Copilot CLI Token Usage & Cost Calculator

Parses Copilot CLI process logs to extract per-model token usage and calculates estimated API-equivalent costs.

Go-based CLI for parsing Copilot token usage and estimating API-equivalent costs.

## Gist

<https://gist.github.com/ekroon/424b81ebca907b5e5de3ce07a649da5e>

## Usage

### Install with mise

```bash
mise use -g github:ekroon/copilot-token-cost
copilot-token-cost                         # default: last 7 days
copilot-token-cost 30                      # last 30 days
copilot-token-cost --json                  # machine-readable output
```

### Go

```bash
cd go && go build -o copilot-token-cost .  # build once
./go/copilot-token-cost                    # default: last 7 days
./go/copilot-token-cost 30                 # last 30 days
./go/copilot-token-cost --all              # all available logs
./go/copilot-token-cost --json             # machine-readable output
```

### Codespaces tail prototype (SSH/SFTP spike)

```bash
cd go
go run ./cmd/codespace-tail-proto --codespace <codespace-name> --run-for 60s --poll-interval 2s
```

By default, the prototype discovers and tails all `process*.log` files, picks up new files that appear during runtime, and stops syncing files that disappear. It does a full copy first, then tails appended bytes; with `--reconnect-once` (default), it reconnects mid-run and performs a full recopy before resuming tail reads.

It persists per-file tail checkpoints to SQLite (`codespace_tail_offsets`) and runs digest checks; if local/remote digests diverge, it performs a defensive full recopy before continuing. Use `--state-db <path>` to override the checkpoint DB path.

## Common Flags

| Flag | Description |
|------|-------------|
| `<days>` | Number of days to look back (default: 7) |
| `--all` | Process all available logs |
| `--today` | Today only |
| `--yesterday` | Yesterday only |
| `--from N` | Start from N days ago (0=today) |
| `--to N` | End at N days ago (0=today) |
| `--logs-dir PATH` | Override logs directory |
| `--project TEXT` | Filter stats/output to projects whose workspace path matches this text (case-insensitive) |
| `--json` | Machine-readable JSON output |
| `--sync` | Force full re-sync of all log files into the database (including codespaces when `--codespaces-sync` is used) |
| `--import-file FILE` | Import data from JSONL or SQLite file |
| `--export-file FILE` | Export data as JSONL |
| `--codespaces-sync` | Sync Copilot data from running Codespaces via `gh cs cp` |
| `--codespaces-include-stopped` | Include stopped Codespaces (requires `--codespaces-sync` or `--web`) |
| `--web` | Run local web dashboard mode (respects date-window flags like `--today`, `--yesterday`, `--from/--to`; cannot be combined with `--json` or `--export-file`) |
| `--web-listen ADDR` | Web mode listen address (default `127.0.0.1:7331`) |
| `--web-refresh-interval DURATION` | Local refresh interval in web mode (default `30s`) |
| `--web-local-streaming` | Experimental: realtime local log streaming updates in web mode (replaces periodic local refresh loop) |
| `--web-codespaces-mode MODE` | Web Codespaces sync mode: `manual` or `auto` (default `auto`: background startup sync + periodic sync) |
| `--web-codespaces-streaming` | Experimental: read live Codespaces streaming checkpoint status (`codespace_tail_offsets`) into dashboard sync status |
| `--web-codespaces-interval DURATION` | Web Codespaces sync interval when mode is `auto` (default `5m`) |
| `--web-log-mode MODE` | Web stderr logging mode: `compact` (default, single-line live status), `verbose` (line-by-line logs), or `errors` (warnings/errors + startup lines) |

## Web mode

```bash
./go/copilot-token-cost --web
./go/copilot-token-cost --web --today
./go/copilot-token-cost --web --web-local-streaming
./go/copilot-token-cost --web --web-codespaces-mode manual
./go/copilot-token-cost --web --web-codespaces-streaming
./go/copilot-token-cost --web --web-log-mode verbose
```

Web mode is Data-star-first: the dashboard shell is server-rendered with Datastar-driven updates (no custom fetch loop).
The overview renders as tables/sections (`Sync status`, `Per-model summary`, `Per-project summary`, `Daily totals`), and live updates emit `datastar-patch-elements` SSE fragments that patch `#overview-summary`, `#sync-status-region`, `#model-summary-region`, `#project-summary-region`, `#daily-totals-region`, and `#stats-json`.
The `/` page is now the Copilot Stats view, with sections for `Today summary` + weekly average, token/money trends, and top projects/models for today and this week; money sections report premium spend and cache-aware API-equivalent spend (`API spend` in the UI).
The dashboard keeps a persistent `GET /events` SSE stream open; reconnecting starts a fresh stream for subsequent broadcasts, and keep-alive heartbeat events are sent while idle.
Web mode respects the same date-window flags as CLI output (`<days>`, `--today`, `--yesterday`, `--from/--to`, `--all`).
The server starts immediately and serves the latest DB snapshot; startup then runs local refresh + Codespaces sync in the background (Codespaces only when mode is `auto`).
Startup and sync progress are logged to stderr. By default (`--web-log-mode compact`), progress is shown as a continuously updated single status line (with heartbeat) so you can see activity without repeated multi-line spam; transient success chatter is suppressed, `next=` countdowns are hidden during streaming states, and warnings/errors still print as regular lines. Use `--web-log-mode verbose` for line-by-line output, or `--web-log-mode errors` for warnings/errors and essential startup lines only.
Periodic behavior remains configurable: local refresh uses `--web-refresh-interval` (default `30s`) and auto Codespaces sync uses `--web-codespaces-interval` (default `5m`).
Codespaces auto-sync runs on each interval and attempts SSH connection reuse (`ControlMaster=auto`, `ControlPersist=15m`) for the `ssh+tar` copy path to reduce repeated auth prompts; if prompts still recur, use `--web-codespaces-mode manual` or a longer interval.
With `--web-local-streaming`, local updates are pushed from realtime log-file change detection instead of periodic local polling, and local checkpoints are persisted in `codespace_tail_offsets` (`source='local'`) including hash metadata and defensive-recopy timestamps. With `--web-codespaces-streaming`, dashboard sync status also reflects live streaming checkpoint state from `codespace_tail_offsets` (active streams, last chunk timestamp, defensive recopy timestamp, and last error).
Use `--web-codespaces-mode manual` to disable startup/periodic Codespaces sync and trigger it on-demand via `POST /actions/sync-codespaces`.

Web endpoints:
- `GET /` — Copilot Stats view (today + weekly average, token/money trends, top projects/models for today/week)
- `GET /details` — dashboard shell with summary tables and JSON snapshot
- `GET /events` — persistent Datastar SSE stream for live patch broadcasts + heartbeat keep-alives
- `GET /api/stats` — current JSON stats snapshot
- `GET /healthz` — liveness check (`ok`)
- `POST /actions/sync-codespaces` — trigger Codespaces sync and return SSE patch

## Semver Tagging Helper

Use the helper script to create and push the next release tag based on existing `vX.Y.Z` tags:

```bash
./scripts/semver-tag.sh patch   # v1.2.3 -> v1.2.4
./scripts/semver-tag.sh minor   # v1.2.3 -> v1.3.0
./scripts/semver-tag.sh major   # v1.2.3 -> v2.0.0
```

If no semver tags exist yet, it starts from `v0.0.0`.

## SQLite Database

The Go implementation uses a single SQLite database (`copilot-tokens.db`) in:
- `$XDG_STATE_HOME/copilot-token-cost/` when `XDG_STATE_HOME` is set
- `~/.local/state/copilot-token-cost/` as fallback

Legacy project/binary locations are not used automatically; migration is manual. The database:

- **Auto-syncs** on every run — only new/modified log files are re-parsed
- **Fast first run** — non-`--all` runs only sync logs in the requested date window
- **Improves performance** — subsequent runs skip already-parsed logs
- **Enables data portability** — export from a codespace, import locally
- **Prompt text persistence semantics** — when prompt text is available to ingestion, it is stored by default (always-on, no opt-in flag); unavailable prompt text remains `NULL`

### Import / Export

Export data from one machine:
```bash
./go/copilot-token-cost --export-file tokens.jsonl
```

Import on another:
```bash
./go/copilot-token-cost --import-file tokens.jsonl
```

You can also copy `copilot-tokens.db` directly, or import from another SQLite DB:
```bash
./go/copilot-token-cost --import-file /path/to/other/copilot-tokens.db
```

Use `--sync` to force a full re-parse of all log files (useful after updates):
```bash
./go/copilot-token-cost --sync
```

### Direct SQL Queries

Run read-only SQL queries directly against the database:
```bash
copilot-token-cost sql "SELECT COUNT(*) FROM api_calls"
copilot-token-cost sql "SELECT DISTINCT cwd FROM session_workspaces"
copilot-token-cost sql --json "SELECT model_normalized, COUNT(*) as n FROM api_calls GROUP BY 1 ORDER BY n DESC"
```

Supports piping:
```bash
echo "SELECT MAX(timestamp) FROM api_calls" | copilot-token-cost sql
```

## Performance Checks

CodSpeed CI benchmark (authoritative performance gate):
```bash
# Workflow: .github/workflows/codspeed.yml
cd go && go test -bench '^BenchmarkParseLogFileSynthetic$' -benchtime=30x -count=1
```
The CodSpeed action is configured with `mode: walltime` for Go benchmarks.
To upload results to CodSpeed from GitHub Actions, set repository secret `CODSPEED_TOKEN` (the workflow falls back to a plain benchmark run if CodSpeed upload is unavailable).

Optional local parser regression ratio check:
```bash
./scripts/check-performance.sh
```

End-to-end cold `go run . --today` check on deterministic 3x synthetic data (isolated `XDG_STATE_HOME`):
```bash
./scripts/check-e2e-go-run-target.sh 250 20
```

## Output

- **Per-model summary** — calls, tokens, cache hit%, cost, and hypothetical no-cache cost
- **Cost per premium request** — API cost divided by premium request count per model
- **Daily breakdown** — usage and cost by day
- **Per-project breakdown** — usage and cost by workspace/project
- **Pricing reference** — rates used for calculations

## Historical Pricing

The `pricing.json` file contains time-ranged pricing periods. Each API call's cost is calculated using the pricing that was in effect at its timestamp. This means pricing changes (e.g., premium request multiplier changes) are correctly applied retroactively when recalculating costs.
