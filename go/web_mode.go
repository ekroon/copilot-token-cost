package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type webModeConfig struct {
	ListenAddress            string
	RefreshInterval          time.Duration
	CodespacesMode           string
	CodespacesStreaming      bool
	LocalStreaming           bool
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
	localStreaming           bool
	codespacesMode           string
	codespacesStreaming      bool
	codespacesIncludeStopped bool
	localRefreshInterval     time.Duration
	localNextRefreshAt       time.Time
	codespacesInterval       time.Duration
	codespacesNextRefreshAt  time.Time
	codespacesLastSuccessAt  time.Time
	codespacesHasSuccess     bool

	snapshotMu   sync.RWMutex
	snapshot     statsPayload
	hasData      bool
	syncStatus   map[string]syncSourceStatus
	expandedRows map[string]map[string]bool // group ("project"/"day") -> rowKey -> expanded

	refreshMu     sync.Mutex
	subscribersMu sync.Mutex
	subscribers   map[chan string]struct{}
	localStreamMu sync.Mutex
	localStream   map[string]localLogState
	localChunkAt  time.Time
	stopCh        chan struct{}
	loopsWG       sync.WaitGroup
	closeOnce     sync.Once
}

type localLogState struct {
	Size  int64
	MTime time.Time
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
	webSyncReasonLocalStreaming      = "local_streaming"
	webSyncReasonCodespacesNoChanges = "codespaces_no_changes"
	webSyncReasonCodespacesCompleted = "codespaces_sync_completed"
	webSyncReasonCodespacesStale     = "codespaces_sync_stale"
	webSyncReasonCodespacesStreaming = "codespaces_streaming"

	defaultWebCodespacesInterval = 5 * time.Minute
	maxWebCodespacesRetryBackoff = 30 * time.Minute
)

var webEventsHeartbeatInterval = 25 * time.Second
var webIndicatorRefreshInterval = time.Second
var newFSWatcher = fsnotify.NewWatcher

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
		localStreaming:           cfg.LocalStreaming,
		codespacesMode:           mode,
		codespacesStreaming:      cfg.CodespacesStreaming,
		codespacesIncludeStopped: cfg.CodespacesIncludeStopped,
		localRefreshInterval:     cfg.RefreshInterval,
		codespacesInterval:       codespacesInterval,
		syncStatus: map[string]syncSourceStatus{
			"local":      newSyncSourceStatus(webSyncCodeSkipped, webSyncReasonNotStarted),
			"codespaces": newSyncSourceStatus(webSyncCodeSkipped, codespacesReason),
		},
		stopCh: make(chan struct{}),
	}
	state.rebuildSnapshot()
	return state, nil
}

func (s *webState) close() {
	s.closeOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
		s.loopsWG.Wait()
		s.closeSubscribers()
		if s.db != nil {
			_ = s.db.Close()
		}
	})
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

func (s *webState) setRowExpanded(group, rowKey string, expanded bool) {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	if s.expandedRows == nil {
		s.expandedRows = make(map[string]map[string]bool)
	}
	if s.expandedRows[group] == nil {
		s.expandedRows[group] = make(map[string]bool)
	}
	if expanded {
		s.expandedRows[group][rowKey] = true
	} else {
		delete(s.expandedRows[group], rowKey)
	}
}

func (s *webState) getExpandedRows() map[string]map[string]bool {
	s.snapshotMu.RLock()
	defer s.snapshotMu.RUnlock()
	result := make(map[string]map[string]bool, len(s.expandedRows))
	for group, rows := range s.expandedRows {
		cp := make(map[string]bool, len(rows))
		for k, v := range rows {
			cp[k] = v
		}
		result[group] = cp
	}
	return result
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
	payload := buildStatsPayload(aggregated, periodLabel, s.dateRange, 0)

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
	return s.refreshLocalSnapshotWithForce(false)
}

func (s *webState) refreshLocalSnapshotWithForce(force bool) error {
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
		syncLogsToDB(s.db, logsDir, s.sessionDir, force, "local", s.syncFrom, s.syncTo)
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

func (s *webState) refreshCodespacesStreamingStatus() {
	if !s.codespacesStreaming {
		return
	}
	rows, err := s.db.Query(
		`SELECT connection_state, COALESCE(last_chunk_at, ''), COALESCE(last_defensive_recopy_at, ''), COALESCE(last_error, '')
		 FROM codespace_tail_offsets
		 WHERE source LIKE 'codespace:%'`,
	)
	if err != nil {
		s.setSyncStatus("codespaces", webSyncCodeError, fmt.Sprintf("%s query_failed=%v", webSyncReasonCodespacesStreaming, err))
		return
	}
	defer rows.Close()

	total := 0
	connected := 0
	connecting := 0
	var lastChunkAt string
	var lastRecopyAt string
	var lastError string
	for rows.Next() {
		var state, chunkAt, recopyAt, errText string
		if err := rows.Scan(&state, &chunkAt, &recopyAt, &errText); err != nil {
			continue
		}
		total++
		switch strings.ToLower(strings.TrimSpace(state)) {
		case "connected":
			connected++
		case "connecting", "reconnecting":
			connecting++
		}
		if chunkAt > lastChunkAt {
			lastChunkAt = chunkAt
		}
		if recopyAt > lastRecopyAt {
			lastRecopyAt = recopyAt
		}
		if errText != "" {
			lastError = errText
		}
	}
	if total == 0 {
		s.setSyncStatus("codespaces", webSyncCodeSkipped, webSyncReasonCodespacesStreaming+" no_stream_state")
		return
	}
	reason := fmt.Sprintf("%s active_streams=%d connected=%d", webSyncReasonCodespacesStreaming, total, connected)
	if lastChunkAt != "" {
		reason += " last_chunk_at=" + lastChunkAt
	}
	if lastRecopyAt != "" {
		reason += " last_defensive_recopy_at=" + lastRecopyAt
	}
	if lastError != "" {
		reason += " last_error=" + lastError
	}
	if connected > 0 {
		s.setSyncStatus("codespaces", webSyncCodeOK, reason)
		return
	}
	if connecting > 0 {
		s.setSyncStatus("codespaces", webSyncCodeSkipped, reason)
		return
	}
	s.setSyncStatus("codespaces", webSyncCodeStale, reason)
}

func scanLocalProcessLogs(logsDir string) (map[string]localLogState, error) {
	pattern := filepath.Join(logsDir, "process*.log")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	out := make(map[string]localLogState, len(matches))
	for _, path := range matches {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		out[path] = localLogState{Size: info.Size(), MTime: info.ModTime().UTC()}
	}
	return out, nil
}

func localProcessLogsChanged(prev, cur map[string]localLogState) bool {
	if len(prev) != len(cur) {
		return true
	}
	for path, st := range cur {
		old, ok := prev[path]
		if !ok {
			return true
		}
		if old.Size != st.Size || !old.MTime.Equal(st.MTime) {
			return true
		}
	}
	return false
}

func isLocalProcessLogName(name string) bool {
	return strings.HasPrefix(name, "process") && strings.HasSuffix(name, ".log")
}

func localProcessLogsShrank(prev, cur map[string]localLogState) bool {
	for path, old := range prev {
		next, ok := cur[path]
		if !ok {
			continue
		}
		if next.Size < old.Size {
			return true
		}
	}
	return false
}

func localSampleHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	hasher := sha256.New()
	const sampleWindow int64 = 64 * 1024
	readRange := func(offset, length int64) error {
		if length <= 0 {
			return nil
		}
		buf := make([]byte, length)
		n, err := file.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			return err
		}
		if _, err := hasher.Write(buf[:n]); err != nil {
			return err
		}
		return nil
	}
	if size <= sampleWindow {
		if err := readRange(0, size); err != nil {
			return "", err
		}
	} else {
		if err := readRange(0, sampleWindow); err != nil {
			return "", err
		}
		if err := readRange(size-sampleWindow, sampleWindow); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func (s *webState) persistLocalStreamingCheckpoints(current map[string]localLogState, lastChunkAt, lastDefensiveRecopyAt string) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for path, st := range current {
		hash := ""
		lastErr := ""
		if h, err := localSampleHash(path); err == nil {
			hash = h
		} else {
			lastErr = err.Error()
		}
		_, _ = s.db.Exec(
			`INSERT INTO codespace_tail_offsets (
				source,log_file,last_offset,last_size,last_mtime,last_hash,connection_state,last_error,last_chunk_at,last_full_copy_at,last_defensive_recopy_at,updated_at
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(source,log_file) DO UPDATE SET
				last_offset=excluded.last_offset,
				last_size=excluded.last_size,
				last_mtime=excluded.last_mtime,
				last_hash=excluded.last_hash,
				connection_state=excluded.connection_state,
				last_error=excluded.last_error,
				last_chunk_at=excluded.last_chunk_at,
				last_full_copy_at=excluded.last_full_copy_at,
				last_defensive_recopy_at=excluded.last_defensive_recopy_at,
				updated_at=excluded.updated_at`,
			"local",
			path,
			st.Size,
			st.Size,
			st.MTime.Format(time.RFC3339Nano),
			hash,
			"connected",
			nullableString(lastErr),
			nullableString(lastChunkAt),
			nullableString(lastChunkAt),
			nullableString(lastDefensiveRecopyAt),
			now,
		)
	}
}

func (s *webState) markLocalStreamingDisconnected(disappeared []string) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, path := range disappeared {
		_, _ = s.db.Exec(
			`INSERT INTO codespace_tail_offsets (source,log_file,last_offset,last_size,connection_state,last_error,updated_at)
			 VALUES (?,?,0,0,'disconnected','file_disappeared',?)
			 ON CONFLICT(source,log_file) DO UPDATE SET
			   connection_state='disconnected',
			   last_error='file_disappeared',
			   updated_at=excluded.updated_at`,
			"local",
			path,
			now,
		)
	}
}

