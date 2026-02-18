#!/usr/bin/env python3
"""
Copilot CLI Token Cost Calculator

Parses Copilot CLI process logs to extract per-model token usage
and calculates estimated API cost based on current pricing.

Usage:
    python3 copilot-token-cost.py              # default: last 7 days
    python3 copilot-token-cost.py 30           # last 30 days
    python3 copilot-token-cost.py 1            # today only
    python3 copilot-token-cost.py --all        # all available logs
    python3 copilot-token-cost.py --json       # machine-readable output
"""

import argparse
import db
import json
import re
import subprocess
import sys
import tempfile
import unicodedata
from collections import defaultdict
from datetime import datetime, timedelta
from pathlib import Path

# ─── Pricing data (loaded from pricing.json) ────────────────────────────────


def load_pricing(script_dir: Path) -> dict:
    """Load pricing data from pricing.json."""
    for candidate in [script_dir / 'pricing.json', Path.cwd() / 'pricing.json']:
        if candidate.exists():
            with open(candidate) as f:
                return json.load(f)
    print(f"Error: pricing.json not found (looked in {script_dir} and {Path.cwd()})", file=sys.stderr)
    sys.exit(1)


_pricing_data = load_pricing(Path(__file__).resolve().parent)
PRICING_PERIODS = _pricing_data['pricing_periods']


def _get_period(timestamp: str = None) -> dict:
    """Find the pricing period for the given timestamp (or date string). Falls back to latest/oldest."""
    if timestamp is None:
        return PRICING_PERIODS[0]
    date_str = timestamp[:10]
    for period in PRICING_PERIODS:
        if date_str >= period['effective_from']:
            return period
    return PRICING_PERIODS[-1]


def get_premium_request_cost(timestamp: str = None) -> float:
    return _get_period(timestamp)['premium_request_cost']


def get_premium_multiplier(model_name: str, timestamp: str = None) -> float:
    normalized = normalize_model(model_name)
    mult = _get_period(timestamp)['premium_multiplier']
    if normalized in mult:
        return mult[normalized]
    for key in mult:
        if normalized.startswith(key) or key.startswith(normalized):
            return mult[key]
    return 1


def normalize_model(model_name: str) -> str:
    """Strip vendor prefixes, reasoning effort suffixes, and date stamps."""
    for prefix in ["sweagent-capi:", "capi:"]:
        if model_name.startswith(prefix):
            model_name = model_name[len(prefix):]
    # Strip CAPI infra routing prefixes: capi-{region}-ptuc-{gpu}[-ib]-
    model_name = re.sub(r'^capi-[a-z]+-ptuc-[a-z0-9]+(?:-ib)?-', '', model_name)
    model_name = re.sub(r':defaultReasoningEffort=\w+', '', model_name)
    model_name = re.sub(r'-\d{4}-\d{2}-\d{2}$', '', model_name)
    return model_name


def get_pricing(model_name: str, timestamp: str = None) -> dict:
    normalized = normalize_model(model_name)
    mp = _get_period(timestamp)['model_pricing']
    if normalized in mp:
        return mp[normalized]
    for key in mp:
        if normalized.startswith(key) or key.startswith(normalized):
            return mp[key]
    return None


def parse_log_file(log_path: Path) -> list[dict]:
    """Parse a log file and return API call records with model, tokens, and timestamp."""
    content = log_path.read_text(errors='replace')
    records = []
    lines = content.split('\n')

    last_model = 'unknown'
    last_timestamp = None
    last_session = None
    last_initiator = 'agent'

    for i, line in enumerate(lines):
        ts_match = re.match(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})', line)
        if ts_match:
            last_timestamp = ts_match.group(1)

        # Track session boundaries (workspace init, ACP sessions, and flush events)
        session_match = re.search(r'(?:Workspace initialized|Created ACP session|Flushed \d+ events to session)[: ]+([0-9a-f-]{36})', line)
        if session_match:
            last_session = session_match.group(1)

        # Track premium request initiator (user = new premium request, agent = tool-loop continuation)
        initiator_match = re.search(r"PremiumRequestProcessor: Setting X-Initiator to '(\w+)'", line)
        if initiator_match:
            last_initiator = initiator_match.group(1)

        model_match = re.search(r'"model"\s*:\s*"([^"]+)"', line)
        if model_match:
            candidate = model_match.group(1)
            if not candidate.startswith('{') and ('claude' in candidate or 'gpt' in candidate or 'gemini' in candidate):
                last_model = candidate

        if '"completion_tokens"' in line:
            block_start = max(0, i - 10)
            block_end = min(len(lines), i + 15)
            block = '\n'.join(lines[block_start:block_end])

            prompt_match = re.search(r'"prompt_tokens"\s*:\s*(\d+)', block)
            completion_match = re.search(r'"completion_tokens"\s*:\s*(\d+)', block)
            cache_creation_match = re.search(r'"cache_creation_input_tokens"\s*:\s*(\d+)', block)
            cache_read_match = re.search(r'"cache_read_input_tokens"\s*:\s*(\d+)', block)
            cached_tokens_match = re.search(r'"cached_tokens"\s*:\s*(\d+)', block)

            block_model_match = re.search(r'"model"\s*:\s*"([^"]+)"', block)
            if block_model_match:
                candidate = block_model_match.group(1)
                if 'claude' in candidate or 'gpt' in candidate or 'gemini' in candidate:
                    last_model = candidate

            if prompt_match and completion_match:
                cache_read = int(cache_read_match.group(1)) if cache_read_match else 0
                if not cache_read and cached_tokens_match:
                    cache_read = int(cached_tokens_match.group(1))

                records.append({
                    'model': last_model,
                    'prompt_tokens': int(prompt_match.group(1)),
                    'completion_tokens': int(completion_match.group(1)),
                    'cache_creation_tokens': int(cache_creation_match.group(1)) if cache_creation_match else 0,
                    'cache_read_tokens': cache_read,
                    'is_user_turn': last_initiator == 'user',
                    'timestamp': last_timestamp,
                    'session_id': last_session,
                    'log_file': log_path.name,
                })
                # Reset initiator after consuming it (only the first API call in a turn is 'user')
                last_initiator = 'agent'

    return records


