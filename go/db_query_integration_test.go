package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTempDBForDBQuery(t *testing.T, name string) (*sql.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), name)
	db := initDB(dbPath)
	t.Cleanup(func() { _ = db.Close() })
	return db, dbPath
}

func TestInitDBCreatesSchema(t *testing.T) {
	db, _ := newTempDBForDBQuery(t, "main.db")

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('api_calls','parsed_logs','session_workspaces','codespace_sync_state')").Scan(&count)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if count != 4 {
		t.Fatalf("expected 4 core tables, got %d", count)
	}
	var hasPromptText int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('api_calls') WHERE name='prompt_text'").Scan(&hasPromptText); err != nil {
		t.Fatalf("prompt_text column check: %v", err)
	}
	if hasPromptText != 1 {
		t.Fatalf("expected prompt_text column to exist")
	}
	var hasPromptIndex int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_index_list('api_calls') WHERE name='idx_api_calls_prompt_text'").Scan(&hasPromptIndex); err != nil {
		t.Fatalf("prompt_text index check: %v", err)
	}
	if hasPromptIndex != 1 {
		t.Fatalf("expected idx_api_calls_prompt_text index to exist")
	}
}

func TestBuildFilters(t *testing.T) {
	where, params := buildFilters("", "", "")
	if where != "" || len(params) != 0 {
		t.Fatalf("expected empty filters, got where=%q params=%d", where, len(params))
	}

	where, params = buildFilters("2026-01-01", "2026-01-03", "  Alpha  ")
	if !strings.Contains(where, "a.timestamp >= ?") || !strings.Contains(where, "a.timestamp < ?") || !strings.Contains(where, "LIKE ?") {
		t.Fatalf("unexpected where clause: %q", where)
	}
	if len(params) != 3 {
		t.Fatalf("expected 3 params, got %d", len(params))
	}
	if params[0] != "2026-01-01" || params[1] != "2026-01-03" || params[2] != "%alpha%" {
		t.Fatalf("unexpected params: %#v", params)
	}
}

func seedQueryData(t *testing.T, db *sql.DB) {
	t.Helper()
	insertRecords(db, []Record{
		{
			Model:               "claude-3-5-sonnet",
			PromptTokens:        10,
			CompletionTokens:    20,
			CacheCreationTokens: 3,
			CacheReadTokens:     4,
			IsUserTurn:          true,
			Timestamp:           "2026-01-01T10:00:00",
			SessionID:           "s1",
			LogFile:             "a.log",
		},
		{
			Model:               "gpt-4.1",
			PromptTokens:        5,
			CompletionTokens:    7,
			CacheCreationTokens: 0,
			CacheReadTokens:     1,
			IsUserTurn:          false,
			Timestamp:           "2026-01-02T11:00:00",
			SessionID:           "s2",
			LogFile:             "b.log",
		},
	}, "local")
	upsertSessionWorkspace(db, "s1", "/proj/alpha", "", "local")
	upsertSessionWorkspace(db, "s2", "/proj/beta", "", "local")
}

func TestQueryFunctionsWithRealSQLite(t *testing.T) {
	db, _ := newTempDBForDBQuery(t, "query.db")
	seedQueryData(t, db)

	modelStats := queryModelStats(db, "", "", "")
	if len(modelStats) != 2 {
		t.Fatalf("expected 2 models, got %d", len(modelStats))
	}
	if modelStats["claude-3-5-sonnet"].APICalls != 1 || modelStats["claude-3-5-sonnet"].UserTurns != 1 {
		t.Fatalf("unexpected claude stats: %+v", *modelStats["claude-3-5-sonnet"])
	}

	daily := queryDailyStats(db, "", "", "")
	if daily["2026-01-01"]["claude-3-5-sonnet"].PromptTokens != 10 {
		t.Fatalf("unexpected daily stats for 2026-01-01")
	}
	if daily["2026-01-02"]["gpt-4.1"].CompletionTokens != 7 {
		t.Fatalf("unexpected daily stats for 2026-01-02")
	}

	projects := queryProjectStats(db, "", "", "")
	if projects["/proj/alpha"].APICalls != 1 || projects["/proj/beta"].APICalls != 1 {
		t.Fatalf("unexpected project stats: %#v", projects)
	}

	records := queryRecords(db, "", "", "alpha")
	if len(records) != 1 {
		t.Fatalf("expected 1 alpha record, got %d", len(records))
	}
	if records[0].SessionID != "s1" || records[0].Source != "local" {
		t.Fatalf("unexpected filtered record: %+v", records[0])
	}

	if got := queryLogFileCount(db, "", "", ""); got != 2 {
		t.Fatalf("expected 2 distinct log files, got %d", got)
	}
	if got := queryLogFileCount(db, "2026-01-02", "", ""); got != 1 {
		t.Fatalf("expected 1 distinct log file after date filter, got %d", got)
	}
}