func nullableString(v string) interface{} {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func (s *webState) refreshLocalStreamingStatus(watched int, lastChunkAt string, lastDefensiveRecopyAt string, streamErr error) {
	reason := fmt.Sprintf("%s watching=%d", webSyncReasonLocalStreaming, watched)
	code := webSyncCodeSkipped
	if streamErr != nil {
		code = webSyncCodeError
		reason += " last_error=" + streamErr.Error()
		s.setSyncStatus("local", code, reason)
		return
	}
	if watched > 0 {
		code = webSyncCodeOK
	}
	if strings.TrimSpace(lastChunkAt) != "" {
		reason += " last_chunk_at=" + lastChunkAt
	}
	if strings.TrimSpace(lastDefensiveRecopyAt) != "" {
		reason += " last_defensive_recopy_at=" + lastDefensiveRecopyAt
	}
	s.setSyncStatus("local", code, reason)
}

func (s *webState) startLocalStreamingLoop(interval, fallbackInterval time.Duration) {
	cadence := interval
	if cadence <= 0 {
		cadence = 2 * time.Second
	}
	fallback := fallbackInterval
	if fallback <= 0 {
		fallback = 30 * time.Second
	}
	s.setLocalRefreshSchedule(0, time.Time{})
	initial, err := scanLocalProcessLogs(s.logsDir)
	if err != nil {
		s.refreshLocalStreamingStatus(0, "", "", err)
		return
	}
	s.localStreamMu.Lock()
	s.localStream = initial
	s.localChunkAt = time.Time{}
	s.localStreamMu.Unlock()
	s.persistLocalStreamingCheckpoints(initial, "", "")
	s.refreshLocalStreamingStatus(len(initial), "", "", nil)

	ticker := time.NewTicker(cadence)
	fallbackTicker := time.NewTicker(fallback)
	watcher, watchErr := newFSWatcher()
	var watcherEvents <-chan fsnotify.Event
	var watcherErrors <-chan error
	if watchErr == nil {
		if err := watcher.Add(s.logsDir); err != nil {
			_ = watcher.Close()
			watcher = nil
		} else {
			watcherEvents = watcher.Events
			watcherErrors = watcher.Errors
		}
	}
	s.loopsWG.Add(1)
	go func() {
		defer s.loopsWG.Done()
		defer ticker.Stop()
		defer fallbackTicker.Stop()
		if watcher != nil {
			defer watcher.Close()
		}
		shouldScan := true
		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				shouldScan = true
			case <-fallbackTicker.C:
				shouldScan = true
			case ev, ok := <-watcherEvents:
				if !ok {
					watcherEvents = nil
					continue
				}
				if ok && isLocalProcessLogName(filepath.Base(ev.Name)) {
					shouldScan = true
				}
			case _, ok := <-watcherErrors:
				if !ok {
					watcherErrors = nil
					continue
				}
				shouldScan = true
			}
			if !shouldScan {
				continue
			}
			shouldScan = false

			tick := time.Now().UTC()
			current, err := scanLocalProcessLogs(s.logsDir)
			if err != nil {
				s.refreshLocalStreamingStatus(0, "", "", err)
				continue
			}

			s.localStreamMu.Lock()
			changed := localProcessLogsChanged(s.localStream, current)
			defensiveRecopy := localProcessLogsShrank(s.localStream, current)
			var disappeared []string
			for path := range s.localStream {
				if _, ok := current[path]; !ok {
					disappeared = append(disappeared, path)
				}
			}
			s.localStream = current
			lastChunkAt := s.localChunkAt
			s.localStreamMu.Unlock()

			shouldSync := changed
			lastDefensiveRecopyAt := ""
			if defensiveRecopy {
				if err := s.refreshLocalSnapshotWithForce(true); err != nil {
					if !isSyncInProgressError(err) {
						s.refreshLocalStreamingStatus(len(current), "", "", err)
					}
					continue
				}
				lastDefensiveRecopyAt = tick.Format(time.RFC3339Nano)
				shouldSync = false
			}
			if shouldSync {
				if err := s.refreshLocalSnapshot(); err != nil {
					if !isSyncInProgressError(err) {
						s.refreshLocalStreamingStatus(len(current), "", lastDefensiveRecopyAt, err)
					}
					continue
				}
				s.localStreamMu.Lock()
				s.localChunkAt = tick
				lastChunkAt = s.localChunkAt
				s.localStreamMu.Unlock()
			}
			lastChunkStr := ""
			if !lastChunkAt.IsZero() {
				lastChunkStr = lastChunkAt.Format(time.RFC3339Nano)
			}
			s.persistLocalStreamingCheckpoints(current, lastChunkStr, lastDefensiveRecopyAt)
			if len(disappeared) > 0 {
				s.markLocalStreamingDisconnected(disappeared)
			}
			s.refreshLocalStreamingStatus(len(current), lastChunkStr, lastDefensiveRecopyAt, nil)
		}
	}()
}

func (s *webState) startCodespacesAutoSyncLoop(interval time.Duration) {
	cadence := normalizeCodespacesInterval(interval)
	s.setCodespacesRefreshSchedule(cadence, time.Now().Add(cadence))
	ticker := time.NewTicker(cadence)
	s.loopsWG.Add(1)
	go func() {
		defer s.loopsWG.Done()
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case tick := <-ticker.C:
				s.setCodespacesRefreshSchedule(cadence, tick.Add(cadence))
				go runScheduledSync(s.syncCodespacesSnapshotAuto, "web codespaces auto sync failed")
			}
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
			s.snapshotMu.RLock()
			status, hasStatus := s.syncStatus[source]
			s.snapshotMu.RUnlock()
			if hasStatus && status.Code == webSyncCodeSkipped {
				fmt.Fprintf(os.Stderr, "web startup %s sync skipped: %s\n", source, status.Reason)
				return
			}
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
	element = strings.ReplaceAll(strings.ReplaceAll(element, "\r\n", "\n"), "\r", "\n")
	element = strings.ReplaceAll(element, "\n", "")
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
	localStreaming := s.localStreaming
	codespacesMode := s.codespacesMode
	codespacesStreaming := s.codespacesStreaming
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
	if localStatus.Reason == webSyncReasonInProgress && !localStreaming {
		localState = "Running"
	}
	if localStreaming {
		localCountdown = ""
		if strings.Contains(localStatus.Reason, webSyncReasonLocalStreaming) {
			switch localStatus.Code {
			case webSyncCodeOK:
				if strings.Contains(localStatus.Reason, "watching=0") {
					localState = "Reconnecting"
				} else {
					localState = "Live"
				}
			case webSyncCodeStale:
				localState = "Disconnected"
			case webSyncCodeError, webSyncCodeTimeout:
				localState = "Error"
			default:
				localState = "Reconnecting"
			}
		} else if localState == "Idle" || localState == "Off" {
			localState = "Reconnecting"
		}
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
	if codespacesStreaming {
		codespacesCountdown = ""
		if codespacesState == "Idle" || codespacesState == "Starting" {
			codespacesState = "Reconnecting"
		}
	}
	if strings.Contains(codespacesStatus.Reason, webSyncReasonCodespacesStreaming) {
		codespacesCountdown = ""
		switch codespacesStatus.Code {
		case webSyncCodeOK:
			if strings.Contains(codespacesStatus.Reason, "connected=0") {
				codespacesState = "Reconnecting"
			} else {
				codespacesState = "Live"
			}
		case webSyncCodeStale:
			codespacesState = "Disconnected"
		case webSyncCodeError, webSyncCodeTimeout:
			codespacesState = "Error"
		default:
			codespacesState = "Reconnecting"
		}
	}
	if codespacesStatus.Reason == webSyncReasonInProgress && !codespacesStreaming {
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
	patch, err := buildRefreshPatch(payload, now, s.getExpandedRows())
	if err != nil {
		return "", err
	}
	return patch + s.buildRefreshIndicatorsPatch(now), nil
}

func buildRefreshPatch(payload statsPayload, now time.Time, expandedRows map[string]map[string]bool) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to encode refresh payload: %w", err)
	}
	escaped := html.EscapeString(string(body))

	var patch strings.Builder
	appendDatastarOuterPatch(&patch, "#overview-summary", `<p id="overview-summary">`+renderWebOverviewSummary(payload, true)+`</p>`)
	appendDatastarOuterPatch(&patch, "#sync-status-summary", renderWebSyncStatusSummary(payload, true))
	appendDatastarOuterPatch(&patch, "#sync-status-table", renderWebSyncStatusTableOnly(payload, true))
	appendDatastarOuterPatch(&patch, "#daily-token-chart-region", `<div id="daily-token-chart-region">`+renderWebDailyTokenChart(payload, true)+`</div>`)
	appendDatastarOuterPatch(&patch, "#model-summary-region", `<div id="model-summary-region">`+renderWebModelSummaryTable(payload, true)+`</div>`)
	appendDatastarOuterPatch(&patch, "#project-summary-region", `<div id="project-summary-region">`+renderWebProjectSummaryTable(payload, true, expandedRows["project"])+`</div>`)
	appendDatastarOuterPatch(&patch, "#daily-totals-region", `<div id="daily-totals-region">`+renderWebDailyTotalsTable(payload, true, expandedRows["day"])+`</div>`)
	appendDatastarOuterPatch(&patch, "#daily-spend-region", renderWebDailySpendRegion(payload, true, now))
	appendDatastarOuterPatch(&patch, "#stats-json", `<pre id="stats-json">`+escaped+`</pre>`)
	return patch.String(), nil
}

func parseWebExpandAction(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func buildProjectRowTogglePatch(payload statsPayload, rowKey string, expand bool) (string, error) {
	rows := sortedWebStatsRows(payload.Projects)
	for _, row := range rows {
		if webStableRowKey("project", row.name) != rowKey {
			continue
		}
		var patch strings.Builder
		appendDatastarOuterPatch(&patch, "#"+webProjectSummaryRowID(rowKey), renderWebProjectSummaryRow(row, rowKey, expand))
		appendDatastarOuterPatch(&patch, "#"+webProjectDetailRowID(rowKey), renderWebProjectDetailRow(payload, row.name, rowKey, expand))
		return patch.String(), nil
	}
	return "", fmt.Errorf("unknown project row key")
}

func buildDayRowTogglePatch(payload statsPayload, rowKey string, expand bool) (string, error) {
	rows := buildWebDailyTotalsRows(payload)
	for _, row := range rows {
		if webStableRowKey("day", row.day) != rowKey {
			continue
		}
		var patch strings.Builder
		appendDatastarOuterPatch(&patch, "#"+webDaySummaryRowID(rowKey), renderWebDaySummaryRow(row, rowKey, expand))
		appendDatastarOuterPatch(&patch, "#"+webDayDetailRowID(rowKey), renderWebDayDetailRow(payload, row.day, rowKey, expand))
		return patch.String(), nil
	}
	return "", fmt.Errorf("unknown day row key")
}

type webStatsRow struct {
	name  string
	stats statsPayloadStats
}

type webDailyTotalsRow struct {
	day              string
	apiCalls         int
	promptTokens     int
	completionTokens int
	premiumRequests  float64
	premRequestCost  float64
	totalCost        float64
	totalCostNoCache float64
}

func webStableRowKey(prefix, value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	var slug strings.Builder
	lastDash := false
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			slug.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			slug.WriteByte('-')
			lastDash = true
		}
	}
	base := strings.Trim(slug.String(), "-")
	if base == "" {
		base = "row"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(value))
	return fmt.Sprintf("%s-%s-%08x", prefix, base, h.Sum32())
}

func webProjectSummaryRowID(rowKey string) string {
	return "project-summary-row-" + rowKey
}

func webProjectDetailRowID(rowKey string) string {
	return "project-detail-row-" + rowKey
}

func webDaySummaryRowID(rowKey string) string {
	return "day-summary-row-" + rowKey
}

