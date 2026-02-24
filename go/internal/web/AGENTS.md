# AGENTS.md

## Scope
This file covers `go/internal/web/`.

## Intent
- Bridge sync/costing services into snapshot and dashboard runtime behavior.
- Provide runtime hooks consumed by `internal/http` dashboard transport.

## Guidance for changes
- Keep this layer focused on orchestration and presentation-ready runtime contracts.
- Preserve action error semantics expected by HTTP handlers.
