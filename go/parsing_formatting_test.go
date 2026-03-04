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
	if first.PromptText != nil {
		t.Fatalf("expected nil prompt text when unavailable, got %#v", first.PromptText)
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
	if second.PromptText != nil {
		t.Fatalf("expected nil prompt text when unavailable, got %#v", second.PromptText)
	}
}

func TestParseLogFileTimestampedWorkspaceInitializedSetsSessionID(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "timestamped-session.log")
	const sessionID = "123e4567-e89b-12d3-a456-426614174010"
	content := `2025-01-01T10:00:00 Workspace initialized: ` + sessionID + `
2025-01-01T10:00:01 {"model":"gpt-4.1"}
2025-01-01T10:00:02 {"prompt_tokens":10,"completion_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}
`
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	records := parseLogFile(logPath)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].SessionID != sessionID {
		t.Fatalf("expected session id %q, got %q", sessionID, records[0].SessionID)
	}
}

func TestParseLogFileIgnoresQuotedWorkspaceInitializedInToolOutput(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "quoted-workspace.log")
	const realSessionID = "123e4567-e89b-12d3-a456-426614174011"
	const embeddedSessionID = "123e4567-e89b-12d3-a456-426614174099"
	content := `2025-01-01T10:00:00 Created ACP session: ` + realSessionID + `
2025-01-01T10:00:01 {"model":"gpt-4.1"}
2025-01-01T10:00:02 {"prompt_tokens":10,"completion_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}
2025-01-01T10:00:03 {"tool_output":"note: \"Workspace initialized: ` + embeddedSessionID + `\""}
2025-01-01T10:00:04 {"model":"gpt-4.1"}
2025-01-01T10:00:05 {"prompt_tokens":11,"completion_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}
`
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	records := parseLogFile(logPath)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if records[1].SessionID != realSessionID {
		t.Fatalf("expected session id to remain %q, got %q", realSessionID, records[1].SessionID)
	}
}

func TestParseLogFileExtractsPromptTextWhenAvailable(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "prompt.log")
	content := `2025-01-01T10:00:00 {"model":"gpt-4.1"}
2025-01-01T10:00:01 {"messages":[{"role":"user","content":[{"type":"text","text":"  Build a sync parser  "}]}]}
2025-01-01T10:00:02 {"prompt_tokens":10,"completion_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}
`
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	records := parseLogFile(logPath)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].PromptText == nil || *records[0].PromptText != "Build a sync parser" {
		t.Fatalf("expected extracted prompt text, got %#v", records[0].PromptText)
	}
}

func TestParseLogFileExtractsPromptTextFromUserStatement(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "statement-prompt.log")
	content := "2025-01-01T10:00:00 {\"model\":\"gpt-4.1\"}\n" +
		"2025-01-01T10:00:01 {\n" +
		"  \"problem\": {\n" +
		"    \"statement\": \"  Build this from statement payload  \"\n" +
		"  }\n" +
		"}\n"
	for i := 0; i < 30; i++ {
		content += "2025-01-01T10:00:01 filler line\n"
	}
	content += "2025-01-01T10:00:02 {\"prompt_tokens\":10,\"completion_tokens\":2,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	records := parseLogFile(logPath)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].PromptText == nil || *records[0].PromptText != "Build this from statement payload" {
		t.Fatalf("expected extracted statement prompt text, got %#v", records[0].PromptText)
	}
}

func TestParseLogFileIgnoresAssistantStatementPromptText(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "assistant-statement.log")
	content := `2025-01-01T10:00:00 {"model":"gpt-4.1"}
2025-01-01T10:00:01 {"conversation_item":{"role":"assistant","statement":"this should not be persisted"}}
2025-01-01T10:00:02 {"prompt_tokens":10,"completion_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}
`
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	records := parseLogFile(logPath)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].PromptText != nil {
		t.Fatalf("expected nil prompt text for assistant statement payload, got %#v", records[0].PromptText)
	}
}

func TestParseLogFileIgnoresNonUserInitiatorVariants(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "initiator.log")
	content := `2025-01-01T10:00:00 PremiumRequestProcessor: Setting X-Initiator to 'user-autopilot'
2025-01-01T10:00:01 {"model":"gpt-4.1"}
2025-01-01T10:00:02 {"prompt_tokens":10,"completion_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}
`
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	records := parseLogFile(logPath)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].IsUserTurn {
		t.Fatalf("expected non-user turn for non-user initiator variant: %+v", records[0])
	}
}

