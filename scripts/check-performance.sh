#!/usr/bin/env bash
set -euo pipefail

out="$(cd go && go test -run '^$' -bench '^BenchmarkParseLogFileSynthetic$' -benchtime=30x -count=1)"
echo "$out"

optimized="$(printf '%s\n' "$out" | awk '/BenchmarkParseLogFileSynthetic-/{print $(NF-1)}')"
baseline="$(awk -F': ' '/"baseline_parser_ns_per_op"/ {gsub(/,/, "", $2); print $2}' go/performance_baseline.json)"
target_ratio="$(awk -F': ' '/"target_ratio_max"/ {gsub(/,/, "", $2); print $2}' go/performance_baseline.json)"

if [[ -z "$baseline" || -z "$optimized" || -z "$target_ratio" ]]; then
  echo "Failed to parse benchmark output" >&2
  exit 1
fi

python3 - "$baseline" "$optimized" "$target_ratio" <<'PY'
import sys

baseline = float(sys.argv[1])
optimized = float(sys.argv[2])
target = float(sys.argv[3])
ratio = optimized / baseline
improvement = (1 - ratio) * 100
print(f"optimized/baseline ratio: {ratio:.4f} (improvement: {improvement:.2f}%)")
if ratio > target:
    raise SystemExit(f"Performance target failed: ratio {ratio:.4f} > {target:.1f}")
PY
