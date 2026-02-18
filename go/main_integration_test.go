package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFixtureLog(t *testing.T, logsDir, sessionID string, ts time.Time, model string, userTurn bool) {
	t.Helper()
	lineTS := ts.Format("2006-01-02T15:04:05")
	initiator := "agent"
	if userTurn {
		initiator = "user"
	}
	content := lineTS + " Created ACP session: " + sessionID + "\n" +
		lineTS + " PremiumRequestProcessor: Setting X-Initiator to '" + initiator + "'\n" +
		lineTS + " {\"model\":\"" + model + "\"}\n" +
		lineTS + " {\"prompt_tokens\":100,\"completion_tokens\":20,\"cache_creation_input_tokens\":5,\"cache_read_input_tokens\":10}\n"
	logPath := filepath.Join(logsDir, "process-"+strings.ReplaceAll(model, ".", "-")+".log")
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture log: %v", err)
	}
	if err := os.Chtimes(logPath, ts, ts); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func prepareMainFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	pricingBytes, err := os.ReadFile("../pricing.json")
	if err != nil {
		t.Fatalf("read pricing.json fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "pricing.json"), pricingBytes, 0o644); err != nil {
		t.Fatalf("write pricing.json fixture: %v", err)
	}
	home := filepath.Join(root, "home")
	logsDir := filepath.Join(home, ".copilot", "logs")
	sessionID := "123e4567-e89b-12d3-a456-426614174999"
	sessionDir := filepath.Join(home, ".copilot", "session-state", sessionID)
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "workspace.yaml"), []byte("cwd: /tmp/main-fixture\n"), 0o644); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	now := time.Now()
	writeFixtureLog(t, logsDir, sessionID, now, "gpt-4.1", true)
	writeFixtureLog(t, logsDir, sessionID, now.AddDate(0, 0, -1), "claude-sonnet-4.6", false)
	return root
}

func runMainWithArgs(t *testing.T, root string, args ...string) (string, string) {
	t.Helper()
	oldArgs := os.Args
	oldFlag := flag.CommandLine
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	oldHome := os.Getenv("HOME")
	oldWd, _ := os.Getwd()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir root: %v", err)
	}
	if err := os.Setenv("HOME", filepath.Join(root, "home")); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "copilot-tokens.db"), []byte{}, 0o644); err != nil {
		t.Fatalf("create db marker: %v", err)
	}

	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout = outW
	os.Stderr = errW
	flag.CommandLine = flag.NewFlagSet("copilot-token-cost", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"copilot-token-cost"}, args...)

	main()

	_ = outW.Close()
	_ = errW.Close()
	outBytes, _ := io.ReadAll(outR)
	errBytes, _ := io.ReadAll(errR)

	os.Args = oldArgs
	flag.CommandLine = oldFlag
	os.Stdout = oldStdout
	os.Stderr = oldStderr
	_ = os.Setenv("HOME", oldHome)
	_ = os.Chdir(oldWd)

	return string(outBytes), string(errBytes)
}

func TestMainPathsWithoutMocking(t *testing.T) {
	root := prepareMainFixture(t)

	out, _ := runMainWithArgs(t, root)
	if !strings.Contains(out, "PER-MODEL SUMMARY") {
		t.Fatalf("default output missing model table")
	}

	out, _ = runMainWithArgs(t, root, "--today")
	if !strings.Contains(out, "today") {
		t.Fatalf("--today output missing period")
	}

	out, _ = runMainWithArgs(t, root, "--yesterday")
	if !strings.Contains(out, "yesterday") {
		t.Fatalf("--yesterday output missing period")
	}

	out, _ = runMainWithArgs(t, root, "--from", "2", "--to", "0", "--project", "main-fixture")
	if !strings.Contains(out, "Project filter: main-fixture") {
		t.Fatalf("--from/--to output missing project filter")
	}

	out, _ = runMainWithArgs(t, root, "2", "--logs-dir", filepath.Join(root, "home", ".copilot", "logs"), "--sync")
	if !strings.Contains(out, "last 2 days") {
		t.Fatalf("positional days output missing period")
	}

	out, _ = runMainWithArgs(t, root, "--all", "--json")
	if !strings.Contains(out, "\"models\"") || !strings.Contains(out, "\"daily\"") {
		t.Fatalf("--json output missing keys")
	}

	exportPath := filepath.Join(root, "export.jsonl")
	_, _ = runMainWithArgs(t, root, "--export-file", exportPath)
	if _, err := os.Stat(exportPath); err != nil {
		t.Fatalf("export file missing: %v", err)
	}

	out, _ = runMainWithArgs(t, root, "--import-file", exportPath, "--json")
	if !strings.Contains(out, "\"api_calls\"") {
		t.Fatalf("--import-file output missing api_calls")
	}

	otherDB := filepath.Join(root, "other.db")
	if b, err := os.ReadFile(filepath.Join(root, "copilot-tokens.db")); err == nil {
		_ = os.WriteFile(otherDB, b, 0o644)
	}
	out, _ = runMainWithArgs(t, root, "--import-file", otherDB, "--json")
	if !strings.Contains(out, "\"models\"") {
		t.Fatalf("--import-file .db output missing models")
	}

	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeFakeGH(t, binDir, true)
	withPath(t, binDir)
	out, _ = runMainWithArgs(t, root, "--codespaces-sync")
	if !strings.Contains(out, "PER-MODEL SUMMARY") {
		t.Fatalf("--codespaces-sync output missing summary")
	}
}
