package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

func TestDisplayAndTableHelpers(t *testing.T) {
	if got := displayWidth("abc"); got != 3 {
		t.Fatalf("displayWidth ascii=%d", got)
	}
	if got := displayWidth("😀"); got != 2 {
		t.Fatalf("displayWidth emoji=%d", got)
	}
	if got := displayWidth("e\u0301"); got != 1 {
		t.Fatalf("displayWidth combining=%d", got)
	}
	if got := displayWidth("❤️"); got != 2 {
		t.Fatalf("displayWidth VS16 emoji=%d", got)
	}
	if !isWideRune('界') || isWideRune('a') {
		t.Fatalf("isWideRune unexpected result")
	}
	if got := padCell("x", 3, false); got != "x  " {
		t.Fatalf("padCell left=%q", got)
	}
	if got := padCell("x", 3, true); got != "  x" {
		t.Fatalf("padCell right=%q", got)
	}
	if got := padCell("hello", 2, false); got != "hello" {
		t.Fatalf("padCell narrow width=%q", got)
	}
	out := captureStdout(t, func() {
		printTable("TITLE", []string{"Col", "Num"}, [][]string{{"😀", "7"}}, []string{"Total", "7"}, []string{"note"})
	})
	if !strings.Contains(out, "TITLE") || !strings.Contains(out, "note") || !strings.Contains(out, "Total") {
		t.Fatalf("printTable output missing expected content: %q", out)
	}
}

func TestIsWideRuneRangeCoverage(t *testing.T) {
	wideRunes := []rune{
		0x1100, 0x2E80, 0x3041, 0x3400, 0x4E00, 0xA960, 0xAC00,
		0xF900, 0xFE10, 0xFF01, 0xFFE0, 0x1F300, 0x20000, 0x30000,
	}
	for _, r := range wideRunes {
		if !isWideRune(r) {
			t.Fatalf("expected wide rune for U+%04X", r)
		}
	}
	if isWideRune('A') {
		t.Fatalf("expected ASCII to be non-wide")
	}
}

func TestGetDBPathUsesOnlyXDGLocation(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	oldHome := os.Getenv("HOME")
	oldXDG := os.Getenv("XDG_STATE_HOME")
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir temp: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
		_ = os.Setenv("HOME", oldHome)
		_ = os.Setenv("XDG_STATE_HOME", oldXDG)
	})
	home := filepath.Join(tmp, "home")
	xdg := filepath.Join(tmp, "state")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	if err := os.Setenv("XDG_STATE_HOME", xdg); err != nil {
		t.Fatalf("set XDG_STATE_HOME: %v", err)
	}
	legacyPath := filepath.Join(tmp, "copilot-tokens.db")
	if err := os.WriteFile(legacyPath, []byte("legacy-db"), 0o644); err != nil {
		t.Fatalf("create db file: %v", err)
	}
	got := getDBPath()
	want := filepath.Join(xdg, "copilot-token-cost", "copilot-tokens.db")
	absGot, err := filepath.Abs(got)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	if absGot != want {
		t.Fatalf("getDBPath=%q (abs %q), want %q", got, absGot, want)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("expected legacy db to remain in place, err=%v", err)
	}
	if _, err := os.Stat(want); err == nil {
		t.Fatalf("did not expect xdg db file to be copied automatically")
	}
}

func TestClearSourceAndCodespaceState(t *testing.T) {
	db := initDB(filepath.Join(t.TempDir(), "state.db"))
	t.Cleanup(func() { _ = db.Close() })
	insertRecords(db, []Record{{
		Model:            "gpt-4.1",
		PromptTokens:     1,
		CompletionTokens: 1,
		Timestamp:        "2026-01-01T00:00:00",
		SessionID:        "s",
		LogFile:          "a.log",
	}}, "codespace:test")
	markLogParsed(db, "a.log", 123.0, 1, "codespace:test")
	upsertSessionWorkspace(db, "s", "/tmp/proj", "codespace:test")
	upsertCodespaceSyncState(db, "cs", "2026-02-18T00:00:00Z")
	if got := getCodespaceLastUsed(db, "cs"); got != "2026-02-18T00:00:00Z" {
		t.Fatalf("last used=%q", got)
	}
	if got := getCodespaceLastUsed(db, "missing"); got != "" {
		t.Fatalf("missing last used=%q", got)
	}
	clearSource(db, "codespace:test")
	for _, q := range []string{
		"SELECT COUNT(*) FROM api_calls WHERE source='codespace:test'",
		"SELECT COUNT(*) FROM parsed_logs WHERE source='codespace:test'",
		"SELECT COUNT(*) FROM session_workspaces WHERE source='codespace:test'",
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("query count: %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 rows for %q got %d", q, n)
		}
	}
}

