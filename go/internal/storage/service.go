package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"copilot-token-cost/internal/domain"
)

const SchemaSQL = `
CREATE TABLE IF NOT EXISTS api_calls (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    model TEXT NOT NULL,
    model_normalized TEXT NOT NULL,
    prompt_tokens INTEGER NOT NULL,
    completion_tokens INTEGER NOT NULL,
    prompt_text TEXT,
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
    branch TEXT,
    PRIMARY KEY (session_id, source)
);

CREATE TABLE IF NOT EXISTS codespace_sync_state (
    codespace_name TEXT PRIMARY KEY,
    last_used_at TEXT,
    last_synced_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS codespace_tail_offsets (
    source TEXT NOT NULL,
    log_file TEXT NOT NULL,
    last_offset INTEGER NOT NULL DEFAULT 0,
    last_size INTEGER NOT NULL DEFAULT 0,
    last_mtime TEXT,
    last_hash TEXT,
    connection_state TEXT NOT NULL DEFAULT 'disconnected',
    last_error TEXT,
    last_chunk_at TEXT,
    last_full_copy_at TEXT,
    last_defensive_recopy_at TEXT,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (source, log_file)
);

CREATE INDEX IF NOT EXISTS idx_codespace_tail_offsets_source ON codespace_tail_offsets(source);

CREATE INDEX IF NOT EXISTS idx_api_calls_timestamp ON api_calls(timestamp);
CREATE INDEX IF NOT EXISTS idx_api_calls_model ON api_calls(model_normalized);
CREATE INDEX IF NOT EXISTS idx_api_calls_session ON api_calls(session_id);
`

type Service struct {
	db *sql.DB
}

type LogSyncTx struct {
	tx                  *sql.Tx
	insertStmt          *sql.Stmt
	parsedStmt          *sql.Stmt
	source              string
	normalizeModel      func(string) string
	promptTextForRecord func(*string) sql.NullString
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

func (s *Service) Database() *sql.DB {
	return s.db
}

func (s *Service) Ping(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	return s.db.PingContext(ctx)
}

func (s *Service) Initialize() error {
	if s.db == nil {
		return nil
	}
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	if _, err := s.db.Exec(SchemaSQL); err != nil {
		return err
	}
	s.MigrateAPICallsSchema()
	s.MigrateSessionWorkspacesSchema()
	return nil
}

func (s *Service) MigrateAPICallsSchema() {
	cols := s.APICallColumns("main")
	if !cols["prompt_text"] {
		_, _ = s.db.Exec("ALTER TABLE api_calls ADD COLUMN prompt_text TEXT")
	}
	_, _ = s.db.Exec("CREATE INDEX IF NOT EXISTS idx_api_calls_prompt_text ON api_calls(prompt_text)")
}

func (s *Service) MigrateSessionWorkspacesSchema() {
	cols := s.SessionWorkspaceColumns("main")
	var pkCols sql.NullString
	_ = s.db.QueryRow(
		"SELECT group_concat(name, ',') FROM (" +
			"SELECT name FROM pragma_table_info('session_workspaces') WHERE pk > 0 ORDER BY pk" +
			")",
	).Scan(&pkCols)
	if !pkCols.Valid || pkCols.String != "session_id,source" {
		sourceExpr := "'local'"
		if cols["source"] {
			sourceExpr = "COALESCE(source, 'local')"
		}
		branchExpr := "NULL"
		if cols["branch"] {
			branchExpr = "branch"
		}
		_, _ = s.db.Exec("ALTER TABLE session_workspaces RENAME TO session_workspaces_old")
		_, _ = s.db.Exec(`CREATE TABLE session_workspaces (
    session_id TEXT NOT NULL,
    cwd TEXT NOT NULL,
    source TEXT DEFAULT 'local',
    branch TEXT,
    PRIMARY KEY (session_id, source)
)`)
		_, _ = s.db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source, branch) SELECT session_id, cwd, " + sourceExpr + ", " + branchExpr + " FROM session_workspaces_old")
		_, _ = s.db.Exec("DROP TABLE session_workspaces_old")
		cols = s.SessionWorkspaceColumns("main")
	}
	if !cols["branch"] {
		_, _ = s.db.Exec("ALTER TABLE session_workspaces ADD COLUMN branch TEXT")
	}
}