func TestExportImportJSONL(t *testing.T) {
	srcDB, _ := newTempDBForDBQuery(t, "src.db")
	prompt := "export this prompt"
	insertRecords(srcDB, []Record{{
		Model:               "claude-3-5-sonnet",
		PromptTokens:        8,
		CompletionTokens:    9,
		PromptText:          &prompt,
		CacheCreationTokens: 1,
		CacheReadTokens:     2,
		IsUserTurn:          true,
		Timestamp:           "2026-01-03T10:00:00",
		SessionID:           "sx",
		LogFile:             "x.log",
	}}, "local")
	upsertSessionWorkspace(srcDB, "sx", "/proj/export", "feature/export", "local")

	jsonlPath := filepath.Join(t.TempDir(), "export.jsonl")
	exportJSONL(srcDB, jsonlPath)

	if _, err := os.Stat(jsonlPath); err != nil {
		t.Fatalf("exported file missing: %v", err)
	}

	dstDB, _ := newTempDBForDBQuery(t, "dst.db")
	imported := importJSONL(dstDB, jsonlPath, "")
	if imported != 2 {
		t.Fatalf("expected 2 imported jsonl entries, got %d", imported)
	}

	records := queryRecords(dstDB, "", "", "")
	if len(records) != 1 || records[0].Source != "local" {
		t.Fatalf("unexpected imported records: %#v", records)
	}
	ws := querySessionWorkspaces(dstDB)
	if ws["local\x1fsx"].CWD != "/proj/export" || ws["local\x1fsx"].Branch != "feature/export" {
		t.Fatalf("unexpected workspace map: %#v", ws)
	}
	var importedPrompt sql.NullString
	if err := dstDB.QueryRow("SELECT prompt_text FROM api_calls WHERE session_id = 'sx' AND source = 'local'").Scan(&importedPrompt); err != nil {
		t.Fatalf("read imported prompt_text: %v", err)
	}
	if !importedPrompt.Valid || importedPrompt.String != prompt {
		t.Fatalf("unexpected imported prompt_text: %#v", importedPrompt)
	}

	overrideDB, _ := newTempDBForDBQuery(t, "override.db")
	if got := importJSONL(overrideDB, jsonlPath, "codespace:unit"); got != 2 {
		t.Fatalf("expected 2 imported with override, got %d", got)
	}
	overrideRecords := queryRecords(overrideDB, "", "", "")
	if len(overrideRecords) != 1 || overrideRecords[0].Source != "codespace:unit" {
		t.Fatalf("unexpected override records: %#v", overrideRecords)
	}
	overrideWS := querySessionWorkspaces(overrideDB)
	if overrideWS["codespace:unit\x1fsx"].CWD != "/proj/export" || overrideWS["codespace:unit\x1fsx"].Branch != "feature/export" {
		t.Fatalf("unexpected override workspaces: %#v", overrideWS)
	}
}

