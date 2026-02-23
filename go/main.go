package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// ─── API Pricing (per 1M tokens) ────────────────────────────────────────────

type Pricing struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type PricingPeriod struct {
	EffectiveFrom      string             `json:"effective_from"`
	PremiumRequestCost float64            `json:"premium_request_cost"`
	ModelPricing       map[string]Pricing `json:"model_pricing"`
	PremiumMultiplier  map[string]float64 `json:"premium_multiplier"`
}

type pricingFile struct {
	PricingPeriods []PricingPeriod `json:"pricing_periods"`
}

var pricingPeriods []PricingPeriod

func loadPricing() {
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)

	candidates := []string{
		filepath.Join(".", "pricing.json"),
		filepath.Join("..", "pricing.json"),
		filepath.Join(exeDir, "pricing.json"),
		filepath.Join(exeDir, "..", "pricing.json"),
	}

	var data []byte
	for _, path := range candidates {
		if d, err := os.ReadFile(path); err == nil {
			data = d
			break
		}
	}
	// Fall back to build-time embedded pricing (set by release workflow)
	if data == nil && embeddedPricingJSON != "" {
		data = []byte(embeddedPricingJSON)
	}
	if len(data) == 0 {
		fmt.Fprintf(os.Stderr, "Error: pricing.json not found\n")
		os.Exit(1)
	}

	var pf pricingFile
	if err := json.Unmarshal(data, &pf); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing pricing.json: %v\n", err)
		os.Exit(1)
	}

	pricingPeriods = pf.PricingPeriods
}

func getPeriod(timestamp string) *PricingPeriod {
	if timestamp == "" || len(pricingPeriods) == 0 {
		return &pricingPeriods[0]
	}
	dateStr := timestamp
	if len(dateStr) > 10 {
		dateStr = dateStr[:10]
	}
	for i := range pricingPeriods {
		if dateStr >= pricingPeriods[i].EffectiveFrom {
			return &pricingPeriods[i]
		}
	}
	return &pricingPeriods[len(pricingPeriods)-1]
}

func getPremiumRequestCost(timestamp string) float64 {
	return getPeriod(timestamp).PremiumRequestCost
}

var (
	reCapiRouting    = regexp.MustCompile(`^capi-[a-z]+-ptuc-[a-z0-9]+(?:-ib)?-`)
	reReasonEffort   = regexp.MustCompile(`:defaultReasoningEffort=\w+`)
	reDateStamp      = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)
	reTimestamp      = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})`)
	reSession        = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[^"]*(?:Workspace initialized|Created ACP session|Flushed \d+ events to session)[: ]+([0-9a-f-]{36})\b`)
	reInitiator      = regexp.MustCompile(`PremiumRequestProcessor: Setting X-Initiator to '([^']*)'`)
	reModelJSON      = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)
	rePromptTokens   = regexp.MustCompile(`"prompt_tokens"\s*:\s*(\d+)`)
	reCompTokens     = regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)
	reCacheCreation  = regexp.MustCompile(`"cache_creation_input_tokens"\s*:\s*(\d+)`)
	reCacheRead      = regexp.MustCompile(`"cache_read_input_tokens"\s*:\s*(\d+)`)
	reCachedTokens   = regexp.MustCompile(`"cached_tokens"\s*:\s*(\d+)`)
	reStatementLine  = regexp.MustCompile(`"statement"\s*:\s*("(?:\\.|[^"\\])*")\s*,?\s*$`)
	reCwd            = regexp.MustCompile(`cwd:\s*(.+)`)
	reBranch         = regexp.MustCompile(`branch:\s*(.+)`)
	reICloudObsidian = regexp.MustCompile(`~/Library/Mobile Documents/iCloud~md~obsidian/Documents/`)
)

func normalizeModel(name string) string {
	for _, prefix := range []string{"sweagent-capi:", "capi:"} {
		if strings.HasPrefix(name, prefix) {
			name = name[len(prefix):]
		}
	}
	name = reCapiRouting.ReplaceAllString(name, "")
	name = reReasonEffort.ReplaceAllString(name, "")
	name = reDateStamp.ReplaceAllString(name, "")
	return name
}

func getPricing(model string, timestamp string) *Pricing {
	n := normalizeModel(model)
	mp := getPeriod(timestamp).ModelPricing
	if p, ok := mp[n]; ok {
		return &p
	}
	for key, p := range mp {
		if strings.HasPrefix(n, key) || strings.HasPrefix(key, n) {
			cp := p
			return &cp
		}
	}
	return nil
}

func getPremiumMultiplier(model string, timestamp string) float64 {
	n := normalizeModel(model)
	mult := getPeriod(timestamp).PremiumMultiplier
	if m, ok := mult[n]; ok {
		return m
	}
	for key, m := range mult {
		if strings.HasPrefix(n, key) || strings.HasPrefix(key, n) {
			return m
		}
	}
	return 1
}

func isUserInitiator(initiator string) bool {
	return strings.EqualFold(strings.TrimSpace(initiator), "user")
}

func promptTextForStorage(promptText *string) sql.NullString {
	if promptText == nil {
		return sql.NullString{}
	}
	trimmed := strings.TrimSpace(*promptText)
	if trimmed == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: trimmed, Valid: true}
}

// ─── Record & Stats ─────────────────────────────────────────────────────────

type Record struct {
	Model               string
	PromptTokens        int
	CompletionTokens    int
	PromptText          *string
	CacheCreationTokens int
	CacheReadTokens     int
	IsUserTurn          bool
	Timestamp           string
	SessionID           string
	LogFile             string
	Source              string
}

