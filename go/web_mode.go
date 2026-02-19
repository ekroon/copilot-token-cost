package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type webModeConfig struct {
	ListenAddress            string
	RefreshInterval          time.Duration
	CodespacesMode           string
	CodespacesInterval       time.Duration
	CodespacesIncludeStopped bool
	LogsDir                  string
	SessionDir               string
	PeriodLabel              string
	DateRange                string
	DateFromQuery            string
	DateToQuery              string
	SyncFrom                 *time.Time
	SyncTo                   *time.Time
}

type webState struct {
	db                       *sql.DB
	logsDir                  string
	sessionDir               string
	periodLabel              string
	dateRange                string
	dateFromQuery            string
	dateToQuery              string
	syncFrom                 *time.Time
	syncTo                   *time.Time
	codespacesMode           string
	codespacesIncludeStopped bool
	localRefreshInterval     time.Duration
	localNextRefreshAt       time.Time
	codespacesInterval       time.Duration
	codespacesNextRefreshAt  time.Time
	codespacesLastSuccessAt  time.Time
	codespacesHasSuccess     bool

	snapshotMu sync.RWMutex
	snapshot   statsPayload
	hasData    bool
	syncStatus map[string]syncSourceStatus

	refreshMu     sync.Mutex
	subscribersMu sync.Mutex
	subscribers   map[chan string]struct{}
}

type webActionError struct {
	status  int
	reason  string
	message string
}

type webActionErrorResponse struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

const (
	webSyncCodeOK      = "ok"
	webSyncCodeSkipped = "skipped"
	webSyncCodeError   = "error"
	webSyncCodeTimeout = "timeout"
	webSyncCodeStale   = "stale"

	webSyncReasonInProgress          = "sync_in_progress"
	webSyncReasonNotStarted          = "not_started"
	webSyncReasonManualMode          = "manual_mode"
	webSyncReasonAutoMode            = "auto_mode"
	webSyncReasonLocalSyncCompleted  = "local_sync_completed"
	webSyncReasonLocalLogsDirMissing = "logs_dir_not_found"
	webSyncReasonCodespacesNoChanges = "codespaces_no_changes"
	webSyncReasonCodespacesCompleted = "codespaces_sync_completed"
	webSyncReasonCodespacesStale     = "codespaces_sync_stale"

	defaultWebCodespacesInterval = 5 * time.Minute
	maxWebCodespacesRetryBackoff = 30 * time.Minute
)

var webEventsHeartbeatInterval = 25 * time.Second
var webIndicatorRefreshInterval = time.Second

func (e *webActionError) Error() string {
	return e.message
}

func newSyncSourceStatus(code, reason string) syncSourceStatus {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "unspecified"
	}
	return syncSourceStatus{
		Code:      code,
		Reason:    reason,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func cloneSyncStatus(in map[string]syncSourceStatus) map[string]syncSourceStatus {
	out := make(map[string]syncSourceStatus, len(in))
	for source, status := range in {
		out[source] = status
	}
	return out
}

func classifySyncCodeFromError(err error) string {
	if err == nil {
		return webSyncCodeOK
	}
	if strings.Contains(strings.ToLower(err.Error()), "timed out") {
		return webSyncCodeTimeout
	}
	return webSyncCodeError
}

func normalizeCodespacesInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return defaultWebCodespacesInterval
	}
	return interval
}

func computeCodespacesRetryCap(interval time.Duration) time.Duration {
	base := normalizeCodespacesInterval(interval)
	capDuration := base * 6
	if capDuration < base {
		return maxWebCodespacesRetryBackoff
	}
	if capDuration > maxWebCodespacesRetryBackoff {
		return maxWebCodespacesRetryBackoff
	}
	return capDuration
}

func nextCodespacesRetryDelay(current, maxDelay time.Duration) time.Duration {
	if current <= 0 {
		return maxDelay
	}
	next := current * 2
	if next < current || next > maxDelay {
		return maxDelay
	}
	return next
}

func newWebState(cfg webModeConfig) (*webState, error) {
	dbPath := getDBPath()
	db := initDB(dbPath)
	mode := strings.ToLower(strings.TrimSpace(cfg.CodespacesMode))
	if mode == "" {
		mode = "auto"
	}
	codespacesInterval := time.Duration(0)
	codespacesReason := webSyncReasonManualMode
	if mode == "auto" {
		codespacesInterval = normalizeCodespacesInterval(cfg.CodespacesInterval)
		codespacesReason = webSyncReasonAutoMode
	}
	state := &webState{
		db:                       db,
		logsDir:                  cfg.LogsDir,
		sessionDir:               cfg.SessionDir,
		periodLabel:              strings.TrimSpace(cfg.PeriodLabel),
		dateRange:                strings.TrimSpace(cfg.DateRange),
		dateFromQuery:            strings.TrimSpace(cfg.DateFromQuery),
		dateToQuery:              strings.TrimSpace(cfg.DateToQuery),
		syncFrom:                 cfg.SyncFrom,
		syncTo:                   cfg.SyncTo,
		codespacesMode:           mode,
		codespacesIncludeStopped: cfg.CodespacesIncludeStopped,
		localRefreshInterval:     cfg.RefreshInterval,
		codespacesInterval:       codespacesInterval,
		syncStatus: map[string]syncSourceStatus{
			"local":      newSyncSourceStatus(webSyncCodeSkipped, webSyncReasonNotStarted),
			"codespaces": newSyncSourceStatus(webSyncCodeSkipped, codespacesReason),
		},
	}
	state.rebuildSnapshot()
	return state, nil
}

