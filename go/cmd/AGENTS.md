# AGENTS.md

## Scope
This file covers `go/cmd/`.

## Subtree map
- `copilot-token-cost/`: scaffold command that wires internal services.
- `codespace-tail-proto/`: prototype utility for remote log tailing and checkpoint persistence.

## Guidance for changes
- Keep command directories focused on argument handling and orchestration.
- Push reusable logic into `internal/*` when it is not command-specific.
- Preserve command output/contracts unless explicitly migrating behavior.
