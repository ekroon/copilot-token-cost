"""SQLite database layer for the Copilot Token Cost Calculator."""

import json
import sqlite3
from pathlib import Path

DB_NAME = "copilot-tokens.db"
DEFAULT_DB_PATH = Path(__file__).resolve().parent / DB_NAME

SCHEMA_SQL = """
CREATE TABLE IF NOT EXISTS api_calls (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    model TEXT NOT NULL,
    model_normalized TEXT NOT NULL,
    prompt_tokens INTEGER NOT NULL,
    completion_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER DEFAULT 0,
    cache_read_tokens INTEGER DEFAULT 0,
    is_user_turn INTEGER DEFAULT 0,
    timestamp TEXT,
    session_id TEXT,
    log_file TEXT,
    source TEXT DEFAULT 'local',
    UNIQUE(timestamp, model, prompt_tokens, completion_tokens, log_file, source)
);

CREATE TABLE IF NOT EXISTS parsed_logs (
    log_file TEXT NOT NULL,
    mtime REAL NOT NULL,
    source TEXT DEFAULT 'local',
    record_count INTEGER DEFAULT 0,
    parsed_at TEXT NOT NULL,
    PRIMARY KEY (log_file, source)
);

CREATE TABLE IF NOT EXISTS session_workspaces (
    session_id TEXT NOT NULL,
    cwd TEXT NOT NULL,
    source TEXT DEFAULT 'local',
    PRIMARY KEY (session_id, source)
);

CREATE TABLE IF NOT EXISTS codespace_sync_state (
    codespace_name TEXT PRIMARY KEY,
    last_used_at TEXT,
    last_synced_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_api_calls_timestamp ON api_calls(timestamp);
CREATE INDEX IF NOT EXISTS idx_api_calls_model ON api_calls(model_normalized);
CREATE INDEX IF NOT EXISTS idx_api_calls_session ON api_calls(session_id);
"""


def get_connection(db_path: Path = None) -> sqlite3.Connection:
    """Get a connection to the DB, creating tables if needed. Default path is copilot-tokens.db next to this script."""
    if db_path is None:
        db_path = DEFAULT_DB_PATH
    conn = sqlite3.connect(str(db_path))
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    conn.executescript(SCHEMA_SQL)
    migrate_session_workspaces_schema(conn)
    return conn


def migrate_session_workspaces_schema(conn):
    """Migrate session_workspaces to composite PK (session_id, source) for source-safe joins."""
    table_exists = conn.execute(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name='session_workspaces'"
    ).fetchone()
    if not table_exists:
        return
    pk_cols = [
        row[1]
        for row in conn.execute("PRAGMA table_info(session_workspaces)").fetchall()
        if row[5] > 0
    ]
    if pk_cols == ['session_id', 'source']:
        return
    conn.executescript("""
    ALTER TABLE session_workspaces RENAME TO session_workspaces_old;
    CREATE TABLE session_workspaces (
        session_id TEXT NOT NULL,
        cwd TEXT NOT NULL,
        source TEXT DEFAULT 'local',
        PRIMARY KEY (session_id, source)
    );
    INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source)
    SELECT session_id, cwd, COALESCE(source, 'local') FROM session_workspaces_old;
    DROP TABLE session_workspaces_old;
    """)
    conn.commit()


def is_log_parsed(conn, log_file: str, mtime: float, source: str = 'local') -> bool:
    """Check if a log file has already been parsed with the same mtime."""
    row = conn.execute(
        "SELECT 1 FROM parsed_logs WHERE log_file = ? AND source = ? AND mtime = ?",
        (log_file, source, mtime)
    ).fetchone()
    return row is not None


def mark_log_parsed(conn, log_file: str, mtime: float, record_count: int, source: str = 'local'):
    """Record that a log file has been parsed."""
    conn.execute(
        "INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) "
        "VALUES (?, ?, ?, ?, datetime('now'))",
        (log_file, mtime, source, record_count)
    )
    conn.commit()