func (s *webState) close() {
	s.closeSubscribers()
	if s.db != nil {
		_ = s.db.Close()
	}
}

func (s *webState) subscribe() (<-chan string, func()) {
	updates := make(chan string, 1)
	s.subscribersMu.Lock()
	if s.subscribers == nil {
		s.subscribers = make(map[chan string]struct{})
	}
	s.subscribers[updates] = struct{}{}
	s.subscribersMu.Unlock()

	return updates, func() {
		s.subscribersMu.Lock()
		if _, ok := s.subscribers[updates]; ok {
			delete(s.subscribers, updates)
			close(updates)
		}
		s.subscribersMu.Unlock()
	}
}

func pushLatestUpdate(ch chan string, patch string) {
	select {
	case ch <- patch:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- patch:
		default:
		}
	}
}

func (s *webState) broadcast(patch string) {
	if patch == "" {
		return
	}
	s.subscribersMu.Lock()
	for ch := range s.subscribers {
		pushLatestUpdate(ch, patch)
	}
	s.subscribersMu.Unlock()
}

func (s *webState) closeSubscribers() {
	s.subscribersMu.Lock()
	for ch := range s.subscribers {
		delete(s.subscribers, ch)
		close(ch)
	}
	s.subscribersMu.Unlock()
}

func (s *webState) setLocalRefreshSchedule(interval time.Duration, next time.Time) {
	s.snapshotMu.Lock()
	s.localRefreshInterval = interval
	s.localNextRefreshAt = next
	s.snapshotMu.Unlock()
}

func (s *webState) setCodespacesRefreshSchedule(interval time.Duration, next time.Time) {
	s.snapshotMu.Lock()
	s.codespacesInterval = interval
	s.codespacesNextRefreshAt = next
	s.snapshotMu.Unlock()
}

func (s *webState) setSyncStatus(source, code, reason string) {
	if strings.TrimSpace(source) == "" {
		source = "unknown"
	}
	if strings.TrimSpace(code) == "" {
		code = webSyncCodeError
	}
	status := newSyncSourceStatus(code, reason)

	s.snapshotMu.Lock()
	if s.syncStatus == nil {
		s.syncStatus = make(map[string]syncSourceStatus)
	}
	s.syncStatus[source] = status
	if s.hasData {
		s.snapshot.SyncStatus = cloneSyncStatus(s.syncStatus)
	}
	s.snapshotMu.Unlock()

	payload, hasSnapshot := s.getSnapshot()
	if !hasSnapshot {
		return
	}
	patch, err := s.buildDashboardPatch(payload, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "web sync status patch build failed: %v\n", err)
		return
	}
	s.broadcast(patch)
}

func (s *webState) beginSyncAction(source string) error {
	if s.refreshMu.TryLock() {
		s.setSyncStatus(source, webSyncCodeSkipped, webSyncReasonInProgress)
		return nil
	}
	s.setSyncStatus(source, webSyncCodeSkipped, webSyncReasonInProgress)
	return &webActionError{
		status:  http.StatusConflict,
		reason:  webSyncReasonInProgress,
		message: "sync already in progress",
	}
}

func (s *webState) rebuildSnapshot() {
	periodLabel := strings.TrimSpace(s.periodLabel)
	if periodLabel == "" {
		periodLabel = "all time"
	}
	aggregated := loadAggregatedStats(s.db, s.dateFromQuery, s.dateToQuery, "")
	payload := buildStatsPayload(aggregated, periodLabel, s.dateRange)

	s.snapshotMu.Lock()
	payload.SyncStatus = cloneSyncStatus(s.syncStatus)
	s.snapshot = payload
	s.hasData = true
	s.snapshotMu.Unlock()

	patch, err := s.buildDashboardPatch(payload, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "web snapshot patch build failed: %v\n", err)
		return
	}
	s.broadcast(patch)
}

