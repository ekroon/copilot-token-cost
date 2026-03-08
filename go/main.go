package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"copilot-token-cost/internal/costing"
	"copilot-token-cost/internal/domain"
	"copilot-token-cost/internal/parsing"
	storagelayer "copilot-token-cost/internal/storage"
	syncservice "copilot-token-cost/internal/sync"

	_ "modernc.org/sqlite"
)

type dbQuerier interface {
	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
}

// ─── API Pricing (per 1M tokens) ────────────────────────────────────────────

type Pricing = costing.Pricing
type PricingPeriod = costing.PricingPeriod

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
	if len(pf.PricingPeriods) == 0 {
		fmt.Fprintf(os.Stderr, "Error: pricing.json contains no pricing periods\n")
		os.Exit(1)
	}

	pricingPeriods = pf.PricingPeriods
}

func getPeriod(timestamp string) *PricingPeriod {
	return costing.GetPeriod(pricingPeriods, timestamp)
}

func getPremiumRequestCost(timestamp string) float64 {
	return costing.GetPremiumRequestCost(pricingPeriods, timestamp)
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
	return costing.NormalizeModel(name)
}

func getPricing(model string, timestamp string) *Pricing {
	return costing.GetPricing(pricingPeriods, model, timestamp)
}

func getPremiumMultiplier(model string, timestamp string) float64 {
	return costing.GetPremiumMultiplier(pricingPeriods, model, timestamp)
}

func isUserInitiator(initiator string) bool {
	return parsing.IsUserInitiator(initiator)
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

type Record = domain.Record
type Stats domain.Stats

func newStats() *Stats { return (*Stats)(domain.NewStats()) }

func (s *Stats) add(r Record, model string) {
	(*domain.Stats)(s).AddRecord(r, getPremiumMultiplier(model, r.Timestamp))
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
	return parsing.ParseLogContent(content, logPath, minTimestamp, maxTimestamp)
}

func extractPromptTextNearLine(lines []string, center int) *string {
	return parsing.ExtractPromptTextNearLine(lines, center)
}

func extractPromptTextFromLine(line string) *string {
	return parsing.ExtractPromptTextFromLine(line)
}

func containsPromptIndicator(line string) bool {
	return parsing.ContainsPromptIndicator(line)
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

const schemaSQLGo = storagelayer.SchemaSQL

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
	if err := storagelayer.NewService(db).Initialize(); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating schema: %v\n", err)
		os.Exit(1)
	}
	return db
}

func migrateAPICallsSchema(db *sql.DB) {
	storagelayer.NewService(db).MigrateAPICallsSchema()
}

func migrateSessionWorkspacesSchema(db *sql.DB) {
	storagelayer.NewService(db).MigrateSessionWorkspacesSchema()
}

func sessionWorkspaceColumns(db *sql.DB, schema string) map[string]bool {
	return storagelayer.NewService(db).SessionWorkspaceColumns(schema)
}

func apiCallColumns(db *sql.DB, schema string) map[string]bool {
	return storagelayer.NewService(db).APICallColumns(schema)
}

func isLogParsed(db *sql.DB, logFile string, mtime float64, source string) bool {
	return storagelayer.NewService(db).IsLogParsed(logFile, mtime, source)
}

func markLogParsed(db *sql.DB, logFile string, mtime float64, recordCount int, source string) {
	storagelayer.NewService(db).MarkLogParsed(logFile, mtime, recordCount, source)
}

func toDomainRecord(r Record) domain.Record {
	return domain.Record{
		Model:               r.Model,
		PromptTokens:        r.PromptTokens,
		CompletionTokens:    r.CompletionTokens,
		PromptText:          r.PromptText,
		CacheCreationTokens: r.CacheCreationTokens,
		CacheReadTokens:     r.CacheReadTokens,
		IsUserTurn:          r.IsUserTurn,
		Timestamp:           r.Timestamp,
		SessionID:           r.SessionID,
		LogFile:             r.LogFile,
		Source:              r.Source,
	}
}

func toDomainRecords(records []Record) []domain.Record {
	out := make([]domain.Record, 0, len(records))
	for _, r := range records {
		out = append(out, toDomainRecord(r))
	}
	return out
}

func toSyncWorkspaceMeta(workspaces map[string]workspaceMeta) map[string]syncservice.WorkspaceMeta {
	out := make(map[string]syncservice.WorkspaceMeta, len(workspaces))
	for sessionID, meta := range workspaces {
		out[sessionID] = syncservice.WorkspaceMeta{CWD: meta.CWD, Branch: meta.Branch}
	}
	return out
}