func TestParseLogFileTelemetryInitiatorOverridesAfterSessionNaming(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "telemetry.log")
	content := `2025-01-01T10:00:00 PremiumRequestProcessor: Setting X-Initiator to 'user'
2025-01-01T10:00:01 {"model":"gpt-5-mini"}
2025-01-01T10:00:02 {"prompt_tokens":300,"completion_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}
2025-01-01T10:00:03 Generated session name: "Test Session"
2025-01-01T10:00:04 [Telemetry] cli.telemetry:
{
  "kind": "assistant_usage",
  "properties": {
    "model": "claude-opus-4.6-1m",
    "initiator": "user",
    "api_call_id": "msg_test"
  }
}
2025-01-01T10:00:05 {"model":"claude-opus-4.6-1m"}
2025-01-01T10:00:06 {"prompt_tokens":5000,"completion_tokens":500,"cache_creation_input_tokens":1000,"cache_read_input_tokens":0}
`
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	records := parseLogFile(logPath)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if !records[0].IsUserTurn {
		t.Fatalf("expected gpt-5-mini record to have IsUserTurn=true (from X-Initiator): %+v", records[0])
	}
	if !records[1].IsUserTurn {
		t.Fatalf("expected opus record to have IsUserTurn=true (from telemetry initiator): %+v", records[1])
	}
	if records[1].Model != "claude-opus-4.6-1m" {
		t.Fatalf("expected opus model, got %s", records[1].Model)
	}
}

func TestParseLogFileTelemetryInitiatorAgentDoesNotPromote(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "agent.log")
	content := `2025-01-01T10:00:00 {"model":"claude-opus-4.6"}
2025-01-01T10:00:01 {"prompt_tokens":5000,"completion_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}
    "initiator": "agent",
2025-01-01T10:00:03 {"model":"claude-opus-4.6"}
2025-01-01T10:00:04 {"prompt_tokens":6000,"completion_tokens":600,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}
`
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	records := parseLogFile(logPath)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if records[0].IsUserTurn {
		t.Fatalf("expected first record to be non-user turn: %+v", records[0])
	}
	if records[1].IsUserTurn {
		t.Fatalf("expected second record to be non-user turn (telemetry says agent): %+v", records[1])
	}
}

func TestParseLogFileMissingFile(t *testing.T) {
	records := parseLogFile(filepath.Join(t.TempDir(), "missing.log"))
	if records != nil {
		t.Fatalf("expected nil records for missing file, got %+v", records)
	}
}

func TestPromptTextForStorage(t *testing.T) {
	if got := promptTextForStorage(nil); got.Valid {
		t.Fatalf("expected invalid null string for nil prompt, got %+v", got)
	}
	blank := "   \n\t"
	if got := promptTextForStorage(&blank); got.Valid {
		t.Fatalf("expected invalid null string for blank prompt, got %+v", got)
	}
	text := "  hello prompt  "
	got := promptTextForStorage(&text)
	if !got.Valid || got.String != "hello prompt" {
		t.Fatalf("expected trimmed valid prompt text, got %+v", got)
	}
}

func TestPromptTextForStorageAlwaysOnNoOptInGate(t *testing.T) {
	t.Setenv("COPILOT_TOKEN_COST_PROMPT_STORAGE_OPT_IN", "0")
	text := "persist me"
	got := promptTextForStorage(&text)
	if !got.Valid || got.String != "persist me" {
		t.Fatalf("expected prompt text to persist regardless of opt-in env, got %+v", got)
	}
}

func TestContainsPromptIndicator(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{name: "plain user", line: `{"role":"user"}`, want: true},
		{name: "escaped user", line: `{\"role\":\"user\"}`, want: true},
		{name: "messages key", line: `{"messages":[]}`, want: true},
		{name: "escaped messages key", line: `{\"messages\":[]}`, want: true},
		{name: "prompt key", line: `{"prompt":"build parser"}`, want: true},
		{name: "statement key", line: `{"statement":"build parser"}`, want: true},
		{name: "escaped statement key", line: `{\"statement\":\"build parser\"}`, want: true},
		{name: "escaped prompt key does not match prefilter", line: `{\"prompt\":\"build parser\"}`, want: false},
		{name: "no indicator", line: `{"role":"assistant","content":"ok"}`, want: false},
	}
	for _, tc := range cases {
		if got := containsPromptIndicator(tc.line); got != tc.want {
			t.Fatalf("%s: expected %v, got %v", tc.name, tc.want, got)
		}
	}
}

func TestExtractPromptTextFromLineHandlesEscapedUserJSON(t *testing.T) {
	line := `{\"messages\":[{\"role\":\"user\",\"content\":\"  keep me  \"}]}`
	got := extractPromptTextFromLine(line)
	if got == nil || *got != "keep me" {
		t.Fatalf("expected escaped user JSON prompt extraction, got %#v", got)
	}
}

func TestParseLogFileInRange(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "sample.log")
	content := `2025-01-01T09:59:59 {"model":"gpt-4.1"}
2025-01-01T09:59:59 {"prompt_tokens":5,"completion_tokens":1}
2025-01-01T10:00:00 {"model":"gpt-4.1"}
2025-01-01T10:00:00 {"prompt_tokens":10,"completion_tokens":2}
2025-01-01T11:00:00 {"model":"gpt-4.1"}
2025-01-01T11:00:00 {"prompt_tokens":20,"completion_tokens":3}
`
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	records := parseLogFileInRange(logPath, "2025-01-01T10:00:00", "2025-01-01T11:00:00")
	if len(records) != 1 {
		t.Fatalf("expected 1 in-range record, got %d", len(records))
	}
	if records[0].Timestamp != "2025-01-01T10:00:00" {
		t.Fatalf("unexpected in-range timestamp: %s", records[0].Timestamp)
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
