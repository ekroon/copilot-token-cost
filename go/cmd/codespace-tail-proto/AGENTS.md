# AGENTS.md

## Scope
This file covers `go/cmd/codespace-tail-proto/`.

## Progressive-disclosure map
- `main.go`: SSH/SFTP connection flow and tail orchestration loop.
- `state.go`: SQLite-backed checkpoint/state tracking for tailed files.
- `*_test.go`: behavior checks for state and orchestration helpers.

## Guidance for changes
- Keep reconnect/tailing behavior deterministic and checkpoint-safe.
- Preserve compatibility of state schema fields used by existing local DB files.
