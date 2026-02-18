package main

import (
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
	if err := os.WriteFile(filepath.Join(workspaceDir, "workspace.yaml"), []byte("cwd: /tmp/demo-project\n"), 0644); err != nil {
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