func (s *webState) refreshLocalSnapshot() error {
	if err := s.beginSyncAction("local"); err != nil {
		return err
	}
	defer s.refreshMu.Unlock()

	if err := s.db.Ping(); err != nil {
		statusReason := fmt.Sprintf("database_unavailable: %v", err)
		s.setSyncStatus("local", webSyncCodeError, statusReason)
		return &webActionError{
			status:  http.StatusServiceUnavailable,
			reason:  "database_unavailable",
			message: statusReason,
		}
	}
	logsDir := strings.TrimSpace(s.logsDir)
	if logsDir == "" {
		statusReason := "logs directory is empty"
		s.setSyncStatus("local", webSyncCodeError, statusReason)
		return &webActionError{
			status:  http.StatusInternalServerError,
			reason:  "logs_dir_empty",
			message: statusReason,
		}
	}
	if info, err := os.Stat(logsDir); err != nil {
		if !os.IsNotExist(err) {
			statusReason := fmt.Sprintf("stat logs directory failed: %v", err)
			s.setSyncStatus("local", webSyncCodeError, statusReason)
			return &webActionError{
				status:  http.StatusInternalServerError,
				reason:  "logs_dir_stat_failed",
				message: statusReason,
			}
		}
		s.setSyncStatus("local", webSyncCodeSkipped, webSyncReasonLocalLogsDirMissing)
	} else if !info.IsDir() {
		statusReason := fmt.Sprintf("logs path is not a directory: %s", logsDir)
		s.setSyncStatus("local", webSyncCodeError, statusReason)
		return &webActionError{
			status:  http.StatusInternalServerError,
			reason:  "logs_dir_not_directory",
			message: statusReason,
		}
	} else {
		syncLogsToDB(s.db, logsDir, s.sessionDir, false, "local", s.syncFrom, s.syncTo)
		s.setSyncStatus("local", webSyncCodeOK, webSyncReasonLocalSyncCompleted)
	}

	s.rebuildSnapshot()
	return nil
}

func (s *webState) classifyCodespacesSyncFailure(err error, auto bool) (string, string) {
	statusCode := classifySyncCodeFromError(err)
	if auto && s.codespacesHasSuccess {
		lastSuccess := "unknown"
		if !s.codespacesLastSuccessAt.IsZero() {
			lastSuccess = s.codespacesLastSuccessAt.UTC().Format(time.RFC3339Nano)
		}
		return webSyncCodeStale, fmt.Sprintf("%s last_success=%s error=%v", webSyncReasonCodespacesStale, lastSuccess, err)
	}
	if auto {
		return statusCode, fmt.Sprintf("codespaces auto sync failed: %v", err)
	}
	return statusCode, fmt.Sprintf("codespaces sync failed: %v", err)
}

func (s *webState) syncCodespacesSnapshotWithMode(requireManual bool) error {
	if err := s.beginSyncAction("codespaces"); err != nil {
		return err
	}
	defer s.refreshMu.Unlock()

	if requireManual && s.codespacesMode != "manual" {
		statusReason := fmt.Sprintf("manual mode required (current: %s)", s.codespacesMode)
		s.setSyncStatus("codespaces", webSyncCodeSkipped, statusReason)
		return &webActionError{
			status:  http.StatusConflict,
			reason:  "codespaces_mode_not_manual",
			message: statusReason,
		}
	}
	if err := s.db.Ping(); err != nil {
		statusReason := fmt.Sprintf("database unavailable: %v", err)
		s.setSyncStatus("codespaces", webSyncCodeError, statusReason)
		return &webActionError{
			status:  http.StatusServiceUnavailable,
			reason:  "database_unavailable",
			message: statusReason,
		}
	}
	total, err := syncCodespacesToDBTick(s.db, s.codespacesIncludeStopped, false)
	if err != nil {
		statusCode, statusReason := s.classifyCodespacesSyncFailure(err, !requireManual)
		s.setSyncStatus("codespaces", statusCode, statusReason)
		actionReason := "codespaces_sync_failed"
		if statusCode == webSyncCodeTimeout {
			actionReason = "codespaces_sync_timeout"
		} else if statusCode == webSyncCodeStale {
			actionReason = webSyncReasonCodespacesStale
		}
		return &webActionError{
			status:  http.StatusBadGateway,
			reason:  actionReason,
			message: statusReason,
		}
	}
	if total == 0 {
		s.setSyncStatus("codespaces", webSyncCodeSkipped, webSyncReasonCodespacesNoChanges)
	} else {
		s.setSyncStatus("codespaces", webSyncCodeOK, webSyncReasonCodespacesCompleted)
	}
	s.codespacesHasSuccess = true
	s.codespacesLastSuccessAt = time.Now().UTC()

	s.rebuildSnapshot()
	return nil
}

func (s *webState) syncCodespacesSnapshot() error {
	return s.syncCodespacesSnapshotWithMode(true)
}

