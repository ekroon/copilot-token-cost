# AGENTS.md

## Scope
This file covers `go/`.

## Progressive-disclosure map
- `main.go`: legacy CLI/web entrypoint and orchestration.
- `stats_service.go`, `web_mode.go`, `pricing_data.go`: top-level wiring and shared runtime helpers.
- `cmd/`: command-focused entrypoints.
- `internal/`: domain services and adapters used by command/web orchestration.

## Guidance for changes
- Keep top-level behavior and CLI flags backward compatible.
- Prefer adding detailed implementation in `internal/*` and keep top-level files focused on wiring.
- When introducing new subtree APIs, add/extend `AGENTS.md` in that subtree.

## Validation
- `cd go && gofmt -w *.go ./cmd/codespace-tail-proto/*.go && go build -o copilot-token-cost . && go vet ./... && go test ./...`
