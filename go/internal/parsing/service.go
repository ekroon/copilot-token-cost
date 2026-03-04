package parsing

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"copilot-token-cost/internal/domain"
)

var (
	reSession            = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[^"]*(?:Workspace initialized|Created ACP session|Flushed \d+ events to session)[: ]+([0-9a-f-]{36})\b`)
	reInitiator          = regexp.MustCompile(`PremiumRequestProcessor: Setting X-Initiator to '([^']*)'`)
	reInitiatorTelemetry = regexp.MustCompile(`^\s*"initiator"\s*:\s*"([^"]+)"`)
	reModelJSON          = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)
	rePromptTokens       = regexp.MustCompile(`"prompt_tokens"\s*:\s*(\d+)`)
	reCompTokens         = regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)
	reCacheCreation      = regexp.MustCompile(`"cache_creation_input_tokens"\s*:\s*(\d+)`)
	reCacheRead          = regexp.MustCompile(`"cache_read_input_tokens"\s*:\s*(\d+)`)
	reCachedTokens       = regexp.MustCompile(`"cached_tokens"\s*:\s*(\d+)`)
	reStatementLine      = regexp.MustCompile(`"statement"\s*:\s*("(?:\\.|[^"\\])*")\s*,?\s*$`)
)

type Service struct{}

func NewService() *Service { return &Service{} }

func (s *Service) ParseLogContent(content, source string) []domain.Record {
	return ParseLogContent(content, source, "", "")
}