func webDayDetailRowID(rowKey string) string {
	return "day-detail-row-" + rowKey
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

func fmtAPIPercent(premCost, cost float64) string {
	if cost == 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", premCost/cost*100)
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
			APICalls:            int(webFloat64(v["api_calls"])),
			UserTurns:           int(webFloat64(v["user_turns"])),
			PromptTokens:        int(webFloat64(v["prompt_tokens"])),
			CompletionTokens:    int(webFloat64(v["completion_tokens"])),
			CacheCreationTokens: int(webFloat64(v["cache_creation_tokens"])),
			CacheReadTokens:     int(webFloat64(v["cache_read_tokens"])),
			PremiumRequests:     webFloat64(v["premium_requests"]),
			PremiumRequestCost:  webFloat64(v["premium_request_cost"]),
			InputUncached:       int(webFloat64(v["input_uncached_tokens"])),
			Cost:                webFloat64(v["cost"]),
			CostWithoutCache:    webFloat64(v["cost_without_cache"]),
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
			row.promptTokens += stats.PromptTokens
			row.completionTokens += stats.CompletionTokens
			row.premiumRequests += stats.PremiumRequests
			row.premRequestCost += stats.PremiumRequestCost
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
	var totalInput, totalOutput int
	var b strings.Builder
	b.WriteString(`<table id="model-summary-table"><thead><tr><th>Model</th><th>Calls</th><th>Input</th><th>Output</th><th>Tok/Prem</th><th>Premium</th><th>Premium Cost</th><th>API Cost</th><th>API%</th></tr></thead><tbody>`)
	for _, row := range rows {
		totalPremium += row.stats.PremiumRequests
		totalInput += row.stats.PromptTokens
		totalOutput += row.stats.CompletionTokens
		tokPerPrem := "–"
		if row.stats.PremiumRequests > 0 {
			tokPerPrem = fmtTokens(int(float64(row.stats.PromptTokens+row.stats.CompletionTokens) / row.stats.PremiumRequests))
		}
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
			html.EscapeString(row.name),
			commaInt(row.stats.APICalls),
			fmtTokens(row.stats.PromptTokens),
			fmtTokens(row.stats.CompletionTokens),
			tokPerPrem,
			commaFloat(row.stats.PremiumRequests, 0),
			fmtCost(row.stats.PremiumRequestCost),
			fmtCost(row.stats.Cost),
			fmtAPIPercent(row.stats.PremiumRequestCost, row.stats.Cost),
		)
	}
	totalTokPerPrem := "–"
	if totalPremium > 0 {
		totalTokPerPrem = fmtTokens(int(float64(totalInput+totalOutput) / totalPremium))
	}
	fmt.Fprintf(&b, "</tbody><tfoot><tr><th>Total</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th></tr></tfoot></table>",
		commaInt(payload.APICalls),
		fmtTokens(totalInput),
		fmtTokens(totalOutput),
		totalTokPerPrem,
		commaFloat(totalPremium, 0),
		fmtCost(payload.TotalPremiumRequestCost),
		fmtCost(payload.TotalCost),
		fmtAPIPercent(payload.TotalPremiumRequestCost, payload.TotalCost),
	)
	return b.String()
}

func renderWebProjectSummaryTable(payload statsPayload, hasSnapshot bool, expandedProjectRows map[string]bool) string {
	if !hasSnapshot {
		return "<p>Loading project summary…</p>"
	}
	rows := sortedWebStatsRows(payload.Projects)
	if len(rows) == 0 {
		return "<p>No project data available.</p>"
	}

	totalPremium := 0.0
	var totalInput, totalOutput int
	var b strings.Builder

	b.WriteString(`<table id="project-summary-table"><thead><tr><th>Project</th><th>Calls</th><th>Input</th><th>Output</th><th>Premium</th><th>Premium Cost</th><th>API Cost</th><th>API%</th></tr></thead><tbody>`)
	for _, row := range rows {
		totalPremium += row.stats.PremiumRequests
		totalInput += row.stats.PromptTokens
		totalOutput += row.stats.CompletionTokens
		rowKey := webStableRowKey("project", row.name)
		expanded := expandedProjectRows[rowKey]
		b.WriteString(renderWebProjectSummaryRow(row, rowKey, expanded))
		b.WriteString(renderWebProjectDetailRow(payload, row.name, rowKey, expanded))
	}
	fmt.Fprintf(&b, "</tbody><tfoot><tr><th>Total</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th></tr></tfoot></table>",
		commaInt(payload.APICalls),
		fmtTokens(totalInput),
		fmtTokens(totalOutput),
		commaFloat(totalPremium, 0),
		fmtCost(payload.TotalPremiumRequestCost),
		fmtCost(payload.TotalCost),
		fmtAPIPercent(payload.TotalPremiumRequestCost, payload.TotalCost),
	)
	return b.String()
}

func renderWebProjectSummaryRow(row webStatsRow, rowKey string, expanded bool) string {
	icon := "▶"
	expand := "true"
	if expanded {
		icon = "▼"
		expand = "false"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<tr id="%s" class="expandable-row" data-row-group="project" data-row-key="%s" data-expand-action="%s" data-on:click="@post('/actions/project-row?row_key=%s&expand=%s')">`,
		webProjectSummaryRowID(rowKey), rowKey, expand, rowKey, expand)
	fmt.Fprintf(&b, `<td>%s %s</td>`, icon, html.EscapeString(row.name))
	fmt.Fprintf(&b, `<td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td>`,
		commaInt(row.stats.APICalls),
		fmtTokens(row.stats.PromptTokens),
		fmtTokens(row.stats.CompletionTokens),
		commaFloat(row.stats.PremiumRequests, 0),
		fmtCost(row.stats.PremiumRequestCost),
		fmtCost(row.stats.Cost),
		fmtAPIPercent(row.stats.PremiumRequestCost, row.stats.Cost),
	)
	b.WriteString(`</tr>`)
	return b.String()
}

func renderWebProjectDetailRow(payload statsPayload, projectName, rowKey string, expanded bool) string {
	models := payload.ProjectModels[projectName]
	modelRows := sortedWebStatsRows(models)
	if !expanded || len(modelRows) == 0 {
		return fmt.Sprintf(`<tr id="%s"></tr>`, webProjectDetailRowID(rowKey))
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<tr id="%s"><td colspan="8"><table class="detail-table"><tbody>`, webProjectDetailRowID(rowKey))
	for _, mr := range modelRows {
		fmt.Fprintf(&b, `<tr class="project-model-row"><td class="model-indent">%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td>`,
			html.EscapeString(mr.name),
			commaInt(mr.stats.APICalls),
			fmtTokens(mr.stats.PromptTokens),
			fmtTokens(mr.stats.CompletionTokens),
			commaFloat(mr.stats.PremiumRequests, 0),
			fmtCost(mr.stats.PremiumRequestCost),
			fmtCost(mr.stats.Cost),
			fmtAPIPercent(mr.stats.PremiumRequestCost, mr.stats.Cost),
		)
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></td></tr>`)
	return b.String()
}

func renderWebDailyTotalsTable(payload statsPayload, hasSnapshot bool, expandedDayRows map[string]bool) string {
	if !hasSnapshot {
		return "<p>Loading daily totals…</p>"
	}
	rows := buildWebDailyTotalsRows(payload)
	if len(rows) == 0 {
		return "<p>No daily totals available.</p>"
	}

	totalPremium := 0.0
	totalPremCost := 0.0
	var totalInput, totalOutput int
	var b strings.Builder

	b.WriteString(`<table id="daily-totals-table"><thead><tr><th>Date</th><th>Calls</th><th>Premium</th><th>Input</th><th>Output</th><th>Premium Cost</th><th>API Cost</th><th>API%</th></tr></thead><tbody>`)
	for _, row := range rows {
		totalPremium += row.premiumRequests
		totalPremCost += row.premRequestCost
		totalInput += row.promptTokens
		totalOutput += row.completionTokens
		rowKey := webStableRowKey("day", row.day)
		expanded := expandedDayRows[rowKey]
		b.WriteString(renderWebDaySummaryRow(row, rowKey, expanded))
		b.WriteString(renderWebDayDetailRow(payload, row.day, rowKey, expanded))
	}
	fmt.Fprintf(&b, "</tbody><tfoot><tr><th>Total</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th><th>%s</th></tr></tfoot></table>",
		commaInt(payload.APICalls),
		commaFloat(totalPremium, 0),
		fmtTokens(totalInput),
		fmtTokens(totalOutput),
		fmtCost(totalPremCost),
		fmtCost(payload.TotalCost),
		fmtAPIPercent(totalPremCost, payload.TotalCost),
	)
	return b.String()
}

func renderWebDaySummaryRow(row webDailyTotalsRow, rowKey string, expanded bool) string {
	icon := "▶"
	expand := "true"
	if expanded {
		icon = "▼"
		expand = "false"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<tr id="%s" class="expandable-row" data-row-group="day" data-row-key="%s" data-expand-action="%s" data-on:click="@post('/actions/day-row?row_key=%s&expand=%s')">`,
		webDaySummaryRowID(rowKey), rowKey, expand, rowKey, expand)
	fmt.Fprintf(&b, `<td>%s %s</td>`, icon, html.EscapeString(row.day))
	fmt.Fprintf(&b, `<td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td>`,
		commaInt(row.apiCalls),
		commaFloat(row.premiumRequests, 0),
		fmtTokens(row.promptTokens),
		fmtTokens(row.completionTokens),
		fmtCost(row.premRequestCost),
		fmtCost(row.totalCost),
		fmtAPIPercent(row.premRequestCost, row.totalCost),
	)
	b.WriteString(`</tr>`)
	return b.String()
}

func sortedWebDailyModelNames(dayMap map[string]interface{}) []string {
	modelNames := make([]string, 0)
	for key := range dayMap {
		if !strings.HasPrefix(key, "_") {
			modelNames = append(modelNames, key)
		}
	}
	sort.Strings(modelNames)
	return modelNames
}

func renderWebDayDetailRow(payload statsPayload, day, rowKey string, expanded bool) string {
	dayMap := payload.Daily[day]
	modelNames := sortedWebDailyModelNames(dayMap)
	if !expanded || len(modelNames) == 0 {
		return fmt.Sprintf(`<tr id="%s"></tr>`, webDayDetailRowID(rowKey))
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<tr id="%s"><td colspan="8"><table class="detail-table"><tbody>`, webDayDetailRowID(rowKey))
	for _, model := range modelNames {
		stats, ok := webDailyStatsValue(dayMap[model])
		if !ok {
			continue
		}
		fmt.Fprintf(&b, `<tr class="daily-model-row"><td class="model-indent">%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td>`,
			html.EscapeString(model),
			commaInt(stats.APICalls),
			commaFloat(stats.PremiumRequests, 0),
			fmtTokens(stats.PromptTokens),
			fmtTokens(stats.CompletionTokens),
			fmtCost(stats.PremiumRequestCost),
			fmtCost(stats.Cost),
			fmtAPIPercent(stats.PremiumRequestCost, stats.Cost),
		)
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></td></tr>`)
	return b.String()
}

func syncStatusIcon(code, reason string) string {
	switch code {
	case webSyncCodeOK:
		return "✓"
	case webSyncCodeSkipped:
		if reason == webSyncReasonInProgress {
			return "⏳"
		}
		return "–"
	case webSyncCodeError:
		return "⚠"
	case webSyncCodeTimeout:
		return "⏱"
	case webSyncCodeStale:
		return "⚠"
	default:
		return "?"
	}
}

