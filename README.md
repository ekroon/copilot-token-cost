# Copilot CLI Token Usage & Cost Calculator

Parses Copilot CLI process logs to extract per-model token usage and calculates estimated API-equivalent costs.

Available in **Python**, **Go**, and **Node.js** — all three produce identical output.

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

### Python

```bash
python3 copilot-token-cost.py              # default: last 7 days
python3 copilot-token-cost.py 30           # last 30 days
python3 copilot-token-cost.py 1            # today only
python3 copilot-token-cost.py --all        # all available logs
python3 copilot-token-cost.py --json       # machine-readable output
```

### Go

```bash
cd go && go build -o copilot-token-cost .  # build once
./go/copilot-token-cost                    # default: last 7 days
./go/copilot-token-cost 30                 # last 30 days
./go/copilot-token-cost --all              # all available logs
./go/copilot-token-cost --json             # machine-readable output
```

### Node.js

```bash
node node/copilot-token-cost.js            # default: last 7 days
node node/copilot-token-cost.js 30         # last 30 days
node node/copilot-token-cost.js --all      # all available logs
node node/copilot-token-cost.js --json     # machine-readable output
```

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
| `--json` | Machine-readable JSON output |
| `--sync` | Force full re-sync of all log files into the database (including codespaces when `--codespaces-sync` is used) |
| `--import-file FILE` | Import data from JSONL or SQLite file |
| `--export-file FILE` | Export data as JSONL |
| `--codespaces-sync` | Sync Copilot data from running Codespaces via `gh cs cp` |
| `--codespaces-include-stopped` | Include stopped Codespaces (requires `--codespaces-sync`) |

## SQLite Database

All three implementations share a single SQLite database (`copilot-tokens.db`) in the project directory. The database:

- **Auto-syncs** on every run — only new/modified log files are re-parsed
- **Improves performance** — subsequent runs skip already-parsed logs
- **Enables data portability** — export from a codespace, import locally

### Import / Export

Export data from one machine:
```bash
python3 copilot-token-cost.py --export-file tokens.jsonl
```

Import on another:
```bash
python3 copilot-token-cost.py --import-file tokens.jsonl
```

You can also copy `copilot-tokens.db` directly, or import from another SQLite DB:
```bash
python3 copilot-token-cost.py --import-file /path/to/other/copilot-tokens.db
```

Use `--sync` to force a full re-parse of all log files (useful after updates):
```bash
python3 copilot-token-cost.py --sync
```

## Output

- **Per-model summary** — calls, tokens, cache hit%, cost, and hypothetical no-cache cost
- **Cost per premium request** — API cost divided by premium request count per model
- **Daily breakdown** — usage and cost by day
- **Per-project breakdown** — usage and cost by workspace/project
- **Pricing reference** — rates used for calculations

## Historical Pricing

The `pricing.json` file contains time-ranged pricing periods. Each API call's cost is calculated using the pricing that was in effect at its timestamp. This means pricing changes (e.g., premium request multiplier changes) are correctly applied retroactively when recalculating costs.
