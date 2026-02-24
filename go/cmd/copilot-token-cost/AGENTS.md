# AGENTS.md

## Scope
This file covers `go/cmd/copilot-token-cost/`.

## Intent
- This command is a composition scaffold for the migrated internal services.
- It should stay thin: service construction + bootstrap behavior only.

## Guidance for changes
- Avoid duplicating logic from legacy `go/main.go`; wire through `internal/*` services.
- Keep startup side effects explicit and minimal.
