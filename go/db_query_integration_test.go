package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	upsertSessionWorkspace(db, "s1", "/proj/alpha", "local")
	upsertSessionWorkspace(db, "s2", "/proj/beta", "local")
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
	insertRecords(srcDB, []Record{{
		Model:               "claude-3-5-sonnet",
		PromptTokens:        8,
		CompletionTokens:    9,
		CacheCreationTokens: 1,
		CacheReadTokens:     2,
		IsUserTurn:          true,
		Timestamp:           "2026-01-03T10:00:00",
		SessionID:           "sx",
		LogFile:             "x.log",
	}}, "local")
	upsertSessionWorkspace(srcDB, "sx", "/proj/export", "local")

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
	if ws["local\x1fsx"] != "/proj/export" {
		t.Fatalf("unexpected workspace map: %#v", ws)
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
	if overrideWS["codespace:unit\x1fsx"] != "/proj/export" {
		t.Fatalf("unexpected override workspaces: %#v", overrideWS)
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
	upsertSessionWorkspace(importDB, "s-import", "/proj/import", "local")
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
	if ws["remote\x1fs-import"] != "/proj/import" {
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