func TestExportImportJSONLFromLegacySchemaWithoutPromptText(t *testing.T) {
	legacyDB, _ := newTempDBForDBQuery(t, "legacy-jsonl.db")
	insertRecords(legacyDB, []Record{{
		Model:            "gpt-4.1",
		PromptTokens:     4,
		CompletionTokens: 2,
		Timestamp:        "2026-01-03T11:00:00",
		SessionID:        "legacy-export",
		LogFile:          "legacy-export.log",
	}}, "local")
	if _, err := legacyDB.Exec(`
ALTER TABLE api_calls RENAME TO api_calls_with_prompt_text;
CREATE TABLE api_calls (
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
INSERT INTO api_calls (id, model, model_normalized, prompt_tokens, completion_tokens, cache_creation_tokens, cache_read_tokens, is_user_turn, timestamp, session_id, log_file, source)
SELECT id, model, model_normalized, prompt_tokens, completion_tokens, cache_creation_tokens, cache_read_tokens, is_user_turn, timestamp, session_id, log_file, source
FROM api_calls_with_prompt_text;
DROP TABLE api_calls_with_prompt_text;
`); err != nil {
		t.Fatalf("strip prompt_text column: %v", err)
	}

	jsonlPath := filepath.Join(t.TempDir(), "legacy-export.jsonl")
	exportJSONL(legacyDB, jsonlPath)

	targetDB, _ := newTempDBForDBQuery(t, "legacy-jsonl-target.db")
	if imported := importJSONL(targetDB, jsonlPath, ""); imported != 1 {
		t.Fatalf("expected 1 imported jsonl entry, got %d", imported)
	}
	var promptText sql.NullString
	if err := targetDB.QueryRow("SELECT prompt_text FROM api_calls WHERE session_id='legacy-export' AND source='local'").Scan(&promptText); err != nil {
		t.Fatalf("query imported prompt_text: %v", err)
	}
	if promptText.Valid {
		t.Fatalf("expected NULL prompt_text for legacy export/import, got %#v", promptText)
	}
}

func TestImportSQLiteDBWithOverride(t *testing.T) {
	importDB, importPath := newTempDBForDBQuery(t, "import.db")
	insertRecords(importDB, []Record{{
		Model:               "gpt-4.1",
		PromptTokens:        3,
		CompletionTokens:    4,
		CacheCreationTokens: 0,
		CacheReadTokens:     1,
		IsUserTurn:          false,
		Timestamp:           "2026-01-04T09:00:00",
		SessionID:           "s-import",
		LogFile:             "import.log",
	}}, "local")
	upsertSessionWorkspace(importDB, "s-import", "/proj/import", "feature/import", "local")
	markLogParsed(importDB, "import.log", 123.45, 1, "local")

	targetDB, _ := newTempDBForDBQuery(t, "target.db")
	changes := importSQLiteDB(targetDB, importPath, "remote")
	if changes != 1 {
		t.Fatalf("expected changes() from last insert to be 1, got %d", changes)
	}

	records := queryRecords(targetDB, "", "", "")
	if len(records) != 1 || records[0].Source != "remote" {
		t.Fatalf("unexpected imported records: %#v", records)
	}
	ws := querySessionWorkspaces(targetDB)
	if ws["remote\x1fs-import"].CWD != "/proj/import" || ws["remote\x1fs-import"].Branch != "feature/import" {
		t.Fatalf("unexpected imported workspaces: %#v", ws)
	}

	var parsedCount int
	if err := targetDB.QueryRow("SELECT COUNT(*) FROM parsed_logs WHERE source = 'remote'").Scan(&parsedCount); err != nil {
		t.Fatalf("count parsed_logs: %v", err)
	}
	if parsedCount != 1 {
		t.Fatalf("expected 1 parsed_log row with override source, got %d", parsedCount)
	}
}

func TestMigrateSessionWorkspacesAddsBranchColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	_, err = legacyDB.Exec(`
CREATE TABLE session_workspaces (
    session_id TEXT NOT NULL,
    cwd TEXT NOT NULL,
    source TEXT DEFAULT 'local',
    PRIMARY KEY (session_id, source)
);
INSERT INTO session_workspaces (session_id, cwd, source) VALUES ('legacy-sid', '/legacy/cwd', 'local');
`)
	if err != nil {
		t.Fatalf("prepare legacy db: %v", err)
	}
	_ = legacyDB.Close()

	db := initDB(dbPath)
	t.Cleanup(func() { _ = db.Close() })

	var hasBranch int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('session_workspaces') WHERE name='branch'").Scan(&hasBranch); err != nil {
		t.Fatalf("branch column check: %v", err)
	}
	if hasBranch != 1 {
		t.Fatalf("expected branch column to exist")
	}
	var cwd, source string
	var branch sql.NullString
	if err := db.QueryRow("SELECT cwd, source, branch FROM session_workspaces WHERE session_id='legacy-sid'").Scan(&cwd, &source, &branch); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if cwd != "/legacy/cwd" || source != "local" || branch.Valid {
		t.Fatalf("unexpected migrated row cwd=%q source=%q branch=%#v", cwd, source, branch)
	}

	migrateSessionWorkspacesSchema(db)
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('session_workspaces') WHERE name='branch'").Scan(&hasBranch); err != nil {
		t.Fatalf("branch column re-check: %v", err)
	}
	if hasBranch != 1 {
		t.Fatalf("expected branch column to remain after re-migration")
	}
}

func TestMigrateAPICallsAddsPromptTextColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-api-calls.db")
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	_, err = legacyDB.Exec(`
CREATE TABLE api_calls (
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
INSERT INTO api_calls (model, model_normalized, prompt_tokens, completion_tokens, timestamp, session_id, log_file, source)
VALUES ('gpt-4.1', 'gpt-4.1', 2, 1, '2026-01-01T00:00:00', 'legacy', 'legacy.log', 'local');
`)
	if err != nil {
		t.Fatalf("prepare legacy db: %v", err)
	}
	_ = legacyDB.Close()

	db := initDB(dbPath)
	t.Cleanup(func() { _ = db.Close() })

	var hasPromptText int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('api_calls') WHERE name='prompt_text'").Scan(&hasPromptText); err != nil {
		t.Fatalf("prompt_text column check: %v", err)
	}
	if hasPromptText != 1 {
		t.Fatalf("expected prompt_text column to exist")
	}
	var promptText sql.NullString
	if err := db.QueryRow("SELECT prompt_text FROM api_calls WHERE session_id='legacy'").Scan(&promptText); err != nil {
		t.Fatalf("read migrated api_call row: %v", err)
	}
	if promptText.Valid {
		t.Fatalf("expected prompt_text to be NULL after migration, got %#v", promptText)
	}

	migrateAPICallsSchema(db)
	var hasPromptIndex int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_index_list('api_calls') WHERE name='idx_api_calls_prompt_text'").Scan(&hasPromptIndex); err != nil {
		t.Fatalf("prompt_text index check: %v", err)
	}
	if hasPromptIndex != 1 {
		t.Fatalf("expected idx_api_calls_prompt_text index to remain after re-migration")
	}
}

func TestImportSQLiteDBWithoutBranchColumn(t *testing.T) {
	importDB, importPath := newTempDBForDBQuery(t, "legacy-import.db")
	insertRecords(importDB, []Record{{
		Model:            "gpt-4.1",
		PromptTokens:     1,
		CompletionTokens: 2,
		Timestamp:        "2026-01-05T12:00:00",
		SessionID:        "legacy-import",
		LogFile:          "legacy.log",
	}}, "local")
	upsertSessionWorkspace(importDB, "legacy-import", "/proj/legacy-import", "legacy/branch", "local")
	if _, err := importDB.Exec(`
ALTER TABLE session_workspaces RENAME TO session_workspaces_with_branch;
CREATE TABLE session_workspaces (
    session_id TEXT NOT NULL,
    cwd TEXT NOT NULL,
    source TEXT DEFAULT 'local',
    PRIMARY KEY (session_id, source)
);
INSERT INTO session_workspaces (session_id, cwd, source)
SELECT session_id, cwd, source FROM session_workspaces_with_branch;
DROP TABLE session_workspaces_with_branch;
`); err != nil {
		t.Fatalf("strip branch column: %v", err)
	}
	_ = importDB.Close()

	targetDB, _ := newTempDBForDBQuery(t, "target-no-branch.db")
	_ = importSQLiteDB(targetDB, importPath, "")

	ws := querySessionWorkspaces(targetDB)
	if ws["local\x1flegacy-import"].CWD != "/proj/legacy-import" {
		t.Fatalf("unexpected imported legacy workspace map: %#v", ws)
	}
	if ws["local\x1flegacy-import"].Branch != "" {
		t.Fatalf("expected empty branch for legacy import, got %#v", ws["local\x1flegacy-import"])
	}
}

