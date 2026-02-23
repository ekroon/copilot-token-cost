package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeLogFile(t *testing.T, path string, ts time.Time, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("set log mtime: %v", err)
	}
}

func tempDBPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "test.db")
}

func TestSyncLogsToDBMinMaxAndWorkspace(t *testing.T) {
	dbPath := tempDBPath(t)
	db := initDB(dbPath)
	defer db.Close()

	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	sessionID := "123e4567-e89b-12d3-a456-426614174111"
	workspaceDir := filepath.Join(sessionDir, sessionID)
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "workspace.yaml"), []byte("cwd: /tmp/demo-project\nbranch: feature/sync-test\n"), 0644); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	oldTS := time.Date(2025, 1, 1, 10, 0, 0, 0, time.Local)
	newTS := time.Date(2025, 1, 2, 10, 0, 0, 0, time.Local)
	oldLog := filepath.Join(logsDir, "process-old.log")
	newLog := filepath.Join(logsDir, "process-new.log")
	base := "Workspace initialized: 123e4567-e89b-12d3-a456-426614174111\nPremiumRequestProcessor: Setting X-Initiator to 'user'\n{\"model\":\"gpt-4.1\"}\n{\"prompt_tokens\":10,\"completion_tokens\":4}\n"
	writeLogFile(t, oldLog, oldTS, "2025-01-01T10:00:00 "+base)
	writeLogFile(t, newLog, newTS, "2025-01-02T10:00:00 "+base)

	minTime := oldTS.Add(12 * time.Hour)
	maxTime := newTS.Add(1 * time.Hour)
	inserted := syncLogsToDB(db, logsDir, sessionDir, false, "local-test", &minTime, &maxTime)
	if inserted != 1 {
		t.Fatalf("syncLogsToDB inserted=%d, want 1", inserted)
	}

	var apiCount, parsedCount, wsCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM api_calls WHERE source = ?", "local-test").Scan(&apiCount); err != nil {
		t.Fatalf("count api_calls: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM parsed_logs WHERE source = ?", "local-test").Scan(&parsedCount); err != nil {
		t.Fatalf("count parsed_logs: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM session_workspaces WHERE source = ?", "local-test").Scan(&wsCount); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	if apiCount != 1 || parsedCount != 1 || wsCount != 1 {
		t.Fatalf("counts mismatch api=%d parsed=%d workspaces=%d", apiCount, parsedCount, wsCount)
	}
	var branch sql.NullString
	if err := db.QueryRow("SELECT branch FROM session_workspaces WHERE session_id = ? AND source = ?", sessionID, "local-test").Scan(&branch); err != nil {
		t.Fatalf("query workspace branch: %v", err)
	}
	if !branch.Valid || branch.String != "feature/sync-test" {
		t.Fatalf("unexpected branch value: %#v", branch)
	}
}

func TestSyncLogsToDBSkipsParsedAndForce(t *testing.T) {
	dbPath := tempDBPath(t)
	db := initDB(dbPath)
	defer db.Close()

	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	ts := time.Date(2025, 1, 3, 10, 0, 0, 0, time.Local)
	logPath := filepath.Join(logsDir, "process-once.log")
	content := "2025-01-03T10:00:00 Workspace initialized: 123e4567-e89b-12d3-a456-426614174222\nPremiumRequestProcessor: Setting X-Initiator to 'user'\n{\"model\":\"gpt-4.1\"}\n{\"prompt_tokens\":20,\"completion_tokens\":8}\n"
	writeLogFile(t, logPath, ts, content)

	first := syncLogsToDB(db, logsDir, sessionDir, false, "local-test-2", nil, nil)
	second := syncLogsToDB(db, logsDir, sessionDir, false, "local-test-2", nil, nil)
	forced := syncLogsToDB(db, logsDir, sessionDir, true, "local-test-2", nil, nil)
	if first != 1 || second != 0 || forced != 1 {
		t.Fatalf("unexpected sync counts first=%d second=%d forced=%d", first, second, forced)
	}

	var apiCount, parsedCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM api_calls WHERE source = ?", "local-test-2").Scan(&apiCount); err != nil {
		t.Fatalf("count api_calls: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM parsed_logs WHERE source = ?", "local-test-2").Scan(&parsedCount); err != nil {
		t.Fatalf("count parsed_logs: %v", err)
	}
	if apiCount != 1 || parsedCount != 1 {
		t.Fatalf("counts mismatch api=%d parsed=%d", apiCount, parsedCount)
	}
}

