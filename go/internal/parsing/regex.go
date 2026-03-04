package parsing

import (
	"path/filepath"
	"strconv"
	"strings"

	"copilot-token-cost/internal/domain"
)

// parseRegexRecords is the legacy parser for logs without assistant_usage
// telemetry (pre-Feb 13 2026). It uses regex matching and block scanning to
// extract token usage from raw log lines.
func parseRegexRecords(content, logPath, minTimestamp, maxTimestamp string) []domain.Record {
	lines := strings.Split(content, "\n")
	records := make([]domain.Record, 0, strings.Count(content, `"completion_tokens"`))

	lastModel := "unknown"
	var lastTimestamp, lastSession string
	lastInitiator := "agent"
	var lastPromptText *string

	for i, line := range lines {
		if len(line) >= 19 &&
			line[4] == '-' && line[7] == '-' &&
			line[10] == 'T' && line[13] == ':' && line[16] == ':' {
			lastTimestamp = line[:19]
		}
		if strings.Contains(line, "Workspace initialized") || strings.Contains(line, "Created ACP session") || strings.Contains(line, "Flushed ") {
			if m := reSession.FindStringSubmatch(line); m != nil {
				lastSession = m[1]
			}
		}
		if strings.Contains(line, "PremiumRequestProcessor: Setting X-Initiator") {
			if m := reInitiator.FindStringSubmatch(line); m != nil {
				lastInitiator = strings.TrimSpace(m[1])
			}
		}
		if strings.Contains(line, `"initiator"`) {
			if m := reInitiatorTelemetry.FindStringSubmatch(line); m != nil {
				lastInitiator = strings.TrimSpace(m[1])
			}
		}
		if strings.Contains(line, `"model"`) {
			if m := reModelJSON.FindStringSubmatch(line); m != nil {
				candidate := m[1]
				if !strings.HasPrefix(candidate, "{") && (strings.Contains(candidate, "claude") || strings.Contains(candidate, "gpt") || strings.Contains(candidate, "gemini")) {
					lastModel = candidate
				}
			}
		}
		if prompt := ExtractPromptTextFromLine(line); prompt != nil {
			lastPromptText = prompt
		} else if prompt := extractPromptTextFromProblemStatementLine(lines, i); prompt != nil {
			lastPromptText = prompt
		}

		if !strings.Contains(line, `"completion_tokens"`) {
			continue
		}
		if lastTimestamp != "" {
			if minTimestamp != "" && lastTimestamp < minTimestamp {
				lastInitiator = "agent"
				continue
			}
			if maxTimestamp != "" && lastTimestamp >= maxTimestamp {
				lastInitiator = "agent"
				continue
			}
		}

		promptMatch := rePromptTokens.FindStringSubmatch(line)
		compMatch := reCompTokens.FindStringSubmatch(line)
		var cacheCreation, cacheReadVal int
		cacheCreationSet := false
		cacheReadSet := false
		if m := reCacheCreation.FindStringSubmatch(line); m != nil {
			cacheCreation, _ = strconv.Atoi(m[1])
			cacheCreationSet = true
		}
		if m := reCacheRead.FindStringSubmatch(line); m != nil {
			cacheReadVal, _ = strconv.Atoi(m[1])
			cacheReadSet = true
		}
		if !cacheReadSet {
			if m := reCachedTokens.FindStringSubmatch(line); m != nil {
				cacheReadVal, _ = strconv.Atoi(m[1])
				cacheReadSet = true
			}
		}

		if promptMatch == nil || compMatch == nil || !cacheCreationSet || !cacheReadSet || lastModel == "unknown" {
			blockStart := i - 10
			if blockStart < 0 {
				blockStart = 0
			}
			blockEnd := i + 16
			if blockEnd > len(lines) {
				blockEnd = len(lines)
			}
			// Extended scan range for model only — new CAPI response format
			// places "model" up to ~110 lines after "completion_tokens".
			modelBlockEnd := i + 120
			if modelBlockEnd > len(lines) {
				modelBlockEnd = len(lines)
			}
			for j := blockStart; j < blockEnd; j++ {
				if promptMatch == nil {
					if m := rePromptTokens.FindStringSubmatch(lines[j]); m != nil {
						promptMatch = m
					}
				}
				if compMatch == nil {
					if m := reCompTokens.FindStringSubmatch(lines[j]); m != nil {
						compMatch = m
					}
				}
				if !cacheCreationSet {
					if m := reCacheCreation.FindStringSubmatch(lines[j]); m != nil {
						cacheCreation, _ = strconv.Atoi(m[1])
						cacheCreationSet = true
					}
				}
				if !cacheReadSet {
					if m := reCacheRead.FindStringSubmatch(lines[j]); m != nil {
						cacheReadVal, _ = strconv.Atoi(m[1])
						cacheReadSet = true
					} else if m := reCachedTokens.FindStringSubmatch(lines[j]); m != nil {
						cacheReadVal, _ = strconv.Atoi(m[1])
						cacheReadSet = true
					}
				}
				if promptMatch != nil && compMatch != nil && cacheCreationSet && cacheReadSet && lastModel != "unknown" {
					break
				}
			}
			if lastModel == "unknown" {
				for j := blockEnd; j < modelBlockEnd; j++ {
					if m := reModelJSON.FindStringSubmatch(lines[j]); m != nil {
						candidate := m[1]
						if strings.Contains(candidate, "claude") || strings.Contains(candidate, "gpt") || strings.Contains(candidate, "gemini") {
							lastModel = candidate
							break
						}
					}
				}
			}
			if promptMatch == nil || compMatch == nil {
				continue
			}
		}

		promptTokens, _ := strconv.Atoi(promptMatch[1])
		completionTokens, _ := strconv.Atoi(compMatch[1])
		promptText := ExtractPromptTextNearLine(lines, i)
		if promptText == nil {
			promptText = lastPromptText
		}

		records = append(records, domain.Record{
			Model:               lastModel,
			PromptTokens:        promptTokens,
			CompletionTokens:    completionTokens,
			PromptText:          promptText,
			CacheCreationTokens: cacheCreation,
			CacheReadTokens:     cacheReadVal,
			IsUserTurn:          IsUserInitiator(lastInitiator),
			Timestamp:           lastTimestamp,
			SessionID:           lastSession,
			LogFile:             filepath.Base(logPath),
		})
		lastInitiator = "agent"
	}
	return records
}