func TestImportSQLiteDBWithoutPromptTextColumn(t *testing.T) {
	importDB, importPath := newTempDBForDBQuery(t, "legacy-no-prompt.db")
	insertRecords(importDB, []Record{{
		Model:            "gpt-4.1",
		PromptTokens:     1,
		CompletionTokens: 2,
		Timestamp:        "2026-01-05T12:00:00",
		SessionID:        "legacy-no-prompt",
		LogFile:          "legacy-no-prompt.log",
	}}, "local")
	if _, err := importDB.Exec(`
ALTER TABLE api_calls RENAME TO api_calls_with_prompt_text;
CREATE TABLE api_calls (
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
INSERT INTO api_calls (id, model, model_normalized, prompt_tokens, completion_tokens, cache_creation_tokens, cache_read_tokens, is_user_turn, timestamp, session_id, log_file, source)
SELECT id, model, model_normalized, prompt_tokens, completion_tokens, cache_creation_tokens, cache_read_tokens, is_user_turn, timestamp, session_id, log_file, source
FROM api_calls_with_prompt_text;
DROP TABLE api_calls_with_prompt_text;
`); err != nil {
		t.Fatalf("strip prompt_text column: %v", err)
	}
	_ = importDB.Close()

	targetDB, _ := newTempDBForDBQuery(t, "target-no-prompt-text.db")
	_ = importSQLiteDB(targetDB, importPath, "")

	var promptText sql.NullString
	if err := targetDB.QueryRow("SELECT prompt_text FROM api_calls WHERE session_id='legacy-no-prompt' AND source='local'").Scan(&promptText); err != nil {
		t.Fatalf("query imported prompt_text: %v", err)
	}
	if promptText.Valid {
		t.Fatalf("expected NULL prompt_text for legacy import, got %#v", promptText)
	}
}

func TestImportSQLiteDBWithPromptTextColumn(t *testing.T) {
	importDB, importPath := newTempDBForDBQuery(t, "import-with-prompt.db")
	if _, err := importDB.Exec(`INSERT INTO api_calls
	(model, model_normalized, prompt_tokens, completion_tokens, cache_creation_tokens, cache_read_tokens, is_user_turn, timestamp, session_id, log_file, source, prompt_text)
	VALUES ('gpt-4.1', 'gpt-4.1', 5, 3, 0, 0, 1, '2026-01-05T13:00:00', 'with-prompt', 'with-prompt.log', 'local', 'Write a hello world in Go')`); err != nil {
		t.Fatalf("insert api_call with prompt_text: %v", err)
	}
	_ = importDB.Close()

	targetDB, _ := newTempDBForDBQuery(t, "target-with-prompt-text.db")
	_ = importSQLiteDB(targetDB, importPath, "")

	var promptText sql.NullString
	if err := targetDB.QueryRow("SELECT prompt_text FROM api_calls WHERE session_id='with-prompt' AND source='local'").Scan(&promptText); err != nil {
		t.Fatalf("query imported prompt_text: %v", err)
	}
	if !promptText.Valid || promptText.String != "Write a hello world in Go" {
		t.Fatalf("unexpected imported prompt_text: %#v", promptText)
	}
}

func TestSyncLogsToDBWithLegacyWorkspaceSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-sync.db")
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
CREATE TABLE session_workspaces (
    session_id TEXT NOT NULL,
    cwd TEXT NOT NULL,
    source TEXT DEFAULT 'local',
    PRIMARY KEY (session_id, source)
);`); err != nil {
		t.Fatalf("prepare legacy workspace table: %v", err)
	}
	_ = legacyDB.Close()

	db := initDB(dbPath)
	t.Cleanup(func() { _ = db.Close() })

	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	sessionID := "123e4567-e89b-12d3-a456-426614174333"
	workspaceDir := filepath.Join(sessionDir, sessionID)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "workspace.yaml"), []byte("cwd: /tmp/legacy-sync\nbranch: feature/legacy-sync\n"), 0o644); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	logPath := filepath.Join(logsDir, "process-legacy.log")
	logTS := time.Date(2026, 1, 6, 9, 0, 0, 0, time.Local)
	writeLogFile(t, logPath, logTS, "2026-01-06T09:00:00 Workspace initialized: "+sessionID+"\nPremiumRequestProcessor: Setting X-Initiator to 'user'\n{\"model\":\"gpt-4.1\"}\n{\"prompt_tokens\":6,\"completion_tokens\":2}\n")

	if inserted := syncLogsToDB(db, logsDir, sessionDir, false, "legacy-sync", nil, nil); inserted != 1 {
		t.Fatalf("syncLogsToDB inserted=%d, want 1", inserted)
	}

	var branch sql.NullString
	if err := db.QueryRow("SELECT branch FROM session_workspaces WHERE session_id = ? AND source = ?", sessionID, "legacy-sync").Scan(&branch); err != nil {
		t.Fatalf("query synced branch: %v", err)
	}
	if !branch.Valid || branch.String != "feature/legacy-sync" {
		t.Fatalf("unexpected synced branch: %#v", branch)
	}
}