func renderSyncStatusSummaryLine(payload statsPayload) string {
	if len(payload.SyncStatus) == 0 {
		return "Sync: no status"
	}
	sources := make([]string, 0, len(payload.SyncStatus))
	for source := range payload.SyncStatus {
		sources = append(sources, source)
	}
	sort.Strings(sources)

	var b strings.Builder
	b.WriteString("Sync: ")
	for i, source := range sources {
		if i > 0 {
			b.WriteString(" · ")
		}
		status := payload.SyncStatus[source]
		fmt.Fprintf(&b, "%s %s", html.EscapeString(source), syncStatusIcon(status.Code, status.Reason))
	}
	return b.String()
}

func renderWebSyncStatusTable(payload statsPayload, hasSnapshot bool) string {
	return renderWebSyncStatusSummary(payload, hasSnapshot) + renderWebSyncStatusTableOnly(payload, hasSnapshot)
}

func renderWebSyncStatusSummary(payload statsPayload, hasSnapshot bool) string {
	if !hasSnapshot {
		return `<summary id="sync-status-summary">Loading sync status…</summary>`
	}
	if len(payload.SyncStatus) == 0 {
		return `<summary id="sync-status-summary">No sync status available.</summary>`
	}
	return fmt.Sprintf(`<summary id="sync-status-summary">%s</summary>`, renderSyncStatusSummaryLine(payload))
}