def load_session_workspaces(session_dir: Path) -> dict[str, str]:
    """Return {session_id: cwd} from session-state workspace.yaml files."""
    workspaces = {}
    if not session_dir.exists():
        return workspaces
    for s in session_dir.iterdir():
        if not s.is_dir():
            continue
        ws_file = s / "workspace.yaml"
        if ws_file.exists():
            m = re.search(r'cwd:\s*(.+)', ws_file.read_text())
            if m:
                workspaces[s.name] = m.group(1).strip()
    return workspaces


def project_name(cwd: str) -> str:
    """Shorten a workspace path to a readable project name."""
    home = str(Path.home())
    path = cwd.replace(home, "~")
    # Collapse iCloud Obsidian path
    path = re.sub(r'~/Library/Mobile Documents/iCloud~md~obsidian/Documents/', '📓 ', path)
    return path


def sync_logs_to_db(conn, logs_dir: Path, session_dir: Path, force: bool = False, source: str = 'local') -> int:
    """Parse log files and sync records into the SQLite database. Returns total new records inserted."""
    existing = conn.execute("SELECT COUNT(*) FROM api_calls WHERE source = ?", (source,)).fetchone()[0]
    log_files = sorted(logs_dir.glob("process-*.log"))

    if force:
        # Clear parse tracker so all logs are re-parsed; keep existing api_calls (INSERT OR IGNORE handles dedup)
        conn.execute("DELETE FROM parsed_logs WHERE source = ?", (source,))
        conn.commit()
        print(
            f"  🔄 Force re-sync ({source}): re-parsing {len(log_files)} log files (keeping {existing:,} existing records)",
            file=sys.stderr
        )

    total_inserted = 0
    parsed_count = 0

    for log_path in log_files:
        filename = log_path.name
        mtime = log_path.stat().st_mtime
        if not force and db.is_log_parsed(conn, filename, mtime, source=source):
            continue
        records = parse_log_file(log_path)
        for r in records:
            r['model_normalized'] = normalize_model(r['model'])
        db.insert_records(conn, records, source=source)
        db.mark_log_parsed(conn, filename, mtime, len(records), source=source)
        total_inserted += len(records)
        parsed_count += 1
        if force:
            print(f"  📄 [{parsed_count}/{len(log_files)}] {filename} ({len(records)} records)", file=sys.stderr)

    workspaces = load_session_workspaces(session_dir)
    for session_id, cwd in workspaces.items():
        db.upsert_session_workspace(conn, session_id, cwd, source=source)

    if parsed_count > 0:
        total_now = conn.execute("SELECT COUNT(*) FROM api_calls WHERE source = ?", (source,)).fetchone()[0]
        new_records = total_now - existing
        print(f"  ✅ Synced {parsed_count} log files ({source}): {new_records:,} new records ({total_now:,} total)", file=sys.stderr)

    return total_inserted


def list_codespaces(include_stopped: bool) -> list[dict]:
    """List codespaces to sync based on state filters."""
    try:
        proc = subprocess.run(
            ["gh", "cs", "list", "--json", "name,state,lastUsedAt", "--limit", "1000"],
            capture_output=True,
            text=True,
            check=False
        )
    except FileNotFoundError:
        print("  ⚠️ Codespaces sync skipped: gh CLI not found", file=sys.stderr)
        return []
    if proc.returncode != 0:
        stderr = (proc.stderr or '').strip()
        print(f"  ⚠️ Codespaces sync skipped: {stderr or 'failed to list codespaces'}", file=sys.stderr)
        return []
    try:
        items = json.loads(proc.stdout or "[]")
    except json.JSONDecodeError:
        print("  ⚠️ Codespaces sync skipped: invalid JSON from gh cs list", file=sys.stderr)
        return []
    allowed = {"Available"}
    if include_stopped:
        allowed.add("Shutdown")
    return [cs for cs in items if cs.get("state") in allowed and cs.get("name")]