def insert_records(conn, records: list[dict], source: str = 'local'):
    """Bulk insert API call records. Uses INSERT OR IGNORE for dedup."""
    if not records:
        return
    conn.executemany(
        "INSERT OR IGNORE INTO api_calls "
        "(model, model_normalized, prompt_tokens, completion_tokens, "
        "cache_creation_tokens, cache_read_tokens, is_user_turn, "
        "timestamp, session_id, log_file, source) "
        "VALUES (:model, :model_normalized, :prompt_tokens, :completion_tokens, "
        ":cache_creation_tokens, :cache_read_tokens, :is_user_turn, "
        ":timestamp, :session_id, :log_file, :source)",
        [{**r, "source": source} for r in records]
    )
    conn.commit()


def upsert_session_workspace(conn, session_id: str, cwd: str, source: str = 'local'):
    """Insert or update a session workspace mapping."""
    conn.execute(
        "INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source) VALUES (?, ?, ?)",
        (session_id, cwd, source)
    )
    conn.commit()


def get_codespace_last_used(conn, codespace_name: str) -> str | None:
    """Return last_used_at recorded at the last successful sync for a codespace."""
    row = conn.execute(
        "SELECT last_used_at FROM codespace_sync_state WHERE codespace_name = ?",
        (codespace_name,)
    ).fetchone()
    return row[0] if row else None


def upsert_codespace_sync_state(conn, codespace_name: str, last_used_at: str | None):
    """Record the latest successful sync marker for a codespace."""
    conn.execute(
        "INSERT OR REPLACE INTO codespace_sync_state (codespace_name, last_used_at, last_synced_at) "
        "VALUES (?, ?, datetime('now'))",
        (codespace_name, last_used_at)
    )
    conn.commit()


def export_jsonl(conn, output_path: str, source: str = None):
    """Export api_calls and session_workspaces as JSONL."""
    with open(output_path, 'w') as f:
        where = " WHERE source = ?" if source else ""
        params = (source,) if source else ()
        rows = conn.execute(
            "SELECT model, model_normalized, prompt_tokens, completion_tokens, "
            "cache_creation_tokens, cache_read_tokens, is_user_turn, "
            "timestamp, session_id, log_file, source FROM api_calls" + where, params
        ).fetchall()
        cols = ["model", "model_normalized", "prompt_tokens", "completion_tokens",
                "cache_creation_tokens", "cache_read_tokens", "is_user_turn",
                "timestamp", "session_id", "log_file", "source"]
        for row in rows:
            rec = dict(zip(cols, row))
            rec["type"] = "api_call"
            f.write(json.dumps(rec) + "\n")
        rows = conn.execute(
            "SELECT session_id, cwd, source FROM session_workspaces" + where, params
        ).fetchall()
        for row in rows:
            rec = {"type": "session_workspace", "session_id": row[0], "cwd": row[1], "source": row[2]}
            f.write(json.dumps(rec) + "\n")


def import_jsonl(conn, input_path: str, source_override: str = None) -> int:
    """Import records from a JSONL file. Returns count of records processed."""
    count = 0
    with open(input_path, 'r') as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            obj = json.loads(line)
            rtype = obj.pop("type", None)
            if rtype == "api_call":
                src = source_override if source_override else obj.get("source", "local")
                obj["source"] = src
                insert_records(conn, [obj], source=src)
            elif rtype == "session_workspace":
                src = source_override if source_override else obj.get("source", "local")
                upsert_session_workspace(conn, obj["session_id"], obj["cwd"], source=src)
            count += 1
    return count