func (s *webState) syncCodespacesSnapshotAuto() error {
	return s.syncCodespacesSnapshotWithMode(false)
}

func (s *webState) startCodespacesAutoSyncLoop(interval time.Duration) {
	cadence := normalizeCodespacesInterval(interval)
	s.setCodespacesRefreshSchedule(cadence, time.Now().Add(cadence))
	ticker := time.NewTicker(cadence)
	go func() {
		for tick := range ticker.C {
			s.setCodespacesRefreshSchedule(cadence, tick.Add(cadence))
			go runScheduledSync(s.syncCodespacesSnapshotAuto, "web codespaces auto sync failed")
		}
	}()
}

func (s *webState) startCodespacesAutoSync(interval time.Duration) {
	if err := s.syncCodespacesSnapshotAuto(); err != nil {
		fmt.Fprintf(os.Stderr, "web codespaces auto sync failed: %v\n", err)
	}
	s.startCodespacesAutoSyncLoop(interval)
}

func isSyncInProgressError(err error) bool {
	if err == nil {
		return false
	}
	var actionErr *webActionError
	return errors.As(err, &actionErr) && actionErr.reason == webSyncReasonInProgress
}

func runScheduledSync(syncFn func() error, logPrefix string) {
	if err := syncFn(); err != nil && !isSyncInProgressError(err) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", logPrefix, err)
	}
}

func (s *webState) runStartupSync(source string, syncFn func() error) {
	fmt.Fprintf(os.Stderr, "web startup %s sync started\n", source)
	for {
		err := syncFn()
		if err == nil {
			fmt.Fprintf(os.Stderr, "web startup %s sync completed\n", source)
			return
		}
		if isSyncInProgressError(err) {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		fmt.Fprintf(os.Stderr, "web startup %s sync failed: %v\n", source, err)
		return
	}
}

func (s *webState) startStartupSyncWith(localSync, codespacesSync func() error, startCodespacesLoop func()) {
	go s.runStartupSync("local", localSync)
	if s.codespacesMode == "auto" {
		go func() {
			s.runStartupSync("codespaces", codespacesSync)
			startCodespacesLoop()
		}()
	}
}

func (s *webState) startStartupSync(codespacesInterval time.Duration) {
	s.startStartupSyncWith(
		s.refreshLocalSnapshot,
		s.syncCodespacesSnapshotAuto,
		func() {
			s.startCodespacesAutoSyncLoop(codespacesInterval)
		},
	)
}

func (s *webState) getSnapshot() (statsPayload, bool) {
	s.snapshotMu.RLock()
	defer s.snapshotMu.RUnlock()
	return s.snapshot, s.hasData
}

func handleMethod(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	http.Error(w, fmt.Sprintf("method not allowed: expected %s", want), http.StatusMethodNotAllowed)
	return false
}

func writeJSONResponse(w http.ResponseWriter, status int, value interface{}) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to encode JSON response: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		fmt.Fprintf(os.Stderr, "web response write failed: %v\n", err)
	}
}

func writeActionError(w http.ResponseWriter, actionErr *webActionError) {
	if actionErr == nil {
		writeJSONResponse(w, http.StatusInternalServerError, webActionErrorResponse{
			Error:  "unknown action error",
			Reason: "unknown_error",
		})
		return
	}
	writeJSONResponse(w, actionErr.status, webActionErrorResponse{
		Error:  actionErr.message,
		Reason: actionErr.reason,
	})
}

func appendDatastarOuterPatch(b *strings.Builder, selector, element string) {
	b.WriteString("event: datastar-patch-elements\n")
	fmt.Fprintf(b, "data: selector %s\n", selector)
	b.WriteString("data: mode outer\n")
	fmt.Fprintf(b, "data: elements %s\n\n", element)
}

func formatRefreshCountdown(remaining time.Duration) string {
	if remaining <= 0 {
		return "0s"
	}
	seconds := int((remaining + time.Second - 1) / time.Second)
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	seconds = seconds % 60
	if minutes < 60 {
		if seconds == 0 {
			return fmt.Sprintf("%dm", minutes)
		}
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	}
	hours := minutes / 60
	minutes = minutes % 60
	if seconds == 0 {
		return fmt.Sprintf("%dh %02dm", hours, minutes)
	}
	return fmt.Sprintf("%dh %02dm %02ds", hours, minutes, seconds)
}

