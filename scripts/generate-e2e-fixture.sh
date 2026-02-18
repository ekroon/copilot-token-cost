#!/usr/bin/env bash
set -euo pipefail

out_dir="${1:-${TMPDIR:-/tmp}/copilot-token-cost-e2e-fixture}"
source_db="${2:-${XDG_STATE_HOME:-$HOME/.local/state}/copilot-token-cost/copilot-tokens.db}"
records_per_file=250
min_target_calls=30000
today_calls_target=250

current_calls=0
if [[ -f "$source_db" ]]; then
  current_calls="$(python3 - "$source_db" <<'PY'
import sqlite3
import sys

db = sqlite3.connect(sys.argv[1])
try:
    row = db.execute("SELECT COUNT(*) FROM api_calls").fetchone()
    print(int(row[0] if row else 0))
finally:
    db.close()
PY
)"
fi

target_calls=$((current_calls * 3))
if (( target_calls < min_target_calls )); then
  target_calls="$min_target_calls"
fi
file_count=$(( (target_calls + records_per_file - 1) / records_per_file ))

rm -rf "$out_dir"
mkdir -p "$out_dir/home/.copilot/logs" "$out_dir/home/.copilot/session-state"
session_id="123e4567-e89b-12d3-a456-426614174888"
mkdir -p "$out_dir/home/.copilot/session-state/$session_id"
cat > "$out_dir/home/.copilot/session-state/$session_id/workspace.yaml" <<'EOF'
cwd: /tmp/e2e-benchmark-project
EOF

python3 - "$out_dir/home/.copilot/logs" "$session_id" "$file_count" "$records_per_file" "$target_calls" "$today_calls_target" <<'PY'
from datetime import datetime, timedelta
from pathlib import Path
import os
import sys

logs_dir = Path(sys.argv[1])
session_id = sys.argv[2]
file_count = int(sys.argv[3])
records_per_file = int(sys.argv[4])
target_calls = int(sys.argv[5])
today_calls_target = int(sys.argv[6])
models = ("gpt-5-mini", "claude-sonnet-4.6", "gemini-2.5-pro")

remaining = target_calls
base = datetime.now().replace(hour=12, minute=0, second=0, microsecond=0)
remaining_today = min(today_calls_target, target_calls)

for file_idx in range(file_count):
    records_this_file = min(records_per_file, remaining)
    if records_this_file <= 0:
        break
    remaining -= records_this_file
    if remaining_today > 0:
        ts = base + timedelta(minutes=file_idx)
        if records_this_file <= remaining_today:
            remaining_today -= records_this_file
        else:
            remaining_today = 0
    else:
        ts = base - timedelta(days=(file_idx - (today_calls_target // records_per_file) + 1))
    lines = []
    for rec in range(records_this_file):
        line_ts = (ts + timedelta(seconds=rec)).strftime("%Y-%m-%dT%H:%M:%S")
        model = models[(file_idx + rec) % len(models)]
        initiator = "user" if (file_idx + rec) % 3 == 0 else "agent"
        prompt = 100 + ((file_idx * 13 + rec) % 200)
        completion = 20 + ((file_idx * 7 + rec) % 80)
        cache_create = (file_idx + rec) % 16
        cache_read = (file_idx * 3 + rec) % 24
        lines.append(f"{line_ts} Created ACP session: {session_id}")
        lines.append(f"{line_ts} PremiumRequestProcessor: Setting X-Initiator to '{initiator}'")
        lines.append(f'{line_ts} {{"model":"{model}"}}')
        lines.append(
            f'{line_ts} {{"prompt_tokens":{prompt},"completion_tokens":{completion},'
            f'"cache_creation_input_tokens":{cache_create},"cache_read_input_tokens":{cache_read}}}'
        )
    p = logs_dir / f"process-e2e-{file_idx:04d}.log"
    p.write_text("\n".join(lines) + "\n", encoding="utf-8")
    ts_epoch = ts.timestamp()
    os.utime(p, (ts_epoch, ts_epoch))
PY

echo "fixture_dir=$out_dir"
echo "source_db=$source_db"
echo "current_calls=$current_calls"
echo "target_calls=$target_calls"
echo "records_per_file=$records_per_file"
echo "file_count=$file_count"
echo "today_calls_target=$today_calls_target"