func ParseLogContent(content, logPath, minTimestamp, maxTimestamp string) []domain.Record {
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
				if lastModel == "unknown" {
					if m := reModelJSON.FindStringSubmatch(lines[j]); m != nil {
						candidate := m[1]
						if strings.Contains(candidate, "claude") || strings.Contains(candidate, "gpt") || strings.Contains(candidate, "gemini") {
							lastModel = candidate
						}
					}
				}
				if promptMatch != nil && compMatch != nil && cacheCreationSet && cacheReadSet && lastModel != "unknown" {
					break
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

func IsUserInitiator(initiator string) bool {
	return strings.EqualFold(strings.TrimSpace(initiator), "user")
}

func ExtractPromptTextNearLine(lines []string, center int) *string {
	start := center - 20
	if start < 0 {
		start = 0
	}
	end := center + 6
	if end > len(lines) {
		end = len(lines)
	}
	for i := center; i >= start; i-- {
		if prompt := ExtractPromptTextFromLine(lines[i]); prompt != nil {
			return prompt
		}
		if prompt := extractPromptTextFromProblemStatementLine(lines, i); prompt != nil {
			return prompt
		}
	}
	for i := center + 1; i < end; i++ {
		if prompt := ExtractPromptTextFromLine(lines[i]); prompt != nil {
			return prompt
		}
		if prompt := extractPromptTextFromProblemStatementLine(lines, i); prompt != nil {
			return prompt
		}
	}
	return nil
}

func ExtractPromptTextFromLine(line string) *string {
	if !ContainsPromptIndicator(line) {
		return nil
	}
	if prompt := extractPromptTextFromJSONLine(line); prompt != nil {
		return prompt
	}
	if strings.Contains(line, `\"`) {
		unescaped := strings.ReplaceAll(line, `\"`, `"`)
		return extractPromptTextFromJSONLine(unescaped)
	}
	return nil
}

func ContainsPromptIndicator(line string) bool {
	for _, indicator := range []string{
		`"user"`,
		`\"user\"`,
		`"messages"`,
		`\"messages\"`,
		`"prompt"`,
		`"statement"`,
		`\"statement\"`,
	} {
		if strings.Contains(line, indicator) {
			return true
		}
	}
	return false
}

func extractPromptTextFromJSONLine(line string) *string {
	trimmed := strings.TrimSpace(line)
	candidates := []string{trimmed}
	if start := strings.IndexByte(trimmed, '{'); start >= 0 {
		if end := strings.LastIndexByte(trimmed, '}'); end > start {
			candidates = append(candidates, trimmed[start:end+1])
		}
	}
	for _, candidate := range candidates {
		var payload interface{}
		if err := json.Unmarshal([]byte(candidate), &payload); err != nil {
			continue
		}
		if prompt := extractPromptTextFromPayload(payload); prompt != nil {
			return prompt
		}
	}
	return nil
}

func extractPromptTextFromStatementLine(line string) *string {
	matches := reStatementLine.FindStringSubmatch(line)
	if matches == nil {
		return nil
	}
	unquoted, err := strconv.Unquote(matches[1])
	if err != nil {
		return nil
	}
	return promptTextPtr(unquoted)
}

func extractPromptTextFromProblemStatementLine(lines []string, index int) *string {
	prompt := extractPromptTextFromStatementLine(lines[index])
	if prompt == nil {
		return nil
	}
	start := index - 6
	if start < 0 {
		start = 0
	}
	for i := index - 1; i >= start; i-- {
		if strings.Contains(lines[i], `"problem"`) || strings.Contains(lines[i], `\"problem\"`) {
			return prompt
		}
	}
	return nil
}

func extractPromptTextFromPayload(payload interface{}) *string {
	switch v := payload.(type) {
	case map[string]interface{}:
		if hasUserContext(v) {
			if prompt := promptTextFromContent(v["content"]); prompt != nil {
				return prompt
			}
			if prompt := promptTextFromContent(v["text"]); prompt != nil {
				return prompt
			}
			if prompt := promptTextFromContent(v["prompt"]); prompt != nil {
				return prompt
			}
			if prompt := promptTextFromContent(v["statement"]); prompt != nil {
				return prompt
			}
		}
		if prompt := promptTextFromContent(v["user_prompt"]); prompt != nil {
			return prompt
		}
		if prompt := promptTextFromContent(v["prompt_text"]); prompt != nil {
			return prompt
		}
		if prompt := promptTextFromContent(v["problem"]); prompt != nil {
			return prompt
		}
		for _, child := range v {
			if prompt := extractPromptTextFromPayload(child); prompt != nil {
				return prompt
			}
		}
	case []interface{}:
		for _, child := range v {
			if prompt := extractPromptTextFromPayload(child); prompt != nil {
				return prompt
			}
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil
		}
		if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
			(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
			var nested interface{}
			if err := json.Unmarshal([]byte(trimmed), &nested); err == nil {
				return extractPromptTextFromPayload(nested)
			}
		}
	}
	return nil
}

func hasUserContext(v map[string]interface{}) bool {
	for _, key := range []string{"role", "author", "speaker", "initiator", "sender", "actor", "origin", "from", "kind", "type"} {
		if label, ok := v[key].(string); ok && isUserLabel(label) {
			return true
		}
	}
	for _, key := range []string{"is_user", "isUser"} {
		if isUser, ok := v[key].(bool); ok && isUser {
			return true
		}
	}
	return false
}

func isUserLabel(label string) bool {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "user", "human", "end-user", "end_user":
		return true
	default:
		return false
	}
}

func promptTextFromContent(content interface{}) *string {
	switch v := content.(type) {
	case string:
		return promptTextPtr(v)
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if part := promptTextPart(item); part != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return promptTextPtr(strings.Join(parts, "\n"))
	case map[string]interface{}:
		if prompt := promptTextFromContent(v["text"]); prompt != nil {
			return prompt
		}
		if prompt := promptTextFromContent(v["input_text"]); prompt != nil {
			return prompt
		}
		if prompt := promptTextFromContent(v["statement"]); prompt != nil {
			return prompt
		}
		if prompt := promptTextFromContent(v["content"]); prompt != nil {
			return prompt
		}
	}
	return nil
}

func promptTextPart(item interface{}) string {
	switch v := item.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]interface{}:
		if text, ok := v["text"].(string); ok {
			return strings.TrimSpace(text)
		}
		if text, ok := v["input_text"].(string); ok {
			return strings.TrimSpace(text)
		}
		if text, ok := v["statement"].(string); ok {
			return strings.TrimSpace(text)
		}
		if content, ok := v["content"].(string); ok {
			return strings.TrimSpace(content)
		}
	}
	return ""
}

func promptTextPtr(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	text := trimmed
	return &text
}