func renderRefreshIndicatorRow(label, status, countdown string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="refresh-indicator-row"><span class="refresh-indicator-name">%s</span><span class="refresh-indicator-state">%s</span>`,
		html.EscapeString(label),
		html.EscapeString(status),
	)
	if strings.TrimSpace(countdown) != "" {
		fmt.Fprintf(&b, `<span class="refresh-indicator-countdown">%s</span>`, html.EscapeString(countdown))
	}
	b.WriteString("</div>")
	return b.String()
}

func (s *webState) renderRefreshIndicators(now time.Time) string {
	s.snapshotMu.RLock()
	localInterval := s.localRefreshInterval
	localNext := s.localNextRefreshAt
	codespacesMode := s.codespacesMode
	codespacesInterval := s.codespacesInterval
	codespacesNext := s.codespacesNextRefreshAt
	localStatus := s.syncStatus["local"]
	codespacesStatus := s.syncStatus["codespaces"]
	s.snapshotMu.RUnlock()

	localState := "Off"
	localCountdown := ""
	if localInterval > 0 {
		localState = "Idle"
		if !localNext.IsZero() {
			localCountdown = formatRefreshCountdown(localNext.Sub(now))
		}
	}
	if localStatus.Reason == webSyncReasonInProgress {
		localState = "Running"
	}

	codespacesState := "Idle"
	codespacesCountdown := ""
	if codespacesMode == "manual" {
		codespacesState = "Manual"
	} else if codespacesInterval <= 0 || codespacesNext.IsZero() {
		codespacesState = "Starting"
	} else {
		codespacesCountdown = formatRefreshCountdown(codespacesNext.Sub(now))
	}
	if codespacesStatus.Reason == webSyncReasonInProgress {
		codespacesState = "Running"
	}

	return `<div id="refresh-indicators-region"><div id="refresh-indicators" class="refresh-indicators">` +
		renderRefreshIndicatorRow("Local", localState, localCountdown) +
		renderRefreshIndicatorRow("Codespaces", codespacesState, codespacesCountdown) +
		`</div></div>`
}

func (s *webState) buildRefreshIndicatorsPatch(now time.Time) string {
	var patch strings.Builder
	appendDatastarOuterPatch(&patch, "#refresh-indicators-region", s.renderRefreshIndicators(now))
	return patch.String()
}

func (s *webState) buildDashboardPatch(payload statsPayload, now time.Time) (string, error) {
	patch, err := buildRefreshPatch(payload)
	if err != nil {
		return "", err
	}
	return patch + s.buildRefreshIndicatorsPatch(now), nil
}

func buildRefreshPatch(payload statsPayload) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to encode refresh payload: %w", err)
	}
	escaped := html.EscapeString(string(body))

	var patch strings.Builder
	appendDatastarOuterPatch(&patch, "#overview-summary", `<p id="overview-summary">`+renderWebOverviewSummary(payload, true)+`</p>`)
	appendDatastarOuterPatch(&patch, "#sync-status-region", `<div id="sync-status-region">`+renderWebSyncStatusTable(payload, true)+`</div>`)
	appendDatastarOuterPatch(&patch, "#model-summary-region", `<div id="model-summary-region">`+renderWebModelSummaryTable(payload, true)+`</div>`)
	appendDatastarOuterPatch(&patch, "#project-summary-region", `<div id="project-summary-region">`+renderWebProjectSummaryTable(payload, true)+`</div>`)
	appendDatastarOuterPatch(&patch, "#daily-totals-region", `<div id="daily-totals-region">`+renderWebDailyTotalsTable(payload, true)+`</div>`)
	appendDatastarOuterPatch(&patch, "#stats-json", `<pre id="stats-json">`+escaped+`</pre>`)
	return patch.String(), nil
}

type webStatsRow struct {
	name  string
	stats statsPayloadStats
}

type webDailyTotalsRow struct {
	day              string
	apiCalls         int
	premiumRequests  float64
	totalCost        float64
	totalCostNoCache float64
}

func sortedWebStatsRows(statsMap map[string]statsPayloadStats) []webStatsRow {
	rows := make([]webStatsRow, 0, len(statsMap))
	for name, stats := range statsMap {
		rows = append(rows, webStatsRow{name: name, stats: stats})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].stats.Cost == rows[j].stats.Cost {
			return rows[i].name < rows[j].name
		}
		return rows[i].stats.Cost > rows[j].stats.Cost
	})
	return rows
}

func webFloat64(value interface{}) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		parsed, err := v.Float64()
		if err == nil {
			return parsed
		}
	}
	return 0
}

func webDailyStatsValue(value interface{}) (statsPayloadStats, bool) {
	switch v := value.(type) {
	case statsPayloadStats:
		return v, true
	case map[string]interface{}:
		return statsPayloadStats{
			APICalls:        int(webFloat64(v["api_calls"])),
			PremiumRequests: webFloat64(v["premium_requests"]),
		}, true
	}
	return statsPayloadStats{}, false
}

func buildWebDailyTotalsRows(payload statsPayload) []webDailyTotalsRow {
	days := make([]string, 0, len(payload.Daily))
	for day := range payload.Daily {
		days = append(days, day)
	}
	sort.Strings(days)

	rows := make([]webDailyTotalsRow, 0, len(days))
	for _, day := range days {
		dayMap := payload.Daily[day]
		row := webDailyTotalsRow{
			day:              day,
			totalCost:        webFloat64(dayMap["_total_cost"]),
			totalCostNoCache: webFloat64(dayMap["_total_cost_without_cache"]),
		}
		for key, value := range dayMap {
			if strings.HasPrefix(key, "_") {
				continue
			}
			stats, ok := webDailyStatsValue(value)
			if !ok {
				continue
			}
			row.apiCalls += stats.APICalls
			row.premiumRequests += stats.PremiumRequests
		}
		rows = append(rows, row)
	}
	return rows
}

func renderWebModelSummaryTable(payload statsPayload, hasSnapshot bool) string {
	if !hasSnapshot {
		return "<p>Loading model summary…</p>"
	}
	rows := sortedWebStatsRows(payload.Models)
	if len(rows) == 0 {
		return "<p>No model data available.</p>"
	}

	totalPremium := 0.0
	var b strings.Builder
	b.WriteString(`<table id="model-summary-table"><thead><tr><th>Model</th><th>Calls</th><th>Premium</th><th>Cost</th><th>Prem Cost</th></tr></thead><tbody>`)
	for _, row := range rows {
		totalPremium += row.stats.PremiumRequests
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
			html.EscapeString(row.name),
			commaInt(row.stats.APICalls),
			commaFloat(row.stats.PremiumRequests, 0),
			fmtCost(row.stats.Cost),
			fmtCost(row.stats.PremiumRequestCost),
		)
	}
	fmt.Fprintf(&b, "</tbody><tfoot><tr><th>Total</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th></tr></tfoot></table>",
		commaInt(payload.APICalls),
		commaFloat(totalPremium, 0),
		fmtCost(payload.TotalCost),
		fmtCost(payload.TotalPremiumRequestCost),
	)
	return b.String()
}

func renderWebProjectSummaryTable(payload statsPayload, hasSnapshot bool) string {
	if !hasSnapshot {
		return "<p>Loading project summary…</p>"
	}
	rows := sortedWebStatsRows(payload.Projects)
	if len(rows) == 0 {
		return "<p>No project data available.</p>"
	}

	totalPremium := 0.0
	var b strings.Builder
	b.WriteString(`<table id="project-summary-table"><thead><tr><th>Project</th><th>Calls</th><th>Premium</th><th>Cost</th><th>Prem Cost</th></tr></thead><tbody>`)
	for _, row := range rows {
		totalPremium += row.stats.PremiumRequests
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
			html.EscapeString(row.name),
			commaInt(row.stats.APICalls),
			commaFloat(row.stats.PremiumRequests, 0),
			fmtCost(row.stats.Cost),
			fmtCost(row.stats.PremiumRequestCost),
		)
	}
	fmt.Fprintf(&b, "</tbody><tfoot><tr><th>Total</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th></tr></tfoot></table>",
		commaInt(payload.APICalls),
		commaFloat(totalPremium, 0),
		fmtCost(payload.TotalCost),
		fmtCost(payload.TotalPremiumRequestCost),
	)
	return b.String()
}

func renderWebDailyTotalsTable(payload statsPayload, hasSnapshot bool) string {
	if !hasSnapshot {
		return "<p>Loading daily totals…</p>"
	}
	rows := buildWebDailyTotalsRows(payload)
	if len(rows) == 0 {
		return "<p>No daily totals available.</p>"
	}

	totalPremium := 0.0
	var b strings.Builder
	b.WriteString(`<table id="daily-totals-table"><thead><tr><th>Date</th><th>Calls</th><th>Premium</th><th>Cost</th></tr></thead><tbody>`)
	for _, row := range rows {
		totalPremium += row.premiumRequests
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
			html.EscapeString(row.day),
			commaInt(row.apiCalls),
			commaFloat(row.premiumRequests, 0),
			fmtCost(row.totalCost),
		)
	}
	fmt.Fprintf(&b, "</tbody><tfoot><tr><th>Total</th><th>%s</th><th>%s</th><th>%s</th></tr></tfoot></table>",
		commaInt(payload.APICalls),
		commaFloat(totalPremium, 0),
		fmtCost(payload.TotalCost),
	)
	return b.String()
}

func renderWebSyncStatusTable(payload statsPayload, hasSnapshot bool) string {
	if !hasSnapshot {
		return "<p>Loading sync status…</p>"
	}
	if len(payload.SyncStatus) == 0 {
		return "<p>No sync status available.</p>"
	}

	sources := make([]string, 0, len(payload.SyncStatus))
	for source := range payload.SyncStatus {
		sources = append(sources, source)
	}
	sort.Strings(sources)

	var b strings.Builder
	b.WriteString(`<table id="sync-status-table"><thead><tr><th>Source</th><th>Code</th><th>Reason</th><th>Updated</th></tr></thead><tbody>`)
	for _, source := range sources {
		status := payload.SyncStatus[source]
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
			html.EscapeString(source),
			html.EscapeString(status.Code),
			html.EscapeString(status.Reason),
			html.EscapeString(status.UpdatedAt),
		)
	}
	b.WriteString("</tbody></table>")
	return b.String()
}

func renderWebOverviewSummary(payload statsPayload, hasSnapshot bool) string {
	if !hasSnapshot {
		return "Loading stats snapshot…"
	}

	dateRange := ""
	if payload.DateRange != nil && strings.TrimSpace(*payload.DateRange) != "" {
		dateRange = fmt.Sprintf(" (%s)", html.EscapeString(*payload.DateRange))
	}
	return fmt.Sprintf("Period: %s%s · Log files: %s · API calls: %s · Total cost: %s",
		html.EscapeString(payload.Period),
		dateRange,
		commaInt(payload.LogFiles),
		commaInt(payload.APICalls),
		fmtCost(payload.TotalCost),
	)
}

func dashboardOverviewHTML(payload statsPayload, hasSnapshot bool) string {
	return fmt.Sprintf(`<p id="overview-summary">%s</p>
  <section>
    <h2>Sync status</h2>
    <div id="sync-status-region">%s</div>
  </section>
  <section>
    <h2>Per-model summary</h2>
    <div id="model-summary-region">%s</div>
  </section>
  <section>
    <h2>Per-project summary</h2>
    <div id="project-summary-region">%s</div>
  </section>
  <section>
    <h2>Daily totals</h2>
    <div id="daily-totals-region">%s</div>
  </section>`,
		renderWebOverviewSummary(payload, hasSnapshot),
		renderWebSyncStatusTable(payload, hasSnapshot),
		renderWebModelSummaryTable(payload, hasSnapshot),
		renderWebProjectSummaryTable(payload, hasSnapshot),
		renderWebDailyTotalsTable(payload, hasSnapshot),
	)
}

func dashboardShellHTML(payload statsPayload, hasSnapshot bool) string {
	placeholderIndicators := `<div id="refresh-indicators-region"><div id="refresh-indicators" class="refresh-indicators">` +
		renderRefreshIndicatorRow("Local", "Idle", "") +
		renderRefreshIndicatorRow("Codespaces", "Idle", "") +
		`</div></div>`
	return dashboardShellHTMLWithIndicators(payload, hasSnapshot, placeholderIndicators)
}

func dashboardShellHTMLWithIndicators(payload statsPayload, hasSnapshot bool, refreshIndicatorsHTML string) string {
	overviewHTML := dashboardOverviewHTML(payload, hasSnapshot)
	statsJSON := "Loading…"
	if hasSnapshot {
		body, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			statsJSON = fmt.Sprintf("failed to encode stats snapshot: %v", err)
		} else {
			statsJSON = string(body)
		}
	}
	if strings.TrimSpace(refreshIndicatorsHTML) == "" {
		refreshIndicatorsHTML = `<div id="refresh-indicators-region"></div>`
	}
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Copilot Token Cost Dashboard</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 1.5rem; }
    #dashboard-header { display: flex; justify-content: space-between; align-items: flex-start; gap: 1rem; margin-bottom: 1rem; }
    #dashboard-header h1 { margin: 0; }
    .refresh-indicators { font-size: 0.78rem; border: 1px solid #d1d5db; background: #f9fafb; border-radius: 0.4rem; padding: 0.3rem 0.5rem; min-width: 13rem; }
    .refresh-indicator-row { display: flex; justify-content: space-between; gap: 0.5rem; white-space: nowrap; }
    .refresh-indicator-row + .refresh-indicator-row { margin-top: 0.2rem; }
    .refresh-indicator-name { font-weight: 600; }
    .refresh-indicator-countdown { color: #4b5563; }
    h2 { margin: 1.25rem 0 0.5rem; }
    table { border-collapse: collapse; width: 100%%; margin-bottom: 1rem; }
    th, td { border: 1px solid #d1d5db; padding: 0.4rem 0.5rem; text-align: right; }
    th:first-child, td:first-child { text-align: left; }
    tfoot th, tfoot td { background: #f3f4f6; font-weight: 600; }
    #status { color: #b91c1c; margin-bottom: 1rem; }
    #stats-json { display: none; }
  </style>
</head>
<body data-signals:status-message="''">
  <header id="dashboard-header">
    <h1>Copilot Token Cost Dashboard</h1>
    %s
  </header>
  <div id="status" data-text="$statusMessage"></div>
  <main id="dashboard-overview">%s</main>
  <pre id="stats-json">%s</pre>
  <div data-init="@get('/events')"></div>
  <div data-on:datastar-fetch="
         evt.detail.type === 'started' && ($statusMessage = '');
         evt.detail.type === 'error' && ($statusMessage = String(evt.detail.error || 'request failed'));
        "></div>
  <script type="module" src="https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.0-RC.7/bundles/datastar.js"></script>
</body>
</html>`, refreshIndicatorsHTML, overviewHTML, html.EscapeString(statsJSON))
}

