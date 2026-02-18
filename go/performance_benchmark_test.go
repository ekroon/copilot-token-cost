package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	benchmarkLogFiles       = 40
	benchmarkRecordsPerFile = 200
	benchmarkFixtureSeed    = 20260218
	e2eBenchmarkLogFiles    = 203
	e2eBenchmarkRecords     = 250
)

func writeSyntheticBenchmarkFixture(tb testing.TB, logsDir, sessionDir string) {
	writeSyntheticBenchmarkFixtureWithSize(tb, logsDir, sessionDir, benchmarkLogFiles, benchmarkRecordsPerFile)
}

func writeSyntheticBenchmarkFixtureWithSize(tb testing.TB, logsDir, sessionDir string, fileCount, recordsPerFile int) {
	tb.Helper()
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		tb.Fatalf("mkdir logs: %v", err)
	}
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		tb.Fatalf("mkdir session-state: %v", err)
	}

	sessionID := "123e4567-e89b-12d3-a456-426614174000"
	workspaceDir := filepath.Join(sessionDir, sessionID)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		tb.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "workspace.yaml"), []byte("cwd: /tmp/benchmark-project\n"), 0o644); err != nil {
		tb.Fatalf("write workspace.yaml: %v", err)
	}

	base := time.Date(2026, 2, 18, 12, 0, 0, 0, time.UTC)
	models := []string{"gpt-5-mini", "claude-sonnet-4.6", "gemini-2.5-pro"}
	for fileIdx := 0; fileIdx < fileCount; fileIdx++ {
		ts := base.Add(-time.Duration(fileIdx) * time.Minute)
		var b strings.Builder
		b.Grow(recordsPerFile * 240)
		for rec := 0; rec < recordsPerFile; rec++ {
			lineTS := ts.Add(time.Duration(rec) * time.Second).Format("2006-01-02T15:04:05")
			model := models[(fileIdx+rec+benchmarkFixtureSeed)%len(models)]
			initiator := "agent"
			if (fileIdx+rec)%3 == 0 {
				initiator = "user"
			}
			prompt := 100 + (fileIdx+rec)%70
			completion := 20 + (fileIdx+rec)%30
			cacheCreate := (fileIdx + rec) % 12
			cacheRead := (fileIdx*3 + rec) % 20

			b.WriteString(lineTS + " Created ACP session: " + sessionID + "\n")
			b.WriteString(lineTS + " PremiumRequestProcessor: Setting X-Initiator to '" + initiator + "'\n")
			b.WriteString(lineTS + " {\"model\":\"" + model + "\"}\n")
			b.WriteString(lineTS + " {\"prompt_tokens\":" + strconv.Itoa(prompt) + ",\"completion_tokens\":" + strconv.Itoa(completion) + ",\"cache_creation_input_tokens\":" + strconv.Itoa(cacheCreate) + ",\"cache_read_input_tokens\":" + strconv.Itoa(cacheRead) + "}\n")
		}
		logPath := filepath.Join(logsDir, fmt.Sprintf("process-%03d.log", fileIdx))
		if err := os.WriteFile(logPath, []byte(b.String()), 0o644); err != nil {
			tb.Fatalf("write fixture log: %v", err)
		}
		if err := os.Chtimes(logPath, ts, ts); err != nil {
			tb.Fatalf("chtimes log: %v", err)
		}
	}
}

func BenchmarkColdStartupAndParse3x(b *testing.B) {
	root := b.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	writeSyntheticBenchmarkFixtureWithSize(b, logsDir, sessionDir, e2eBenchmarkLogFiles, e2eBenchmarkRecords)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dbPath := filepath.Join(root, fmt.Sprintf("bench-3x-%d.db", i))
		db := initDB(dbPath)
		inserted := syncLogsToDB(db, logsDir, sessionDir, false, "bench-3x", nil, nil)
		_ = db.Close()
		if inserted == 0 {
			b.Fatal("syncLogsToDB inserted 0 records for 3x fixture")
		}
	}
}