func writeFakeGH(t *testing.T, dir string, withCodespaces bool) {
	t.Helper()
	script := `#!/bin/sh
set -eu
if [ "${1:-}" = "cs" ] && [ "${2:-}" = "list" ]; then
  if [ "` + fmt.Sprintf("%t", withCodespaces) + `" = "true" ]; then
    printf '[{"name":"cs1","state":"Available","lastUsedAt":"2026-02-18T00:00:00Z"},{"name":"cs2","state":"Shutdown","lastUsedAt":"2026-02-17T00:00:00Z"},{"name":"","state":"Available","lastUsedAt":"2026-02-18T00:00:00Z"}]'
  else
    printf '[]'
  fi
  exit 0
fi
if [ "${1:-}" = "cs" ] && [ "${2:-}" = "cp" ]; then
  stage=""
  for arg in "$@"; do
    stage="$arg"
  done
  mkdir -p "$stage/.copilot/logs" "$stage/.copilot/session-state/123e4567-e89b-12d3-a456-426614174000"
  cat > "$stage/.copilot/logs/process-codespace.log" <<'EOF'
2026-02-18T10:00:00 Created ACP session: 123e4567-e89b-12d3-a456-426614174000
2026-02-18T10:00:01 PremiumRequestProcessor: Setting X-Initiator to 'user'
2026-02-18T10:00:02 {"model":"gpt-4.1"}
2026-02-18T10:00:03 {"prompt_tokens":12,"completion_tokens":3}
EOF
  cat > "$stage/.copilot/session-state/123e4567-e89b-12d3-a456-426614174000/workspace.yaml" <<'EOF'
cwd: /tmp/codespace-repo
EOF
  exit 0
fi
if [ "${1:-}" = "cs" ] && [ "${2:-}" = "stop" ]; then
  exit 0
fi
exit 1
`
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
}

func withPath(t *testing.T, dir string) {
	t.Helper()
	old := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+old); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", old) })
}

func TestListCodespacesFilters(t *testing.T) {
	binDir := t.TempDir()
	writeFakeGH(t, binDir, true)
	withPath(t, binDir)
	got := listCodespaces(false)
	if len(got) != 1 || got[0].Name != "cs1" {
		t.Fatalf("listCodespaces(false)=%+v", got)
	}
	got = listCodespaces(true)
	if len(got) != 2 {
		t.Fatalf("listCodespaces(true)=%+v", got)
	}
}

func TestSyncCodespacesToDBWithFakeGH(t *testing.T) {
	binDir := t.TempDir()
	writeFakeGH(t, binDir, true)
	withPath(t, binDir)
	db := initDB(filepath.Join(t.TempDir(), "codespaces.db"))
	t.Cleanup(func() { _ = db.Close() })

	inserted := syncCodespacesToDB(db, false, false)
	if inserted != 1 {
		t.Fatalf("syncCodespacesToDB inserted=%d, want 1", inserted)
	}
	var apiCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM api_calls WHERE source = 'codespace:cs1'").Scan(&apiCount); err != nil {
		t.Fatalf("query api count: %v", err)
	}
	if apiCount != 1 {
		t.Fatalf("api_count=%d", apiCount)
	}
	if got := getCodespaceLastUsed(db, "cs1"); got == "" {
		t.Fatalf("codespace sync state missing")
	}

	second := syncCodespacesToDB(db, false, false)
	if second != 0 {
		t.Fatalf("second sync should skip unchanged codespace, got %d", second)
	}
}

func TestSyncCodespacesToDBIncludeStopped(t *testing.T) {
	binDir := t.TempDir()
	writeFakeGH(t, binDir, true)
	withPath(t, binDir)
	db := initDB(filepath.Join(t.TempDir(), "codespaces-stopped.db"))
	t.Cleanup(func() { _ = db.Close() })

	inserted := syncCodespacesToDB(db, true, false)
	if inserted != 2 {
		t.Fatalf("syncCodespacesToDB include stopped inserted=%d, want 2", inserted)
	}
	if countRows(t, db, "SELECT COUNT(*) FROM api_calls WHERE source='codespace:cs1'") != 1 {
		t.Fatalf("expected one cs1 record")
	}
	if countRows(t, db, "SELECT COUNT(*) FROM api_calls WHERE source='codespace:cs2'") != 1 {
		t.Fatalf("expected one cs2 record")
	}
}

func TestListCodespacesNoGhBinary(t *testing.T) {
	old := os.Getenv("PATH")
	_ = os.Setenv("PATH", t.TempDir())
	t.Cleanup(func() { _ = os.Setenv("PATH", old) })
	if got := listCodespaces(false); got != nil {
		t.Fatalf("expected nil when gh is missing, got %+v", got)
	}
}

func countRows(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("count rows query: %v", err)
	}
	return n
}