func TestSyncLogsToDBPersistsPromptTextWhenAvailable(t *testing.T) {
	dbPath := tempDBPath(t)
	db := initDB(dbPath)
	defer db.Close()

	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}

	withPrompt := filepath.Join(logsDir, "process-with-prompt.log")
	withoutPrompt := filepath.Join(logsDir, "process-without-prompt.log")
	ts := time.Date(2025, 1, 4, 10, 0, 0, 0, time.Local)

	withPromptContent := "2025-01-04T10:00:00 {\"model\":\"gpt-4.1\"}\n" +
		"2025-01-04T10:00:01 {\"messages\":[{\"role\":\"user\",\"content\":\"  persist this prompt  \"}]}\n" +
		"2025-01-04T10:00:02 {\"prompt_tokens\":12,\"completion_tokens\":5}\n"
	withoutPromptContent := "2025-01-04T10:00:10 {\"model\":\"gpt-4.1\"}\n" +
		"2025-01-04T10:00:11 {\"prompt_tokens\":8,\"completion_tokens\":3}\n"

	writeLogFile(t, withPrompt, ts, withPromptContent)
	writeLogFile(t, withoutPrompt, ts, withoutPromptContent)

	if inserted := syncLogsToDB(db, logsDir, sessionDir, false, "prompt-sync", nil, nil); inserted != 2 {
		t.Fatalf("syncLogsToDB inserted=%d, want 2", inserted)
	}

	var withPromptText sql.NullString
	if err := db.QueryRow("SELECT prompt_text FROM api_calls WHERE source = ? AND log_file = ?", "prompt-sync", "process-with-prompt.log").Scan(&withPromptText); err != nil {
		t.Fatalf("query prompt_text with prompt: %v", err)
	}
	if !withPromptText.Valid || withPromptText.String != "persist this prompt" {
		t.Fatalf("unexpected persisted prompt text: %#v", withPromptText)
	}

	var withoutPromptText sql.NullString
	if err := db.QueryRow("SELECT prompt_text FROM api_calls WHERE source = ? AND log_file = ?", "prompt-sync", "process-without-prompt.log").Scan(&withoutPromptText); err != nil {
		t.Fatalf("query prompt_text without prompt: %v", err)
	}
	if withoutPromptText.Valid {
		t.Fatalf("expected NULL prompt_text when unavailable, got %#v", withoutPromptText)
	}
}