def sync_codespaces_to_db(conn, include_stopped: bool = False, force: bool = False) -> int:
    """Sync ~/.copilot from codespaces via gh cs cp and import into DB sources."""
    codespaces = list_codespaces(include_stopped=include_stopped)
    if not codespaces:
        return 0
    total = 0
    for cs in codespaces:
        name = cs["name"]
        state = cs.get("state", "")
        last_used_at = cs.get("lastUsedAt")
        if last_used_at and db.get_codespace_last_used(conn, name) == last_used_at:
            print(f"  ⏭️  Skipping {name} (unchanged lastUsedAt)", file=sys.stderr)
            continue

        source = f"codespace:{name}"
        should_stop = state == "Shutdown"
        copied = False
        try:
            with tempfile.TemporaryDirectory(prefix="copilot-cs-") as tmp:
                stage = Path(tmp) / name
                stage.mkdir(parents=True, exist_ok=True)
                try:
                    cp = subprocess.run(
                        ["gh", "cs", "cp", "-e", "-r", "-c", name, "remote:/home/vscode/.copilot", str(stage)],
                        capture_output=True,
                        text=True,
                        check=False
                    )
                except FileNotFoundError:
                    print("  ⚠️ Codespaces sync skipped: gh CLI not found", file=sys.stderr)
                    return total
                if cp.returncode != 0:
                    stderr = (cp.stderr or '').strip()
                    if "No such file or directory" in stderr:
                        print(f"  ⚠️ Skipping {name}: /home/vscode/.copilot not found", file=sys.stderr)
                    else:
                        print(f"  ⚠️ Failed to copy {name}: {stderr or 'gh cs cp failed'}", file=sys.stderr)
                    continue

                copilot_dir = stage / ".copilot"
                if not (copilot_dir / "logs").exists():
                    alt = stage / "home" / "vscode" / ".copilot"
                    if (alt / "logs").exists():
                        copilot_dir = alt
                logs_dir = copilot_dir / "logs"
                session_dir = copilot_dir / "session-state"
                if not logs_dir.exists():
                    print(f"  ⚠️ Skipping {name}: no .copilot/logs in copied data", file=sys.stderr)
                    continue

                total += sync_logs_to_db(conn, logs_dir, session_dir, force=force, source=source)
                copied = True
        finally:
            if should_stop:
                try:
                    subprocess.run(
                        ["gh", "cs", "stop", "-c", name],
                        stdout=subprocess.DEVNULL,
                        stderr=subprocess.DEVNULL,
                        check=False
                    )
                except FileNotFoundError:
                    pass

        if copied:
            db.upsert_codespace_sync_state(conn, name, last_used_at)
    return total


# ─── Cost helpers ────────────────────────────────────────────────────────────

def calc_cost(model: str, stats: dict, timestamp: str = None) -> float:
    """Actual cost: cached tokens at discounted rate."""
    pricing = get_pricing(model, timestamp)
    if not pricing:
        return 0.0
    net_input = max(0, stats['prompt_tokens'] - stats['cache_read_tokens'] - stats['cache_creation_tokens'])
    return (
        (net_input / 1_000_000) * pricing['input']
        + (stats['completion_tokens'] / 1_000_000) * pricing['output']
        + (stats['cache_read_tokens'] / 1_000_000) * pricing['cache_read']
        + (stats['cache_creation_tokens'] / 1_000_000) * pricing['cache_write']
    )


def calc_cost_nocache(model: str, stats: dict, timestamp: str = None) -> float:
    """Hypothetical cost: all input tokens at full rate (no cache discount)."""
    pricing = get_pricing(model, timestamp)
    if not pricing:
        return 0.0
    return (
        (stats['prompt_tokens'] / 1_000_000) * pricing['input']
        + (stats['completion_tokens'] / 1_000_000) * pricing['output']
    )


def _sum_daily_cost(model, daily_stats, cost_fn):
    """Aggregate a cost function for a model across daily stats."""
    return sum(cost_fn(model, daily_stats[day][model], day) for day in daily_stats if model in daily_stats[day])


def _sum_daily_prem_cost(model, daily_stats):
    """Aggregate premium request cost for a model across daily stats."""
    return sum(
        daily_stats[day][model]['premium_requests'] * get_premium_request_cost(day)
        for day in daily_stats if model in daily_stats[day]
    )