func BenchmarkParseLogFileSynthetic(b *testing.B) {
	root := b.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	writeSyntheticBenchmarkFixture(b, logsDir, sessionDir)
	target := filepath.Join(logsDir, "process-000.log")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		records := parseLogFile(target)
		if len(records) == 0 {
			b.Fatal("parseLogFile returned 0 records")
		}
	}
}

func parseLogFileBaseline(logPath string) []Record {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	content := string(data)
	lines := strings.Split(content, "\n")
	var records []Record

	lastModel := "unknown"
	var lastTimestamp, lastSession string
	lastInitiator := "agent"

	for i, line := range lines {
		if m := reTimestamp.FindStringSubmatch(line); m != nil {
			lastTimestamp = m[1]
		}
		if m := reSession.FindStringSubmatch(line); m != nil {
			lastSession = m[1]
		}
		if m := reInitiator.FindStringSubmatch(line); m != nil {
			lastInitiator = m[1]
		}
		if m := reModelJSON.FindStringSubmatch(line); m != nil {
			candidate := m[1]
			if !strings.HasPrefix(candidate, "{") && (strings.Contains(candidate, "claude") || strings.Contains(candidate, "gpt") || strings.Contains(candidate, "gemini")) {
				lastModel = candidate
			}
		}

		if !strings.Contains(line, `"completion_tokens"`) {
			continue
		}

		blockStart := i - 10
		if blockStart < 0 {
			blockStart = 0
		}
		blockEnd := i + 15
		if blockEnd > len(lines) {
			blockEnd = len(lines)
		}
		block := strings.Join(lines[blockStart:blockEnd], "\n")

		promptMatch := rePromptTokens.FindStringSubmatch(block)
		compMatch := reCompTokens.FindStringSubmatch(block)
		if promptMatch == nil || compMatch == nil {
			continue
		}

		if m := reModelJSON.FindStringSubmatch(block); m != nil {
			candidate := m[1]
			if strings.Contains(candidate, "claude") || strings.Contains(candidate, "gpt") || strings.Contains(candidate, "gemini") {
				lastModel = candidate
			}
		}

		promptTokens, _ := strconv.Atoi(promptMatch[1])
		completionTokens, _ := strconv.Atoi(compMatch[1])

		var cacheCreation, cacheReadVal int
		if m := reCacheCreation.FindStringSubmatch(block); m != nil {
			cacheCreation, _ = strconv.Atoi(m[1])
		}
		if m := reCacheRead.FindStringSubmatch(block); m != nil {
			cacheReadVal, _ = strconv.Atoi(m[1])
		}
		if cacheReadVal == 0 {
			if m := reCachedTokens.FindStringSubmatch(block); m != nil {
				cacheReadVal, _ = strconv.Atoi(m[1])
			}
		}

		records = append(records, Record{
			Model:               lastModel,
			PromptTokens:        promptTokens,
			CompletionTokens:    completionTokens,
			CacheCreationTokens: cacheCreation,
			CacheReadTokens:     cacheReadVal,
			IsUserTurn:          lastInitiator == "user",
			Timestamp:           lastTimestamp,
			SessionID:           lastSession,
			LogFile:             filepath.Base(logPath),
		})
		lastInitiator = "agent"
	}
	return records
}

func BenchmarkParseLogFileBaselineSynthetic(b *testing.B) {
	root := b.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	writeSyntheticBenchmarkFixture(b, logsDir, sessionDir)
	target := filepath.Join(logsDir, "process-000.log")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		records := parseLogFileBaseline(target)
		if len(records) == 0 {
			b.Fatal("parseLogFileBaseline returned 0 records")
		}
	}
}

func BenchmarkColdStartupAndParse(b *testing.B) {
	root := b.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	writeSyntheticBenchmarkFixture(b, logsDir, sessionDir)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dbPath := filepath.Join(root, fmt.Sprintf("bench-%d.db", i))
		db := initDB(dbPath)
		inserted := syncLogsToDB(db, logsDir, sessionDir, false, "bench", nil, nil)
		_ = db.Close()
		if inserted == 0 {
			b.Fatal("syncLogsToDB inserted 0 records")
		}
	}
}