func renderWebSyncStatusTableOnly(payload statsPayload, hasSnapshot bool) string {
	if !hasSnapshot || len(payload.SyncStatus) == 0 {
		return `<table id="sync-status-table"></table>`
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

func renderWebHourlyUsageTable(payload statsPayload, hasSnapshot bool) string {
	if !hasSnapshot {
		return "<p>Loading hourly usage…</p>"
	}
	if len(payload.Hourly) == 0 {
		return "<p>No hourly usage available.</p>"
	}

	type hourlyCell struct {
		hour     string
		requests int
		tokens   int
		cost     float64
	}
	cells := make([]hourlyCell, 0, 24)
	maxRequests, maxTokens := 0, 0
	maxCost := 0.0
	for hour := 0; hour < 24; hour++ {
		key := fmt.Sprintf("%02d", hour)
		stats := payload.Hourly[key]
		tokens := stats.PromptTokens + stats.CompletionTokens
		cells = append(cells, hourlyCell{hour: key, requests: stats.APICalls, tokens: tokens, cost: stats.Cost})
		if stats.APICalls > maxRequests {
			maxRequests = stats.APICalls
		}
		if tokens > maxTokens {
			maxTokens = tokens
		}
		if stats.Cost > maxCost {
			maxCost = stats.Cost
		}
	}
	if maxRequests == 0 && maxTokens == 0 && maxCost == 0 {
		return "<p>No hourly usage available.</p>"
	}

	var b strings.Builder
	b.WriteString(`<div id="hourly-heatmap" data-hourly-heatmap data-metric-index="0">`)
	b.WriteString(`<div id="hourly-heatmap-grid" class="hourly-heatmap-grid">`)
	for _, cell := range cells {
		tokensPct := 0.0
		if maxTokens > 0 {
			tokensPct = float64(cell.tokens) / float64(maxTokens) * 100
		}
		requestsPct := 0.0
		if maxRequests > 0 {
			requestsPct = float64(cell.requests) / float64(maxRequests) * 100
		}
		costPct := 0.0
		if maxCost > 0 {
			costPct = cell.cost / maxCost * 100
		}
		hour := html.EscapeString(cell.hour)
		tokensLabel := html.EscapeString(fmtTokens(cell.tokens))
		requestsLabel := html.EscapeString(commaInt(cell.requests))
		costLabel := html.EscapeString(fmtCost(cell.cost))
		fmt.Fprintf(&b, `<div class="hourly-heatmap-cell" data-hourly-heatmap-cell data-hour="%s" data-pct-tokens="%.2f" data-pct-requests="%.2f" data-pct-cost="%.2f" data-tokens-label="%s" data-requests-label="%s" data-cost-label="%s" style="--heat: %.4f;" title="%s:00 · Tokens: %s"><span class="hourly-heatmap-hour">%s</span></div>`,
			hour,
			tokensPct, requestsPct, costPct,
			tokensLabel, requestsLabel, costLabel,
			tokensPct/100,
			hour, tokensLabel, hour,
		)
	}
	b.WriteString(`</div></div>`)
	return b.String()
}

func renderWebDailyTokenChart(payload statsPayload, hasSnapshot bool) string {
	if !hasSnapshot {
		return "<p>Loading chart…</p>"
	}
	days := make([]string, 0, len(payload.Daily))
	for day := range payload.Daily {
		days = append(days, day)
	}
	sort.Strings(days)
	if len(days) == 0 {
		return "<p>No daily data available.</p>"
	}

	type dayTokens struct {
		day          string
		inputTokens  int
		outputTokens int
		cachedTokens int
	}
	rows := make([]dayTokens, 0, len(days))
	maxTokens := 0
	for _, day := range days {
		dayMap := payload.Daily[day]
		var dt dayTokens
		dt.day = day
		for key, value := range dayMap {
			if strings.HasPrefix(key, "_") {
				continue
			}
			stats, ok := webDailyStatsValue(value)
			if !ok {
				continue
			}
			dt.inputTokens += stats.PromptTokens
			dt.outputTokens += stats.CompletionTokens
			dt.cachedTokens += stats.CacheReadTokens
		}
		if dt.inputTokens > maxTokens {
			maxTokens = dt.inputTokens
		}
		if dt.outputTokens > maxTokens {
			maxTokens = dt.outputTokens
		}
		rows = append(rows, dt)
	}
	if maxTokens == 0 {
		return "<p>No token data available.</p>"
	}

	var b strings.Builder
	b.WriteString(`<div class="token-chart">`)
	for _, dt := range rows {
		label := dt.day
		if t, err := time.Parse("2006-01-02", dt.day); err == nil {
			label = t.Format("Jan 2")
		}
		inputPct := float64(dt.inputTokens) / float64(maxTokens) * 100
		outputPct := float64(dt.outputTokens) / float64(maxTokens) * 100
		cachedPct := 0.0
		if dt.inputTokens > 0 {
			cachedPct = float64(dt.cachedTokens) / float64(dt.inputTokens) * 100
		}
		inputTitle := fmt.Sprintf("Input: %s (cached: %s)", fmtTokens(dt.inputTokens), fmtTokens(dt.cachedTokens))
		outputTitle := fmt.Sprintf("Output: %s", fmtTokens(dt.outputTokens))

		inputLabel := fmtTokens(dt.inputTokens) + " in"
		outputLabel := fmtTokens(dt.outputTokens) + " out"
		fmt.Fprintf(&b, `<div class="token-chart-row">`+
			`<span class="token-chart-label">%s</span>`+
			`<div class="token-chart-bars">`+
			`<div class="token-bar-row">`+
			`<div class="token-bar token-bar-input" style="width: %.1f%%" title="%s">`+
			`<div class="token-bar-cached" style="width: %.1f%%"></div></div>`+
			`<span class="token-bar-label">%s</span></div>`+
			`<div class="token-bar-row">`+
			`<div class="token-bar token-bar-output" style="width: %.1f%%" title="%s"></div>`+
			`<span class="token-bar-label">%s</span></div>`+
			`</div></div>`,
			html.EscapeString(label), inputPct, html.EscapeString(inputTitle),
			cachedPct, html.EscapeString(inputLabel),
			outputPct, html.EscapeString(outputTitle), html.EscapeString(outputLabel))
	}
	b.WriteString(`</div>`)
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
	apiPct := "–"
	if payload.TotalCost > 0 {
		apiPct = fmt.Sprintf("%.0f%%", payload.TotalPremiumRequestCost/payload.TotalCost*100)
	}
	return fmt.Sprintf("Period: %s%s · API calls: %s · Cost: %s · API%%: %s",
		html.EscapeString(payload.Period),
		dateRange,
		commaInt(payload.APICalls),
		fmtCost(payload.TotalPremiumRequestCost),
		apiPct,
	)
}

func dashboardOverviewHTML(payload statsPayload, hasSnapshot bool, expandedRows map[string]map[string]bool) string {
	return fmt.Sprintf(`<p id="overview-summary">%s</p>
  <details id="sync-status-region" class="sync-status-compact">%s</details>
  <section>
    <h2>Daily Token Usage</h2>
    <div id="daily-token-chart-region">%s</div>
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
		renderWebDailyTokenChart(payload, hasSnapshot),
		renderWebModelSummaryTable(payload, hasSnapshot),
		renderWebProjectSummaryTable(payload, hasSnapshot, expandedRows["project"]),
		renderWebDailyTotalsTable(payload, hasSnapshot, expandedRows["day"]),
	)
}

func dashboardShellHTML(payload statsPayload, hasSnapshot bool, expandedRows map[string]map[string]bool) string {
	placeholderIndicators := `<div id="refresh-indicators-region"><div id="refresh-indicators" class="refresh-indicators">` +
		renderRefreshIndicatorRow("Local", "Idle", "") +
		renderRefreshIndicatorRow("Codespaces", "Idle", "") +
		`</div></div>`
	return dashboardShellHTMLWithIndicators(payload, hasSnapshot, placeholderIndicators, expandedRows)
}

func renderWebExpansionReplayScript() string {
	return `<script>
  (function () {
    const storageKeys = {
      project: 'copilot-token-cost:web:expanded-project-rows',
      day: 'copilot-token-cost:web:expanded-day-rows'
    };
    const actionPaths = {
      project: '/actions/project-row',
      day: '/actions/day-row'
    };

    function readStoredRowKeys(group) {
      try {
        const parsed = JSON.parse(localStorage.getItem(storageKeys[group]) || '[]');
        if (!Array.isArray(parsed)) return [];
        return parsed.filter((value) => typeof value === 'string' && value.trim() !== '');
      } catch (_err) {
        return [];
      }
    }

    function writeStoredRowKeys(group, rowKeys) {
      try {
        localStorage.setItem(storageKeys[group], JSON.stringify(Array.from(new Set(rowKeys))));
      } catch (_err) {
      }
    }

    function rememberExpansionState(group, rowKey, shouldExpand) {
      const rowKeys = new Set(readStoredRowKeys(group));
      if (shouldExpand) {
        rowKeys.add(rowKey);
      } else {
        rowKeys.delete(rowKey);
      }
      writeStoredRowKeys(group, Array.from(rowKeys));
    }

    function applyPatchElements(patch) {
      for (const frame of patch.split('\n\n')) {
        let selector = '';
        let elements = '';
        for (const line of frame.split('\n')) {
          if (line.startsWith('data: selector ')) {
            selector = line.slice('data: selector '.length).trim();
          } else if (line.startsWith('data: elements ')) {
            elements = line.slice('data: elements '.length);
          }
        }
        if (!selector || !elements) continue;
        const target = document.querySelector(selector);
        if (target) {
          target.outerHTML = elements;
        }
      }
    }

    async function replayGroup(group) {
      const rowKeys = readStoredRowKeys(group);
      for (const rowKey of rowKeys) {
        const actionURL = actionPaths[group] + '?row_key=' + encodeURIComponent(rowKey) + '&expand=true';
        try {
          const response = await fetch(actionURL, { method: 'POST', headers: { 'Accept': 'text/event-stream' } });
          if (!response.ok) continue;
          applyPatchElements(await response.text());
        } catch (_err) {
        }
      }
    }

    document.addEventListener('click', function (evt) {
      if (!(evt.target instanceof Element)) return;
      const row = evt.target.closest('tr.expandable-row[data-row-group][data-row-key][data-expand-action]');
      if (!row) return;
      const group = row.getAttribute('data-row-group');
      const rowKey = row.getAttribute('data-row-key');
      if (!group || !rowKey) return;
      rememberExpansionState(group, rowKey, row.getAttribute('data-expand-action') === 'true');
    }, true);

    void replayGroup('project').then(function () {
      return replayGroup('day');
    });
  })();
  </script>`
}

func dashboardShellHTMLWithIndicators(payload statsPayload, hasSnapshot bool, refreshIndicatorsHTML string, expandedRows map[string]map[string]bool) string {
	overviewHTML := dashboardOverviewHTML(payload, hasSnapshot, expandedRows)
	replayScript := renderWebExpansionReplayScript()
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
    #dashboard-header p { margin: 0.35rem 0 0; }
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
    .sync-status-compact { font-size: 0.82rem; color: #6b7280; margin: 0.3rem 0 0.5rem; }
    .sync-status-compact summary { cursor: pointer; }
    .sync-status-compact table { font-size: 0.82rem; margin-top: 0.3rem; }
    .sync-status-compact th, .sync-status-compact td { padding: 0.2rem 0.4rem; }
    .hourly-heatmap-grid { display: grid; grid-template-columns: repeat(12, minmax(0, 1fr)); gap: 0.35rem; max-width: 40rem; }
    .hourly-heatmap-cell { aspect-ratio: 1 / 1; border: 1px solid #d1d5db; border-radius: 0.35rem; background: rgba(99, 102, 241, calc(0.08 + var(--heat, 0) * 0.82)); color: #1f2937; display: flex; align-items: flex-end; justify-content: flex-end; padding: 0.2rem; box-sizing: border-box; }
    .hourly-heatmap-hour { font-size: 0.68rem; line-height: 1; }
    .expandable-row { cursor: pointer; }
    .expandable-row:hover { background: #f9fafb; }
    .detail-table { margin: 0; width: 100%%; }
    .detail-table td { border: 0; padding: 0.2rem 0.3rem; }
    .daily-model-row td, .project-model-row td { font-size: 0.82rem; color: #6b7280; }
    .model-indent { padding-left: 1.5rem !important; }
    .token-chart { margin: 1rem 0; }
    .token-chart-row { display: flex; align-items: center; gap: 0.5rem; margin: 0.15rem 0; }
    .token-chart-label { width: 3.5rem; font-size: 0.78rem; text-align: right; color: #6b7280; flex-shrink: 0; }
    .token-chart-bars { flex: 1; display: flex; flex-direction: column; gap: 2px; }
    .token-bar { height: 10px; border-radius: 2px; min-width: 2px; position: relative; }
    .token-bar-input { background: #3b82f6; }
    .token-bar-cached { background: #93c5fd; height: 100%%; border-radius: 2px; }
    .token-bar-output { background: #f59e0b; }
    .token-bar-row { display: flex; align-items: center; gap: 0.4rem; }
    .token-bar-label { font-size: 0.72rem; color: #6b7280; white-space: nowrap; flex-shrink: 0; }
  </style>
</head>
<body data-signals:status-message="''">
  <header id="dashboard-header">
    <div>
      <h1>Copilot Token Cost Dashboard</h1>
      <p><a href="/">View Copilot Stats</a></p>
    </div>
    %s
  </header>
  <div id="status" data-text="$statusMessage"></div>
  <main id="dashboard-overview">%s</main>
  <pre id="stats-json">%s</pre>
  %s
  <div data-init="@get('/events')"></div>
  <div data-on:datastar-fetch="
         evt.detail.type === 'started' && ($statusMessage = '');
         evt.detail.type === 'error' && ($statusMessage = String(evt.detail.error || 'request failed'));
        "></div>
  <script type="module" src="https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.0-RC.7/bundles/datastar.js"></script>
</body>
</html>`, refreshIndicatorsHTML, overviewHTML, html.EscapeString(statsJSON), replayScript)
}

func findWebDailyTotalsRow(payload statsPayload, day string) (webDailyTotalsRow, bool) {
	for _, row := range buildWebDailyTotalsRows(payload) {
		if row.day == day {
			return row, true
		}
	}
	return webDailyTotalsRow{day: day}, false
}

func webCurrentDailyUsageStreak(rows []webDailyTotalsRow, day string) int {
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return 0
	}
	byDay := make(map[string]int, len(rows))
	for _, row := range rows {
		byDay[row.day] = row.apiCalls
	}
	streak := 0
	for cursor := day; ; {
		calls, ok := byDay[cursor]
		if !ok || calls <= 0 {
			break
		}
		streak++
		parsed, err := time.Parse("2006-01-02", cursor)
		if err != nil {
			break
		}
		cursor = parsed.AddDate(0, 0, -1).Format("2006-01-02")
	}
	return streak
}

func webDailySpendTierAndProgress(spend float64) (string, string, float64) {
	tiers := []struct {
		name     string
		minSpend float64
	}{
		{name: "Starter", minSpend: 0},
		{name: "Bronze", minSpend: 0.10},
		{name: "Silver", minSpend: 0.25},
		{name: "Gold", minSpend: 0.50},
		{name: "Platinum", minSpend: 1.00},
	}
	currentIdx := 0
	for i := 0; i < len(tiers); i++ {
		if spend >= tiers[i].minSpend {
			currentIdx = i
		}
	}
	if currentIdx == len(tiers)-1 {
		return tiers[currentIdx].name, "Max", 100
	}
	nextIdx := currentIdx + 1
	progressRange := tiers[nextIdx].minSpend - tiers[currentIdx].minSpend
	progress := 0.0
	if progressRange > 0 {
		progress = (spend - tiers[currentIdx].minSpend) / progressRange * 100
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	return tiers[currentIdx].name, tiers[nextIdx].name, progress
}

type webDailySpendTotals struct {
	InputTokens  int
	OutputTokens int
	PremiumSpend float64
	APISpend     float64
}

type webDailySpendAverages struct {
	InputTokens  float64
	OutputTokens float64
	PremiumSpend float64
	APISpend     float64
}

type webDailySpendTokenTrendPoint struct {
	Day          string
	InputTokens  int
	OutputTokens int
}

type webDailySpendMoneyTrendPoint struct {
	Day          string
	PremiumSpend float64
	APISpend     float64
}

type webDailySpendTopRow struct {
	Name         string
	InputTokens  int
	OutputTokens int
	PremiumSpend float64
	APISpend     float64
	PromptCount  int
}

type webDailySpendMetricCard struct {
	ID    string
	Label string
	Value string
}

type webDailySpendData struct {
	Day                 string
	WindowDays          int
	Today               webDailySpendTotals
	Rolling7DayAverage  webDailySpendAverages
	TokenTrend          []webDailySpendTokenTrendPoint
	MoneyTrend          []webDailySpendMoneyTrendPoint
	TopModelsToday      []webDailySpendTopRow
	TopModelsRolling7   []webDailySpendTopRow
	TopProjectsToday    []webDailySpendTopRow
	TopProjectsRolling7 []webDailySpendTopRow
}

type webDailySpendTopAccumulator struct {
	inputTokens  float64
	outputTokens float64
	premiumSpend float64
	apiSpend     float64
	promptCount  float64
}

func webDailySpendHybridScore(apiSpend float64, inputTokens, outputTokens int) float64 {
	return 0.7*apiSpend + 0.3*float64(inputTokens+outputTokens)
}

func webParseDateRangeBounds(dateRange *string) (string, string, bool) {
	if dateRange == nil {
		return "", "", false
	}
	raw := strings.TrimSpace(*dateRange)
	if raw == "" {
		return "", "", false
	}
	parts := strings.Split(raw, "→")
	if len(parts) != 2 {
		return "", "", false
	}
	start := strings.TrimSpace(parts[0])
	end := strings.TrimSpace(parts[1])
	if _, err := time.Parse("2006-01-02", start); err != nil {
		return "", "", false
	}
	if _, err := time.Parse("2006-01-02", end); err != nil {
		return "", "", false
	}
	if start > end {
		start, end = end, start
	}
	return start, end, true
}

func webDailySpendWindowDays(payload statsPayload, now time.Time) (string, []string) {
	day := now.Format("2006-01-02")
	windowDays := webRecentDayWindow(day, 7)
	start, end, ok := webParseDateRangeBounds(payload.DateRange)
	if !ok {
		return day, windowDays
	}
	day = end
	bounded := make([]string, 0, len(windowDays))
	for _, windowDay := range webRecentDayWindow(day, 7) {
		if windowDay >= start {
			bounded = append(bounded, windowDay)
		}
	}
	if len(bounded) == 0 {
		return day, []string{day}
	}
	return day, bounded
}

func buildWebDailySpendData(payload statsPayload, now time.Time) webDailySpendData {
	day, windowDays := webDailySpendWindowDays(payload, now)
	todayModels := webDailyModelStatsByDay(payload, day)
	rollingModels := webDailyModelStatsByDays(payload, windowDays)
	todayProjects := webDailyProjectStatsByDay(payload, day)
	rollingProjects := webDailyProjectStatsByDays(payload, windowDays)
	if len(todayProjects) == 0 {
		todayProjects = webDailySpendTopProjectStatsFromModelWeights(payload, todayModels)
	}
	if len(rollingProjects) == 0 {
		rollingProjects = webDailySpendTopProjectStatsFromModelWeights(payload, rollingModels)
	}
	todayTotals := webDailySpendTotalsFromStatsMap(todayModels)
	rollingTotals := webDailySpendTotalsFromStatsMap(rollingModels)
	windowSize := float64(len(windowDays))
	if windowSize == 0 {
		windowSize = 1
	}
	tokenTrend := make([]webDailySpendTokenTrendPoint, 0, len(windowDays))
	moneyTrend := make([]webDailySpendMoneyTrendPoint, 0, len(windowDays))
	for _, dayKey := range windowDays {
		totals := webDailySpendTotalsFromStatsMap(webDailyModelStatsByDay(payload, dayKey))
		tokenTrend = append(tokenTrend, webDailySpendTokenTrendPoint{
			Day:          dayKey,
			InputTokens:  totals.InputTokens,
			OutputTokens: totals.OutputTokens,
		})
		moneyTrend = append(moneyTrend, webDailySpendMoneyTrendPoint{
			Day:          dayKey,
			PremiumSpend: totals.PremiumSpend,
			APISpend:     totals.APISpend,
		})
	}
	return webDailySpendData{
		Day:        day,
		WindowDays: len(windowDays),
		Today:      todayTotals,
		Rolling7DayAverage: webDailySpendAverages{
			InputTokens:  float64(rollingTotals.InputTokens) / windowSize,
			OutputTokens: float64(rollingTotals.OutputTokens) / windowSize,
			PremiumSpend: rollingTotals.PremiumSpend / windowSize,
			APISpend:     rollingTotals.APISpend / windowSize,
		},
		TokenTrend:          tokenTrend,
		MoneyTrend:          moneyTrend,
		TopModelsToday:      webDailySpendTopRowsFromStatsMap(todayModels),
		TopModelsRolling7:   webDailySpendTopRowsFromStatsMap(rollingModels),
		TopProjectsToday:    webDailySpendTopRowsFromStatsMap(todayProjects),
		TopProjectsRolling7: webDailySpendTopRowsFromStatsMap(rollingProjects),
	}
}

func webRecentDayWindow(day string, size int) []string {
	if size <= 0 {
		return nil
	}
	parsedDay, err := time.Parse("2006-01-02", day)
	if err != nil {
		return []string{day}
	}
	window := make([]string, 0, size)
	for i := size - 1; i >= 0; i-- {
		window = append(window, parsedDay.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return window
}

func webDailyModelStatsByDay(payload statsPayload, day string) map[string]statsPayloadStats {
	dayMap, ok := payload.Daily[day]
	if !ok {
		return map[string]statsPayloadStats{}
	}
	out := make(map[string]statsPayloadStats)
	for key, value := range dayMap {
		if strings.HasPrefix(key, "_") {
			continue
		}
		stats, ok := webDailyStatsValue(value)
		if !ok {
			continue
		}
		out[key] = stats
	}
	return out
}

func webDailyModelStatsByDays(payload statsPayload, days []string) map[string]statsPayloadStats {
	out := make(map[string]statsPayloadStats)
	for _, day := range days {
		for model, stats := range webDailyModelStatsByDay(payload, day) {
			acc := out[model]
			webMergeDailySpendStats(&acc, stats)
			out[model] = acc
		}
	}
	return out
}

func webDailyProjectStatsByDay(payload statsPayload, day string) map[string]statsPayloadStats {
	if len(payload.DailyProjects) == 0 {
		return map[string]statsPayloadStats{}
	}
	dayMap, ok := payload.DailyProjects[day]
	if !ok {
		return map[string]statsPayloadStats{}
	}
	out := make(map[string]statsPayloadStats, len(dayMap))
	for project, stats := range dayMap {
		out[project] = stats
	}
	return out
}

func webDailyProjectStatsByDays(payload statsPayload, days []string) map[string]statsPayloadStats {
	out := make(map[string]statsPayloadStats)
	for _, day := range days {
		for project, stats := range webDailyProjectStatsByDay(payload, day) {
			acc := out[project]
			webMergeDailySpendStats(&acc, stats)
			out[project] = acc
		}
	}
	return out
}

func webMergeDailySpendStats(acc *statsPayloadStats, stats statsPayloadStats) {
	acc.APICalls += stats.APICalls
	acc.UserTurns += stats.UserTurns
	acc.PromptTokens += stats.PromptTokens
	acc.CompletionTokens += stats.CompletionTokens
	acc.CacheCreationTokens += stats.CacheCreationTokens
	acc.CacheReadTokens += stats.CacheReadTokens
	acc.PremiumRequests += stats.PremiumRequests
	acc.PremiumRequestCost += stats.PremiumRequestCost
	acc.InputUncached += stats.InputUncached
	acc.Cost += stats.Cost
	acc.CostWithoutCache += stats.CostWithoutCache
}

func webDailySpendTotalsFromStatsMap(statsMap map[string]statsPayloadStats) webDailySpendTotals {
	totals := webDailySpendTotals{}
	for _, stats := range statsMap {
		totals.InputTokens += stats.PromptTokens
		totals.OutputTokens += stats.CompletionTokens
		totals.PremiumSpend += stats.PremiumRequestCost
		totals.APISpend += stats.Cost
	}
	return totals
}

func webDailySpendTopRowsFromStatsMap(statsMap map[string]statsPayloadStats) []webDailySpendTopRow {
	rows := make([]webDailySpendTopRow, 0, len(statsMap))
	for name, stats := range statsMap {
		rows = append(rows, webDailySpendTopRow{
			Name:         name,
			InputTokens:  stats.PromptTokens,
			OutputTokens: stats.CompletionTokens,
			PremiumSpend: stats.PremiumRequestCost,
			APISpend:     stats.Cost,
			PromptCount:  stats.UserTurns,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		iScore := webDailySpendHybridScore(rows[i].APISpend, rows[i].InputTokens, rows[i].OutputTokens)
		jScore := webDailySpendHybridScore(rows[j].APISpend, rows[j].InputTokens, rows[j].OutputTokens)
		if iScore == jScore {
			return rows[i].Name < rows[j].Name
		}
		return iScore > jScore
	})
	if len(rows) > 3 {
		rows = rows[:3]
	}
	return rows
}

func webDailySpendTopProjectStatsFromModelWeights(payload statsPayload, modelStats map[string]statsPayloadStats) map[string]statsPayloadStats {
	projectWeights := webDailySpendProjectWeightsByModel(payload)
	if len(projectWeights) == 0 {
		return payload.Projects
	}
	projectAcc := make(map[string]webDailySpendTopAccumulator)
	for model, stats := range modelStats {
		weights := projectWeights[model]
		if len(weights) == 0 {
			weights = map[string]float64{"(unknown)": 1}
		}
		for project, weight := range weights {
			if weight <= 0 {
				continue
			}
			acc := projectAcc[project]
			acc.inputTokens += float64(stats.PromptTokens) * weight
			acc.outputTokens += float64(stats.CompletionTokens) * weight
			acc.premiumSpend += stats.PremiumRequestCost * weight
			acc.apiSpend += stats.Cost * weight
			acc.promptCount += float64(stats.UserTurns) * weight
			projectAcc[project] = acc
		}
	}
	statsMap := make(map[string]statsPayloadStats, len(projectAcc))
	for project, acc := range projectAcc {
		statsMap[project] = statsPayloadStats{
			APICalls:           0,
			UserTurns:          webRoundPositiveInt(acc.promptCount),
			PromptTokens:       webRoundPositiveInt(acc.inputTokens),
			CompletionTokens:   webRoundPositiveInt(acc.outputTokens),
			PremiumRequestCost: acc.premiumSpend,
			Cost:               acc.apiSpend,
		}
	}
	return statsMap
}

func webDailySpendTopProjectRows(payload statsPayload, modelStats map[string]statsPayloadStats) []webDailySpendTopRow {
	return webDailySpendTopRowsFromStatsMap(webDailySpendTopProjectStatsFromModelWeights(payload, modelStats))
}

func webDailySpendProjectWeightsByModel(payload statsPayload) map[string]map[string]float64 {
	weights := make(map[string]map[string]float64)
	for project, modelMap := range payload.ProjectModels {
		for model, stats := range modelMap {
			weight := stats.PremiumRequestCost
			if weight <= 0 {
				weight = float64(stats.APICalls)
			}
			if weight <= 0 {
				weight = float64(stats.PromptTokens + stats.CompletionTokens)
			}
			if weight <= 0 {
				weight = 1
			}
			if _, ok := weights[model]; !ok {
				weights[model] = make(map[string]float64)
			}
			weights[model][project] += weight
		}
	}
	for model, modelWeights := range weights {
		sum := 0.0
		for _, weight := range modelWeights {
			sum += weight
		}
		if sum <= 0 {
			continue
		}
		for project, weight := range modelWeights {
			modelWeights[project] = weight / sum
		}
		weights[model] = modelWeights
	}
	return weights
}

func webRoundPositiveInt(value float64) int {
	if value <= 0 {
		return 0
	}
	return int(value + 0.5)
}

func renderWebDailySpendMetricCards(cards []webDailySpendMetricCard) string {
	var b strings.Builder
	b.WriteString(`<div class="daily-spend-metrics">`)
	for _, card := range cards {
		valueID := ""
		if strings.TrimSpace(card.ID) != "" {
			valueID = fmt.Sprintf(` id="%s"`, card.ID)
		}
		fmt.Fprintf(&b, `<article class="daily-spend-metric"><h4>%s</h4><p%s>%s</p></article>`,
			html.EscapeString(card.Label),
			valueID,
			html.EscapeString(card.Value),
		)
	}
	b.WriteString(`</div>`)
	return b.String()
}

type webDailySpendLineSeries struct {
	Label  string
	Stroke string
	Axis   string
	Values []float64
}

func renderWebDailySpendLineChart(id string, points []string, series []webDailySpendLineSeries, formatY func(float64) string) string {
	if len(points) == 0 || len(series) == 0 {
		return fmt.Sprintf(`<div id="%s" class="daily-spend-chart-empty">No data yet.</div>`, id)
	}
	leftMaxValue := 0.0
	rightMaxValue := 0.0
	hasRightAxis := false
	seriesAxis := func(item webDailySpendLineSeries) string {
		if strings.EqualFold(strings.TrimSpace(item.Axis), "right") {
			return "right"
		}
		return "left"
	}
	for _, item := range series {
		axis := seriesAxis(item)
		if axis == "right" {
			hasRightAxis = true
		}
		for _, value := range item.Values {
			if axis == "right" {
				if value > rightMaxValue {
					rightMaxValue = value
				}
				continue
			}
			if value > leftMaxValue {
				leftMaxValue = value
			}
		}
	}
	if leftMaxValue <= 0 {
		leftMaxValue = 1
	}
	if rightMaxValue <= 0 {
		rightMaxValue = leftMaxValue
	}
	const (
		svgWidth  = 640.0
		svgHeight = 220.0
		leftPad   = 44.0
		topPad    = 12.0
		bottomPad = 28.0
	)
	rightPad := 12.0
	if hasRightAxis {
		rightPad = 44
	}
	plotWidth := svgWidth - leftPad - rightPad
	plotHeight := svgHeight - topPad - bottomPad
	xAt := func(index int) float64 {
		if len(points) <= 1 {
			return leftPad + plotWidth/2
		}
		return leftPad + float64(index)*plotWidth/float64(len(points)-1)
	}
	yAt := func(value, maxValue float64) float64 {
		return topPad + (1-value/maxValue)*plotHeight
	}
	buildPath := func(values []float64, maxValue float64) string {
		if len(values) == 0 {
			return ""
		}
		var path strings.Builder
		for index, value := range values {
			if index == 0 {
				fmt.Fprintf(&path, "M %.2f %.2f", xAt(index), yAt(value, maxValue))
				continue
			}
			fmt.Fprintf(&path, " L %.2f %.2f", xAt(index), yAt(value, maxValue))
		}
		return path.String()
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<figure id="%s" class="daily-spend-chart"><svg viewBox="0 0 %.0f %.0f" role="img" aria-label="Daily spend trend chart">`,
		id, svgWidth, svgHeight)
	for _, ratio := range []float64{0, 0.5, 1} {
		y := topPad + ratio*plotHeight
		leftValue := leftMaxValue * (1 - ratio)
		fmt.Fprintf(&b, `<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="#e5e7eb" stroke-width="1"/>`,
			leftPad, y, leftPad+plotWidth, y)
		fmt.Fprintf(&b, `<text x="4" y="%.2f" font-size="10" fill="#6b7280">%s</text>`,
			y+3, html.EscapeString(formatY(leftValue)))
		if hasRightAxis {
			rightValue := rightMaxValue * (1 - ratio)
			fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="10" fill="#6b7280" text-anchor="end">%s</text>`,
				svgWidth-4, y+3, html.EscapeString(formatY(rightValue)))
		}
	}
	fmt.Fprintf(&b, `<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="#9ca3af" stroke-width="1"/>`,
		leftPad, topPad+plotHeight, leftPad+plotWidth, topPad+plotHeight)
	for _, item := range series {
		maxValue := leftMaxValue
		if seriesAxis(item) == "right" {
			maxValue = rightMaxValue
		}
		path := buildPath(item.Values, maxValue)
		if path == "" {
			continue
		}
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="2"/>`,
			html.EscapeString(path),
			html.EscapeString(item.Stroke),
		)
		for index, value := range item.Values {
			fmt.Fprintf(&b, `<circle cx="%.2f" cy="%.2f" r="2.5" fill="%s"/>`,
				xAt(index),
				yAt(value, maxValue),
				html.EscapeString(item.Stroke),
			)
		}
	}
	firstLabel := html.EscapeString(webDailySpendShortDayLabel(points[0]))
	lastLabel := html.EscapeString(webDailySpendShortDayLabel(points[len(points)-1]))
	fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="10" fill="#6b7280" text-anchor="start">%s</text>`,
		xAt(0), svgHeight-8, firstLabel)
	fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="10" fill="#6b7280" text-anchor="end">%s</text>`,
		xAt(len(points)-1), svgHeight-8, lastLabel)
	b.WriteString(`</svg><figcaption class="daily-spend-chart-legend">`)
	for _, item := range series {
		fmt.Fprintf(&b, `<span><span class="daily-spend-chart-swatch" style="background:%s"></span>%s</span>`,
			html.EscapeString(item.Stroke),
			html.EscapeString(item.Label),
		)
	}
	b.WriteString(`</figcaption></figure>`)
	return b.String()
}

func webDailySpendShortDayLabel(day string) string {
	parsed, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day
	}
	return parsed.Format("01-02")
}

func renderWebDailySpendTokenTrendTable(id string, points []webDailySpendTokenTrendPoint) string {
	dayLabels := make([]string, 0, len(points))
	inputValues := make([]float64, 0, len(points))
	outputValues := make([]float64, 0, len(points))
	for _, point := range points {
		dayLabels = append(dayLabels, point.Day)
		inputValues = append(inputValues, float64(point.InputTokens))
		outputValues = append(outputValues, float64(point.OutputTokens))
	}
	return renderWebDailySpendLineChart(id, dayLabels, []webDailySpendLineSeries{
		{Label: "Input tokens (left axis)", Stroke: "#2563eb", Axis: "left", Values: inputValues},
		{Label: "Output tokens (right axis)", Stroke: "#14b8a6", Axis: "right", Values: outputValues},
	}, func(value float64) string { return fmtTokens(webRoundPositiveInt(value)) })
}

func renderWebDailySpendMoneyTrendTable(id string, points []webDailySpendMoneyTrendPoint) string {
	dayLabels := make([]string, 0, len(points))
	premiumValues := make([]float64, 0, len(points))
	apiValues := make([]float64, 0, len(points))
	for _, point := range points {
		dayLabels = append(dayLabels, point.Day)
		premiumValues = append(premiumValues, point.PremiumSpend)
		apiValues = append(apiValues, point.APISpend)
	}
	return renderWebDailySpendLineChart(id, dayLabels, []webDailySpendLineSeries{
		{Label: "Premium spend", Stroke: "#7c3aed", Values: premiumValues},
		{Label: "API spend", Stroke: "#f97316", Values: apiValues},
	}, func(value float64) string { return fmtCost(value) })
}

func renderWebDailySpendTopListTable(id string, rows []webDailySpendTopRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<table id="%s" class="daily-spend-table"><thead><tr><th>Name</th><th>Input tokens</th><th>Output tokens</th><th>Premium spend</th><th>API spend</th><th>Prompt count</th></tr></thead><tbody>`, id)
	if len(rows) == 0 {
		b.WriteString(`<tr><td colspan="6">No data yet.</td></tr>`)
	} else {
		for _, row := range rows {
			fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
				html.EscapeString(row.Name),
				html.EscapeString(fmtTokens(row.InputTokens)),
				html.EscapeString(fmtTokens(row.OutputTokens)),
				html.EscapeString(fmtCost(row.PremiumSpend)),
				html.EscapeString(fmtCost(row.APISpend)),
				html.EscapeString(commaInt(row.PromptCount)),
			)
		}
	}
	b.WriteString(`</tbody></table>`)
	return b.String()
}

func renderWebDailyLeaderboardList(id string, rows []webStatsRow) string {
	if len(rows) == 0 {
		return fmt.Sprintf(`<ol id="%s" class="daily-spend-leaderboard"><li>No data yet.</li></ol>`, id)
	}
	maxItems := 3
	if len(rows) < maxItems {
		maxItems = len(rows)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<ol id="%s" class="daily-spend-leaderboard">`, id)
	for i := 0; i < maxItems; i++ {
		row := rows[i]
		fmt.Fprintf(&b, `<li><span class="daily-spend-leaderboard-name">%s</span><span class="daily-spend-leaderboard-value">%s</span></li>`,
			html.EscapeString(row.name),
			fmtCost(row.stats.PremiumRequestCost),
		)
	}
	b.WriteString(`</ol>`)
	return b.String()
}

func webDailyCacheTokens(payload statsPayload, day string) (int, int) {
	dayMap, ok := payload.Daily[day]
	if !ok {
		return 0, 0
	}
	promptTokens := 0
	cacheReadTokens := 0
	for key, value := range dayMap {
		if strings.HasPrefix(key, "_") {
			continue
		}
		stats, ok := webDailyStatsValue(value)
		if !ok {
			continue
		}
		promptTokens += stats.PromptTokens
		cacheReadTokens += stats.CacheReadTokens
	}
	return promptTokens, cacheReadTokens
}

func webDailyCacheEfficiencyTrend(rows []webDailyTotalsRow, day string, currentPct float64) string {
	for idx, row := range rows {
		if row.day != day || idx == 0 {
			continue
		}
		prev := rows[idx-1]
		prevPct := 0.0
		if prev.totalCostNoCache > 0 {
			prevPct = (prev.totalCostNoCache - prev.totalCost) / prev.totalCostNoCache * 100
		}
		delta := currentPct - prevPct
		if delta > 0.1 {
			return fmt.Sprintf("↑ %+.1fpp vs previous day", delta)
		}
		if delta < -0.1 {
			return fmt.Sprintf("↓ %+.1fpp vs previous day", delta)
		}
		return fmt.Sprintf("→ %+.1fpp vs previous day", delta)
	}
	return "No previous-day trend yet."
}

func renderWebDailySpendRegion(payload statsPayload, hasSnapshot bool, now time.Time) string {
	if !hasSnapshot {
		return `<section id="daily-spend-region" class="daily-spend-region"><h2>Copilot Stats</h2><p>Loading stats…</p></section>`
	}
	data := buildWebDailySpendData(payload, now)
	nowDay := now.Format("2006-01-02")
	isToday := data.Day == nowDay
	sectionTitle := "Copilot Stats"
	daySummaryTitle := "Today summary"
	windowSummaryTitle := "Weekly average (rolling 7 days including today)"
	topProjectsDayTitle := "Top projects today"
	topProjectsWindowTitle := "Top projects this week"
	topModelsDayTitle := "Top models today"
	topModelsWindowTitle := "Top models this week"
	emptyStateMessage := "No usage recorded yet for today."
	if !isToday {
		daySummaryTitle = "Selected-day summary"
		topProjectsDayTitle = "Top projects on selected day"
		topModelsDayTitle = "Top models on selected day"
		emptyStateMessage = "No usage recorded for the selected day."
	}
	if data.WindowDays != 7 || !isToday {
		windowUnit := "days"
		if data.WindowDays == 1 {
			windowUnit = "day"
		}
		windowAnchor := "selected day"
		if isToday {
			windowAnchor = "today"
		}
		windowSummaryTitle = fmt.Sprintf("Rolling average (%d %s including %s)", data.WindowDays, windowUnit, windowAnchor)
		topProjectsWindowTitle = "Top projects in rolling window"
		topModelsWindowTitle = "Top models in rolling window"
	}
	emptyState := ""
	if len(webDailyModelStatsByDay(payload, data.Day)) == 0 {
		emptyState = `<p class="daily-spend-note">` + html.EscapeString(emptyStateMessage) + `</p>`
	}
	todayCards := renderWebDailySpendMetricCards([]webDailySpendMetricCard{
		{ID: "daily-spend-tokens", Label: "Input tokens", Value: fmtTokens(data.Today.InputTokens)},
		{ID: "daily-spend-output-tokens", Label: "Output tokens", Value: fmtTokens(data.Today.OutputTokens)},
		{ID: "daily-spend-cost", Label: "Premium spend", Value: fmtCost(data.Today.PremiumSpend)},
		{ID: "daily-spend-api-spend", Label: "API spend", Value: fmtCost(data.Today.APISpend)},
	})
	weeklyCards := renderWebDailySpendMetricCards([]webDailySpendMetricCard{
		{ID: "daily-spend-weekly-input-tokens", Label: "Input tokens", Value: fmtTokens(webRoundPositiveInt(data.Rolling7DayAverage.InputTokens))},
		{ID: "daily-spend-weekly-output-tokens", Label: "Output tokens", Value: fmtTokens(webRoundPositiveInt(data.Rolling7DayAverage.OutputTokens))},
		{ID: "daily-spend-weekly-premium-spend", Label: "Premium spend", Value: fmtCost(data.Rolling7DayAverage.PremiumSpend)},
		{ID: "daily-spend-weekly-api-spend", Label: "API spend", Value: fmtCost(data.Rolling7DayAverage.APISpend)},
	})

	return fmt.Sprintf(`<section id="daily-spend-region" class="daily-spend-region">
  <h2>%s</h2>
  <p class="daily-spend-date">%s</p>
  <div class="daily-spend-top-panels">
    <section id="daily-spend-today-summary" class="daily-spend-section daily-spend-top-panel"><h3>%s</h3>%s</section>
    <section id="daily-spend-weekly-average" class="daily-spend-section daily-spend-top-panel"><h3>%s</h3>%s</section>
  </div>
  <div class="daily-spend-trend-panels">
    <section id="daily-spend-token-trend-section" class="daily-spend-section"><h3>Token trend</h3>%s</section>
    <section id="daily-spend-money-trend-section" class="daily-spend-section"><h3>Money trend</h3>%s</section>
  </div>
  <section id="daily-spend-top-projects-today-section" class="daily-spend-section"><h3>%s</h3>%s</section>
  <section id="daily-spend-top-projects-week-section" class="daily-spend-section"><h3>%s</h3>%s</section>
  <section id="daily-spend-top-models-today-section" class="daily-spend-section"><h3>%s</h3>%s</section>
  <section id="daily-spend-top-models-week-section" class="daily-spend-section"><h3>%s</h3>%s</section>
  %s
</section>`,
		html.EscapeString(sectionTitle),
		html.EscapeString(data.Day),
		html.EscapeString(daySummaryTitle),
		todayCards,
		html.EscapeString(windowSummaryTitle),
		weeklyCards,
		renderWebDailySpendTokenTrendTable("daily-spend-token-trend", data.TokenTrend),
		renderWebDailySpendMoneyTrendTable("daily-spend-money-trend", data.MoneyTrend),
		html.EscapeString(topProjectsDayTitle),
		renderWebDailySpendTopListTable("daily-spend-top-projects-today", data.TopProjectsToday),
		html.EscapeString(topProjectsWindowTitle),
		renderWebDailySpendTopListTable("daily-spend-top-projects-week", data.TopProjectsRolling7),
		html.EscapeString(topModelsDayTitle),
		renderWebDailySpendTopListTable("daily-spend-top-models-today", data.TopModelsToday),
		html.EscapeString(topModelsWindowTitle),
		renderWebDailySpendTopListTable("daily-spend-top-models-week", data.TopModelsRolling7),
		emptyState,
	)
}

func dailySpendShellHTML(payload statsPayload, hasSnapshot bool, now time.Time) string {
	placeholderIndicators := `<div id="refresh-indicators-region"><div id="refresh-indicators" class="refresh-indicators">` +
		renderRefreshIndicatorRow("Local", "Idle", "") +
		renderRefreshIndicatorRow("Codespaces", "Idle", "") +
		`</div></div>`
	return dailySpendShellHTMLWithIndicators(payload, hasSnapshot, now, placeholderIndicators)
}

func dailySpendShellHTMLWithIndicators(payload statsPayload, hasSnapshot bool, now time.Time, refreshIndicatorsHTML string) string {
	if strings.TrimSpace(refreshIndicatorsHTML) == "" {
		refreshIndicatorsHTML = `<div id="refresh-indicators-region"></div>`
	}
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Copilot Stats</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 1.5rem; }
    #dashboard-header { display: flex; justify-content: space-between; align-items: flex-start; gap: 1rem; margin-bottom: 1rem; }
    #dashboard-header h1 { margin: 0; }
    #dashboard-header p { margin: 0.35rem 0 0; }
    .refresh-indicators { font-size: 0.78rem; border: 1px solid #d1d5db; background: #f9fafb; border-radius: 0.4rem; padding: 0.3rem 0.5rem; min-width: 13rem; }
    .refresh-indicator-row { display: flex; justify-content: space-between; gap: 0.5rem; white-space: nowrap; }
    .refresh-indicator-row + .refresh-indicator-row { margin-top: 0.2rem; }
    .refresh-indicator-name { font-weight: 600; }
    .refresh-indicator-countdown { color: #4b5563; }
    #status { color: #b91c1c; margin-bottom: 1rem; }
    .daily-spend-region { max-width: 72rem; }
    .daily-spend-date { margin: 0.4rem 0 1rem; color: #4b5563; font-weight: 600; }
    .daily-spend-top-panels { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 1rem; }
    .daily-spend-trend-panels { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 1rem; }
    .daily-spend-top-panel { margin-top: 0; }
    .daily-spend-metrics { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0.75rem; }
    .daily-spend-metric { border: 1px solid #d1d5db; border-radius: 0.6rem; padding: 0.8rem; background: #f9fafb; min-height: 6.2rem; display: flex; flex-direction: column; justify-content: flex-start; gap: 0.35rem; }
    .daily-spend-metric h4 { margin: 0; font-size: 0.95rem; color: #4b5563; }
    .daily-spend-metric p { margin: 0; font-size: 1.55rem; font-weight: 700; line-height: 1.1; }
    .daily-spend-section { margin-top: 1rem; }
    .daily-spend-section h3 { margin: 0 0 0.6rem; color: #374151; font-size: 1rem; }
    .daily-spend-chart { margin: 0; border: 1px solid #d1d5db; border-radius: 0.4rem; background: #fff; padding: 0.5rem; }
    .daily-spend-chart svg { display: block; width: 100%%; height: auto; }
    .daily-spend-chart-legend { margin-top: 0.4rem; display: flex; flex-wrap: wrap; gap: 0.75rem; color: #4b5563; font-size: 0.82rem; }
    .daily-spend-chart-legend > span { display: inline-flex; align-items: center; gap: 0.35rem; }
    .daily-spend-chart-swatch { display: inline-block; width: 0.7rem; height: 0.7rem; border-radius: 999px; }
    .daily-spend-chart-empty { border: 1px solid #d1d5db; border-radius: 0.4rem; background: #fff; padding: 0.9rem; color: #6b7280; font-size: 0.84rem; }
    .daily-spend-table { width: 100%%; border-collapse: collapse; background: #fff; border: 1px solid #d1d5db; border-radius: 0.4rem; overflow: hidden; }
    .daily-spend-table th, .daily-spend-table td { text-align: left; padding: 0.45rem 0.55rem; border-bottom: 1px solid #e5e7eb; font-size: 0.84rem; }
    .daily-spend-table th { background: #f9fafb; color: #374151; }
    .daily-spend-table tr:last-child td { border-bottom: none; }
    .daily-spend-note { margin: 0.9rem 0 0; color: #6b7280; }
    @media (max-width: 60rem) { .daily-spend-top-panels, .daily-spend-trend-panels { grid-template-columns: 1fr; } }
  </style>
</head>
<body data-signals:status-message="''">
  <header id="dashboard-header">
    <div>
      <h1>Copilot Stats</h1>
      <p><a href="/details">View details</a></p>
    </div>
    %s
  </header>
  <div id="status" data-text="$statusMessage"></div>
  <main id="daily-spend-main">%s</main>
  <div data-init="@get('/events')"></div>
  <div data-on:datastar-fetch="
         evt.detail.type === 'started' && ($statusMessage = '');
         evt.detail.type === 'error' && ($statusMessage = String(evt.detail.error || 'request failed'));
        "></div>
  <script type="module" src="https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.0-RC.7/bundles/datastar.js"></script>
</body>
</html>`, refreshIndicatorsHTML, renderWebDailySpendRegion(payload, hasSnapshot, now))
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
		now := time.Now()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := dailySpendShellHTMLWithIndicators(payload, hasSnapshot, now, state.renderRefreshIndicators(now))
		if _, err := w.Write([]byte(page)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write / response: %v\n", err)
		}
	})
	mux.HandleFunc("/details", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodGet) {
			return
		}
		payload, hasSnapshot := state.getSnapshot()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := dashboardShellHTMLWithIndicators(payload, hasSnapshot, state.renderRefreshIndicators(time.Now()), state.getExpandedRows())
		if _, err := w.Write([]byte(page)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write /details response: %v\n", err)
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
	mux.HandleFunc("/actions/project-row", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodPost) {
			return
		}
		rowKey := strings.TrimSpace(r.URL.Query().Get("row_key"))
		if rowKey == "" {
			writeActionError(w, &webActionError{
				status:  http.StatusBadRequest,
				reason:  "row_key_required",
				message: "project row action failed: row_key is required",
			})
			return
		}
		payload, ok := state.getSnapshot()
		if !ok {
			writeActionError(w, &webActionError{
				status:  http.StatusServiceUnavailable,
				reason:  "snapshot_unavailable",
				message: "project row action failed: snapshot unavailable",
			})
			return
		}
		expand := parseWebExpandAction(r.URL.Query().Get("expand"))
		state.setRowExpanded("project", rowKey, expand)
		patch, err := buildProjectRowTogglePatch(payload, rowKey, expand)
		if err != nil {
			writeActionError(w, &webActionError{
				status:  http.StatusNotFound,
				reason:  "project_row_not_found",
				message: "project row action failed: unknown row_key",
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		if _, err := w.Write([]byte(patch)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write /actions/project-row response: %v\n", err)
		}
	})
	mux.HandleFunc("/actions/day-row", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodPost) {
			return
		}
		rowKey := strings.TrimSpace(r.URL.Query().Get("row_key"))
		if rowKey == "" {
			writeActionError(w, &webActionError{
				status:  http.StatusBadRequest,
				reason:  "row_key_required",
				message: "day row action failed: row_key is required",
			})
			return
		}
		payload, ok := state.getSnapshot()
		if !ok {
			writeActionError(w, &webActionError{
				status:  http.StatusServiceUnavailable,
				reason:  "snapshot_unavailable",
				message: "day row action failed: snapshot unavailable",
			})
			return
		}
		expand := parseWebExpandAction(r.URL.Query().Get("expand"))
		state.setRowExpanded("day", rowKey, expand)
		patch, err := buildDayRowTogglePatch(payload, rowKey, expand)
		if err != nil {
			writeActionError(w, &webActionError{
				status:  http.StatusNotFound,
				reason:  "day_row_not_found",
				message: "day row action failed: unknown row_key",
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		if _, err := w.Write([]byte(patch)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write /actions/day-row response: %v\n", err)
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

	if cfg.LocalStreaming {
		state.startLocalStreamingLoop(2*time.Second, cfg.RefreshInterval)
	} else if cfg.RefreshInterval > 0 {
		state.setLocalRefreshSchedule(cfg.RefreshInterval, time.Now().Add(cfg.RefreshInterval))
		ticker := time.NewTicker(cfg.RefreshInterval)
		state.loopsWG.Add(1)
		go func() {
			defer state.loopsWG.Done()
			defer ticker.Stop()
			for {
				select {
				case <-state.stopCh:
					return
				case tick := <-ticker.C:
					state.setLocalRefreshSchedule(cfg.RefreshInterval, tick.Add(cfg.RefreshInterval))
					go runScheduledSync(state.refreshLocalSnapshot, "web refresh tick failed")
				}
			}
		}()
	} else {
		state.setLocalRefreshSchedule(0, time.Time{})
	}
	if cfg.CodespacesStreaming {
		state.refreshCodespacesStreamingStatus()
		streamingTicker := time.NewTicker(2 * time.Second)
		state.loopsWG.Add(1)
		go func() {
			defer state.loopsWG.Done()
			defer streamingTicker.Stop()
			for {
				select {
				case <-state.stopCh:
					return
				case <-streamingTicker.C:
					state.refreshCodespacesStreamingStatus()
				}
			}
		}()
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