func TestSyncLogsToDBSkipsSymlinkedProcessLogs(t *testing.T) {
	dbPath := tempDBPath(t)
	db := initDB(dbPath)
	defer db.Close()

	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	realLog := filepath.Join(logsDir, "process-real.log")
	content := "2025-01-03T10:00:00 Workspace initialized: 123e4567-e89b-12d3-a456-426614174222\nPremiumRequestProcessor: Setting X-Initiator to 'user'\n{\"model\":\"gpt-4.1\"}\n{\"prompt_tokens\":20,\"completion_tokens\":8}\n"
	writeLogFile(t, realLog, time.Date(2025, 1, 3, 10, 0, 0, 0, time.Local), content)
	target := filepath.Join(logsDir, "other.log")
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(logsDir, "process-link.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	inserted := syncLogsToDB(db, logsDir, sessionDir, false, "local-symlink", nil, nil)
	if inserted != 1 {
		t.Fatalf("syncLogsToDB inserted=%d, want=1 (symlink skipped)", inserted)
	}
	if countRowsSyncUtils(t, db, "SELECT COUNT(*) FROM parsed_logs WHERE source='local-symlink'") != 1 {
		t.Fatalf("expected only real log to be parsed")
	}
}

func countRowsSyncUtils(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var c int
	if err := db.QueryRow(q).Scan(&c); err != nil {
		t.Fatalf("query row count: %v", err)
	}
	return c
}

func TestSyncLogsToDBBackfillsMissingPromptTextIdempotently(t *testing.T) {
	dbPath := tempDBPath(t)
	db := initDB(dbPath)
	defer db.Close()

	for _, source := range []string{"local-backfill", "codespace:demo-backfill"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			logsDir := filepath.Join(root, "logs")
			sessionDir := filepath.Join(root, "session-state")
			if err := os.MkdirAll(logsDir, 0o755); err != nil {
				t.Fatalf("mkdir logs: %v", err)
			}

			ts := time.Date(2025, 1, 5, 10, 0, 0, 0, time.Local)
			logPath := filepath.Join(logsDir, "process-backfill.log")
			content := "2025-01-05T10:00:00 {\"model\":\"gpt-4.1\"}\n" +
				"2025-01-05T10:00:01 {\"messages\":[{\"role\":\"user\",\"content\":\"  backfill this prompt  \"}]}\n" +
				"2025-01-05T10:00:02 {\"prompt_tokens\":30,\"completion_tokens\":7}\n"
			writeLogFile(t, logPath, ts, content)

			if _, err := db.Exec(
				"INSERT INTO api_calls (model, model_normalized, prompt_tokens, completion_tokens, prompt_text, cache_creation_tokens, cache_read_tokens, is_user_turn, timestamp, session_id, log_file, source) VALUES (?, ?, ?, ?, NULL, ?, ?, 0, ?, ?, ?, ?)",
				"gpt-4.1", normalizeModel("gpt-4.1"), 30, 7, 99, 88, "2025-01-05T10:00:02", "session-backfill", "process-backfill.log", source,
			); err != nil {
				t.Fatalf("seed api_calls row: %v", err)
			}

			if inserted := syncLogsToDB(db, logsDir, sessionDir, true, source, nil, nil); inserted != 1 {
				t.Fatalf("syncLogsToDB inserted=%d, want 1", inserted)
			}
			if inserted := syncLogsToDB(db, logsDir, sessionDir, true, source, nil, nil); inserted != 1 {
				t.Fatalf("second syncLogsToDB inserted=%d, want 1", inserted)
			}

			var count, cacheCreate, cacheRead int
			var promptText sql.NullString
			if err := db.QueryRow("SELECT COUNT(*), prompt_text, cache_creation_tokens, cache_read_tokens FROM api_calls WHERE source = ? AND log_file = ?", source, "process-backfill.log").Scan(&count, &promptText, &cacheCreate, &cacheRead); err != nil {
				t.Fatalf("query backfilled row: %v", err)
			}
			if count != 1 {
				t.Fatalf("expected exactly 1 row after backfill, got %d", count)
			}
			if !promptText.Valid || promptText.String != "backfill this prompt" {
				t.Fatalf("unexpected backfilled prompt_text: %#v", promptText)
			}
			if cacheCreate != 99 || cacheRead != 88 {
				t.Fatalf("expected token fields unchanged, got cache_creation=%d cache_read=%d", cacheCreate, cacheRead)
			}
		})
	}
}