type Stats struct {
	APICalls            int     `json:"api_calls"`
	UserTurns           int     `json:"user_turns,omitempty"`
	PromptTokens        int     `json:"prompt_tokens"`
	CompletionTokens    int     `json:"completion_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	PremiumRequests     float64 `json:"premium_requests"`
}

func newStats() *Stats { return &Stats{} }

func (s *Stats) add(r Record, model string) {
	s.APICalls++
	s.PromptTokens += r.PromptTokens
	s.CompletionTokens += r.CompletionTokens
	s.CacheCreationTokens += r.CacheCreationTokens
	s.CacheReadTokens += r.CacheReadTokens
	if r.IsUserTurn {
		s.UserTurns++
		s.PremiumRequests += getPremiumMultiplier(model, r.Timestamp)
	}
}

// ─── Log parsing ────────────────────────────────────────────────────────────

func parseLogFile(logPath string) []Record {
	return parseLogFileInRange(logPath, "", "")
}

func parseLogFileInRange(logPath string, minTimestamp, maxTimestamp string) []Record {
	content, tailUsed, err := readLogContentForRange(logPath, minTimestamp, maxTimestamp)
	if err != nil {
		return nil
	}
	records := parseLogContent(content, logPath, minTimestamp, maxTimestamp)
	if tailUsed && len(records) == 0 {
		data, err := os.ReadFile(logPath)
		if err != nil {
			return records
		}
		return parseLogContent(string(data), logPath, minTimestamp, maxTimestamp)
	}
	return records
}

func parseLogContent(content, logPath, minTimestamp, maxTimestamp string) []Record {
	lines := strings.Split(content, "\n")
	records := make([]Record, 0, strings.Count(content, `"completion_tokens"`))

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
		if strings.Contains(line, `"model"`) {
			if m := reModelJSON.FindStringSubmatch(line); m != nil {
				candidate := m[1]
				if !strings.HasPrefix(candidate, "{") && (strings.Contains(candidate, "claude") || strings.Contains(candidate, "gpt") || strings.Contains(candidate, "gemini")) {
					lastModel = candidate
				}
			}
		}
		if prompt := extractPromptTextFromLine(line); prompt != nil {
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
		promptText := extractPromptTextNearLine(lines, i)
		if promptText == nil {
			promptText = lastPromptText
		}

		records = append(records, Record{
			Model:               lastModel,
			PromptTokens:        promptTokens,
			CompletionTokens:    completionTokens,
			PromptText:          promptText,
			CacheCreationTokens: cacheCreation,
			CacheReadTokens:     cacheReadVal,
			IsUserTurn:          isUserInitiator(lastInitiator),
			Timestamp:           lastTimestamp,
			SessionID:           lastSession,
			LogFile:             filepath.Base(logPath),
		})
		lastInitiator = "agent"
	}
	return records
}

func extractPromptTextNearLine(lines []string, center int) *string {
	start := center - 20
	if start < 0 {
		start = 0
	}
	end := center + 6
	if end > len(lines) {
		end = len(lines)
	}
	for i := center; i >= start; i-- {
		if prompt := extractPromptTextFromLine(lines[i]); prompt != nil {
			return prompt
		}
		if prompt := extractPromptTextFromProblemStatementLine(lines, i); prompt != nil {
			return prompt
		}
	}
	for i := center + 1; i < end; i++ {
		if prompt := extractPromptTextFromLine(lines[i]); prompt != nil {
			return prompt
		}
		if prompt := extractPromptTextFromProblemStatementLine(lines, i); prompt != nil {
			return prompt
		}
	}
	return nil
}

func extractPromptTextFromLine(line string) *string {
	if !containsPromptIndicator(line) {
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

func containsPromptIndicator(line string) bool {
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

func readLogContentForRange(logPath, minTimestamp, maxTimestamp string) (string, bool, error) {
	if minTimestamp == "" && maxTimestamp == "" {
		data, err := os.ReadFile(logPath)
		return string(data), false, err
	}
	const tailBytes int64 = 4 * 1024 * 1024
	f, err := os.Open(logPath)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if info.Size() <= tailBytes {
		data, err := io.ReadAll(f)
		return string(data), false, err
	}
	if _, err := f.Seek(info.Size()-tailBytes, io.SeekStart); err != nil {
		return "", false, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", false, err
	}
	if idx := strings.IndexByte(string(data), '\n'); idx >= 0 {
		return string(data[idx+1:]), true, nil
	}
	return string(data), true, nil
}

type workspaceMeta struct {
	CWD    string
	Branch string
}

func loadSessionWorkspaces(sessionDir string) map[string]workspaceMeta {
	workspaces := make(map[string]workspaceMeta)
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return workspaces
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		wsFile := filepath.Join(sessionDir, entry.Name(), "workspace.yaml")
		data, err := os.ReadFile(wsFile)
		if err != nil {
			continue
		}
		content := string(data)
		m := reCwd.FindStringSubmatch(content)
		if m == nil {
			continue
		}
		meta := workspaceMeta{CWD: strings.TrimSpace(m[1])}
		if m := reBranch.FindStringSubmatch(content); m != nil {
			meta.Branch = strings.TrimSpace(m[1])
		}
		workspaces[entry.Name()] = meta
	}
	return workspaces
}

// ─── SQLite DB layer ────────────────────────────────────────────────────────

const schemaSQLGo = `
CREATE TABLE IF NOT EXISTS api_calls (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    model TEXT NOT NULL,
    model_normalized TEXT NOT NULL,
    prompt_tokens INTEGER NOT NULL,
    completion_tokens INTEGER NOT NULL,
    prompt_text TEXT,
    cache_creation_tokens INTEGER DEFAULT 0,
    cache_read_tokens INTEGER DEFAULT 0,
    is_user_turn INTEGER DEFAULT 0,
    timestamp TEXT,
    session_id TEXT,
    log_file TEXT,
    source TEXT DEFAULT 'local',
    UNIQUE(timestamp, model, prompt_tokens, completion_tokens, log_file, source)
);

CREATE TABLE IF NOT EXISTS parsed_logs (
    log_file TEXT NOT NULL,
    mtime REAL NOT NULL,
    source TEXT DEFAULT 'local',
    record_count INTEGER DEFAULT 0,
    parsed_at TEXT NOT NULL,
    PRIMARY KEY (log_file, source)
);

CREATE TABLE IF NOT EXISTS session_workspaces (
    session_id TEXT NOT NULL,
    cwd TEXT NOT NULL,
    source TEXT DEFAULT 'local',
    branch TEXT,
    PRIMARY KEY (session_id, source)
);

CREATE TABLE IF NOT EXISTS codespace_sync_state (
    codespace_name TEXT PRIMARY KEY,
    last_used_at TEXT,
    last_synced_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS codespace_tail_offsets (
    source TEXT NOT NULL,
    log_file TEXT NOT NULL,
    last_offset INTEGER NOT NULL DEFAULT 0,
    last_size INTEGER NOT NULL DEFAULT 0,
    last_mtime TEXT,
    last_hash TEXT,
    connection_state TEXT NOT NULL DEFAULT 'disconnected',
    last_error TEXT,
    last_chunk_at TEXT,
    last_full_copy_at TEXT,
    last_defensive_recopy_at TEXT,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (source, log_file)
);

CREATE INDEX IF NOT EXISTS idx_codespace_tail_offsets_source ON codespace_tail_offsets(source);

CREATE INDEX IF NOT EXISTS idx_api_calls_timestamp ON api_calls(timestamp);
CREATE INDEX IF NOT EXISTS idx_api_calls_model ON api_calls(model_normalized);
CREATE INDEX IF NOT EXISTS idx_api_calls_session ON api_calls(session_id);
`

func getDBPath() string {
	xdgStateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if xdgStateHome == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			xdgStateHome = filepath.Join(home, ".local", "state")
		}
	}
	if xdgStateHome == "" {
		xdgStateHome = filepath.Join(".local", "state")
	}
	xdgDBPath := filepath.Join(xdgStateHome, "copilot-token-cost", "copilot-tokens.db")
	_ = os.MkdirAll(filepath.Dir(xdgDBPath), 0o755)
	return xdgDBPath
}

func initDB(dbPath string) *sql.DB {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA synchronous=NORMAL")
	db.Exec("PRAGMA foreign_keys=ON")
	_, err = db.Exec(schemaSQLGo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating schema: %v\n", err)
		os.Exit(1)
	}
	migrateAPICallsSchema(db)
	migrateSessionWorkspacesSchema(db)
	return db
}

func migrateAPICallsSchema(db *sql.DB) {
	cols := apiCallColumns(db, "main")
	if !cols["prompt_text"] {
		_, _ = db.Exec("ALTER TABLE api_calls ADD COLUMN prompt_text TEXT")
	}
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_api_calls_prompt_text ON api_calls(prompt_text)")
}

func migrateSessionWorkspacesSchema(db *sql.DB) {
	cols := sessionWorkspaceColumns(db, "main")
	var pkCols sql.NullString
	_ = db.QueryRow(
		"SELECT group_concat(name, ',') FROM (" +
			"SELECT name FROM pragma_table_info('session_workspaces') WHERE pk > 0 ORDER BY pk" +
			")",
	).Scan(&pkCols)
	if !pkCols.Valid || pkCols.String != "session_id,source" {
		sourceExpr := "'local'"
		if cols["source"] {
			sourceExpr = "COALESCE(source, 'local')"
		}
		branchExpr := "NULL"
		if cols["branch"] {
			branchExpr = "branch"
		}
		_, _ = db.Exec("ALTER TABLE session_workspaces RENAME TO session_workspaces_old")
		_, _ = db.Exec(`CREATE TABLE session_workspaces (
    session_id TEXT NOT NULL,
    cwd TEXT NOT NULL,
    source TEXT DEFAULT 'local',
    branch TEXT,
    PRIMARY KEY (session_id, source)
)`)
		_, _ = db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source, branch) SELECT session_id, cwd, " + sourceExpr + ", " + branchExpr + " FROM session_workspaces_old")
		_, _ = db.Exec("DROP TABLE session_workspaces_old")
		cols = sessionWorkspaceColumns(db, "main")
	}
	if !cols["branch"] {
		_, _ = db.Exec("ALTER TABLE session_workspaces ADD COLUMN branch TEXT")
	}
}

func sessionWorkspaceColumns(db *sql.DB, schema string) map[string]bool {
	cols := make(map[string]bool)
	pragma := "PRAGMA table_info(session_workspaces)"
	if schema != "" {
		pragma = fmt.Sprintf("PRAGMA %s.table_info(session_workspaces)", schema)
	}
	rows, err := db.Query(pragma)
	if err != nil {
		return cols
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err == nil {
			cols[name] = true
		}
	}
	return cols
}

func apiCallColumns(db *sql.DB, schema string) map[string]bool {
	cols := make(map[string]bool)
	pragma := "PRAGMA table_info(api_calls)"
	if schema != "" {
		pragma = fmt.Sprintf("PRAGMA %s.table_info(api_calls)", schema)
	}
	rows, err := db.Query(pragma)
	if err != nil {
		return cols
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err == nil {
			cols[name] = true
		}
	}
	return cols
}

func isLogParsed(db *sql.DB, logFile string, mtime float64, source string) bool {
	var n int
	err := db.QueryRow("SELECT 1 FROM parsed_logs WHERE log_file = ? AND source = ? AND mtime = ?",
		logFile, source, mtime).Scan(&n)
	return err == nil
}

func markLogParsed(db *sql.DB, logFile string, mtime float64, recordCount int, source string) {
	db.Exec("INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) VALUES (?, ?, ?, ?, datetime('now'))",
		logFile, mtime, source, recordCount)
}

func insertRecords(db *sql.DB, records []Record, source string) {
	if len(records) == 0 {
		return
	}
	tx, err := db.Begin()
	if err != nil {
		return
	}
	stmt, err := tx.Prepare(
		"INSERT OR IGNORE INTO api_calls " +
			"(model, model_normalized, prompt_tokens, completion_tokens, " +
			"prompt_text, cache_creation_tokens, cache_read_tokens, is_user_turn, " +
			"timestamp, session_id, log_file, source) " +
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		tx.Rollback()
		return
	}
	defer stmt.Close()
	for _, r := range records {
		isUT := 0
		if r.IsUserTurn {
			isUT = 1
		}
		promptText := promptTextForStorage(r.PromptText)
		stmt.Exec(r.Model, normalizeModel(r.Model), r.PromptTokens, r.CompletionTokens,
			promptText, r.CacheCreationTokens, r.CacheReadTokens, isUT,
			r.Timestamp, r.SessionID, r.LogFile, source)
	}
	tx.Commit()
}

func upsertSessionWorkspace(db *sql.DB, sessionID, cwd, branch, source string) {
	var branchValue interface{}
	if strings.TrimSpace(branch) != "" {
		branchValue = branch
	}
	db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source, branch) VALUES (?, ?, ?, ?)",
		sessionID, cwd, source, branchValue)
}

func clearSource(db *sql.DB, source string) {
	db.Exec("DELETE FROM api_calls WHERE source = ?", source)
	db.Exec("DELETE FROM parsed_logs WHERE source = ?", source)
	db.Exec("DELETE FROM session_workspaces WHERE source = ?", source)
}

func getCodespaceLastUsed(db *sql.DB, codespaceName string) string {
	var lastUsed sql.NullString
	err := db.QueryRow(
		"SELECT last_used_at FROM codespace_sync_state WHERE codespace_name = ?",
		codespaceName,
	).Scan(&lastUsed)
	if err != nil || !lastUsed.Valid {
		return ""
	}
	return lastUsed.String
}

func upsertCodespaceSyncState(db *sql.DB, codespaceName string, lastUsedAt string) {
	db.Exec(
		"INSERT OR REPLACE INTO codespace_sync_state (codespace_name, last_used_at, last_synced_at) VALUES (?, ?, datetime('now'))",
		codespaceName,
		lastUsedAt,
	)
}

func buildFilters(dateFrom, dateTo, projectFilter string) (string, []interface{}) {
	var clauses []string
	var params []interface{}
	if dateFrom != "" {
		clauses = append(clauses, "a.timestamp >= ?")
		params = append(params, dateFrom)
	}
	if dateTo != "" {
		clauses = append(clauses, "a.timestamp < ?")
		params = append(params, dateTo)
	}
	if projectNeedle := strings.TrimSpace(strings.ToLower(projectFilter)); projectNeedle != "" {
		clauses = append(clauses,
			"LOWER(COALESCE(("+
				"SELECT sw.cwd FROM session_workspaces sw "+
				"WHERE sw.session_id = a.session_id AND sw.source = a.source LIMIT 1"+
				"), '(unknown)')) LIKE ?")
		params = append(params, "%"+projectNeedle+"%")
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	return where, params
}

type dbModelStats struct {
	APICalls            int
	PromptTokens        int
	CompletionTokens    int
	CacheCreationTokens int
	CacheReadTokens     int
	UserTurns           int
}

func queryModelStats(db *sql.DB, dateFrom, dateTo, projectFilter string) map[string]*dbModelStats {
	where, params := buildFilters(dateFrom, dateTo, projectFilter)
	q := "SELECT model_normalized, COUNT(*) AS api_calls, " +
		"SUM(prompt_tokens), SUM(completion_tokens), " +
		"SUM(cache_creation_tokens), SUM(cache_read_tokens), " +
		"SUM(CASE WHEN is_user_turn = 1 THEN 1 ELSE 0 END) " +
		"FROM api_calls a" + where + " GROUP BY model_normalized"
	rows, err := db.Query(q, params...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]*dbModelStats)
	for rows.Next() {
		var model string
		var s dbModelStats
		rows.Scan(&model, &s.APICalls, &s.PromptTokens, &s.CompletionTokens,
			&s.CacheCreationTokens, &s.CacheReadTokens, &s.UserTurns)
		result[model] = &s
	}
	return result
}

func queryDailyStats(db *sql.DB, dateFrom, dateTo, projectFilter string) map[string]map[string]*dbModelStats {
	where, params := buildFilters(dateFrom, dateTo, projectFilter)
	q := "SELECT substr(a.timestamp, 1, 10) AS day, model_normalized, " +
		"COUNT(*) AS api_calls, SUM(prompt_tokens), SUM(completion_tokens), " +
		"SUM(cache_creation_tokens), SUM(cache_read_tokens), " +
		"SUM(CASE WHEN is_user_turn = 1 THEN 1 ELSE 0 END) " +
		"FROM api_calls a" + where + " GROUP BY day, model_normalized"
	rows, err := db.Query(q, params...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]map[string]*dbModelStats)
	for rows.Next() {
		var day, model string
		var s dbModelStats
		rows.Scan(&day, &model, &s.APICalls, &s.PromptTokens, &s.CompletionTokens,
			&s.CacheCreationTokens, &s.CacheReadTokens, &s.UserTurns)
		if day == "" {
			day = "unknown"
		}
		if result[day] == nil {
			result[day] = make(map[string]*dbModelStats)
		}
		result[day][model] = &s
	}
	return result
}

func queryProjectStats(db *sql.DB, dateFrom, dateTo, projectFilter string) map[string]*dbModelStats {
	where, params := buildFilters(dateFrom, dateTo, projectFilter)
	q := "SELECT COALESCE(sw.cwd, '') AS cwd, COUNT(*) AS api_calls, " +
		"SUM(a.prompt_tokens), SUM(a.completion_tokens), " +
		"SUM(a.cache_creation_tokens), SUM(a.cache_read_tokens), " +
		"SUM(CASE WHEN a.is_user_turn = 1 THEN 1 ELSE 0 END) " +
		"FROM api_calls a LEFT JOIN session_workspaces sw ON a.session_id = sw.session_id AND a.source = sw.source" + where +
		" GROUP BY cwd"
	rows, err := db.Query(q, params...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]*dbModelStats)
	for rows.Next() {
		var cwd string
		var s dbModelStats
		rows.Scan(&cwd, &s.APICalls, &s.PromptTokens, &s.CompletionTokens,
			&s.CacheCreationTokens, &s.CacheReadTokens, &s.UserTurns)
		result[cwd] = &s
	}
	return result
}

func queryProjectModelStats(db *sql.DB, dateFrom, dateTo, projectFilter string) map[string]map[string]*dbModelStats {
	where, params := buildFilters(dateFrom, dateTo, projectFilter)
	q := "SELECT COALESCE(sw.cwd, '') AS cwd, a.model_normalized, COUNT(*) AS api_calls, " +
		"SUM(a.prompt_tokens), SUM(a.completion_tokens), " +
		"SUM(a.cache_creation_tokens), SUM(a.cache_read_tokens), " +
		"SUM(CASE WHEN a.is_user_turn = 1 THEN 1 ELSE 0 END) " +
		"FROM api_calls a LEFT JOIN session_workspaces sw ON a.session_id = sw.session_id AND a.source = sw.source" + where +
		" GROUP BY cwd, a.model_normalized"
	rows, err := db.Query(q, params...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]map[string]*dbModelStats)
	for rows.Next() {
		var cwd, model string
		var s dbModelStats
		rows.Scan(&cwd, &model, &s.APICalls, &s.PromptTokens, &s.CompletionTokens,
			&s.CacheCreationTokens, &s.CacheReadTokens, &s.UserTurns)
		if result[cwd] == nil {
			result[cwd] = make(map[string]*dbModelStats)
		}
		result[cwd][model] = &s
	}
	return result
}

func queryRecords(db *sql.DB, dateFrom, dateTo, projectFilter string) []Record {
	where, params := buildFilters(dateFrom, dateTo, projectFilter)
	q := "SELECT model, model_normalized, prompt_tokens, completion_tokens, " +
		"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
		"timestamp, session_id, log_file, source FROM api_calls a" + where
	rows, err := db.Query(q, params...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var records []Record
	for rows.Next() {
		var r Record
		var modelNorm string
		var isUT int
		var ts, sid, lf, src sql.NullString
		rows.Scan(&r.Model, &modelNorm, &r.PromptTokens, &r.CompletionTokens,
			&r.CacheCreationTokens, &r.CacheReadTokens, &isUT,
			&ts, &sid, &lf, &src)
		r.IsUserTurn = isUT == 1
		if ts.Valid {
			r.Timestamp = ts.String
		}
		if sid.Valid {
			r.SessionID = sid.String
		}
		if lf.Valid {
			r.LogFile = lf.String
		}
		if src.Valid {
			r.Source = src.String
		}
		records = append(records, r)
	}
	return records
}

func querySessionWorkspaces(db *sql.DB) map[string]workspaceMeta {
	branchExpr := "NULL"
	if sessionWorkspaceColumns(db, "")["branch"] {
		branchExpr = "branch"
	}
	rows, err := db.Query("SELECT session_id, cwd, source, " + branchExpr + " FROM session_workspaces")
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]workspaceMeta)
	for rows.Next() {
		var sid, cwd, source string
		var branch sql.NullString
		rows.Scan(&sid, &cwd, &source, &branch)
		meta := workspaceMeta{CWD: cwd}
		if branch.Valid {
			meta.Branch = branch.String
		}
		result[source+"\x1f"+sid] = meta
	}
	return result
}

func queryLogFileCount(db *sql.DB, dateFrom, dateTo, projectFilter string) int {
	where, params := buildFilters(dateFrom, dateTo, projectFilter)
	q := "SELECT COUNT(DISTINCT log_file) FROM api_calls a" + where
	var count int
	db.QueryRow(q, params...).Scan(&count)
	return count
}

func exportJSONL(db *sql.DB, outputPath string) {
	f, err := os.Create(outputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating export file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	apiCols := apiCallColumns(db, "main")
	promptTextExpr := "NULL"
	if apiCols["prompt_text"] {
		promptTextExpr = "prompt_text"
	}
	rows, _ := db.Query("SELECT model, model_normalized, prompt_tokens, completion_tokens, " +
		"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
		"timestamp, session_id, log_file, source, " + promptTextExpr + " FROM api_calls")
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var model, modelNorm, source string
			var pt, ct, cct, crt, isUT int
			var ts, sid, lf, promptText sql.NullString
			rows.Scan(&model, &modelNorm, &pt, &ct, &cct, &crt, &isUT, &ts, &sid, &lf, &source, &promptText)
			promptTextValue := interface{}(nil)
			if promptText.Valid {
				promptTextValue = promptText.String
			}
			rec := map[string]interface{}{
				"type": "api_call", "model": model, "model_normalized": modelNorm,
				"prompt_tokens": pt, "completion_tokens": ct,
				"cache_creation_tokens": cct, "cache_read_tokens": crt,
				"is_user_turn": isUT, "timestamp": ts.String,
				"session_id": sid.String, "log_file": lf.String, "source": source, "prompt_text": promptTextValue,
			}
			b, _ := json.Marshal(rec)
			w.Write(b)
			w.WriteByte('\n')
		}
	}

	rows2, _ := db.Query("SELECT session_id, cwd, source, branch FROM session_workspaces")
	if rows2 != nil {
		defer rows2.Close()
		for rows2.Next() {
			var sid, cwd, source string
			var branch sql.NullString
			rows2.Scan(&sid, &cwd, &source, &branch)
			branchValue := interface{}(nil)
			if branch.Valid {
				branchValue = branch.String
			}
			rec := map[string]interface{}{
				"type": "session_workspace", "session_id": sid, "cwd": cwd, "source": source, "branch": branchValue,
			}
			b, _ := json.Marshal(rec)
			w.Write(b)
			w.WriteByte('\n')
		}
	}
}

func importJSONL(db *sql.DB, inputPath, sourceOverride string) int {
	f, err := os.Open(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening import file: %v\n", err)
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	apiCols := apiCallColumns(db, "main")
	hasPromptText := apiCols["prompt_text"]
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		rtype, _ := obj["type"].(string)
		src := sourceOverride
		if src == "" {
			if s, ok := obj["source"].(string); ok && s != "" {
				src = s
			} else {
				src = "local"
			}
		}
		if rtype == "api_call" {
			isUT := 0
			if v, ok := obj["is_user_turn"].(float64); ok && v != 0 {
				isUT = 1
			}
			var promptText interface{}
			if v, ok := obj["prompt_text"].(string); ok {
				promptText = v
			}
			if hasPromptText {
				db.Exec("INSERT OR IGNORE INTO api_calls "+
					"(model, model_normalized, prompt_tokens, completion_tokens, "+
					"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
					"timestamp, session_id, log_file, source, prompt_text) "+
					"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
					obj["model"], obj["model_normalized"],
					int(obj["prompt_tokens"].(float64)), int(obj["completion_tokens"].(float64)),
					int(obj["cache_creation_tokens"].(float64)), int(obj["cache_read_tokens"].(float64)),
					isUT, obj["timestamp"], obj["session_id"], obj["log_file"], src, promptText)
			} else {
				db.Exec("INSERT OR IGNORE INTO api_calls "+
					"(model, model_normalized, prompt_tokens, completion_tokens, "+
					"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
					"timestamp, session_id, log_file, source) "+
					"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
					obj["model"], obj["model_normalized"],
					int(obj["prompt_tokens"].(float64)), int(obj["completion_tokens"].(float64)),
					int(obj["cache_creation_tokens"].(float64)), int(obj["cache_read_tokens"].(float64)),
					isUT, obj["timestamp"], obj["session_id"], obj["log_file"], src)
			}
		} else if rtype == "session_workspace" {
			var branch interface{}
			if b, ok := obj["branch"].(string); ok && strings.TrimSpace(b) != "" {
				branch = b
			}
			db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source, branch) VALUES (?, ?, ?, ?)",
				obj["session_id"], obj["cwd"], src, branch)
		}
		count++
	}
	return count
}

func importSQLiteDB(db *sql.DB, otherDBPath, sourceOverride string) int {
	db.Exec("ATTACH DATABASE ? AS import_db", otherDBPath)
	targetAPICallCols := apiCallColumns(db, "main")
	importAPICallCols := apiCallColumns(db, "import_db")
	importPromptTextExpr := "NULL"
	if importAPICallCols["prompt_text"] {
		importPromptTextExpr = "prompt_text"
	}
	importCols := sessionWorkspaceColumns(db, "import_db")
	sourceExpr := "'local'"
	if importCols["source"] {
		sourceExpr = "COALESCE(source, 'local')"
	}
	branchExpr := "NULL"
	if importCols["branch"] {
		branchExpr = "branch"
	}
	var count int64
	if sourceOverride != "" {
		if targetAPICallCols["prompt_text"] {
			db.Exec("INSERT OR IGNORE INTO api_calls "+
				"(model, model_normalized, prompt_tokens, completion_tokens, "+
				"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
				"timestamp, session_id, log_file, source, prompt_text) "+
				"SELECT model, model_normalized, prompt_tokens, completion_tokens, "+
				"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
				"timestamp, session_id, log_file, ?, "+importPromptTextExpr+" FROM import_db.api_calls", sourceOverride)
		} else {
			db.Exec("INSERT OR IGNORE INTO api_calls "+
				"(model, model_normalized, prompt_tokens, completion_tokens, "+
				"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
				"timestamp, session_id, log_file, source) "+
				"SELECT model, model_normalized, prompt_tokens, completion_tokens, "+
				"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
				"timestamp, session_id, log_file, ? FROM import_db.api_calls", sourceOverride)
		}
		db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source, branch) "+
			"SELECT session_id, cwd, ?, "+branchExpr+" FROM import_db.session_workspaces", sourceOverride)
		db.Exec("INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) "+
			"SELECT log_file, mtime, ?, record_count, parsed_at FROM import_db.parsed_logs", sourceOverride)
	} else {
		if targetAPICallCols["prompt_text"] {
			db.Exec("INSERT OR IGNORE INTO api_calls " +
				"(model, model_normalized, prompt_tokens, completion_tokens, " +
				"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
				"timestamp, session_id, log_file, source, prompt_text) " +
				"SELECT model, model_normalized, prompt_tokens, completion_tokens, " +
				"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
				"timestamp, session_id, log_file, source, " + importPromptTextExpr + " FROM import_db.api_calls")
		} else {
			db.Exec("INSERT OR IGNORE INTO api_calls " +
				"(model, model_normalized, prompt_tokens, completion_tokens, " +
				"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
				"timestamp, session_id, log_file, source) " +
				"SELECT model, model_normalized, prompt_tokens, completion_tokens, " +
				"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
				"timestamp, session_id, log_file, source FROM import_db.api_calls")
		}
		db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source, branch) " +
			"SELECT session_id, cwd, " + sourceExpr + ", " + branchExpr + " FROM import_db.session_workspaces")
		db.Exec("INSERT OR REPLACE INTO parsed_logs SELECT * FROM import_db.parsed_logs")
	}
	db.QueryRow("SELECT changes()").Scan(&count)
	db.Exec("DETACH DATABASE import_db")
	return int(count)
}

func syncLogsToDB(db *sql.DB, logsDir, sessionDir string, force bool, source string, minTime, maxTime *time.Time) int {
	var existing int
	db.QueryRow("SELECT COUNT(*) FROM api_calls WHERE source = ?", source).Scan(&existing)
	matches, _ := filepath.Glob(filepath.Join(logsDir, "process-*.log"))
	sort.Strings(matches)
	if len(matches) > 0 {
		fmt.Fprintf(os.Stderr, "  🔎 Scanning %d log files (%s)\n", len(matches), source)
	}

	if force {
		// Clear parse tracker so all logs are re-parsed; keep existing api_calls (INSERT OR IGNORE handles dedup)
		db.Exec("DELETE FROM parsed_logs WHERE source = ?", source)
		fmt.Fprintf(os.Stderr, "  🔄 Force re-sync (%s): re-parsing %d log files (keeping %s existing records)\n", source, len(matches), addCommas(strconv.Itoa(existing)))
	}

	totalInserted := 0
	parsedCount := 0
	minTimestamp := ""
	maxTimestamp := ""
	if minTime != nil {
		minTimestamp = minTime.Format("2006-01-02T15:04:05")
	}
	if maxTime != nil {
		maxTimestamp = maxTime.Format("2006-01-02T15:04:05")
	}
	parsedMtimeByFile := map[string]float64{}
	if !force {
		rows, err := db.Query("SELECT log_file, mtime FROM parsed_logs WHERE source = ?", source)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var file string
				var mtime float64
				if err := rows.Scan(&file, &mtime); err == nil {
					parsedMtimeByFile[file] = mtime
				}
			}
		}
	}
	var tx *sql.Tx
	var insertStmt *sql.Stmt
	var parsedStmt *sql.Stmt

	for _, logPath := range matches {
		filename := filepath.Base(logPath)
		info, err := os.Stat(logPath)
		if err != nil {
			continue
		}
		if !force {
			modTime := info.ModTime()
			if minTime != nil && modTime.Before(*minTime) {
				continue
			}
			if maxTime != nil && !modTime.Before(*maxTime) {
				continue
			}
		}
		mtime := float64(info.ModTime().UnixMilli()) / 1000.0
		if !force {
			if parsedMtime, ok := parsedMtimeByFile[filename]; ok && parsedMtime == mtime {
				continue
			}
		}
		if tx == nil {
			tx, err = db.Begin()
			if err != nil {
				return 0
			}
			insertStmt, err = tx.Prepare(
				"INSERT INTO api_calls " +
					"(model, model_normalized, prompt_tokens, completion_tokens, " +
					"prompt_text, cache_creation_tokens, cache_read_tokens, is_user_turn, " +
					"timestamp, session_id, log_file, source) " +
					"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) " +
					"ON CONFLICT(timestamp, model, prompt_tokens, completion_tokens, log_file, source) DO UPDATE SET " +
					"prompt_text = CASE " +
					"WHEN COALESCE(api_calls.prompt_text, '') = '' AND COALESCE(excluded.prompt_text, '') <> '' THEN excluded.prompt_text " +
					"ELSE api_calls.prompt_text END, " +
					"session_id = CASE " +
					"WHEN COALESCE(excluded.session_id, '') <> '' THEN excluded.session_id " +
					"ELSE api_calls.session_id END")
			if err != nil {
				_ = tx.Rollback()
				return 0
			}
			parsedStmt, err = tx.Prepare("INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) VALUES (?, ?, ?, ?, datetime('now'))")
			if err != nil {
				_ = insertStmt.Close()
				_ = tx.Rollback()
				return 0
			}
		}
		records := parseLogFileInRange(logPath, minTimestamp, maxTimestamp)
		for _, r := range records {
			isUT := 0
			if r.IsUserTurn {
				isUT = 1
			}
			promptText := promptTextForStorage(r.PromptText)
			_, _ = insertStmt.Exec(r.Model, normalizeModel(r.Model), r.PromptTokens, r.CompletionTokens,
				promptText, r.CacheCreationTokens, r.CacheReadTokens, isUT,
				r.Timestamp, r.SessionID, r.LogFile, source)
		}
		_, _ = parsedStmt.Exec(filename, mtime, source, len(records))
		totalInserted += len(records)
		parsedCount++
		if force {
			fmt.Fprintf(os.Stderr, "  📄 [%d/%d] %s (%d records)\n", parsedCount, len(matches), filename, len(records))
		}
	}
	if tx != nil {
		_ = insertStmt.Close()
		_ = parsedStmt.Close()
		if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
			return 0
		}
	}

	if parsedCount > 0 {
		workspaces := loadSessionWorkspaces(sessionDir)
		for sessionID, meta := range workspaces {
			upsertSessionWorkspace(db, sessionID, meta.CWD, meta.Branch, source)
		}
	}

	if parsedCount > 0 {
		var totalNow int
		db.QueryRow("SELECT COUNT(*) FROM api_calls WHERE source = ?", source).Scan(&totalNow)
		newRecords := totalNow - existing
		fmt.Fprintf(os.Stderr, "  ✅ Synced %d log files (%s): %s new records (%s total)\n", parsedCount, source, addCommas(strconv.Itoa(newRecords)), addCommas(strconv.Itoa(totalNow)))
	}

	return totalInserted
}

type codespaceInfo struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	LastUsedAt string `json:"lastUsedAt"`
}

type codespaceCopyResult struct {
	Idx        int
	Codespace  codespaceInfo
	TmpDir     string
	LogsDir    string
	SessionDir string
	Copied     bool
}

func listCodespaces(includeStopped bool) []codespaceInfo {
	fmt.Fprintf(os.Stderr, "  🔄 Codespaces: listing...\n")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "cs", "list", "--json", "name,state,lastUsedAt", "--limit", "1000")
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "  ⚠️ Codespaces sync skipped: listing timed out\n")
			return nil
		}
		fmt.Fprintf(os.Stderr, "  ⚠️ Codespaces sync skipped: failed to list codespaces\n")
		return nil
	}
	var all []codespaceInfo
	if err := json.Unmarshal(out, &all); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️ Codespaces sync skipped: invalid JSON from gh cs list\n")
		return nil
	}
	allowed := map[string]bool{"Available": true}
	if includeStopped {
		allowed["Shutdown"] = true
	}
	var filtered []codespaceInfo
	for _, cs := range all {
		if cs.Name != "" && allowed[cs.State] {
			filtered = append(filtered, cs)
		}
	}
	fmt.Fprintf(os.Stderr, "  📦 Codespaces: %d to sync\n", len(filtered))
	return filtered
}

func isCodespaceStartThrottleError(msg string) bool {
	if msg == "" {
		return false
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "too many codespaces starting") ||
		(strings.Contains(lower, "http 400") && strings.Contains(lower, "codespaces"))
}

func codespaceThrottleBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 4 {
		attempt = 4
	}
	return time.Duration(1<<attempt) * time.Second
}

func summarizeSyncCommandStderr(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return ""
	}
	oneLine := strings.Join(strings.Fields(trimmed), " ")
	if len(oneLine) > 240 {
		return oneLine[:240] + "..."
	}
	return oneLine
}

func formatSshTarFailure(sshErr, tarErr error, sshStderr, tarStderr string) string {
	var parts []string
	if sshErr != nil {
		parts = append(parts, fmt.Sprintf("ssh error: %v", sshErr))
	}
	if tarErr != nil {
		parts = append(parts, fmt.Sprintf("extract error: %v", tarErr))
	}
	if msg := summarizeSyncCommandStderr(sshStderr); msg != "" {
		parts = append(parts, "ssh stderr: "+msg)
	}
	if msg := summarizeSyncCommandStderr(tarStderr); msg != "" {
		parts = append(parts, "extract stderr: "+msg)
	}
	return strings.Join(parts, "; ")
}

func copyCodespaceData(cs codespaceInfo, idx, total int, stoppedStartLimiter chan struct{}) codespaceCopyResult {
	res := codespaceCopyResult{
		Idx:       idx,
		Codespace: cs,
	}
	shouldStop := cs.State == "Shutdown"
	tmpDir, err := os.MkdirTemp("", "copilot-cs-")
	if err != nil {
		return res
	}
	res.TmpDir = tmpDir

	if shouldStop && stoppedStartLimiter != nil {
		stoppedStartLimiter <- struct{}{}
		defer func() { <-stoppedStartLimiter }()
	}

	if shouldStop {
		defer func() {
			stopStart := time.Now()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer stopCancel()
			stopCmd := exec.CommandContext(stopCtx, "gh", "cs", "stop", "-c", cs.Name)
			stopCmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
			_ = stopCmd.Run()
			fmt.Fprintf(os.Stderr, "  🛑 Stopping %s... (%.1fs)\n", cs.Name, time.Since(stopStart).Seconds())
		}()
	}

	stage := filepath.Join(tmpDir, cs.Name)
	_ = os.MkdirAll(stage, 0755)
	fmt.Fprintf(os.Stderr, "  📦 [%d/%d] Copying %s...\n", idx+1, total, cs.Name)
	cpStart := time.Now()
	cpCtx, cpCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cpCancel()

	// Try ssh+tar first (targeted paths, compressed stream)
	copied := false
	sshTarCmd := exec.CommandContext(cpCtx, "gh", "cs", "ssh", "-c", cs.Name, "--",
		"tar", "czf", "-", "-C", "/home/vscode", ".copilot/logs", ".copilot/session-state")
	sshTarCmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	var sshErrBuf bytes.Buffer
	sshTarCmd.Stderr = &sshErrBuf
	if pipe, pipeErr := sshTarCmd.StdoutPipe(); pipeErr == nil {
		tarExtract := exec.CommandContext(cpCtx, "tar", "xzf", "-", "-C", stage)
		tarExtract.Stdin = pipe
		var tarErrBuf bytes.Buffer
		tarExtract.Stderr = &tarErrBuf
		if sshErr := sshTarCmd.Start(); sshErr == nil {
			if tarErr := tarExtract.Start(); tarErr == nil {
				tarWaitErr := tarExtract.Wait()
				sshWaitErr := sshTarCmd.Wait()
				if sshWaitErr == nil && tarWaitErr == nil {
					fmt.Fprintf(os.Stderr, "  ✅ Copied %s via ssh+tar (%.1fs)\n", cs.Name, time.Since(cpStart).Seconds())
					copied = true
				} else {
					detail := formatSshTarFailure(sshWaitErr, tarWaitErr, sshErrBuf.String(), tarErrBuf.String())
					if detail != "" {
						fmt.Fprintf(os.Stderr, "  ⚠️ ssh+tar failed for %s (%.1fs): %s; falling back to gh cs cp\n", cs.Name, time.Since(cpStart).Seconds(), detail)
					} else {
						fmt.Fprintf(os.Stderr, "  ⚠️ ssh+tar failed for %s (%.1fs), falling back to gh cs cp\n", cs.Name, time.Since(cpStart).Seconds())
					}
				}
			}
		}
	}

	// Fallback: gh cs cp (original approach, copies all of .copilot/)
	if !copied {
		const maxThrottleRetries = 3
		for attempt := 1; attempt <= maxThrottleRetries; attempt++ {
			cpStart = time.Now()
			cpCmd := exec.CommandContext(cpCtx, "gh", "cs", "cp", "-e", "-r", "-c", cs.Name, "remote:/home/vscode/.copilot", stage)
			cpCmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
			var cpErrBuf bytes.Buffer
			cpCmd.Stdout = io.Discard
			cpCmd.Stderr = &cpErrBuf
			cpErr := cpCmd.Run()
			if cpErr == nil {
				fmt.Fprintf(os.Stderr, "  ✅ Copied %s (%.1fs)\n", cs.Name, time.Since(cpStart).Seconds())
				copied = true
				break
			}
			if cpCtx.Err() == context.DeadlineExceeded {
				fmt.Fprintf(os.Stderr, "  ⚠️ Failed to copy %s: timed out after %.1fs\n", cs.Name, time.Since(cpStart).Seconds())
				return res
			}
			msg := strings.TrimSpace(cpErrBuf.String())
			if isCodespaceStartThrottleError(msg) && attempt < maxThrottleRetries {
				wait := codespaceThrottleBackoff(attempt)
				fmt.Fprintf(os.Stderr, "  ⏳ Start throttled for %s, retrying copy in %.0fs (%d/%d)\n", cs.Name, wait.Seconds(), attempt+1, maxThrottleRetries)
				select {
				case <-time.After(wait):
					continue
				case <-cpCtx.Done():
					fmt.Fprintf(os.Stderr, "  ⚠️ Failed to copy %s: timed out while waiting to retry\n", cs.Name)
					return res
				}
			}
			if strings.Contains(msg, "No such file or directory") {
				fmt.Fprintf(os.Stderr, "  ⚠️ Skipping %s: /home/vscode/.copilot not found\n", cs.Name)
			} else {
				if msg == "" {
					msg = "gh cs cp failed"
				}
				fmt.Fprintf(os.Stderr, "  ⚠️ Failed to copy %s: %s (%.1fs)\n", cs.Name, msg, time.Since(cpStart).Seconds())
			}
			return res
		}
		if !copied {
			return res
		}
	}

	copilotDir := filepath.Join(stage, ".copilot")
	if _, err := os.Stat(filepath.Join(copilotDir, "logs")); err != nil {
		alt := filepath.Join(stage, "home", "vscode", ".copilot")
		if _, altErr := os.Stat(filepath.Join(alt, "logs")); altErr == nil {
			copilotDir = alt
		}
	}
	logsDir := filepath.Join(copilotDir, "logs")
	sessionDir := filepath.Join(copilotDir, "session-state")
	if _, err := os.Stat(logsDir); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️ Skipping %s: no .copilot/logs in copied data\n", cs.Name)
		return res
	}

	fileCount, totalBytes := dirStats(logsDir)
	fmt.Fprintf(os.Stderr, "  📊 %s: %d log files, %s copied\n", cs.Name, fileCount, humanSize(totalBytes))

	res.LogsDir = logsDir
	res.SessionDir = sessionDir
	res.Copied = true
	return res
}

func humanSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func dirStats(root string) (int, int64) {
	var count int
	var total int64
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil {
			count++
			total += info.Size()
		}
		return nil
	})
	return count, total
}

func listRemoteLogFiles(csName string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "cs", "ssh", "-c", csName, "--",
		"ls", "/home/vscode/.copilot/logs/")
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.TrimSpace(line)
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

func getKnownLogFiles(db *sql.DB, source string) map[string]bool {
	known := map[string]bool{}
	rows, err := db.Query("SELECT log_file FROM parsed_logs WHERE source = ?", source)
	if err != nil {
		return known
	}
	defer rows.Close()
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err == nil {
			known[f] = true
		}
	}
	return known
}

func syncCodespacesToDBTick(db *sql.DB, includeStopped bool, force bool) (int, error) {
	codespaces := listCodespaces(includeStopped)
	if codespaces == nil {
		return 0, fmt.Errorf("failed to list codespaces")
	}
	if len(codespaces) == 0 {
		return 0, nil
	}
	var pending []codespaceInfo
	for _, cs := range codespaces {
		if cs.State != "Available" && cs.LastUsedAt != "" && getCodespaceLastUsed(db, cs.Name) == cs.LastUsedAt {
			fmt.Fprintf(os.Stderr, "  ⏭️  Skipping %s (shutdown, unchanged lastUsedAt)\n", cs.Name)
			continue
		}
		if !force && cs.State != "Available" {
			source := "codespace:" + cs.Name
			known := getKnownLogFiles(db, source)
			if len(known) > 0 {
				remoteFiles, err := listRemoteLogFiles(cs.Name)
				if err == nil && len(remoteFiles) > 0 {
					allKnown := true
					for _, f := range remoteFiles {
						if !known[f] {
							allKnown = false
							break
						}
					}
					if allKnown {
						fmt.Fprintf(os.Stderr, "  ⏭️  Skipping %s copy: all %d log files already synced\n", cs.Name, len(remoteFiles))
						continue
					}
				}
			}
		}
		pending = append(pending, cs)
	}
	if len(pending) == 0 {
		return 0, nil
	}

	var stoppedPending int
	for _, cs := range pending {
		if cs.State == "Shutdown" {
			stoppedPending++
		}
	}
	var stoppedStartLimiter chan struct{}
	if stoppedPending > 0 {
		stoppedStartLimiter = make(chan struct{}, 1)
		fmt.Fprintf(os.Stderr, "  🧯 Stopped Codespaces startup parallelism: 1 (%d stopped)\n", stoppedPending)
	}

	workers := 4
	fmt.Fprintf(os.Stderr, "  🚚 Codespaces copy parallelism: %d workers (%d pending)\n", workers, len(pending))
	jobs := make(chan int, len(pending))
	results := make(chan codespaceCopyResult, len(pending))
	for w := 0; w < workers; w++ {
		go func() {
			for idx := range jobs {
				results <- copyCodespaceData(pending[idx], idx, len(pending), stoppedStartLimiter)
			}
		}()
	}
	for i := 0; i < len(pending); i++ {
		jobs <- i
	}
	close(jobs)

	ordered := make([]codespaceCopyResult, len(pending))
	for i := 0; i < len(pending); i++ {
		res := <-results
		ordered[res.Idx] = res
	}

	total := 0
	failedCopies := 0
	for _, res := range ordered {
		if res.Copied {
			total += syncLogsToDB(db, res.LogsDir, res.SessionDir, force, "codespace:"+res.Codespace.Name, nil, nil)
			upsertCodespaceSyncState(db, res.Codespace.Name, res.Codespace.LastUsedAt)
		} else {
			failedCopies++
		}
		if res.TmpDir != "" {
			_ = os.RemoveAll(res.TmpDir)
		}
	}
	if failedCopies > 0 {
		return total, fmt.Errorf("codespaces sync incomplete: %d of %d copies failed", failedCopies, len(pending))
	}
	return total, nil
}

func syncCodespacesToDB(db *sql.DB, includeStopped bool, force bool) int {
	total, _ := syncCodespacesToDBTick(db, includeStopped, force)
	return total
}

func projectName(cwd string) string {
	home, _ := os.UserHomeDir()
	path := strings.Replace(cwd, home, "~", 1)
	path = reICloudObsidian.ReplaceAllString(path, "📓 ")
	return path
}

// ─── Cost helpers ───────────────────────────────────────────────────────────

func calcCost(model string, s *Stats, timestamp string) float64 {
	p := getPricing(model, timestamp)
	if p == nil {
		return 0.0
	}
	netInput := s.PromptTokens - s.CacheReadTokens - s.CacheCreationTokens
	if netInput < 0 {
		netInput = 0
	}
	return float64(netInput)/1e6*p.Input +
		float64(s.CompletionTokens)/1e6*p.Output +
		float64(s.CacheReadTokens)/1e6*p.CacheRead +
		float64(s.CacheCreationTokens)/1e6*p.CacheWrite
}

func calcCostNocache(model string, s *Stats, timestamp string) float64 {
	p := getPricing(model, timestamp)
	if p == nil {
		return 0.0
	}
	return float64(s.PromptTokens)/1e6*p.Input +
		float64(s.CompletionTokens)/1e6*p.Output
}

func sumDailyCost(model string, dailyStats map[string]map[string]*Stats, costFn func(string, *Stats, string) float64) float64 {
	var total float64
	for day, models := range dailyStats {
		if s, ok := models[model]; ok {
			total += costFn(model, s, day)
		}
	}
	return total
}

func sumDailyPremCost(model string, dailyStats map[string]map[string]*Stats) float64 {
	var total float64
	for day, models := range dailyStats {
		if s, ok := models[model]; ok {
			total += s.PremiumRequests * getPremiumRequestCost(day)
		}
	}
	return total
}

func sumDailyPremCostAll(dailyStats map[string]map[string]*Stats) float64 {
	var total float64
	for day, models := range dailyStats {
		prc := getPremiumRequestCost(day)
		for _, s := range models {
			total += s.PremiumRequests * prc
		}
	}
	return total
}

func uncachedInput(s *Stats) int {
	v := s.PromptTokens - s.CacheReadTokens - s.CacheCreationTokens
	if v < 0 {
		return 0
	}
	return v
}

func cacheHitPct(promptTokens, cacheReadTokens int) string {
	if promptTokens == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", float64(cacheReadTokens)/float64(promptTokens)*100)
}

func fmtTokens(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%s", commaFloat(float64(n)/1e6, 1)+"M")
	}
	if n >= 1_000 {
		return fmt.Sprintf("%s", commaFloat(float64(n)/1e3, 1)+"K")
	}
	return strconv.Itoa(n)
}

func commaFloat(f float64, decimals int) string {
	format := fmt.Sprintf("%%.%df", decimals)
	s := fmt.Sprintf(format, f)
	return addCommas(s)
}

func addCommas(s string) string {
	dotIdx := strings.Index(s, ".")
	intPart := s
	decPart := ""
	if dotIdx >= 0 {
		intPart = s[:dotIdx]
		decPart = s[dotIdx:]
	}
	negative := false
	if strings.HasPrefix(intPart, "-") {
		negative = true
		intPart = intPart[1:]
	}
	var result []byte
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	prefix := ""
	if negative {
		prefix = "-"
	}
	return prefix + string(result) + decPart
}

func fmtCost(cost float64) string {
	if cost >= 100 {
		return "$" + addCommas(fmt.Sprintf("%.0f", cost))
	}
	if cost >= 1 {
		return "$" + addCommas(fmt.Sprintf("%.2f", cost))
	}
	return "$" + addCommas(fmt.Sprintf("%.3f", cost))
}

func commaInt(n int) string {
	return addCommas(strconv.Itoa(n))
}

// ─── Display width ──────────────────────────────────────────────────────────

func displayWidth(s string) int {
	runes := []rune(s)
	w := 0
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		// VS16 (emoji presentation selector)
		if i+1 < len(runes) && runes[i+1] == '\uFE0F' {
			w += 2
			i++
			continue
		}
		if unicode.In(ch, unicode.Mn, unicode.Mc, unicode.Me) {
			continue
		}
		if isWideRune(ch) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func isWideRune(r rune) bool {
	// East Asian Width W/F ranges
	if r >= 0x1100 && r <= 0x115F {
		return true
	}
	if r >= 0x2E80 && r <= 0x303E {
		return true
	}
	if r >= 0x3040 && r <= 0x33BF {
		return true
	}
	if r >= 0x3400 && r <= 0x4DBF {
		return true
	}
	if r >= 0x4E00 && r <= 0xA4CF {
		return true
	}
	if r >= 0xA960 && r <= 0xA97C {
		return true
	}
	if r >= 0xAC00 && r <= 0xD7FF {
		return true
	}
	if r >= 0xF900 && r <= 0xFAFF {
		return true
	}
	if r >= 0xFE10 && r <= 0xFE6F {
		return true
	}
	if r >= 0xFF01 && r <= 0xFF60 {
		return true
	}
	if r >= 0xFFE0 && r <= 0xFFE6 {
		return true
	}
	if r >= 0x1F000 && r <= 0x1FFFF {
		return true
	}
	if r >= 0x20000 && r <= 0x2FFFF {
		return true
	}
	if r >= 0x30000 && r <= 0x3FFFF {
		return true
	}
	return false
}

func padCell(cell string, width int, alignRight bool) string {
	dw := displayWidth(cell)
	padding := width - dw
	if padding < 0 {
		padding = 0
	}
	if alignRight {
		return strings.Repeat(" ", padding) + cell
	}
	return cell + strings.Repeat(" ", padding)
}

// ─── Pretty table ───────────────────────────────────────────────────────────

func printTable(title string, headers []string, rows [][]string, footer []string, notes []string) {
	// All rows for width calculation
	allRows := [][]string{headers}
	allRows = append(allRows, rows...)
	if footer != nil {
		allRows = append(allRows, footer)
	}

	colWidths := make([]int, len(headers))
	for _, row := range allRows {
		for i, cell := range row {
			w := displayWidth(cell)
			if w > colWidths[i] {
				colWidths[i] = w
			}
		}
	}

	innerWidth := 4 // leading 2 + trailing 2 spaces
	for i, w := range colWidths {
		innerWidth += w
		if i > 0 {
			innerWidth += 2 // gap between columns
		}
	}

	fmtRow := func(cells []string) string {
		var parts []string
		for i, cell := range cells {
			parts = append(parts, padCell(cell, colWidths[i], i > 0))
		}
		content := "  " + strings.Join(parts, "  ")
		contentWidth := displayWidth(content)
		pad := innerWidth - contentWidth
		if pad < 0 {
			pad = 0
		}
		return "│" + content + strings.Repeat(" ", pad) + "│"
	}

	separator := func() string {
		var parts []string
		for _, w := range colWidths {
			parts = append(parts, strings.Repeat("─", w))
		}
		content := "  " + strings.Join(parts, "  ") + "  "
		pad := innerWidth - len(content)
		if pad < 0 {
			pad = 0
		}
		return "│" + content + strings.Repeat(" ", pad) + "│"
	}

	bar := strings.Repeat("─", innerWidth)
	titlePad := innerWidth - displayWidth(title) - 3
	if titlePad < 0 {
		titlePad = 0
	}

	fmt.Printf("┌─ %s %s┐\n", title, strings.Repeat("─", titlePad))
	fmt.Printf("│%s│\n", strings.Repeat(" ", innerWidth))
	fmt.Println(fmtRow(headers))
	fmt.Println(separator())
	for _, row := range rows {
		fmt.Println(fmtRow(row))
	}
	if footer != nil {
		fmt.Println(separator())
		fmt.Println(fmtRow(footer))
	}
	fmt.Printf("│%s│\n", strings.Repeat(" ", innerWidth))
	fmt.Printf("└%s┘\n", bar)
	for _, note := range notes {
		fmt.Printf("  %s\n", note)
	}
}

// ─── Timestamp parsing ──────────────────────────────────────────────────────

func parseTS(s string) (time.Time, bool) {
	// Try common ISO formats
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05.000",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	// Try prefix match
	if len(s) >= 19 {
		if t, err := time.ParseInLocation("2006-01-02T15:04:05", s[:19], time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ─── Main ───────────────────────────────────────────────────────────────────

func main() {
	loadPricing()

	allFlag := flag.Bool("all", false, "Process all available logs")
	todayFlag := flag.Bool("today", false, "Today only")
	yesterdayFlag := flag.Bool("yesterday", false, "Yesterday only")
	fromDays := flag.Int("from", -1, "Start from N days ago (0=today, 1=yesterday)")
	toDays := flag.Int("to", -1, "End at N days ago (0=today, 1=yesterday)")
	logsDirFlag := flag.String("logs-dir", "", "Override logs directory")
	projectFilter := flag.String("project", "", "Filter by project/workspace path (case-insensitive substring)")
	jsonFlag := flag.Bool("json", false, "Output as JSON")
	syncFlag := flag.Bool("sync", false, "Force full re-sync of all log files")
	importFile := flag.String("import-file", "", "Import data from JSONL or SQLite file")
	exportFile := flag.String("export-file", "", "Export data as JSONL")
	codespacesSync := flag.Bool("codespaces-sync", false, "Sync Copilot data from running Codespaces via gh cs cp")
	codespacesIncludeStopped := flag.Bool("codespaces-include-stopped", false, "Include stopped Codespaces (will wake and sync them)")
	webFlag := flag.Bool("web", false, "Run in web mode (respects date-window flags)")
	webListen := flag.String("web-listen", "127.0.0.1:7331", "Web mode listen address")
	webRefreshInterval := flag.Duration("web-refresh-interval", 30*time.Second, "Web mode refresh interval")
	webLocalStreaming := flag.Bool("web-local-streaming", false, "Enable experimental realtime local log streaming in web mode")
	webCodespacesMode := flag.String("web-codespaces-mode", "auto", "Web mode Codespaces sync mode: manual|auto (default auto: background startup sync + periodic sync)")
	webCodespacesStreaming := flag.Bool("web-codespaces-streaming", false, "Enable experimental codespaces streaming status from tail checkpoints")
	webCodespacesInterval := flag.Duration("web-codespaces-interval", 5*time.Minute, "Web mode Codespaces periodic sync interval when mode=auto")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: copilot-token-cost [days] [--all] [--today] [--yesterday]\n")
		fmt.Fprintf(os.Stderr, "                         [--from N] [--to N] [--logs-dir PATH] [--project TEXT] [--json]\n")
		fmt.Fprintf(os.Stderr, "                         [--sync] [--import-file FILE] [--export-file FILE]\n\n")
		fmt.Fprintf(os.Stderr, "                         [--codespaces-sync] [--codespaces-include-stopped]\n\n")
		fmt.Fprintf(os.Stderr, "                         [--web] [--web-listen ADDR] [--web-refresh-interval DURATION]\n")
		fmt.Fprintf(os.Stderr, "                         [--web-local-streaming] [--web-codespaces-mode manual|auto]\n")
		fmt.Fprintf(os.Stderr, "                         [--web-codespaces-streaming] [--web-codespaces-interval DURATION]\n\n")
		fmt.Fprintf(os.Stderr, "Copilot CLI Token Cost Calculator\n\n")
		fmt.Fprintf(os.Stderr, "Prompt text storage is always-on when prompt text is available; unavailable prompt text is stored as NULL.\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost              # last 7 days\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost 30           # last 30 days\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost 1            # today\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --today      # today\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --yesterday  # yesterday only\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --from 3     # 3 days ago until now\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --all        # all logs\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --project graph-hopper  # filter to matching projects\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --sync       # force full re-sync\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --export-file data.jsonl  # export\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --import-file data.jsonl  # import\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --codespaces-sync  # sync running codespaces\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --today  # web mode with date window\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --web-codespaces-mode manual  # disable auto codespaces sync\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --web-local-streaming  # enable experimental realtime local streaming\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --web-codespaces-interval 15s  # near-continuous codespaces sync\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --web-codespaces-streaming  # show experimental live streaming status\n")
	}
	flag.Parse()

	webCodespacesModeValue := strings.ToLower(strings.TrimSpace(*webCodespacesMode))
	if webCodespacesModeValue != "manual" && webCodespacesModeValue != "auto" {
		fmt.Fprintln(os.Stderr, "--web-codespaces-mode must be one of: manual, auto")
		os.Exit(1)
	}

	if *webFlag && *jsonFlag {
		fmt.Fprintln(os.Stderr, "--web cannot be used with --json")
		os.Exit(1)
	}
	if *webFlag && *exportFile != "" {
		fmt.Fprintln(os.Stderr, "--web cannot be used with --export-file")
		os.Exit(1)
	}

	if *codespacesIncludeStopped && !*codespacesSync && !*webFlag {
		fmt.Fprintln(os.Stderr, "--codespaces-include-stopped requires either --codespaces-sync or --web")
		os.Exit(1)
	}

	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".copilot", "logs")
	if *logsDirFlag != "" {
		logsDir = *logsDirFlag
	}
	sessionDir := filepath.Join(home, ".copilot", "session-state")

	logsExist := true
	if _, err := os.Stat(logsDir); os.IsNotExist(err) {
		logsExist = false
	}

	now := time.Now()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	var cutoff time.Time
	var cutoffEnd *time.Time
	var periodLabel string
	useCutoffMin := false

	// Parse positional days argument
	var days int
	daysSet := false
	if flag.NArg() > 0 {
		if d, err := strconv.Atoi(flag.Arg(0)); err == nil {
			days = d
			daysSet = true
		}
	}

	if *allFlag {
		useCutoffMin = true
		periodLabel = "all time"
	} else if *todayFlag {
		cutoff = todayMidnight
		periodLabel = "today"
	} else if *yesterdayFlag {
		cutoff = todayMidnight.AddDate(0, 0, -1)
		end := todayMidnight
		cutoffEnd = &end
		periodLabel = "yesterday"
	} else if *fromDays >= 0 {
		fd := *fromDays
		td := *toDays
		if td < 0 {
			td = 0
		}
		if fd < td {
			fd, td = td, fd
		}
		cutoff = todayMidnight.AddDate(0, 0, -fd)
		if td > 0 {
			end := todayMidnight.AddDate(0, 0, -td+1)
			cutoffEnd = &end
		}
		if fd == td {
			periodLabel = fmt.Sprintf("%s (1 day)", cutoff.Format("2006-01-02"))
		} else {
			toStr := "today"
			if td > 0 {
				toStr = fmt.Sprintf("%dd ago", td)
			}
			periodLabel = fmt.Sprintf("%dd ago → %s", fd, toStr)
		}
	} else {
		if !daysSet {
			days = 7
		}
		cutoff = todayMidnight.AddDate(0, 0, -(days - 1))
		if days == 1 {
			periodLabel = "last 1 day"
		} else {
			periodLabel = fmt.Sprintf("last %d days", days)
		}
	}

	// Date range label and DB query params
	var dateFromDisplay, dateToDisplay, dateRange string
	var dateFromQuery, dateToQuery string // ISO timestamps for DB queries
	if !useCutoffMin {
		dateFromDisplay = cutoff.Format("2006-01-02")
		dateFromQuery = cutoff.Format("2006-01-02T15:04:05")
	}
	if cutoffEnd != nil {
		dateToDisplay = cutoffEnd.AddDate(0, 0, -1).Format("2006-01-02")
		dateToQuery = cutoffEnd.Format("2006-01-02T15:04:05")
	} else {
		dateToDisplay = now.Format("2006-01-02")
	}
	if dateFromDisplay != "" {
		dateRange = dateFromDisplay + " → " + dateToDisplay
	}

	var syncFrom, syncTo *time.Time
	if !useCutoffMin {
		c := cutoff
		syncFrom = &c
	}
	if cutoffEnd != nil {
		c := *cutoffEnd
		syncTo = &c
	}

	if *webFlag {
		cfg := webModeConfig{
			ListenAddress:            *webListen,
			RefreshInterval:          *webRefreshInterval,
			LocalStreaming:           *webLocalStreaming,
			CodespacesMode:           webCodespacesModeValue,
			CodespacesStreaming:      *webCodespacesStreaming,
			CodespacesInterval:       *webCodespacesInterval,
			CodespacesIncludeStopped: *codespacesIncludeStopped,
			LogsDir:                  logsDir,
			SessionDir:               sessionDir,
			PeriodLabel:              periodLabel,
			DateRange:                dateRange,
			DateFromQuery:            dateFromQuery,
			DateToQuery:              dateToQuery,
			SyncFrom:                 syncFrom,
			SyncTo:                   syncTo,
		}
		if err := runWebMode(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// ─── DB setup and sync ─────────────────────────────────────────────
	dbPath := getDBPath()
	database := initDB(dbPath)
	defer database.Close()

	if logsExist {
		syncLogsToDB(database, logsDir, sessionDir, *syncFlag, "local", syncFrom, syncTo)
	}

	if *codespacesSync {
		syncCodespacesToDB(database, *codespacesIncludeStopped, *syncFlag)
	}

	if *importFile != "" {
		if strings.HasSuffix(*importFile, ".db") || strings.HasSuffix(*importFile, ".sqlite") {
			importSQLiteDB(database, *importFile, "")
		} else {
			importJSONL(database, *importFile, "")
		}
	}

	if *exportFile != "" {
		exportJSONL(database, *exportFile)
		return
	}

	projectFilterValue := strings.TrimSpace(*projectFilter)

	// ─── Query aggregated stats from DB ────────────────────────────────
	aggregatedStats := loadAggregatedStats(database, dateFromQuery, dateToQuery, projectFilterValue)
	dailyStats := aggregatedStats.DailyStats
	modelStats := aggregatedStats.ModelStats
	projectStats := aggregatedStats.ProjectStats
	filtered := aggregatedStats.Records
	sessionWorkspaces := aggregatedStats.SessionWorkspaces
	totalRecords := aggregatedStats.TotalRecords
	logFileCount := aggregatedStats.LogFileCount

	if totalRecords == 0 {
		fmt.Printf("No API calls found in %s.\n", periodLabel)
		return
	}

	// ─── JSON output ────────────────────────────────────────────────────
	if *jsonFlag {
		type jsonStats struct {
			APICalls            int     `json:"api_calls"`
			PromptTokens        int     `json:"prompt_tokens"`
			CompletionTokens    int     `json:"completion_tokens"`
			CacheCreationTokens int     `json:"cache_creation_tokens"`
			CacheReadTokens     int     `json:"cache_read_tokens"`
			PremiumRequests     float64 `json:"premium_requests"`
			PremiumRequestCost  float64 `json:"premium_request_cost"`
			InputUncached       int     `json:"input_uncached_tokens"`
			Cost                float64 `json:"cost"`
			CostWithoutCache    float64 `json:"cost_without_cache"`
		}
		type dailyModel struct {
			jsonStats
		}
		type dailyDay struct {
			Models           map[string]jsonStats `json:"-"`
			TotalCost        float64              `json:"_total_cost"`
			TotalCostNoCache float64              `json:"_total_cost_without_cache"`
		}

		type output struct {
			Period                  string                            `json:"period"`
			DateRange               *string                           `json:"date_range"`
			LogFiles                int                               `json:"log_files"`
			APICalls                int                               `json:"api_calls"`
			Models                  map[string]jsonStats              `json:"models"`
			Daily                   map[string]map[string]interface{} `json:"daily"`
			Projects                map[string]jsonStats              `json:"projects"`
			TotalCost               float64                           `json:"total_cost"`
			TotalCostNoCache        float64                           `json:"total_cost_without_cache"`
			TotalPremiumRequestCost float64                           `json:"total_premium_request_cost"`
		}

		out := output{
			Period:   periodLabel,
			LogFiles: logFileCount,
			APICalls: totalRecords,
			Models:   make(map[string]jsonStats),
			Daily:    make(map[string]map[string]interface{}),
			Projects: make(map[string]jsonStats),
		}
		if dateRange != "" {
			out.DateRange = &dateRange
		}

		// Models
		models := sortedKeys(modelStats)
		for _, model := range models {
			s := modelStats[model]
			cost := sumDailyCost(model, dailyStats, calcCost)
			costNC := sumDailyCost(model, dailyStats, calcCostNocache)
			premCost := sumDailyPremCost(model, dailyStats)
			out.TotalCost += cost
			out.TotalCostNoCache += costNC
			out.Models[model] = jsonStats{
				APICalls: s.APICalls, PromptTokens: s.PromptTokens,
				CompletionTokens: s.CompletionTokens, CacheCreationTokens: s.CacheCreationTokens,
				CacheReadTokens: s.CacheReadTokens, PremiumRequests: s.PremiumRequests,
				PremiumRequestCost: roundN(premCost, 4),
				InputUncached:      uncachedInput(s),
				Cost:               roundN(cost, 4), CostWithoutCache: roundN(costNC, 4),
			}
			out.TotalPremiumRequestCost += premCost
		}

		// Daily
		daysKeys := sortedKeysStr(dailyStats)
		for _, day := range daysKeys {
			dayMap := make(map[string]interface{})
			var dayTotal, dayTotalNC float64
			for model, s := range dailyStats[day] {
				cost := calcCost(model, s, day)
				costNC := calcCostNocache(model, s, day)
				dayTotal += cost
				dayTotalNC += costNC
				dayMap[model] = jsonStats{
					APICalls: s.APICalls, PromptTokens: s.PromptTokens,
					CompletionTokens: s.CompletionTokens, CacheCreationTokens: s.CacheCreationTokens,
					CacheReadTokens: s.CacheReadTokens, PremiumRequests: s.PremiumRequests,
					PremiumRequestCost: roundN(s.PremiumRequests*getPremiumRequestCost(day), 4),
					InputUncached:      uncachedInput(s),
					Cost:               roundN(cost, 4), CostWithoutCache: roundN(costNC, 4),
				}
			}
			dayMap["_total_cost"] = roundN(dayTotal, 4)
			dayMap["_total_cost_without_cache"] = roundN(dayTotalNC, 4)
			out.Daily[day] = dayMap
		}

		// Projects - recalculate costs per record
		projCosts := make(map[string][2]float64) // [cost, costNC]
		for _, r := range filtered {
			cwd := ""
			if r.SessionID != "" {
				if meta, ok := sessionWorkspaces[r.Source+"\x1f"+r.SessionID]; ok {
					cwd = meta.CWD
				}
			}
			proj := "(unknown)"
			if cwd != "" {
				proj = projectName(cwd)
			}
			model := normalizeModel(r.Model)
			rs := &Stats{
				PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
				CacheCreationTokens: r.CacheCreationTokens, CacheReadTokens: r.CacheReadTokens,
			}
			c := projCosts[proj]
			c[0] += calcCost(model, rs, r.Timestamp)
			c[1] += calcCostNocache(model, rs, r.Timestamp)
			projCosts[proj] = c
		}
		for proj, s := range projectStats {
			pc := projCosts[proj]
			out.Projects[proj] = jsonStats{
				APICalls: s.APICalls, PromptTokens: s.PromptTokens,
				CompletionTokens: s.CompletionTokens, CacheCreationTokens: s.CacheCreationTokens,
				CacheReadTokens: s.CacheReadTokens, PremiumRequests: s.PremiumRequests,
				PremiumRequestCost: roundN(s.PremiumRequests*getPremiumRequestCost(""), 4),
				InputUncached:      uncachedInput(s),
				Cost:               roundN(pc[0], 4), CostWithoutCache: roundN(pc[1], 4),
			}
		}

		out.TotalCost = roundN(out.TotalCost, 4)
		out.TotalCostNoCache = roundN(out.TotalCostNoCache, 4)
		out.TotalPremiumRequestCost = roundN(out.TotalPremiumRequestCost, 4)

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return
	}

	// ─── Pretty output ──────────────────────────────────────────────────
	fmt.Println()
	title := "COPILOT CLI - TOKEN USAGE & COST REPORT"
	titleWidth := len(title) + 10
	titlePadL := (titleWidth - len(title)) / 2
	titlePadR := titleWidth - len(title) - titlePadL
	fmt.Printf("╔%s╗\n", strings.Repeat("═", titleWidth))
	fmt.Printf("║%s%s%s║\n", strings.Repeat(" ", titlePadL), title, strings.Repeat(" ", titlePadR))
	fmt.Printf("╚%s╝\n", strings.Repeat("═", titleWidth))

	totalPremium := 0.0
	for _, s := range modelStats {
		totalPremium += s.PremiumRequests
	}
	dateSuffix := ""
	if dateRange != "" {
		dateSuffix = " (" + dateRange + ")"
	}
	projectSuffix := ""
	if projectFilterValue != "" {
		projectSuffix = "  │  Project filter: " + projectFilterValue
	}
	fmt.Printf("  Period: %s%s  │  Log files: %d  │  API calls: %s  │  Premium requests: %s\n",
		periodLabel, dateSuffix, logFileCount, commaInt(totalRecords), commaFloat(totalPremium, 0))
	if projectSuffix != "" {
		fmt.Println(projectSuffix)
	}
	fmt.Println()

	// ── Per-model table ─────────────────────────────────────────────────
	modelHeaders := []string{"Model", "Calls", "Premium", "Prem Cost", "Input", "Cached", "Cache Write", "Output", "Hit%", "Cost", "No-Cache"}
	var modelRows [][]string
	var tCost, tNC, tPremCost float64
	var tUnc, tCached, tCW, tOut, tCalls, tPrompt int
	var tPremium float64

	sortedModels := sortedKeysByFunc(modelStats, func(m string) float64 {
		return -sumDailyCost(m, dailyStats, calcCostNocache)
	})

	for _, model := range sortedModels {
		s := modelStats[model]
		cost := sumDailyCost(model, dailyStats, calcCost)
		costNC := sumDailyCost(model, dailyStats, calcCostNocache)
		unc := uncachedInput(s)
		tCost += cost
		tNC += costNC
		tUnc += unc
		tCached += s.CacheReadTokens
		tCW += s.CacheCreationTokens
		tOut += s.CompletionTokens
		tCalls += s.APICalls
		tPrompt += s.PromptTokens
		tPremium += s.PremiumRequests

		p := getPricing(model, "")
		mult := getPremiumMultiplier(model, "")
		premiumStr := commaFloat(s.PremiumRequests, 0)
		premCost := sumDailyPremCost(model, dailyStats)
		tPremCost += premCost
		premCostStr := fmtCost(premCost)
		if mult == 0 {
			premiumStr = "-"
			premCostStr = "-"
		}
		costStr, costNCStr := fmtCost(cost), fmtCost(costNC)
		if p == nil {
			costStr, costNCStr = "N/A", "N/A"
		}
		modelRows = append(modelRows, []string{
			model, commaInt(s.APICalls), premiumStr, premCostStr,
			fmtTokens(unc), fmtTokens(s.CacheReadTokens),
			fmtTokens(s.CacheCreationTokens), fmtTokens(s.CompletionTokens),
			cacheHitPct(s.PromptTokens, s.CacheReadTokens),
			costStr, costNCStr,
		})
	}
	modelFooter := []string{
		"TOTAL", commaInt(tCalls), commaFloat(tPremium, 0), fmtCost(tPremCost),
		fmtTokens(tUnc), fmtTokens(tCached),
		fmtTokens(tCW), fmtTokens(tOut),
		cacheHitPct(tPrompt, tCached),
		fmtCost(tCost), fmtCost(tNC),
	}
	var modelNotes []string
	if tNC > 0 {
		savingsPct := (1 - tCost/tNC) * 100
		modelNotes = append(modelNotes, fmt.Sprintf("💰 Cache savings: %s (%.0f%% reduction)", fmtCost(tNC-tCost), savingsPct))
	}
	printTable("PER-MODEL SUMMARY", modelHeaders, modelRows, modelFooter, modelNotes)
	fmt.Println()

	// ── Cost per premium request ────────────────────────────────────────
	premHeaders := []string{"Model", "Multiplier", "Premiums", "API Cost", "$/Premium", "Prem Cost", "Discount"}
	var premRows [][]string
	var premTotalCost float64
	var premTotalReqs float64

	sortedPremModels := sortedKeysByFunc(modelStats, func(m string) float64 {
		return -modelStats[m].PremiumRequests
	})

	for _, model := range sortedPremModels {
		s := modelStats[model]
		mult := getPremiumMultiplier(model, "")
		if mult == 0 {
			continue
		}
		cost := sumDailyCost(model, dailyStats, calcCost)
		if s.PremiumRequests > 0 {
			premTotalCost += cost
			premTotalReqs += s.PremiumRequests
			costPer := cost / s.PremiumRequests
			premCost := sumDailyPremCost(model, dailyStats)
			discount := "-"
			if cost > 0 {
				discount = fmt.Sprintf("%.0f%%", (1-premCost/cost)*100)
			}
			multStr := fmt.Sprintf("%.2g×", mult)
			premRows = append(premRows, []string{
				model, multStr, commaFloat(s.PremiumRequests, 0),
				fmtCost(cost), fmtCost(costPer), fmtCost(premCost), discount,
			})
		} else {
			multStr := fmt.Sprintf("%.2g×", mult)
			premRows = append(premRows, []string{
				model, multStr, "-",
				fmtCost(cost), "N/A", "-", "-",
			})
		}
	}

	if len(premRows) > 0 {
		avgCost := 0.0
		if premTotalReqs > 0 {
			avgCost = premTotalCost / premTotalReqs
		}
		totalPremCost := sumDailyPremCostAll(dailyStats)
		totalDiscount := "-"
		if premTotalCost > 0 {
			totalDiscount = fmt.Sprintf("%.0f%%", (1-totalPremCost/premTotalCost)*100)
		}
		premFooter := []string{"TOTAL", "", commaFloat(premTotalReqs, 0), fmtCost(premTotalCost), fmtCost(avgCost), fmtCost(totalPremCost), totalDiscount}
		premNotes := []string{"ℹ️  Models with 0× multiplier (free tier) are excluded"}
		missingCost := tCost - premTotalCost
		if missingCost > 0.001 {
			premNotes = append(premNotes, fmt.Sprintf("⚠  %s from models without premium data excluded from $/premium avg", fmtCost(missingCost)))
		}
		printTable("COST PER PREMIUM REQUEST", premHeaders, premRows, premFooter, premNotes)
		fmt.Println()
	}

	// ── Daily table ─────────────────────────────────────────────────────
	dailyHeaders := []string{"Date", "Calls", "Premium", "Input", "Cached", "Output", "Hit%", "Cost", "No-Cache", "Prem Cost", "Discount"}
	var dailyRows [][]string
	dailyDays := sortedKeysStr(dailyStats)
	for _, day := range dailyDays {
		var dCalls, dUnc, dCached, dOut int
		var dPremium, dCost, dNC float64
		for model, s := range dailyStats[day] {
			dCalls += s.APICalls
			dPremium += s.PremiumRequests
			dUnc += uncachedInput(s)
			dCached += s.CacheReadTokens
			dOut += s.CompletionTokens
			dCost += calcCost(model, s, day)
			dNC += calcCostNocache(model, s, day)
		}
		dTotal := dUnc + dCached
		dPremCost := dPremium * getPremiumRequestCost(day)
		dDiscount := "-"
		if dCost > 0 {
			dDiscount = fmt.Sprintf("%.0f%%", (1-dPremCost/dCost)*100)
		}
		dailyRows = append(dailyRows, []string{
			day, commaInt(dCalls), commaFloat(dPremium, 0),
			fmtTokens(dUnc), fmtTokens(dCached),
			fmtTokens(dOut), cacheHitPct(dTotal, dCached),
			fmtCost(dCost), fmtCost(dNC), fmtCost(dPremCost), dDiscount,
		})
	}
	printTable("DAILY BREAKDOWN", dailyHeaders, dailyRows, nil, nil)
	fmt.Println()

	// ── Per-project table ───────────────────────────────────────────────
	projCosts := make(map[string][3]float64)
	for _, r := range filtered {
		cwd := ""
		if r.SessionID != "" {
			if meta, ok := sessionWorkspaces[r.Source+"\x1f"+r.SessionID]; ok {
				cwd = meta.CWD
			}
		}
		proj := "(unknown)"
		if cwd != "" {
			proj = projectName(cwd)
		}
		model := normalizeModel(r.Model)
		rs := &Stats{
			PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
			CacheCreationTokens: r.CacheCreationTokens, CacheReadTokens: r.CacheReadTokens,
		}
		c := projCosts[proj]
		c[0] += calcCost(model, rs, r.Timestamp)
		c[1] += calcCostNocache(model, rs, r.Timestamp)
		if r.IsUserTurn {
			c[2] += getPremiumMultiplier(model, r.Timestamp) * getPremiumRequestCost(r.Timestamp)
		}
		projCosts[proj] = c
	}

	projHeaders := []string{"Project", "Calls", "Premium", "Input", "Cached", "Output", "Cost", "Prem Cost"}
	// Sort projects by no-cache cost descending
	type projEntry struct {
		name string
		s    *Stats
	}
	var projList []projEntry
	for name, s := range projectStats {
		projList = append(projList, projEntry{name, s})
	}
	sort.Slice(projList, func(i, j int) bool {
		return projCosts[projList[i].name][1] > projCosts[projList[j].name][1]
	})

	var projRows [][]string
	for _, pe := range projList {
		s := pe.s
		pc := projCosts[pe.name]
		projRows = append(projRows, []string{
			pe.name, commaInt(s.APICalls), commaFloat(s.PremiumRequests, 0),
			fmtTokens(uncachedInput(s)),
			fmtTokens(s.CacheReadTokens), fmtTokens(s.CompletionTokens),
			fmtCost(pc[0]), fmtCost(pc[2]),
		})
	}
	printTable("PER-PROJECT BREAKDOWN", projHeaders, projRows, nil, nil)
	fmt.Println()

	// ── Pricing reference ───────────────────────────────────────────────
	priceHeaders := []string{"Model", "Input/1M", "Output/1M", "Cache Read/1M", "Cache Write/1M"}
	var usedList []string
	for m := range modelStats {
		usedList = append(usedList, m)
	}
	sort.Strings(usedList)

	var priceRows [][]string
	for _, model := range usedList {
		p := getPricing(model, "")
		if p != nil {
			priceRows = append(priceRows, []string{
				model,
				fmt.Sprintf("$%.2f", p.Input),
				fmt.Sprintf("$%.2f", p.Output),
				fmt.Sprintf("$%.3f", p.CacheRead),
				fmt.Sprintf("$%.2f", p.CacheWrite),
			})
		} else {
			priceRows = append(priceRows, []string{model, "N/A", "N/A", "N/A", "N/A"})
		}
	}
	printTable("PRICING REFERENCE", priceHeaders, priceRows, nil, nil)
	fmt.Println()
	fmt.Println("  ⚠  Estimated API-equivalent costs. Copilot subscriptions include token usage.")
	fmt.Println()
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func sortedKeys(m map[string]*Stats) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysStr(m map[string]map[string]*Stats) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysByFunc(m map[string]*Stats, less func(string) float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return less(keys[i]) < less(keys[j])
	})
	return keys
}

func roundN(f float64, n int) float64 {
	pow := math.Pow(10, float64(n))
	return math.Round(f*pow) / pow
}

// Ensure utf8 import is used
var _ = utf8.RuneLen
