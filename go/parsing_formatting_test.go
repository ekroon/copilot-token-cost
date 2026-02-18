package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLogFile(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "sample.log")
	const uuid = "123e4567-e89b-12d3-a456-426614174000"
	content := `2025-01-01T10:00:00 Created ACP session: ` + uuid + `
2025-01-01T10:00:01 PremiumRequestProcessor: Setting X-Initiator to 'user'
2025-01-01T10:00:02 {"model":"claude-3-5-sonnet"}
2025-01-01T10:00:03 {"prompt_tokens":100,"completion_tokens":25,"cache_creation_input_tokens":10,"cache_read_input_tokens":20}
2025-01-01T10:10:00 filler
2025-01-01T10:10:01 filler
2025-01-01T10:10:02 filler
2025-01-01T10:10:03 filler
2025-01-01T10:10:04 filler
2025-01-01T10:10:05 filler
2025-01-01T10:10:06 filler
2025-01-01T10:10:07 filler
2025-01-01T10:10:08 filler
2025-01-01T10:10:09 filler
2025-01-01T10:10:10 filler
2025-01-01T11:00:00 Workspace initialized: ` + uuid + `
2025-01-01T11:00:01 {"model":"gpt-4.1"}
2025-01-01T11:00:02 {"prompt_tokens":50,"completion_tokens":5,"cached_tokens":8}
`
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	records := parseLogFile(logPath)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	first := records[0]
	if first.Model != "claude-3-5-sonnet" || first.PromptTokens != 100 || first.CompletionTokens != 25 {
		t.Fatalf("unexpected first record tokens/model: %+v", first)
	}
	if first.CacheCreationTokens != 10 || first.CacheReadTokens != 20 {
		t.Fatalf("unexpected first record cache values: %+v", first)
	}
	if !first.IsUserTurn || first.Timestamp != "2025-01-01T10:00:03" || first.SessionID != uuid || first.LogFile != "sample.log" {
		t.Fatalf("unexpected first record metadata: %+v", first)
	}

	second := records[1]
	if second.Model != "gpt-4.1" || second.PromptTokens != 50 || second.CompletionTokens != 5 {
		t.Fatalf("unexpected second record tokens/model: %+v", second)
	}
	if second.CacheCreationTokens != 0 || second.CacheReadTokens != 8 {
		t.Fatalf("unexpected second record cache values: %+v", second)
	}
	if second.IsUserTurn {
		t.Fatalf("expected second record to be non-user turn: %+v", second)
	}
}

func TestParseLogFileMissingFile(t *testing.T) {
	records := parseLogFile(filepath.Join(t.TempDir(), "missing.log"))
	if records != nil {
		t.Fatalf("expected nil records for missing file, got %+v", records)
	}
}

func TestCacheHitPct(t *testing.T) {
	if got := cacheHitPct(0, 10); got != "-" {
		t.Fatalf("expected '-', got %q", got)
	}
	if got := cacheHitPct(100, 25); got != "25%" {
		t.Fatalf("expected '25%%', got %q", got)
	}
}

func TestFmtTokens(t *testing.T) {
	cases := map[int]string{
		999:     "999",
		1000:    "1.0K",
		1540:    "1.5K",
		1000000: "1.0M",
		2500000: "2.5M",
	}
	for in, want := range cases {
		if got := fmtTokens(in); got != want {
			t.Fatalf("fmtTokens(%d): want %q, got %q", in, want, got)
		}
	}
}

func TestCommaFloat(t *testing.T) {
	if got := commaFloat(12345.678, 2); got != "12,345.68" {
		t.Fatalf("unexpected commaFloat: %q", got)
	}
	if got := commaFloat(-1234.5, 1); got != "-1,234.5" {
		t.Fatalf("unexpected negative commaFloat: %q", got)
	}
}

func TestAddCommas(t *testing.T) {
	cases := map[string]string{
		"1234567":     "1,234,567",
		"1234567.89":  "1,234,567.89",
		"-1234567":    "-1,234,567",
		"-1234567.89": "-1,234,567.89",
	}
	for in, want := range cases {
		if got := addCommas(in); got != want {
			t.Fatalf("addCommas(%q): want %q, got %q", in, want, got)
		}
	}
}

func TestFmtCost(t *testing.T) {
	cases := map[float64]string{
		1234.567: "$1,235",
		12.3:     "$12.30",
		0.4567:   "$0.457",
	}
	for in, want := range cases {
		if got := fmtCost(in); got != want {
			t.Fatalf("fmtCost(%f): want %q, got %q", in, want, got)
		}
	}
}

func TestCommaInt(t *testing.T) {
	if got := commaInt(1234567); got != "1,234,567" {
		t.Fatalf("unexpected commaInt: %q", got)
	}
	if got := commaInt(-1234567); got != "-1,234,567" {
		t.Fatalf("unexpected negative commaInt: %q", got)
	}
}

func TestRoundN(t *testing.T) {
	if got := roundN(1.234, 2); got != 1.23 {
		t.Fatalf("roundN(1.234, 2): got %v", got)
	}
	if got := roundN(1.235, 2); got != 1.24 {
		t.Fatalf("roundN(1.235, 2): got %v", got)
	}
}
