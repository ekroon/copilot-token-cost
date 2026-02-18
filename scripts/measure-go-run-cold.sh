#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "Usage: $0 <fixture_dir> [runs] [assert_ms] [cold_compile]" >&2
  exit 1
fi

fixture_dir="$1"
runs="${2:-3}"
assert_ms="${3:-}"
cold_compile="${4:-0}"

if [[ ! -d "$fixture_dir/home/.copilot/logs" || ! -d "$fixture_dir/home/.copilot/session-state" ]]; then
  echo "Invalid fixture dir: expected home/.copilot/logs and home/.copilot/session-state" >&2
  exit 1
fi

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
tmp_root="${TMPDIR:-/tmp}/copilot-token-cost-e2e-runs"
mkdir -p "$tmp_root"
shared_gopath="$(cd "$repo_root/go" && go env GOPATH)"
shared_gomodcache="$(cd "$repo_root/go" && go env GOMODCACHE)"
shared_gocache="$(cd "$repo_root/go" && go env GOCACHE)"

sum_ms=0
max_ms=0
min_ms=999999999

for i in $(seq 1 "$runs"); do
  run_root="$(mktemp -d "$tmp_root/run.XXXXXX")"
  state_home="$run_root/state"
  home_dir="$run_root/home"
  mkdir -p "$state_home" "$home_dir/.copilot"
  cp -R "$fixture_dir/home/.copilot/logs" "$home_dir/.copilot/"
  cp -R "$fixture_dir/home/.copilot/session-state" "$home_dir/.copilot/"

  time_file="$run_root/time.txt"
  (
    cd "$repo_root/go"
    if [[ "$cold_compile" == "1" ]]; then
      local_gocache="$run_root/gocache"
      mkdir -p "$local_gocache"
      XDG_STATE_HOME="$state_home" HOME="$home_dir" GOPATH="$shared_gopath" GOMODCACHE="$shared_gomodcache" GOCACHE="$local_gocache" /usr/bin/time -p \
        go run . --today --logs-dir "$home_dir/.copilot/logs" >"$run_root/stdout.txt" 2>"$time_file"
    else
      XDG_STATE_HOME="$state_home" HOME="$home_dir" GOPATH="$shared_gopath" GOMODCACHE="$shared_gomodcache" GOCACHE="$shared_gocache" /usr/bin/time -p \
        go run . --today --logs-dir "$home_dir/.copilot/logs" >"$run_root/stdout.txt" 2>"$time_file"
    fi
  )

  real_sec="$(awk '/^real / {print $2}' "$time_file" | tail -n 1)"
  if [[ -z "$real_sec" ]]; then
    echo "Failed to parse timing for run $i" >&2
    cat "$time_file" >&2
    exit 1
  fi
  real_ms="$(python3 - "$real_sec" <<'PY'
import sys
print(int(round(float(sys.argv[1]) * 1000)))
PY
)"
  echo "run=$i cold_go_run_today_ms=$real_ms"

  sum_ms=$((sum_ms + real_ms))
  if (( real_ms > max_ms )); then max_ms=$real_ms; fi
  if (( real_ms < min_ms )); then min_ms=$real_ms; fi
done

avg_ms=$((sum_ms / runs))
echo "summary runs=$runs avg_ms=$avg_ms min_ms=$min_ms max_ms=$max_ms"

if [[ -n "$assert_ms" ]]; then
  if (( max_ms > assert_ms )); then
    echo "FAIL: max_ms=$max_ms exceeds assert_ms=$assert_ms" >&2
    exit 1
  fi
  echo "PASS: max_ms=$max_ms <= assert_ms=$assert_ms"
fi