def import_sqlite_db(conn, other_db_path: str, source_override: str = None) -> int:
    """Import records from another copilot-tokens.db. Returns count of records imported."""
    conn.execute("ATTACH DATABASE ? AS import_db", (other_db_path,))
    try:
        if source_override:
            src = source_override
            conn.execute(
                "INSERT OR IGNORE INTO api_calls "
                "(model, model_normalized, prompt_tokens, completion_tokens, "
                "cache_creation_tokens, cache_read_tokens, is_user_turn, "
                "timestamp, session_id, log_file, source) "
                "SELECT model, model_normalized, prompt_tokens, completion_tokens, "
                "cache_creation_tokens, cache_read_tokens, is_user_turn, "
                "timestamp, session_id, log_file, ? FROM import_db.api_calls", (src,)
            )
            conn.execute(
                "INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source) "
                "SELECT session_id, cwd, ? FROM import_db.session_workspaces", (src,)
            )
            conn.execute(
                "INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) "
                "SELECT log_file, mtime, ?, record_count, parsed_at FROM import_db.parsed_logs", (src,)
            )
        else:
            conn.execute(
                "INSERT OR IGNORE INTO api_calls "
                "(model, model_normalized, prompt_tokens, completion_tokens, "
                "cache_creation_tokens, cache_read_tokens, is_user_turn, "
                "timestamp, session_id, log_file, source) "
                "SELECT model, model_normalized, prompt_tokens, completion_tokens, "
                "cache_creation_tokens, cache_read_tokens, is_user_turn, "
                "timestamp, session_id, log_file, source FROM import_db.api_calls"
            )
            conn.execute(
                "INSERT OR REPLACE INTO session_workspaces SELECT * FROM import_db.session_workspaces"
            )
            conn.execute(
                "INSERT OR REPLACE INTO parsed_logs SELECT * FROM import_db.parsed_logs"
            )
        count = conn.execute("SELECT changes()").fetchone()[0]
        conn.commit()
    finally:
        conn.execute("DETACH DATABASE import_db")
    return count


def _build_filters(date_from=None, date_to=None, source=None, table_alias='a'):
    """Build WHERE clause fragments and params for common filters."""
    clauses = []
    params = []
    if date_from is not None:
        clauses.append(f"{table_alias}.timestamp >= ?")
        params.append(date_from)
    if date_to is not None:
        clauses.append(f"{table_alias}.timestamp < ?")
        params.append(date_to)
    if source is not None:
        clauses.append(f"{table_alias}.source = ?")
        params.append(source)
    where = (" WHERE " + " AND ".join(clauses)) if clauses else ""
    return where, params


def query_model_stats(conn, date_from=None, date_to=None, source=None) -> dict:
    """Aggregate stats grouped by model_normalized.

    Returns {model_normalized: {api_calls, prompt_tokens, completion_tokens,
    cache_creation_tokens, cache_read_tokens, user_turns}}.
    """
    where, params = _build_filters(date_from, date_to, source)
    sql = (
        "SELECT model_normalized, "
        "COUNT(*) AS api_calls, "
        "SUM(prompt_tokens) AS prompt_tokens, "
        "SUM(completion_tokens) AS completion_tokens, "
        "SUM(cache_creation_tokens) AS cache_creation_tokens, "
        "SUM(cache_read_tokens) AS cache_read_tokens, "
        "SUM(CASE WHEN is_user_turn = 1 THEN 1 ELSE 0 END) AS user_turns "
        f"FROM api_calls a{where} "
        "GROUP BY model_normalized"
    )
    result = {}
    for row in conn.execute(sql, params):
        result[row[0]] = {
            'api_calls': row[1],
            'prompt_tokens': row[2],
            'completion_tokens': row[3],
            'cache_creation_tokens': row[4],
            'cache_read_tokens': row[5],
            'user_turns': row[6],
        }
    return result


def query_daily_stats(conn, date_from=None, date_to=None, source=None) -> dict:
    """Aggregate stats grouped by date and model_normalized.

    Returns {date_str: {model_normalized: {api_calls, prompt_tokens, completion_tokens,
    cache_creation_tokens, cache_read_tokens, user_turns}}}.
    """
    where, params = _build_filters(date_from, date_to, source)
    sql = (
        "SELECT substr(a.timestamp, 1, 10) AS day, model_normalized, "
        "COUNT(*) AS api_calls, "
        "SUM(prompt_tokens) AS prompt_tokens, "
        "SUM(completion_tokens) AS completion_tokens, "
        "SUM(cache_creation_tokens) AS cache_creation_tokens, "
        "SUM(cache_read_tokens) AS cache_read_tokens, "
        "SUM(CASE WHEN is_user_turn = 1 THEN 1 ELSE 0 END) AS user_turns "
        f"FROM api_calls a{where} "
        "GROUP BY day, model_normalized"
    )
    result = {}
    for row in conn.execute(sql, params):
        day = row[0] or 'unknown'
        if day not in result:
            result[day] = {}
        result[day][row[1]] = {
            'api_calls': row[2],
            'prompt_tokens': row[3],
            'completion_tokens': row[4],
            'cache_creation_tokens': row[5],
            'cache_read_tokens': row[6],
            'user_turns': row[7],
        }
    return result