func newWebMux(state *webState) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if !handleMethod(w, r, http.MethodGet) {
			return
		}
		payload, hasSnapshot := state.getSnapshot()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := dashboardShellHTMLWithIndicators(payload, hasSnapshot, state.renderRefreshIndicators(time.Now()))
		if _, err := w.Write([]byte(page)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write / response: %v\n", err)
		}
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodGet) {
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		updates, unsubscribe := state.subscribe()
		defer unsubscribe()

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		heartbeat := time.NewTicker(webEventsHeartbeatInterval)
		defer heartbeat.Stop()
		indicators := time.NewTicker(webIndicatorRefreshInterval)
		defer indicators.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case patch, ok := <-updates:
				if !ok {
					return
				}
				if _, err := w.Write([]byte(patch)); err != nil {
					fmt.Fprintf(os.Stderr, "failed to write /events patch: %v\n", err)
					return
				}
				flusher.Flush()
			case t := <-heartbeat.C:
				if _, err := fmt.Fprintf(w, "event: heartbeat\ndata: %s\n\n", t.UTC().Format(time.RFC3339Nano)); err != nil {
					fmt.Fprintf(os.Stderr, "failed to write /events heartbeat: %v\n", err)
					return
				}
				flusher.Flush()
			case t := <-indicators.C:
				patch := state.buildRefreshIndicatorsPatch(t)
				if _, err := w.Write([]byte(patch)); err != nil {
					fmt.Fprintf(os.Stderr, "failed to write /events indicators patch: %v\n", err)
					return
				}
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodGet) {
			return
		}
		payload, ok := state.getSnapshot()
		if !ok {
			http.Error(w, "stats snapshot unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSONResponse(w, http.StatusOK, payload)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodGet) {
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write([]byte("ok\n")); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write /healthz response: %v\n", err)
		}
	})
	mux.HandleFunc("/actions/sync-codespaces", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodPost) {
			return
		}
		if err := state.syncCodespacesSnapshot(); err != nil {
			if actionErr, ok := err.(*webActionError); ok {
				writeActionError(w, actionErr)
				return
			}
			writeActionError(w, &webActionError{
				status:  http.StatusInternalServerError,
				reason:  "codespaces_sync_failed",
				message: fmt.Sprintf("codespaces sync failed: %v", err),
			})
			return
		}
		payload, ok := state.getSnapshot()
		if !ok {
			writeActionError(w, &webActionError{
				status:  http.StatusInternalServerError,
				reason:  "snapshot_unavailable",
				message: "codespaces sync failed: snapshot unavailable",
			})
			return
		}
		patch, err := state.buildDashboardPatch(payload, time.Now())
		if err != nil {
			writeActionError(w, &webActionError{
				status:  http.StatusInternalServerError,
				reason:  "refresh_patch_failed",
				message: err.Error(),
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		if _, err := w.Write([]byte(patch)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write /actions/sync-codespaces response: %v\n", err)
		}
	})
	return mux
}

