package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
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
	reSession        = regexp.MustCompile(`(?:Workspace initialized|Created ACP session|Flushed \d+ events to session)[: ]+([0-9a-f-]{36})`)
	reInitiator      = regexp.MustCompile(`PremiumRequestProcessor: Setting X-Initiator to '(\w+)'`)
	reModelJSON      = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)
	rePromptTokens   = regexp.MustCompile(`"prompt_tokens"\s*:\s*(\d+)`)
	reCompTokens     = regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)
	reCacheCreation  = regexp.MustCompile(`"cache_creation_input_tokens"\s*:\s*(\d+)`)
	reCacheRead      = regexp.MustCompile(`"cache_read_input_tokens"\s*:\s*(\d+)`)
	reCachedTokens   = regexp.MustCompile(`"cached_tokens"\s*:\s*(\d+)`)
	reCwd            = regexp.MustCompile(`cwd:\s*(.+)`)
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

// ─── Record & Stats ─────────────────────────────────────────────────────────

type Record struct {
	Model               string
	PromptTokens        int
	CompletionTokens    int
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
		s.PremiumRequests += getPremiumMultiplier(model, r.Timestamp)
	}
}

// ─── Log parsing ────────────────────────────────────────────────────────────

func parseLogFile(logPath string) []Record {
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

		// Check for model in block
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

func loadSessionWorkspaces(sessionDir string) map[string]string {
	workspaces := make(map[string]string)
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
		if m := reCwd.FindStringSubmatch(string(data)); m != nil {
			workspaces[entry.Name()] = strings.TrimSpace(m[1])
		}
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
    PRIMARY KEY (session_id, source)
);

CREATE TABLE IF NOT EXISTS codespace_sync_state (
    codespace_name TEXT PRIMARY KEY,
    last_used_at TEXT,
    last_synced_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_api_calls_timestamp ON api_calls(timestamp);
CREATE INDEX IF NOT EXISTS idx_api_calls_model ON api_calls(model_normalized);
CREATE INDEX IF NOT EXISTS idx_api_calls_session ON api_calls(session_id);
`

func getDBPath() string {
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	candidates := []string{
		filepath.Join(exeDir, "copilot-tokens.db"),
		filepath.Join(exeDir, "..", "copilot-tokens.db"),
		filepath.Join(".", "copilot-tokens.db"),
		filepath.Join("..", "copilot-tokens.db"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join("..", "copilot-tokens.db")
}

func initDB(dbPath string) *sql.DB {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA foreign_keys=ON")
	_, err = db.Exec(schemaSQLGo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating schema: %v\n", err)
		os.Exit(1)
	}
	migrateSessionWorkspacesSchema(db)
	return db
}

func migrateSessionWorkspacesSchema(db *sql.DB) {
	var pkCols sql.NullString
	_ = db.QueryRow(
		"SELECT group_concat(name, ',') FROM (" +
			"SELECT name FROM pragma_table_info('session_workspaces') WHERE pk > 0 ORDER BY pk" +
			")",
	).Scan(&pkCols)
	if pkCols.Valid && pkCols.String == "session_id,source" {
		return
	}
	_, _ = db.Exec(`
ALTER TABLE session_workspaces RENAME TO session_workspaces_old;
CREATE TABLE session_workspaces (
    session_id TEXT NOT NULL,
    cwd TEXT NOT NULL,
    source TEXT DEFAULT 'local',
    PRIMARY KEY (session_id, source)
);
INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source)
SELECT session_id, cwd, COALESCE(source, 'local') FROM session_workspaces_old;
DROP TABLE session_workspaces_old;
`)
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
			"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
			"timestamp, session_id, log_file, source) " +
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
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
		stmt.Exec(r.Model, normalizeModel(r.Model), r.PromptTokens, r.CompletionTokens,
			r.CacheCreationTokens, r.CacheReadTokens, isUT,
			r.Timestamp, r.SessionID, r.LogFile, source)
	}
	tx.Commit()
}

func upsertSessionWorkspace(db *sql.DB, sessionID, cwd, source string) {
	db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source) VALUES (?, ?, ?)",
		sessionID, cwd, source)
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

func querySessionWorkspaces(db *sql.DB) map[string]string {
	rows, err := db.Query("SELECT session_id, cwd, source FROM session_workspaces")
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var sid, cwd, source string
		rows.Scan(&sid, &cwd, &source)
		result[source+"\x1f"+sid] = cwd
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

	rows, _ := db.Query("SELECT model, model_normalized, prompt_tokens, completion_tokens, " +
		"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
		"timestamp, session_id, log_file, source FROM api_calls")
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var model, modelNorm, source string
			var pt, ct, cct, crt, isUT int
			var ts, sid, lf sql.NullString
			rows.Scan(&model, &modelNorm, &pt, &ct, &cct, &crt, &isUT, &ts, &sid, &lf, &source)
			rec := map[string]interface{}{
				"type": "api_call", "model": model, "model_normalized": modelNorm,
				"prompt_tokens": pt, "completion_tokens": ct,
				"cache_creation_tokens": cct, "cache_read_tokens": crt,
				"is_user_turn": isUT, "timestamp": ts.String,
				"session_id": sid.String, "log_file": lf.String, "source": source,
			}
			b, _ := json.Marshal(rec)
			w.Write(b)
			w.WriteByte('\n')
		}
	}

	rows2, _ := db.Query("SELECT session_id, cwd, source FROM session_workspaces")
	if rows2 != nil {
		defer rows2.Close()
		for rows2.Next() {
			var sid, cwd, source string
			rows2.Scan(&sid, &cwd, &source)
			rec := map[string]interface{}{
				"type": "session_workspace", "session_id": sid, "cwd": cwd, "source": source,
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
			db.Exec("INSERT OR IGNORE INTO api_calls "+
				"(model, model_normalized, prompt_tokens, completion_tokens, "+
				"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
				"timestamp, session_id, log_file, source) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				obj["model"], obj["model_normalized"],
				int(obj["prompt_tokens"].(float64)), int(obj["completion_tokens"].(float64)),
				int(obj["cache_creation_tokens"].(float64)), int(obj["cache_read_tokens"].(float64)),
				isUT, obj["timestamp"], obj["session_id"], obj["log_file"], src)
		} else if rtype == "session_workspace" {
			db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source) VALUES (?, ?, ?)",
				obj["session_id"], obj["cwd"], src)
		}
		count++
	}
	return count
}

func importSQLiteDB(db *sql.DB, otherDBPath, sourceOverride string) int {
	db.Exec("ATTACH DATABASE ? AS import_db", otherDBPath)
	var count int64
	if sourceOverride != "" {
		db.Exec("INSERT OR IGNORE INTO api_calls "+
			"(model, model_normalized, prompt_tokens, completion_tokens, "+
			"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
			"timestamp, session_id, log_file, source) "+
			"SELECT model, model_normalized, prompt_tokens, completion_tokens, "+
			"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
			"timestamp, session_id, log_file, ? FROM import_db.api_calls", sourceOverride)
		db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source) "+
			"SELECT session_id, cwd, ? FROM import_db.session_workspaces", sourceOverride)
		db.Exec("INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) "+
			"SELECT log_file, mtime, ?, record_count, parsed_at FROM import_db.parsed_logs", sourceOverride)
	} else {
		db.Exec("INSERT OR IGNORE INTO api_calls " +
			"(model, model_normalized, prompt_tokens, completion_tokens, " +
			"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
			"timestamp, session_id, log_file, source) " +
			"SELECT model, model_normalized, prompt_tokens, completion_tokens, " +
			"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
			"timestamp, session_id, log_file, source FROM import_db.api_calls")
		db.Exec("INSERT OR REPLACE INTO session_workspaces SELECT * FROM import_db.session_workspaces")
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

	if force {
		// Clear parse tracker so all logs are re-parsed; keep existing api_calls (INSERT OR IGNORE handles dedup)
		db.Exec("DELETE FROM parsed_logs WHERE source = ?", source)
		fmt.Fprintf(os.Stderr, "  🔄 Force re-sync (%s): re-parsing %d log files (keeping %s existing records)\n", source, len(matches), addCommas(strconv.Itoa(existing)))
	}

	totalInserted := 0
	parsedCount := 0

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
		if !force && isLogParsed(db, filename, mtime, source) {
			continue
		}
		records := parseLogFile(logPath)
		insertRecords(db, records, source)
		markLogParsed(db, filename, mtime, len(records), source)
		totalInserted += len(records)
		parsedCount++
		if force {
			fmt.Fprintf(os.Stderr, "  📄 [%d/%d] %s (%d records)\n", parsedCount, len(matches), filename, len(records))
		}
	}

	workspaces := loadSessionWorkspaces(sessionDir)
	for sessionID, cwd := range workspaces {
		upsertSessionWorkspace(db, sessionID, cwd, source)
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

func listCodespaces(includeStopped bool) []codespaceInfo {
	cmd := exec.Command("gh", "cs", "list", "--json", "name,state,lastUsedAt", "--limit", "1000")
	out, err := cmd.Output()
	if err != nil {
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
	return filtered
}

func syncCodespacesToDB(db *sql.DB, includeStopped bool, force bool) int {
	codespaces := listCodespaces(includeStopped)
	if len(codespaces) == 0 {
		return 0
	}
	total := 0
	for _, cs := range codespaces {
		if cs.LastUsedAt != "" && getCodespaceLastUsed(db, cs.Name) == cs.LastUsedAt {
			fmt.Fprintf(os.Stderr, "  ⏭️  Skipping %s (unchanged lastUsedAt)\n", cs.Name)
			continue
		}

		shouldStop := cs.State == "Shutdown"
		tmpDir, err := os.MkdirTemp("", "copilot-cs-")
		if err != nil {
			continue
		}
		copied := false
		func() {
			defer os.RemoveAll(tmpDir)
			if shouldStop {
				defer exec.Command("gh", "cs", "stop", "-c", cs.Name).Run()
			}

			stage := filepath.Join(tmpDir, cs.Name)
			_ = os.MkdirAll(stage, 0755)
			cpCmd := exec.Command("gh", "cs", "cp", "-e", "-r", "-c", cs.Name, "remote:/home/vscode/.copilot", stage)
			cpOut, cpErr := cpCmd.CombinedOutput()
			if cpErr != nil {
				msg := strings.TrimSpace(string(cpOut))
				if strings.Contains(msg, "No such file or directory") {
					fmt.Fprintf(os.Stderr, "  ⚠️ Skipping %s: /home/vscode/.copilot not found\n", cs.Name)
				} else {
					if msg == "" {
						msg = "gh cs cp failed"
					}
					fmt.Fprintf(os.Stderr, "  ⚠️ Failed to copy %s: %s\n", cs.Name, msg)
				}
				return
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
				return
			}

			total += syncLogsToDB(db, logsDir, sessionDir, force, "codespace:"+cs.Name, nil, nil)
			copied = true
		}()

		if copied {
			upsertCodespaceSyncState(db, cs.Name, cs.LastUsedAt)
		}
	}
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

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: copilot-token-cost [days] [--all] [--today] [--yesterday]\n")
		fmt.Fprintf(os.Stderr, "                         [--from N] [--to N] [--logs-dir PATH] [--project TEXT] [--json]\n")
		fmt.Fprintf(os.Stderr, "                         [--sync] [--import-file FILE] [--export-file FILE]\n\n")
		fmt.Fprintf(os.Stderr, "                         [--codespaces-sync] [--codespaces-include-stopped]\n\n")
		fmt.Fprintf(os.Stderr, "Copilot CLI Token Cost Calculator\n\n")
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
	}
	flag.Parse()

	if *codespacesIncludeStopped && !*codespacesSync {
		fmt.Fprintln(os.Stderr, "--codespaces-include-stopped requires --codespaces-sync")
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

	// ─── DB setup and sync ─────────────────────────────────────────────
	dbPath := getDBPath()
	database := initDB(dbPath)
	defer database.Close()

	var syncFrom, syncTo *time.Time
	if !useCutoffMin {
		c := cutoff
		syncFrom = &c
	}
	if cutoffEnd != nil {
		c := *cutoffEnd
		syncTo = &c
	}

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
	dbDailyStats := queryDailyStats(database, dateFromQuery, dateToQuery, projectFilterValue)
	dailyStats := make(map[string]map[string]*Stats)
	for day, models := range dbDailyStats {
		dailyStats[day] = make(map[string]*Stats)
		for model, dbs := range models {
			dailyStats[day][model] = &Stats{
				APICalls:            dbs.APICalls,
				PromptTokens:        dbs.PromptTokens,
				CompletionTokens:    dbs.CompletionTokens,
				CacheCreationTokens: dbs.CacheCreationTokens,
				CacheReadTokens:     dbs.CacheReadTokens,
				PremiumRequests:     float64(dbs.UserTurns) * getPremiumMultiplier(model, day),
			}
		}
	}

	// Compute model-level premium_requests from daily (multiplier varies by day)
	dbModelStatsMap := queryModelStats(database, dateFromQuery, dateToQuery, projectFilterValue)
	modelStats := make(map[string]*Stats)
	for model, dbs := range dbModelStatsMap {
		var premReqs float64
		for _, models := range dailyStats {
			if s, ok := models[model]; ok {
				premReqs += s.PremiumRequests
			}
		}
		modelStats[model] = &Stats{
			APICalls:            dbs.APICalls,
			PromptTokens:        dbs.PromptTokens,
			CompletionTokens:    dbs.CompletionTokens,
			CacheCreationTokens: dbs.CacheCreationTokens,
			CacheReadTokens:     dbs.CacheReadTokens,
			PremiumRequests:     premReqs,
		}
	}

	dbProjectStats := queryProjectStats(database, dateFromQuery, dateToQuery, projectFilterValue)
	projectStats := make(map[string]*Stats)
	for cwd, dbs := range dbProjectStats {
		proj := "(unknown)"
		if cwd != "" {
			proj = projectName(cwd)
		}
		s := &Stats{
			APICalls:            dbs.APICalls,
			PromptTokens:        dbs.PromptTokens,
			CompletionTokens:    dbs.CompletionTokens,
			CacheCreationTokens: dbs.CacheCreationTokens,
			CacheReadTokens:     dbs.CacheReadTokens,
			PremiumRequests:     float64(dbs.UserTurns), // already aggregated across models
		}
		if existing, ok := projectStats[proj]; ok {
			existing.APICalls += s.APICalls
			existing.PromptTokens += s.PromptTokens
			existing.CompletionTokens += s.CompletionTokens
			existing.CacheCreationTokens += s.CacheCreationTokens
			existing.CacheReadTokens += s.CacheReadTokens
			existing.PremiumRequests += s.PremiumRequests
		} else {
			projectStats[proj] = s
		}
	}

	filtered := queryRecords(database, dateFromQuery, dateToQuery, projectFilterValue)
	sessionWorkspaces := querySessionWorkspaces(database)

	totalRecords := 0
	for _, s := range modelStats {
		totalRecords += s.APICalls
	}
	logFileCount := queryLogFileCount(database, dateFromQuery, dateToQuery, projectFilterValue)

	if totalRecords == 0 {
		fmt.Printf("No API calls found in %s.\n", periodLabel)
		os.Exit(0)
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
				cwd = sessionWorkspaces[r.Source+"\x1f"+r.SessionID]
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
			cwd = sessionWorkspaces[r.Source+"\x1f"+r.SessionID]
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