def query_project_stats(conn, date_from=None, date_to=None, source=None) -> dict:
    """Aggregate stats grouped by project (cwd from session_workspaces).

    Returns {cwd: {api_calls, prompt_tokens, completion_tokens,
    cache_creation_tokens, cache_read_tokens, user_turns}}.
    Records without a matching session workspace are grouped under ''.
    """
    where, params = _build_filters(date_from, date_to, source)
    sql = (
        "SELECT COALESCE(sw.cwd, '') AS cwd, "
        "COUNT(*) AS api_calls, "
        "SUM(a.prompt_tokens) AS prompt_tokens, "
        "SUM(a.completion_tokens) AS completion_tokens, "
        "SUM(a.cache_creation_tokens) AS cache_creation_tokens, "
        "SUM(a.cache_read_tokens) AS cache_read_tokens, "
        "SUM(CASE WHEN a.is_user_turn = 1 THEN 1 ELSE 0 END) AS user_turns "
        f"FROM api_calls a LEFT JOIN session_workspaces sw "
        f"ON a.session_id = sw.session_id AND a.source = sw.source{where} "
        "GROUP BY cwd"
    )
    result = {}
    for row in conn.execute(sql, params):
        result[row[0]] = {
            'api_calls': row[1],
            'prompt_tokens': row[2],
            'completion_tokens': row[3],
            'cache_creation_tokens': row[4],
            'cache_read_tokens': row[5],
            'user_turns': row[6],
        }
    return result


def query_records(conn, date_from=None, date_to=None, source=None) -> list[dict]:
    """Return all api_call records matching the filters as a list of dicts."""
    where, params = _build_filters(date_from, date_to, source)
    sql = (
        "SELECT model, model_normalized, prompt_tokens, completion_tokens, "
        "cache_creation_tokens, cache_read_tokens, is_user_turn, "
        f"timestamp, session_id, log_file, source FROM api_calls a{where}"
    )
    cols = ['model', 'model_normalized', 'prompt_tokens', 'completion_tokens',
            'cache_creation_tokens', 'cache_read_tokens', 'is_user_turn',
            'timestamp', 'session_id', 'log_file', 'source']
    return [dict(zip(cols, row)) for row in conn.execute(sql, params)]


def query_session_workspaces(conn, source=None) -> dict:
    """Return {session_id: cwd} dict, optionally filtered by source."""
    if source is not None:
        rows = conn.execute(
            "SELECT session_id, cwd FROM session_workspaces WHERE source = ?",
            (source,)
        )
    else:
        rows = conn.execute("SELECT session_id, cwd FROM session_workspaces")
    return {row[0]: row[1] for row in rows}


def query_session_workspaces_by_source(conn, source=None) -> dict:
    """Return {(session_id, source): cwd} dict, optionally filtered by source."""
    if source is not None:
        rows = conn.execute(
            "SELECT session_id, cwd, source FROM session_workspaces WHERE source = ?",
            (source,)
        )
    else:
        rows = conn.execute("SELECT session_id, cwd, source FROM session_workspaces")
    return {(row[0], row[2]): row[1] for row in rows}


def clear_source(conn, source: str = 'local'):
    """Delete all records for a given source (used for --sync full re-parse)."""
    conn.execute("DELETE FROM api_calls WHERE source = ?", (source,))
    conn.execute("DELETE FROM parsed_logs WHERE source = ?", (source,))
    conn.execute("DELETE FROM session_workspaces WHERE source = ?", (source,))
    conn.commit()
