#!/usr/bin/env bash
set -euo pipefail

target_ms="${1:-250}"
runs="${2:-20}"
fixture_dir="${3:-${TMPDIR:-/tmp}/copilot-token-cost-e2e-fixture}"

./scripts/generate-e2e-fixture.sh "$fixture_dir"
./scripts/measure-go-run-cold.sh "$fixture_dir" "$runs" "$target_ms"