func syncServiceForDBWithLogf(db *sql.DB, logf func(format string, args ...interface{})) *syncservice.Service {
	service := syncservice.NewService(storagelayer.NewService(db), parsing.NewService())
	service.SetRuntimeDeps(syncservice.RuntimeDeps{
		ParseLogFileInRange: func(logPath, minTimestamp, maxTimestamp string) []domain.Record {
			return toDomainRecords(parseLogFileInRange(logPath, minTimestamp, maxTimestamp))
		},
		LoadSessionWorkspaces: func(sessionDir string) map[string]syncservice.WorkspaceMeta {
			return toSyncWorkspaceMeta(loadSessionWorkspaces(sessionDir))
		},
		NormalizeModel:       normalizeModel,
		PromptTextForStorage: promptTextForStorage,
		AddCommas:            addCommas,
		ReattributeUserTurns: func(records []domain.Record) {
			costing.ReattributeUserTurns(records, func(model, timestamp string) float64 {
				return costing.GetPremiumMultiplier(pricingPeriods, model, timestamp)
			})
		},
	})
	service.SetLogf(logf)
	return service
}

func syncServiceForDB(db *sql.DB) *syncservice.Service {
	return syncServiceForDBWithLogf(db, nil)
}

func insertRecords(db *sql.DB, records []Record, source string) {
	storagelayer.NewService(db).InsertRecords(toDomainRecords(records), source, normalizeModel, promptTextForStorage)
}

func upsertSessionWorkspace(db *sql.DB, sessionID, cwd, branch, source string) {
	storagelayer.NewService(db).UpsertSessionWorkspace(sessionID, cwd, branch, source)
}

func clearSource(db *sql.DB, source string) {
	storagelayer.NewService(db).ClearSource(source)
}

func getCodespaceLastUsed(db *sql.DB, codespaceName string) string {
	return storagelayer.NewService(db).GetCodespaceLastUsed(codespaceName)
}

func upsertCodespaceSyncState(db *sql.DB, codespaceName string, lastUsedAt string) {
	storagelayer.NewService(db).UpsertCodespaceSyncState(codespaceName, lastUsedAt)
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

func queryDailyProjectModelStats(q dbQuerier, dateFrom, dateTo, projectFilter string) map[string]map[string]map[string]*dbModelStats {
	where, params := buildFilters(dateFrom, dateTo, projectFilter)
	query := "SELECT substr(a.timestamp, 1, 10) AS day, COALESCE(sw.cwd, '') AS cwd, " +
		"a.model_normalized, COUNT(*) AS api_calls, " +
		"SUM(a.prompt_tokens), SUM(a.completion_tokens), " +
		"SUM(a.cache_creation_tokens), SUM(a.cache_read_tokens), " +
		"SUM(CASE WHEN a.is_user_turn = 1 THEN 1 ELSE 0 END) " +
		"FROM api_calls a LEFT JOIN session_workspaces sw ON a.session_id = sw.session_id AND a.source = sw.source" + where +
		" GROUP BY day, cwd, a.model_normalized"
	rows, err := q.Query(query, params...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]map[string]map[string]*dbModelStats)
	for rows.Next() {
		var day, cwd, model string
		var s dbModelStats
		rows.Scan(&day, &cwd, &model, &s.APICalls, &s.PromptTokens, &s.CompletionTokens,
			&s.CacheCreationTokens, &s.CacheReadTokens, &s.UserTurns)
		if day == "" {
			day = "unknown"
		}
		if result[day] == nil {
			result[day] = make(map[string]map[string]*dbModelStats)
		}
		if result[day][cwd] == nil {
			result[day][cwd] = make(map[string]*dbModelStats)
		}
		result[day][cwd][model] = &s
	}
	return result
}