def uncached_input(stats: dict) -> int:
    return max(0, stats['prompt_tokens'] - stats['cache_read_tokens'] - stats['cache_creation_tokens'])


def cache_hit_pct(prompt_tokens: int, cache_read_tokens: int) -> str:
    if prompt_tokens == 0:
        return "-"
    return f"{cache_read_tokens / prompt_tokens * 100:.0f}%"


def fmt_tokens(n: int) -> str:
    """Format token count in human-readable form (e.g. 1.2M, 456K)."""
    if n >= 1_000_000:
        return f"{n / 1_000_000:,.1f}M"
    if n >= 1_000:
        return f"{n / 1_000:,.1f}K"
    return str(n)


def fmt_cost(cost: float) -> str:
    if cost >= 100:
        return f"${cost:,.0f}"
    if cost >= 1:
        return f"${cost:,.2f}"
    return f"${cost:,.3f}"


def display_width(s: str) -> int:
    """Return the terminal display width of a string, accounting for wide/emoji chars.
    
    Uses East Asian Width (W/F = 2-wide) and VS16 (emoji presentation = 2-wide).
    BMP symbols like ⚠ (U+26A0) without VS16 are treated as 1-wide, matching
    most Western terminal emulators (iTerm2, Terminal.app, etc.).
    """
    w = 0
    chars = list(s)
    i = 0
    while i < len(chars):
        ch = chars[i]
        # VS16 (emoji presentation selector) makes the preceding char 2-wide
        has_vs16 = (i + 1 < len(chars) and chars[i + 1] == '\uFE0F')
        if has_vs16:
            w += 2
            i += 2
            continue
        cat = unicodedata.category(ch)
        if cat in ('Mn', 'Mc', 'Me'):  # combining marks: zero width
            i += 1
            continue
        eaw = unicodedata.east_asian_width(ch)
        w += 2 if eaw in ('W', 'F') else 1
        i += 1
    return w


def pad_cell(cell: str, width: int, align_right: bool = False) -> str:
    """Pad a cell string to a target display width, respecting multi-byte chars."""
    dw = display_width(cell)
    padding = max(0, width - dw)
    if align_right:
        return ' ' * padding + cell
    return cell + ' ' * padding


# ─── Pretty table helpers ───────────────────────────────────────────────────

def print_table(title: str, headers: list[str], rows: list[list[str]], footer: list[str] = None, notes: list[str] = None):
    """Print a box-drawn table with dynamic column widths."""
    # Calculate column widths from headers + all rows + footer (using display width)
    all_rows = [headers] + rows + ([footer] if footer else [])
    col_widths = [max(display_width(str(row[i])) for row in all_rows) for i in range(len(headers))]

    inner_width = sum(col_widths) + 2 * (len(col_widths) - 1) + 4

    def fmt_row(cells):
        parts = []
        for i, cell in enumerate(cells):
            w = col_widths[i]
            parts.append(pad_cell(str(cell), w, align_right=(i > 0)))
        content = "  " + "  ".join(parts)
        padding = max(0, inner_width - display_width(content))
        return "│" + content + " " * padding + "│"

    def separator(char="─"):
        content = "  " + "  ".join(char * w for w in col_widths) + "  "
        padding = max(0, inner_width - len(content))
        return "│" + content + " " * padding + "│"

    bar = "─" * inner_width

    print(f"┌─ {title} {'─' * (inner_width - len(title) - 3)}┐")
    print(f"│{' ' * inner_width}│")
    print(fmt_row(headers))
    print(separator())
    for row in rows:
        print(fmt_row(row))
    if footer:
        print(separator())
        print(fmt_row(footer))
    print(f"│{' ' * inner_width}│")
    print(f"└{bar}┘")
    if notes:
        for note in notes:
            print(f"  {note}")