func TestSyncLogsToDBForceResyncUpdatesSessionIDOnConflict(t *testing.T) {
	dbPath := tempDBPath(t)
	db := initDB(dbPath)
	defer db.Close()

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
	if err := os.WriteFile(filepath.Join(workspaceDir, "workspace.yaml"), []byte("cwd: /tmp/reassign-project\n"), 0o644); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	ts := time.Date(2025, 1, 8, 10, 0, 0, 0, time.Local)
	logPath := filepath.Join(logsDir, "process-session-reassign.log")
	content := "2025-01-08T10:00:00 Workspace initialized: " + sessionID + "\n" +
		"2025-01-08T10:00:01 PremiumRequestProcessor: Setting X-Initiator to 'user'\n" +
		"2025-01-08T10:00:02 {\"model\":\"gpt-4.1\"}\n" +
		"2025-01-08T10:00:03 {\"prompt_tokens\":42,\"completion_tokens\":9}\n"
	writeLogFile(t, logPath, ts, content)

	placeholderSessionID := "123e4567-e89b-12d3-a456-426614174000"
	if _, err := db.Exec(
		"INSERT INTO api_calls (model, model_normalized, prompt_tokens, completion_tokens, prompt_text, cache_creation_tokens, cache_read_tokens, is_user_turn, timestamp, session_id, log_file, source) VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?)",
		"gpt-4.1", normalizeModel("gpt-4.1"), 42, 9, 0, 0, 1, "2025-01-08T10:00:03", placeholderSessionID, "process-session-reassign.log", "local-reassign",
	); err != nil {
		t.Fatalf("seed api_calls row: %v", err)
	}

	if inserted := syncLogsToDB(db, logsDir, sessionDir, true, "local-reassign", nil, nil); inserted != 1 {
		t.Fatalf("syncLogsToDB inserted=%d, want 1", inserted)
	}

	var count int
	var gotSessionID string
	if err := db.QueryRow(
		"SELECT COUNT(*), COALESCE(MAX(session_id), '') FROM api_calls WHERE source = ? AND log_file = ?",
		"local-reassign", "process-session-reassign.log",
	).Scan(&count, &gotSessionID); err != nil {
		t.Fatalf("query synced row: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row after force re-sync, got %d", count)
	}
	if gotSessionID != sessionID {
		t.Fatalf("expected session_id=%q after conflict update, got %q", sessionID, gotSessionID)
	}
}

func TestSyncLogsToDBBackfillsMissingPromptTextFromUserStatement(t *testing.T) {
	dbPath := tempDBPath(t)
	db := initDB(dbPath)
	defer db.Close()

	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}

	ts := time.Date(2025, 1, 7, 10, 0, 0, 0, time.Local)
	logPath := filepath.Join(logsDir, "process-statement-backfill.log")
	content := "2025-01-07T10:00:00 {\"model\":\"gpt-4.1\"}\n" +
		"2025-01-07T10:00:01 {\n" +
		"  \"problem\": {\n" +
		"    \"statement\": \"  backfill statement prompt  \"\n" +
		"  }\n" +
		"}\n"
	for i := 0; i < 30; i++ {
		content += "2025-01-07T10:00:01 filler line\n"
	}
	content += "2025-01-07T10:00:02 {\"prompt_tokens\":30,\"completion_tokens\":7}\n"
	writeLogFile(t, logPath, ts, content)

	if _, err := db.Exec(
		"INSERT INTO api_calls (model, model_normalized, prompt_tokens, completion_tokens, prompt_text, cache_creation_tokens, cache_read_tokens, is_user_turn, timestamp, session_id, log_file, source) VALUES (?, ?, ?, ?, NULL, ?, ?, 0, ?, ?, ?, ?)",
		"gpt-4.1", normalizeModel("gpt-4.1"), 30, 7, 99, 88, "2025-01-07T10:00:02", "session-statement-backfill", "process-statement-backfill.log", "local-statement-backfill",
	); err != nil {
		t.Fatalf("seed api_calls row: %v", err)
	}

	if inserted := syncLogsToDB(db, logsDir, sessionDir, true, "local-statement-backfill", nil, nil); inserted != 1 {
		t.Fatalf("syncLogsToDB inserted=%d, want 1", inserted)
	}
	if inserted := syncLogsToDB(db, logsDir, sessionDir, true, "local-statement-backfill", nil, nil); inserted != 1 {
		t.Fatalf("second syncLogsToDB inserted=%d, want 1", inserted)
	}

	var count int
	var promptText sql.NullString
	if err := db.QueryRow("SELECT COUNT(*), prompt_text FROM api_calls WHERE source = ? AND log_file = ?", "local-statement-backfill", "process-statement-backfill.log").Scan(&count, &promptText); err != nil {
		t.Fatalf("query statement backfill row: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row after statement backfill, got %d", count)
	}
	if !promptText.Valid || promptText.String != "backfill statement prompt" {
		t.Fatalf("unexpected statement backfilled prompt_text: %#v", promptText)
	}
}