func queryRecords(q dbQuerier, dateFrom, dateTo, projectFilter string) []Record {
	where, params := buildFilters(dateFrom, dateTo, projectFilter)
	query := "SELECT model, model_normalized, prompt_tokens, completion_tokens, " +
		"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
		"timestamp, session_id, log_file, source FROM api_calls a" + where
	rows, err := q.Query(query, params...)
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

func querySessionWorkspaces(q dbQuerier, hasBranch bool) map[string]workspaceMeta {
	branchExpr := "NULL"
	if hasBranch {
		branchExpr = "branch"
	}
	rows, err := q.Query("SELECT session_id, cwd, source, " + branchExpr + " FROM session_workspaces")
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

func queryLogFileCount(q dbQuerier, dateFrom, dateTo, projectFilter string) int {
	where, params := buildFilters(dateFrom, dateTo, projectFilter)
	query := "SELECT COUNT(DISTINCT log_file) FROM api_calls a" + where
	var count int
	q.QueryRow(query, params...).Scan(&count)
	return count
}

func syncLogsToDB(db *sql.DB, logsDir, sessionDir string, force bool, source string, minTime, maxTime *time.Time) int {
	return syncServiceForDB(db).SyncLogsToDB(logsDir, sessionDir, force, source, minTime, maxTime)
}

func syncLogsToDBWithLogf(db *sql.DB, logsDir, sessionDir string, force bool, source string, minTime, maxTime *time.Time, logf func(format string, args ...interface{})) int {
	return syncServiceForDBWithLogf(db, logf).SyncLogsToDB(logsDir, sessionDir, force, source, minTime, maxTime)
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

func toSyncCodespaceInfo(cs codespaceInfo) syncservice.CodespaceInfo {
	return syncservice.CodespaceInfo{Name: cs.Name, State: cs.State, LastUsedAt: cs.LastUsedAt}
}

func fromSyncCodespaceInfo(cs syncservice.CodespaceInfo) codespaceInfo {
	return codespaceInfo{Name: cs.Name, State: cs.State, LastUsedAt: cs.LastUsedAt}
}

func fromSyncCodespaces(codespaces []syncservice.CodespaceInfo) []codespaceInfo {
	if codespaces == nil {
		return nil
	}
	out := make([]codespaceInfo, 0, len(codespaces))
	for _, cs := range codespaces {
		out = append(out, fromSyncCodespaceInfo(cs))
	}
	return out
}

func fromSyncCodespaceCopyResult(res syncservice.CodespaceCopyResult) codespaceCopyResult {
	return codespaceCopyResult{
		Idx:        res.Idx,
		Codespace:  fromSyncCodespaceInfo(res.Codespace),
		TmpDir:     res.TmpDir,
		LogsDir:    res.LogsDir,
		SessionDir: res.SessionDir,
		Copied:     res.Copied,
	}
}

func listCodespaces(includeStopped bool) []codespaceInfo {
	return fromSyncCodespaces(syncservice.NewService(nil, nil).ListCodespaces(includeStopped))
}

func isCodespaceStartThrottleError(msg string) bool {
	return syncservice.IsCodespaceStartThrottleError(msg)
}

func codespaceThrottleBackoff(attempt int) time.Duration {
	return syncservice.CodespaceThrottleBackoff(attempt)
}

func summarizeSyncCommandStderr(stderr string) string {
	return syncservice.SummarizeSyncCommandStderr(stderr)
}

// isTarFileChangedWarning returns true when tar exited with code 1 due to
// "file changed as we read it" — a benign warning, not a fatal error.
func isTarFileChangedWarning(err error, stderr string) bool {
	return syncservice.IsTarFileChangedWarning(err, stderr)
}

func formatSshTarFailure(sshErr, tarErr error, sshStderr, tarStderr string) string {
	return syncservice.FormatSshTarFailure(sshErr, tarErr, sshStderr, tarStderr)
}

func copyCodespaceData(cs codespaceInfo, idx, total int, stoppedStartLimiter chan struct{}) codespaceCopyResult {
	return fromSyncCodespaceCopyResult(
		syncservice.NewService(nil, nil).CopyCodespaceData(toSyncCodespaceInfo(cs), idx, total, stoppedStartLimiter),
	)
}

func humanSize(bytes int64) string {
	return syncservice.HumanSize(bytes)
}

func dirStats(root string) (int, int64) {
	return syncservice.DirStats(root)
}

func listRemoteLogFiles(csName string) ([]string, error) {
	return syncservice.NewService(nil, nil).ListRemoteLogFiles(csName)
}

func getKnownLogFiles(db *sql.DB, source string) map[string]bool {
	return storagelayer.NewService(db).KnownLogFiles(source)
}

func syncCodespacesToDBTick(db *sql.DB, includeStopped bool, force bool) (int, error) {
	return syncServiceForDB(db).SyncCodespacesToDBTick(includeStopped, force)
}

func syncCodespacesToDBTickWithLogf(db *sql.DB, includeStopped bool, force bool, logf func(format string, args ...interface{})) (int, error) {
	return syncServiceForDBWithLogf(db, logf).SyncCodespacesToDBTick(includeStopped, force)
}

func syncCodespacesToDB(db *sql.DB, includeStopped bool, force bool) int {
	return syncServiceForDB(db).SyncCodespacesToDB(includeStopped, force)
}

func syncCodespacesToDBWithLogf(db *sql.DB, includeStopped bool, force bool, logf func(format string, args ...interface{})) int {
	return syncServiceForDBWithLogf(db, logf).SyncCodespacesToDB(includeStopped, force)
}

func projectName(cwd string) string {
	home, _ := os.UserHomeDir()
	path := strings.Replace(cwd, home, "~", 1)
	path = reICloudObsidian.ReplaceAllString(path, "📓 ")
	return path
}

// ─── Cost helpers ───────────────────────────────────────────────────────────

func calcCost(model string, s *Stats, timestamp string) float64 {
	return costing.CalcCost(pricingPeriods, model, (*domain.Stats)(s), timestamp)
}

func calcCostNocache(model string, s *Stats, timestamp string) float64 {
	return costing.CalcCostNoCache(pricingPeriods, model, (*domain.Stats)(s), timestamp)
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
	return costing.UncachedInput((*domain.Stats)(s))
}

// ─── Timestamp parsing ──────────────────────────────────────────────────────

// ─── Main ───────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sql" {
		runSQL(os.Args[2:])
		return
	}
	runLegacyCLI()
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
