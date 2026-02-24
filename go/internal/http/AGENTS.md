# AGENTS.md

## Scope
This file covers `go/internal/http/`.

## Intent
- Expose HTTP routes and transport behavior for health, snapshot, and dashboard actions.
- Delegate business logic to `internal/web` runtime/service objects.

## Guidance for changes
- Keep handlers transport-focused (validation, status codes, encoding).
- Do not move parsing/costing/storage logic into this package.