func (s *Service) SessionWorkspaceColumns(schema string) map[string]bool {
	cols := make(map[string]bool)
	pragma := "PRAGMA table_info(session_workspaces)"
	if schema != "" {
		pragma = fmt.Sprintf("PRAGMA %s.table_info(session_workspaces)", schema)
	}
	rows, err := s.db.Query(pragma)
	if err != nil {
		return cols
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err == nil {
			cols[name] = true
		}
	}
	return cols
}

func (s *Service) APICallColumns(schema string) map[string]bool {
	cols := make(map[string]bool)
	pragma := "PRAGMA table_info(api_calls)"
	if schema != "" {
		pragma = fmt.Sprintf("PRAGMA %s.table_info(api_calls)", schema)
	}
	rows, err := s.db.Query(pragma)
	if err != nil {
		return cols
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err == nil {
			cols[name] = true
		}
	}
	return cols
}

func (s *Service) IsLogParsed(logFile string, mtime float64, source string) bool {
	var n int
	err := s.db.QueryRow("SELECT 1 FROM parsed_logs WHERE log_file = ? AND source = ? AND mtime = ?",
		logFile, source, mtime).Scan(&n)
	return err == nil
}

func (s *Service) MarkLogParsed(logFile string, mtime float64, recordCount int, source string) {
	_, _ = s.db.Exec("INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) VALUES (?, ?, ?, ?, datetime('now'))",
		logFile, mtime, source, recordCount)
}

func (s *Service) InsertRecords(records []domain.Record, source string, normalizeModel func(string) string, promptTextForRecord func(*string) sql.NullString) {
	if len(records) == 0 {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	stmt, err := tx.Prepare(
		"INSERT OR IGNORE INTO api_calls " +
			"(model, model_normalized, prompt_tokens, completion_tokens, " +
			"prompt_text, cache_creation_tokens, cache_read_tokens, is_user_turn, " +
			"timestamp, session_id, log_file, source) " +
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		_ = tx.Rollback()
		return
	}
	defer stmt.Close()
	for _, r := range records {
		isUT := 0
		if r.IsUserTurn {
			isUT = 1
		}
		promptText := promptTextForRecord(r.PromptText)
		_, _ = stmt.Exec(r.Model, normalizeModel(r.Model), r.PromptTokens, r.CompletionTokens,
			promptText, r.CacheCreationTokens, r.CacheReadTokens, isUT,
			r.Timestamp, r.SessionID, r.LogFile, source)
	}
	_ = tx.Commit()
}

func (s *Service) UpsertSessionWorkspace(sessionID, cwd, branch, source string) {
	var branchValue interface{}
	if strings.TrimSpace(branch) != "" {
		branchValue = branch
	}
	_, _ = s.db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source, branch) VALUES (?, ?, ?, ?)",
		sessionID, cwd, source, branchValue)
}

func (s *Service) ClearSource(source string) {
	_, _ = s.db.Exec("DELETE FROM api_calls WHERE source = ?", source)
	_, _ = s.db.Exec("DELETE FROM parsed_logs WHERE source = ?", source)
	_, _ = s.db.Exec("DELETE FROM session_workspaces WHERE source = ?", source)
}

func (s *Service) GetCodespaceLastUsed(codespaceName string) string {
	var lastUsed sql.NullString
	err := s.db.QueryRow(
		"SELECT last_used_at FROM codespace_sync_state WHERE codespace_name = ?",
		codespaceName,
	).Scan(&lastUsed)
	if err != nil || !lastUsed.Valid {
		return ""
	}
	return lastUsed.String
}