func runWebMode(cfg webModeConfig) error {
	if strings.TrimSpace(cfg.ListenAddress) == "" {
		return fmt.Errorf("web listen address is required")
	}

	state, err := newWebState(cfg)
	if err != nil {
		return err
	}
	defer state.close()

	if cfg.RefreshInterval > 0 {
		state.setLocalRefreshSchedule(cfg.RefreshInterval, time.Now().Add(cfg.RefreshInterval))
		ticker := time.NewTicker(cfg.RefreshInterval)
		defer ticker.Stop()
		go func() {
			for tick := range ticker.C {
				state.setLocalRefreshSchedule(cfg.RefreshInterval, tick.Add(cfg.RefreshInterval))
				go runScheduledSync(state.refreshLocalSnapshot, "web refresh tick failed")
			}
		}()
	} else {
		state.setLocalRefreshSchedule(0, time.Time{})
	}
	mux := newWebMux(state)

	server := &http.Server{
		Addr:    cfg.ListenAddress,
		Handler: mux,
	}
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("web listen failed: %w", err)
	}
	defer func() { _ = listener.Close() }()
	fmt.Fprintf(os.Stderr, "Web mode listening on http://%s\n", cfg.ListenAddress)
	fmt.Fprintln(os.Stderr, "Web startup handoff: serving initial snapshot from existing DB state; initial local/codespaces sync running in background")
	state.startStartupSync(cfg.CodespacesInterval)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("web server failed: %w", err)
	}
	return nil
}