def main():
    parser = argparse.ArgumentParser(
        description='Copilot CLI Token Cost Calculator',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="Examples:\n  %(prog)s              # last 7 days\n  %(prog)s 30            # last 30 days\n  %(prog)s 1             # today\n  %(prog)s --today       # today\n  %(prog)s --yesterday   # yesterday only\n  %(prog)s --from 3      # 3 days ago until now\n  %(prog)s --from 3 --to 1  # 3 days ago to yesterday\n  %(prog)s --from 1 --to 1  # yesterday only\n  %(prog)s --all         # all logs",
    )
    parser.add_argument('days', nargs='?', type=int, default=None, help='Number of days to look back (default: 7)')
    parser.add_argument('--all', action='store_true', help='Process all available logs')
    parser.add_argument('--today', action='store_true', help='Today only')
    parser.add_argument('--yesterday', action='store_true', help='Yesterday only')
    parser.add_argument('--from', type=int, dest='from_days', metavar='N', help='Start from N days ago (0=today, 1=yesterday)')
    parser.add_argument('--to', type=int, dest='to_days', metavar='N', help='End at N days ago (0=today, 1=yesterday)')
    parser.add_argument('--logs-dir', type=str, default=None, help='Override logs directory')
    parser.add_argument('--json', action='store_true', help='Output as JSON')
    parser.add_argument('--sync', action='store_true', help='Force full re-sync of all log files')
    parser.add_argument('--import-file', type=str, metavar='FILE', help='Import data from JSONL or SQLite file')
    parser.add_argument('--export-file', type=str, metavar='FILE', help='Export data as JSONL')
    parser.add_argument('--codespaces-sync', action='store_true', help='Sync Copilot data from running Codespaces via gh cs cp')
    parser.add_argument('--codespaces-include-stopped', action='store_true', help='Include stopped Codespaces (will wake and sync them)')
    args = parser.parse_args()

    if args.codespaces_include_stopped and not args.codespaces_sync:
        parser.error('--codespaces-include-stopped requires --codespaces-sync')

    home = Path.home()
    logs_dir = Path(args.logs_dir) if args.logs_dir else home / ".copilot" / "logs"
    session_dir = home / ".copilot" / "session-state"

    # ─── DB setup and sync ──────────────────────────────────────────────
    conn = db.get_connection()

    if logs_dir.exists():
        sync_logs_to_db(conn, logs_dir, session_dir, force=args.sync)

    if args.codespaces_sync:
        sync_codespaces_to_db(conn, include_stopped=args.codespaces_include_stopped, force=args.sync)

    if args.import_file:
        import_path = args.import_file
        if import_path.endswith('.db') or import_path.endswith('.sqlite'):
            db.import_sqlite_db(conn, import_path)
        else:
            db.import_jsonl(conn, import_path)

    if args.export_file:
        db.export_jsonl(conn, args.export_file)
        conn.close()
        return

    today_midnight = datetime.now().replace(hour=0, minute=0, second=0, microsecond=0)

    if args.all:
        cutoff = datetime.min
        cutoff_end = None
        period_label = "all time"
    elif args.today:
        cutoff = today_midnight
        cutoff_end = None
        period_label = "today"
    elif args.yesterday:
        cutoff = today_midnight - timedelta(days=1)
        cutoff_end = today_midnight
        period_label = "yesterday"
    elif args.from_days is not None:
        from_days = args.from_days
        to_days = args.to_days if args.to_days is not None else 0
        # Values are days ago: 0=today, 1=yesterday, 2=two days ago
        # Ensure from >= to (from is further in the past)
        if from_days < to_days:
            from_days, to_days = to_days, from_days
        cutoff = today_midnight - timedelta(days=from_days)
        cutoff_end = (today_midnight - timedelta(days=to_days) + timedelta(days=1)) if to_days > 0 else None
        if from_days == to_days:
            date_str = cutoff.strftime('%Y-%m-%d')
            period_label = f"{date_str} (1 day)"
        else:
            period_label = f"{from_days}d ago → {'today' if to_days == 0 else f'{to_days}d ago'}"
    else:
        days = args.days if args.days is not None else 7
        cutoff = (today_midnight - timedelta(days=days - 1))
        cutoff_end = None
        period_label = f"last {days} day{'s' if days != 1 else ''}"

    date_from = cutoff.strftime('%Y-%m-%dT%H:%M:%S') if cutoff != datetime.min else None
    date_to = cutoff_end.strftime('%Y-%m-%dT%H:%M:%S') if cutoff_end else None
    date_from_display = cutoff.strftime('%Y-%m-%d') if cutoff != datetime.min else None
    date_to_display = (cutoff_end - timedelta(days=1)).strftime('%Y-%m-%d') if cutoff_end else datetime.now().strftime('%Y-%m-%d')
    date_range = f"{date_from_display} → {date_to_display}" if date_from_display else None

    # ─── Query aggregated stats from DB ──────────────────────────────────
    model_stats = db.query_model_stats(conn, date_from, date_to)

    daily_stats = db.query_daily_stats(conn, date_from, date_to)
    for day, day_models in daily_stats.items():
        for model, s in day_models.items():
            s['premium_requests'] = s.pop('user_turns') * get_premium_multiplier(model, day)

    # Compute model-level premium_requests from daily (multiplier varies by day)
    for model, s in model_stats.items():
        s.pop('user_turns')
        s['premium_requests'] = sum(
            daily_stats[day][model]['premium_requests']
            for day in daily_stats if model in daily_stats[day]
        )

    project_stats_raw = db.query_project_stats(conn, date_from, date_to)
    project_stats = {}
    for cwd, s in project_stats_raw.items():
        proj = project_name(cwd) if cwd else '(unknown)'
        s['premium_requests'] = s.pop('user_turns')  # already aggregated across models
        if proj in project_stats:
            for k in s:
                project_stats[proj][k] += s[k]
        else:
            project_stats[proj] = dict(s)

    # ─── Get filtered records and session workspaces for per-project cost calc ─
    filtered_records = db.query_records(conn, date_from, date_to)
    session_workspaces = db.query_session_workspaces_by_source(conn)

    total_records = sum(s['api_calls'] for s in model_stats.values())
    log_file_count = conn.execute(
        "SELECT COUNT(DISTINCT log_file) FROM api_calls" + (" WHERE timestamp >= ? AND timestamp < ?" if date_from and date_to else " WHERE timestamp >= ?" if date_from else ""),
        ([date_from, date_to] if date_from and date_to else [date_from] if date_from else [])
    ).fetchone()[0]

    if total_records == 0:
        print(f"No API calls found in {period_label}.")
        conn.close()
        sys.exit(0)

    # ─── JSON output ─────────────────────────────────────────────────────
    if args.json:
        output = {
            'period': period_label, 'date_range': date_range, 'log_files': log_file_count,
            'api_calls': total_records, 'models': {}, 'daily': {}, 'projects': {},
            'total_cost': 0.0, 'total_cost_without_cache': 0.0,
            'total_premium_request_cost': round(sum(_sum_daily_prem_cost(m, daily_stats) for m in model_stats), 4),
        }
        for model, stats in sorted(model_stats.items()):
            cost = _sum_daily_cost(model, daily_stats, calc_cost)
            cost_nc = _sum_daily_cost(model, daily_stats, calc_cost_nocache)
            output['models'][model] = {
                **stats, 'input_uncached_tokens': uncached_input(stats),
                'cost': round(cost, 4), 'cost_without_cache': round(cost_nc, 4),
                'premium_request_cost': round(_sum_daily_prem_cost(model, daily_stats), 4),
            }
            output['total_cost'] += cost
            output['total_cost_without_cache'] += cost_nc
        for day in sorted(daily_stats.keys()):
            day_total = day_total_nc = 0.0
            output['daily'][day] = {}
            for model, stats in daily_stats[day].items():
                cost, cost_nc = calc_cost(model, stats, day), calc_cost_nocache(model, stats, day)
                output['daily'][day][model] = {
                    **stats, 'input_uncached_tokens': uncached_input(stats),
                    'cost': round(cost, 4), 'cost_without_cache': round(cost_nc, 4),
                }
                day_total += cost; day_total_nc += cost_nc
            output['daily'][day]['_total_cost'] = round(day_total, 4)
            output['daily'][day]['_total_cost_without_cache'] = round(day_total_nc, 4)
        for proj, stats in sorted(project_stats.items(), key=lambda x: calc_cost_nocache('claude-opus-4.6', x[1]), reverse=True):
            # Use a generic model for project-level cost since it's aggregated across models
            # We need per-record cost, so recalculate from filtered_records
            pass
        # Per-project costs: aggregate actual per-record costs
        for proj, stats in project_stats.items():
            output['projects'][proj] = {
                **stats, 'input_uncached_tokens': uncached_input(stats),
            }
        # Recalculate per-project costs from records
        proj_costs = defaultdict(lambda: {'cost': 0.0, 'cost_without_cache': 0.0})
        for r in filtered_records:
            sid = r.get('session_id')
            src = r.get('source', '')
            cwd = session_workspaces.get((sid, src), '') if sid else ''
            proj = project_name(cwd) if cwd else '(unknown)'
            model = normalize_model(r['model'])
            rs = {'prompt_tokens': r['prompt_tokens'], 'completion_tokens': r['completion_tokens'],
                  'cache_creation_tokens': r['cache_creation_tokens'], 'cache_read_tokens': r['cache_read_tokens']}
            proj_costs[proj]['cost'] += calc_cost(model, rs, r.get('timestamp'))
            proj_costs[proj]['cost_without_cache'] += calc_cost_nocache(model, rs, r.get('timestamp'))
        for proj in output['projects']:
            output['projects'][proj]['cost'] = round(proj_costs[proj]['cost'], 4)
            output['projects'][proj]['cost_without_cache'] = round(proj_costs[proj]['cost_without_cache'], 4)
        output['total_cost'] = round(output['total_cost'], 4)
        output['total_cost_without_cache'] = round(output['total_cost_without_cache'], 4)
        print(json.dumps(output, indent=2))
        conn.close()
        return

    # ─── Pretty output ───────────────────────────────────────────────────
    print()
    title = "COPILOT CLI - TOKEN USAGE & COST REPORT"
    title_width = len(title) + 10  # padding around title
    title_pad_l = (title_width - len(title)) // 2
    title_pad_r = title_width - len(title) - title_pad_l
    print(f"╔{'═' * title_width}╗")
    print(f"║{' ' * title_pad_l}{title}{' ' * title_pad_r}║")
    print(f"╚{'═' * title_width}╝")
    total_premium = sum(s['premium_requests'] for s in model_stats.values())
    date_suffix = f" ({date_range})" if date_range else ""
    print(f"  Period: {period_label}{date_suffix}  │  Log files: {log_file_count}  │  API calls: {total_records:,}  │  Premium requests: {total_premium:,}")
    print()

    # ── Per-model table ──────────────────────────────────────────────────
    model_headers = ["Model", "Calls", "Premium", "Prem Cost", "Input", "Cached", "Cache Write", "Output", "Hit%", "Cost", "No-Cache"]
    model_rows = []
    t_cost = t_nc = t_prem_cost = 0.0
    t_unc = t_cached = t_cw = t_out = t_calls = t_prompt = t_premium = 0
    for model in sorted(model_stats.keys(), key=lambda m: _sum_daily_cost(m, daily_stats, calc_cost_nocache), reverse=True):
        s = model_stats[model]
        cost = _sum_daily_cost(model, daily_stats, calc_cost)
        cost_nc = _sum_daily_cost(model, daily_stats, calc_cost_nocache)
        unc = uncached_input(s)
        t_cost += cost; t_nc += cost_nc
        t_unc += unc; t_cached += s['cache_read_tokens']; t_cw += s['cache_creation_tokens']
        t_out += s['completion_tokens']; t_calls += s['api_calls']; t_prompt += s['prompt_tokens']
        t_premium += s['premium_requests']
        p = get_pricing(model)
        mult = get_premium_multiplier(model)
        premium_str = f"{s['premium_requests']:,}" if mult > 0 else "-"
        prem_cost = _sum_daily_prem_cost(model, daily_stats)
        t_prem_cost += prem_cost
        model_rows.append([
            model, f"{s['api_calls']:,}", premium_str,
            fmt_cost(prem_cost) if mult > 0 else "-",
            fmt_tokens(unc), fmt_tokens(s['cache_read_tokens']),
            fmt_tokens(s['cache_creation_tokens']), fmt_tokens(s['completion_tokens']),
            cache_hit_pct(s['prompt_tokens'], s['cache_read_tokens']),
            fmt_cost(cost) if p else "N/A", fmt_cost(cost_nc) if p else "N/A",
        ])
    model_footer = [
        "TOTAL", f"{t_calls:,}", f"{t_premium:,}",
        fmt_cost(t_prem_cost),
        fmt_tokens(t_unc), fmt_tokens(t_cached),
        fmt_tokens(t_cw), fmt_tokens(t_out),
        cache_hit_pct(t_prompt, t_cached),
        fmt_cost(t_cost), fmt_cost(t_nc),
    ]
    savings_pct = (1 - t_cost / t_nc) * 100 if t_nc > 0 else 0
    notes = [f"💰 Cache savings: {fmt_cost(t_nc - t_cost)} ({savings_pct:.0f}% reduction)"] if t_nc > 0 else []
    print_table("PER-MODEL SUMMARY", model_headers, model_rows, model_footer, notes)
    print()

    # ── Cost per premium request ─────────────────────────────────────────
    prem_headers = ["Model", "Multiplier", "Premiums", "API Cost", "$/Premium", "Prem Cost", "Discount"]
    prem_rows = []
    prem_total_cost = 0.0
    prem_total_reqs = 0
    for model in sorted(model_stats.keys(), key=lambda m: model_stats[m]['premium_requests'], reverse=True):
        s = model_stats[model]
        mult = get_premium_multiplier(model)
        if mult == 0:
            continue
        cost = _sum_daily_cost(model, daily_stats, calc_cost)
        if s['premium_requests'] > 0:
            # Only include models where we actually tracked premium requests
            prem_total_cost += cost
            prem_total_reqs += s['premium_requests']
            cost_per = cost / s['premium_requests']
            prem_cost = _sum_daily_prem_cost(model, daily_stats)
            discount = f"{(1 - prem_cost / cost) * 100:.0f}%" if cost > 0 else "-"
            prem_rows.append([
                model, f"{mult}×", f"{s['premium_requests']:,}",
                fmt_cost(cost), fmt_cost(cost_per), fmt_cost(prem_cost), discount,
            ])
        else:
            prem_rows.append([
                model, f"{mult}×", "-",
                fmt_cost(cost), "N/A", "-", "-",
            ])
    if prem_rows:
        avg_cost = prem_total_cost / prem_total_reqs if prem_total_reqs > 0 else 0
        total_prem_cost = sum(_sum_daily_prem_cost(m, daily_stats) for m in model_stats)
        total_discount = f"{(1 - total_prem_cost / prem_total_cost) * 100:.0f}%" if prem_total_cost > 0 else "-"
        prem_footer = ["TOTAL", "", f"{prem_total_reqs:,}", fmt_cost(prem_total_cost), fmt_cost(avg_cost), fmt_cost(total_prem_cost), total_discount]
        notes = ["ℹ️  Models with 0× multiplier (free tier) are excluded"]
        if prem_total_reqs < t_premium:
            pass  # all accounted for
        missing_cost = t_cost - prem_total_cost
        if missing_cost > 0.001:
            notes.append(f"⚠  {fmt_cost(missing_cost)} from models without premium data excluded from $/premium avg")
        print_table("COST PER PREMIUM REQUEST", prem_headers, prem_rows, prem_footer, notes)
        print()

    # ── Daily table ──────────────────────────────────────────────────────
    daily_headers = ["Date", "Calls", "Premium", "Input", "Cached", "Output", "Hit%", "Cost", "No-Cache", "Prem Cost", "Discount"]
    daily_rows = []
    for day in sorted(daily_stats.keys()):
        d_calls = sum(s['api_calls'] for s in daily_stats[day].values())
        d_premium = sum(s['premium_requests'] for s in daily_stats[day].values())
        d_unc = sum(uncached_input(s) for s in daily_stats[day].values())
        d_cached = sum(s['cache_read_tokens'] for s in daily_stats[day].values())
        d_out = sum(s['completion_tokens'] for s in daily_stats[day].values())
        d_cost = sum(calc_cost(m, s, day) for m, s in daily_stats[day].items())
        d_nc = sum(calc_cost_nocache(m, s, day) for m, s in daily_stats[day].items())
        d_total = d_unc + d_cached
        d_prem_cost = sum(s['premium_requests'] * get_premium_request_cost(day) for s in daily_stats[day].values())
        d_discount = f"{(1 - d_prem_cost / d_cost) * 100:.0f}%" if d_cost > 0 else "-"
        daily_rows.append([
            day, f"{d_calls:,}", f"{d_premium:,}",
            fmt_tokens(d_unc), fmt_tokens(d_cached),
            fmt_tokens(d_out), cache_hit_pct(d_total, d_cached),
            fmt_cost(d_cost), fmt_cost(d_nc), fmt_cost(d_prem_cost), d_discount,
        ])
    print_table("DAILY BREAKDOWN", daily_headers, daily_rows)
    print()

    # ── Per-project table ────────────────────────────────────────────────
    # Calculate per-project costs from individual records (preserving per-model pricing)
    proj_costs = defaultdict(lambda: {'cost': 0.0, 'cost_nc': 0.0})
    for r in filtered_records:
        sid = r.get('session_id')
        src = r.get('source', '')
        cwd = session_workspaces.get((sid, src), '') if sid else ''
        proj = project_name(cwd) if cwd else '(unknown)'
        model = normalize_model(r['model'])
        rs = {'prompt_tokens': r['prompt_tokens'], 'completion_tokens': r['completion_tokens'],
              'cache_creation_tokens': r['cache_creation_tokens'], 'cache_read_tokens': r['cache_read_tokens']}
        proj_costs[proj]['cost'] += calc_cost(model, rs, r.get('timestamp'))
        proj_costs[proj]['cost_nc'] += calc_cost_nocache(model, rs, r.get('timestamp'))

    proj_headers = ["Project", "Calls", "Premium", "Input", "Cached", "Output", "Hit%", "Cost", "No-Cache"]
    proj_rows = []
    for proj in sorted(project_stats.keys(), key=lambda p: proj_costs[p]['cost_nc'], reverse=True):
        s = project_stats[proj]
        proj_rows.append([
            proj, f"{s['api_calls']:,}", f"{s['premium_requests']:,}",
            fmt_tokens(uncached_input(s)),
            fmt_tokens(s['cache_read_tokens']), fmt_tokens(s['completion_tokens']),
            cache_hit_pct(s['prompt_tokens'], s['cache_read_tokens']),
            fmt_cost(proj_costs[proj]['cost']), fmt_cost(proj_costs[proj]['cost_nc']),
        ])
    print_table("PER-PROJECT BREAKDOWN", proj_headers, proj_rows)
    print()

    # ── Pricing reference ────────────────────────────────────────────────
    price_headers = ["Model", "Input/1M", "Output/1M", "Cache Read/1M", "Cache Write/1M"]
    price_rows = []
    used_models = sorted(model_stats.keys())
    for model in used_models:
        p = get_pricing(model)
        if p:
            price_rows.append([model, f"${p['input']:.2f}", f"${p['output']:.2f}", f"${p['cache_read']:.3f}", f"${p['cache_write']:.2f}"])
        else:
            price_rows.append([model, "N/A", "N/A", "N/A", "N/A"])
    print_table("PRICING REFERENCE", price_headers, price_rows)
    print()
    print("  ⚠  Estimated API-equivalent costs. Copilot subscriptions include token usage.")
    print()

    conn.close()


if __name__ == '__main__':
    main()
