package parsing

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"copilot-token-cost/internal/domain"
)

// assistantUsageEvent maps the telemetry JSON structure emitted per API call.
type assistantUsageEvent struct {
	Kind       string `json:"kind"`
	Properties struct {
		Model     string `json:"model"`
		Initiator string `json:"initiator"`
	} `json:"properties"`
	Metrics struct {
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
		CacheReadTokens  int `json:"cache_read_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"metrics"`
	SessionID string `json:"session_id"`
}

// parseTelemetryRecords parses assistant_usage telemetry blocks as the primary
// data source. Available in logs from ~Feb 13 2026 onwards. Each block contains
// model, initiator, all token counts, and session_id in clean JSON.
func parseTelemetryRecords(content, logPath, minTimestamp, maxTimestamp string) []domain.Record {
	lines := strings.Split(content, "\n")
	var records []domain.Record
	var lastTimestamp string
	var lastPromptText *string

	for i, line := range lines {
		if len(line) >= 19 &&
			line[4] == '-' && line[7] == '-' &&
			line[10] == 'T' && line[13] == ':' && line[16] == ':' {
			lastTimestamp = line[:19]
		}

		if prompt := ExtractPromptTextFromLine(line); prompt != nil {
			lastPromptText = prompt
		} else if prompt := extractPromptTextFromProblemStatementLine(lines, i); prompt != nil {
			lastPromptText = prompt
		}

		if !strings.Contains(line, `"assistant_usage"`) {
			continue
		}

		// Collect the JSON block starting from before this line.
		// The structure is: timestamp [Telemetry] cli.telemetry:\n{ ... }
		// Find the opening brace.
		blockStart := -1
		for j := i - 1; j <= i+2 && j < len(lines); j++ {
			if j >= 0 && strings.TrimSpace(lines[j]) == "{" {
				blockStart = j
				break
			}
		}
		if blockStart == -1 {
			continue
		}

		// Collect lines until we find the matching closing brace.
		depth := 0
		var blockLines []string
		for j := blockStart; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			blockLines = append(blockLines, lines[j])
			depth += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")
			if depth <= 0 {
				break
			}
		}

		block := strings.Join(blockLines, "\n")
		var event assistantUsageEvent
		if err := json.Unmarshal([]byte(block), &event); err != nil {
			continue
		}
		if event.Kind != "assistant_usage" {
			continue
		}

		if lastTimestamp != "" {
			if minTimestamp != "" && lastTimestamp < minTimestamp {
				continue
			}
			if maxTimestamp != "" && lastTimestamp >= maxTimestamp {
				continue
			}
		}

		model := event.Properties.Model
		if model == "" {
			model = "unknown"
		}

		promptText := ExtractPromptTextNearLine(lines, i)
		if promptText == nil {
			promptText = lastPromptText
		}

		records = append(records, domain.Record{
			Model:               model,
			PromptTokens:        event.Metrics.InputTokens,
			CompletionTokens:    event.Metrics.OutputTokens,
			PromptText:          promptText,
			CacheCreationTokens: event.Metrics.CacheWriteTokens,
			CacheReadTokens:     event.Metrics.CacheReadTokens,
			IsUserTurn:          IsUserInitiator(event.Properties.Initiator),
			Timestamp:           lastTimestamp,
			SessionID:           event.SessionID,
			LogFile:             filepath.Base(logPath),
		})
		lastPromptText = nil
	}
	return records
}