func (s *Service) UpsertCodespaceSyncState(codespaceName string, lastUsedAt string) {
	_, _ = s.db.Exec(
		"INSERT OR REPLACE INTO codespace_sync_state (codespace_name, last_used_at, last_synced_at) VALUES (?, ?, datetime('now'))",
		codespaceName,
		lastUsedAt,
	)
}

func (s *Service) CountAPICallsBySource(source string) int {
	var count int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM api_calls WHERE source = ?", source).Scan(&count)
	return count
}

func (s *Service) DeleteParsedLogsBySource(source string) {
	_, _ = s.db.Exec("DELETE FROM parsed_logs WHERE source = ?", source)
}

func (s *Service) ParsedMtimeByFile(source string) map[string]float64 {
	parsedMtimeByFile := map[string]float64{}
	rows, err := s.db.Query("SELECT log_file, mtime FROM parsed_logs WHERE source = ?", source)
	if err != nil {
		return parsedMtimeByFile
	}
	defer rows.Close()
	for rows.Next() {
		var file string
		var mtime float64
		if err := rows.Scan(&file, &mtime); err == nil {
			parsedMtimeByFile[file] = mtime
		}
	}
	return parsedMtimeByFile
}

func (s *Service) KnownLogFiles(source string) map[string]bool {
	known := map[string]bool{}
	rows, err := s.db.Query("SELECT log_file FROM parsed_logs WHERE source = ?", source)
	if err != nil {
		return known
	}
	defer rows.Close()
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err == nil {
			known[f] = true
		}
	}
	return known
}

func (s *Service) BeginLogSyncTx(source string, normalizeModel func(string) string, promptTextForRecord func(*string) sql.NullString) (*LogSyncTx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	insertStmt, err := tx.Prepare(
		"INSERT INTO api_calls " +
			"(model, model_normalized, prompt_tokens, completion_tokens, " +
			"prompt_text, cache_creation_tokens, cache_read_tokens, is_user_turn, " +
			"timestamp, session_id, log_file, source) " +
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) " +
			"ON CONFLICT(timestamp, model, prompt_tokens, completion_tokens, log_file, source) DO UPDATE SET " +
			"prompt_text = CASE " +
			"WHEN COALESCE(api_calls.prompt_text, '') = '' AND COALESCE(excluded.prompt_text, '') <> '' THEN excluded.prompt_text " +
			"ELSE api_calls.prompt_text END, " +
			"session_id = CASE " +
			"WHEN COALESCE(excluded.session_id, '') <> '' THEN excluded.session_id " +
			"ELSE api_calls.session_id END")
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	parsedStmt, err := tx.Prepare("INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) VALUES (?, ?, ?, ?, datetime('now'))")
	if err != nil {
		_ = insertStmt.Close()
		_ = tx.Rollback()
		return nil, err
	}
	return &LogSyncTx{
		tx:                  tx,
		insertStmt:          insertStmt,
		parsedStmt:          parsedStmt,
		source:              source,
		normalizeModel:      normalizeModel,
		promptTextForRecord: promptTextForRecord,
	}, nil
}

func (t *LogSyncTx) InsertRecord(r domain.Record) {
	isUT := 0
	if r.IsUserTurn {
		isUT = 1
	}
	promptText := t.promptTextForRecord(r.PromptText)
	_, _ = t.insertStmt.Exec(r.Model, t.normalizeModel(r.Model), r.PromptTokens, r.CompletionTokens,
		promptText, r.CacheCreationTokens, r.CacheReadTokens, isUT,
		r.Timestamp, r.SessionID, r.LogFile, t.source)
}

func (t *LogSyncTx) MarkLogParsed(logFile string, mtime float64, recordCount int) {
	_, _ = t.parsedStmt.Exec(logFile, mtime, t.source, recordCount)
}

func (t *LogSyncTx) Commit() error {
	_ = t.insertStmt.Close()
	_ = t.parsedStmt.Close()
	if err := t.tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (t *LogSyncTx) Rollback() {
	if t == nil || t.tx == nil {
		return
	}
	_ = t.insertStmt.Close()
	_ = t.parsedStmt.Close()
	_ = t.tx.Rollback()
}