func TestSyncLogsToDBDoesNotOverwriteExistingPromptText(t *testing.T) {
	dbPath := tempDBPath(t)
	db := initDB(dbPath)
	defer db.Close()

	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}

	ts := time.Date(2025, 1, 6, 10, 0, 0, 0, time.Local)
	logPath := filepath.Join(logsDir, "process-no-overwrite.log")
	content := "2025-01-06T10:00:00 {\"model\":\"gpt-4.1\"}\n" +
		"2025-01-06T10:00:01 {\"messages\":[{\"role\":\"user\",\"content\":\"new prompt from log\"}]}\n" +
		"2025-01-06T10:00:02 {\"prompt_tokens\":40,\"completion_tokens\":9}\n"
	writeLogFile(t, logPath, ts, content)

	if _, err := db.Exec(
		"INSERT INTO api_calls (model, model_normalized, prompt_tokens, completion_tokens, prompt_text, cache_creation_tokens, cache_read_tokens, is_user_turn, timestamp, session_id, log_file, source) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)",
		"gpt-4.1", normalizeModel("gpt-4.1"), 40, 9, "keep existing prompt", 21, 13, "2025-01-06T10:00:02", "session-no-overwrite", "process-no-overwrite.log", "local-no-overwrite",
	); err != nil {
		t.Fatalf("seed api_calls row: %v", err)
	}

	if inserted := syncLogsToDB(db, logsDir, sessionDir, true, "local-no-overwrite", nil, nil); inserted != 1 {
		t.Fatalf("syncLogsToDB inserted=%d, want 1", inserted)
	}

	var count int
	var promptText string
	if err := db.QueryRow("SELECT COUNT(*), prompt_text FROM api_calls WHERE source = ? AND log_file = ?", "local-no-overwrite", "process-no-overwrite.log").Scan(&count, &promptText); err != nil {
		t.Fatalf("query preserved prompt_text: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row after sync, got %d", count)
	}
	if promptText != "keep existing prompt" {
		t.Fatalf("expected existing prompt_text to be preserved, got %q", promptText)
	}
}

func TestProjectName(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	obsidian := filepath.Join(home, "Library/Mobile Documents/iCloud~md~obsidian/Documents/MyVault")
	if got := projectName(obsidian); got != "📓 MyVault" {
		t.Fatalf("projectName obsidian=%q, want %q", got, "📓 MyVault")
	}

	regular := filepath.Join(home, "work/repo")
	if got := projectName(regular); got != "~/work/repo" {
		t.Fatalf("projectName regular=%q, want %q", got, "~/work/repo")
	}
}

func TestParseTS(t *testing.T) {
	cases := []string{
		"2025-01-01T10:20:30",
		"2025-01-01T10:20:30Z",
		"2025-01-01T10:20:30-07:00",
		"2025-01-01T10:20:30.123456",
	}
	for _, in := range cases {
		got, ok := parseTS(in)
		if !ok {
			t.Fatalf("parseTS(%q) failed", in)
		}
		if got.Year() != 2025 || got.Month() != 1 || got.Day() != 1 {
			t.Fatalf("parseTS(%q) unexpected date %v", in, got)
		}
	}

	if _, ok := parseTS("not-a-time"); ok {
		t.Fatalf("parseTS should fail for invalid input")
	}
}

func TestSortedKeyHelpers(t *testing.T) {
	stats := map[string]*Stats{
		"b": {PromptTokens: 20},
		"a": {PromptTokens: 10},
		"c": {PromptTokens: 30},
	}
	if got := sortedKeys(stats); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("sortedKeys=%v", got)
	}

	grouped := map[string]map[string]*Stats{
		"z": {},
		"x": {},
		"y": {},
	}
	if got := sortedKeysStr(grouped); !reflect.DeepEqual(got, []string{"x", "y", "z"}) {
		t.Fatalf("sortedKeysStr=%v", got)
	}

	byPrompt := sortedKeysByFunc(stats, func(k string) float64 { return float64(stats[k].PromptTokens) })
	if !reflect.DeepEqual(byPrompt, []string{"a", "b", "c"}) {
		t.Fatalf("sortedKeysByFunc=%v", byPrompt)
	}
}
