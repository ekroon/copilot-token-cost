# AGENTS.md

## Scope
This file covers `go/internal/`.

## Package map
- `domain`: shared record/stat types.
- `parsing`: Copilot log parsing.
- `storage`: SQLite schema, migrations, and persistence.
- `sync`: local/codespaces sync orchestration.
- `costing`: pricing normalization and cost math.
- `web`: dashboard/runtime-facing orchestration.
- `http`: HTTP transport and route wiring.

## Dependency guidance
- Keep `domain` dependency-light and reusable.
- Keep adapters (`http`, CLI commands) depending on services, not the reverse.
- Prefer explicit service wiring over hidden globals.
