package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestWebState(t *testing.T, logsDir string) *webState {
	t.Helper()
	db := initDB(tempDBPath(t))
	t.Cleanup(func() {
		_ = db.Close()
	})
	return &webState{
		db:             db,
		logsDir:        logsDir,
		sessionDir:     filepath.Join(t.TempDir(), "session-state"),
		codespacesMode: "manual",
		syncStatus: map[string]syncSourceStatus{
			"local":      newSyncSourceStatus(webSyncCodeSkipped, webSyncReasonNotStarted),
			"codespaces": newSyncSourceStatus(webSyncCodeSkipped, webSyncReasonManualMode),
		},
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stderr = old
	return <-done
}

func waitForSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitForPatch(t *testing.T, ch <-chan string, label string) string {
	t.Helper()
	select {
	case patch, ok := <-ch:
		if !ok {
			t.Fatalf("patch channel closed while waiting for %s", label)
		}
		return patch
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
	return ""
}

func readSSEFrame(t *testing.T, reader *bufio.Reader, label string) string {
	t.Helper()
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read %s frame: %v", label, err)
		}
		frame.WriteString(line)
		if line == "\n" {
			return frame.String()
		}
	}
}

func readSSEFrameContaining(t *testing.T, reader *bufio.Reader, label, contains string) string {
	t.Helper()
	for i := 0; i < 32; i++ {
		frame := readSSEFrame(t, reader, label)
		if strings.Contains(frame, contains) {
			return frame
		}
	}
	t.Fatalf("did not receive %s containing %q", label, contains)
	return ""
}

func TestNewWebStateCodespacesModeDefaultsAndExplicitManual(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		wantMode   string
		wantReason string
	}{
		{
			name:       "default_auto",
			mode:       "",
			wantMode:   "auto",
			wantReason: webSyncReasonAutoMode,
		},
		{
			name:       "explicit_manual",
			mode:       "manual",
			wantMode:   "manual",
			wantReason: webSyncReasonManualMode,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			logsDir := filepath.Join(root, "logs")
			if err := os.MkdirAll(logsDir, 0o755); err != nil {
				t.Fatalf("mkdir logs: %v", err)
			}
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))

			state, err := newWebState(webModeConfig{
				CodespacesMode: tc.mode,
				LogsDir:        logsDir,
				SessionDir:     filepath.Join(root, "session-state"),
			})
			if err != nil {
				t.Fatalf("newWebState: %v", err)
			}
			t.Cleanup(state.close)

			if state.codespacesMode != tc.wantMode {
				t.Fatalf("codespacesMode=%q, want=%q", state.codespacesMode, tc.wantMode)
			}
			if got := state.syncStatus["codespaces"].Reason; got != tc.wantReason {
				t.Fatalf("codespaces reason=%q, want=%q", got, tc.wantReason)
			}
		})
	}
}

func TestStartStartupSyncDoesNotBlockAndTriggersImmediateBackgroundSyncs(t *testing.T) {
	state := &webState{codespacesMode: "auto"}

	localStarted := make(chan struct{}, 1)
	codespacesStarted := make(chan struct{}, 1)
	localRelease := make(chan struct{})
	codespacesRelease := make(chan struct{})
	codespacesLoopStarted := make(chan struct{}, 1)

	startedAt := time.Now()
	state.startStartupSyncWith(
		func() error {
			localStarted <- struct{}{}
			<-localRelease
			return nil
		},
		func() error {
			codespacesStarted <- struct{}{}
			<-codespacesRelease
			return nil
		},
		func() {
			codespacesLoopStarted <- struct{}{}
		},
	)
	if elapsed := time.Since(startedAt); elapsed > 200*time.Millisecond {
		t.Fatalf("startup sync launch blocked for %s, want <=200ms", elapsed)
	}

	waitForSignal(t, localStarted, "local startup sync trigger")
	waitForSignal(t, codespacesStarted, "codespaces startup sync trigger")

	select {
	case <-codespacesLoopStarted:
		t.Fatal("codespaces loop started before startup sync completed")
	default:
	}

	close(localRelease)
	close(codespacesRelease)
	waitForSignal(t, codespacesLoopStarted, "codespaces auto-sync loop start")
}

func TestRunStartupSyncEmitsExpectedStartupLogMessages(t *testing.T) {
	state := &webState{}
	logs := captureStderr(t, func() {
		state.runStartupSync("local", func() error { return nil })
		state.runStartupSync("codespaces", func() error { return nil })
	})

	for _, want := range []string{
		"web startup local sync started",
		"web startup local sync completed",
		"web startup codespaces sync started",
		"web startup codespaces sync completed",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("startup logs missing %q: %q", want, logs)
		}
	}
}

func TestStartCodespacesAutoSyncPerformsStartupSync(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	state := newTestWebState(t, logsDir)
	state.codespacesMode = "auto"

	binDir := t.TempDir()
	writeFakeGH(t, binDir, true)
	withPath(t, binDir)

	state.startCodespacesAutoSync(time.Hour)

	payload, ok := state.getSnapshot()
	if !ok {
		t.Fatal("expected snapshot after startup codespaces sync")
	}
	if payload.SyncStatus["codespaces"].Code != webSyncCodeOK {
		t.Fatalf("codespaces status code=%q, want=%q", payload.SyncStatus["codespaces"].Code, webSyncCodeOK)
	}
}

func TestWebStateTodayWindowPropagatesToSnapshotAndOutput(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	sessionDir := filepath.Join(root, "session-state")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session-state: %v", err)
	}

	oldTS := time.Date(2026, time.January, 1, 10, 0, 0, 0, time.Local)
	todayTS := time.Date(2026, time.January, 2, 10, 0, 0, 0, time.Local)

	todayMidnight := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.Local)
	dateRange := "2026-01-02 → 2026-01-02"
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	seedDB := initDB(getDBPath())
	insertRecords(seedDB, []Record{
		{
			Model:            "gpt-4.1",
			PromptTokens:     10,
			CompletionTokens: 5,
			IsUserTurn:       true,
			Timestamp:        oldTS.Format("2006-01-02T15:04:05"),
			SessionID:        "session-old",
			LogFile:          "old.log",
		},
		{
			Model:            "claude-sonnet-4.6",
			PromptTokens:     20,
			CompletionTokens: 7,
			IsUserTurn:       true,
			Timestamp:        todayTS.Format("2006-01-02T15:04:05"),
			SessionID:        "session-today",
			LogFile:          "today.log",
		},
	}, "local")
	_ = seedDB.Close()

	state, err := newWebState(webModeConfig{
		LogsDir:       logsDir,
		SessionDir:    sessionDir,
		PeriodLabel:   "today",
		DateRange:     dateRange,
		DateFromQuery: todayMidnight.Format("2006-01-02T15:04:05"),
		SyncFrom:      &todayMidnight,
	})
	if err != nil {
		t.Fatalf("newWebState: %v", err)
	}
	t.Cleanup(state.close)

	payload, ok := state.getSnapshot()
	if !ok {
		t.Fatal("expected snapshot")
	}
	if payload.Period != "today" {
		t.Fatalf("period=%q, want=%q", payload.Period, "today")
	}
	if payload.DateRange == nil || *payload.DateRange != dateRange {
		t.Fatalf("date range=%v, want=%q", payload.DateRange, dateRange)
	}
	if payload.APICalls != 1 {
		t.Fatalf("api calls=%d, want=1", payload.APICalls)
	}
	if payload.SyncStatus["local"].Reason != webSyncReasonNotStarted {
		t.Fatalf("local reason=%q, want=%q", payload.SyncStatus["local"].Reason, webSyncReasonNotStarted)
	}
	if _, ok := payload.Daily["2026-01-01"]; ok {
		t.Fatalf("unexpected daily stats for filtered-out day")
	}
	if _, ok := payload.Daily["2026-01-02"]; !ok {
		t.Fatalf("missing daily stats for included day")
	}

	mux := newWebMux(state)
	homeRec := httptest.NewRecorder()
	mux.ServeHTTP(homeRec, httptest.NewRequest(http.MethodGet, "/", nil))
	if homeRec.Code != http.StatusOK {
		t.Fatalf("GET / status=%d", homeRec.Code)
	}
	if !strings.Contains(homeRec.Body.String(), "Period: today ("+dateRange+")") {
		t.Fatalf("GET / body missing date range summary")
	}
	if strings.Contains(homeRec.Body.String(), "2026-01-01") {
		t.Fatalf("GET / body unexpectedly contains filtered-out date")
	}

	statsRec := httptest.NewRecorder()
	mux.ServeHTTP(statsRec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if statsRec.Code != http.StatusOK {
		t.Fatalf("GET /api/stats status=%d", statsRec.Code)
	}
	var apiPayload statsPayload
	if err := json.Unmarshal(statsRec.Body.Bytes(), &apiPayload); err != nil {
		t.Fatalf("decode /api/stats: %v", err)
	}
	if apiPayload.APICalls != 1 {
		t.Fatalf("/api/stats api_calls=%d, want=1", apiPayload.APICalls)
	}
	if apiPayload.DateRange == nil || *apiPayload.DateRange != dateRange {
		t.Fatalf("/api/stats date_range=%v, want=%q", apiPayload.DateRange, dateRange)
	}
}

func TestWebDefaultAutoStartupRunsCodespacesSyncPath(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))

	state, err := newWebState(webModeConfig{
		LogsDir:    logsDir,
		SessionDir: filepath.Join(root, "session-state"),
	})
	if err != nil {
		t.Fatalf("newWebState: %v", err)
	}
	t.Cleanup(state.close)
	if state.codespacesMode != "auto" {
		t.Fatalf("codespacesMode=%q, want=%q", state.codespacesMode, "auto")
	}

	binDir := t.TempDir()
	writeFakeGH(t, binDir, true)
	withPath(t, binDir)

	if state.codespacesMode == "auto" {
		state.startCodespacesAutoSync(time.Hour)
	}

	payload, ok := state.getSnapshot()
	if !ok {
		t.Fatal("expected snapshot after startup auto-sync path")
	}
	status := payload.SyncStatus["codespaces"]
	if status.Reason == webSyncReasonAutoMode {
		t.Fatalf("codespaces reason=%q, want startup sync result", status.Reason)
	}
	if status.Code != webSyncCodeOK && status.Code != webSyncCodeSkipped {
		t.Fatalf("codespaces status code=%q, want one of [%q, %q]", status.Code, webSyncCodeOK, webSyncCodeSkipped)
	}
	if !state.codespacesHasSuccess {
		t.Fatal("expected successful startup auto-sync")
	}
}

func TestStartCodespacesAutoSyncLoopWaitsForInterval(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	state := newTestWebState(t, logsDir)

	binDir := t.TempDir()
	writeFakeGH(t, binDir, true)
	withPath(t, binDir)

	state.startCodespacesAutoSyncLoop(200 * time.Millisecond)

	time.Sleep(80 * time.Millisecond)
	if _, ok := state.getSnapshot(); ok {
		t.Fatal("unexpected snapshot before first auto-sync interval elapsed")
	}

	time.Sleep(700 * time.Millisecond)
	payload, ok := state.getSnapshot()
	if !ok {
		t.Fatal("expected snapshot after auto-sync interval elapsed")
	}
	code := payload.SyncStatus["codespaces"].Code
	if code != webSyncCodeOK && code != webSyncCodeSkipped {
		t.Fatalf("codespaces status code=%q, want one of [%q, %q]", code, webSyncCodeOK, webSyncCodeSkipped)
	}
	if !state.codespacesHasSuccess {
		t.Fatal("expected at least one successful auto-sync")
	}

	state.refreshMu.Lock()
}

func TestRefreshLocalSnapshotSingleFlightConflict(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	state := newTestWebState(t, logsDir)

	state.refreshMu.Lock()
	defer state.refreshMu.Unlock()

	err := state.refreshLocalSnapshot()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	var actionErr *webActionError
	if !errors.As(err, &actionErr) {
		t.Fatalf("expected webActionError, got %T", err)
	}
	if actionErr.status != http.StatusConflict {
		t.Fatalf("status=%d, want=%d", actionErr.status, http.StatusConflict)
	}
	if actionErr.reason != webSyncReasonInProgress {
		t.Fatalf("reason=%q, want=%q", actionErr.reason, webSyncReasonInProgress)
	}
}

func TestRefreshLocalSnapshotUpdatesSyncStatusInSnapshot(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	state := newTestWebState(t, logsDir)

	if err := state.refreshLocalSnapshot(); err != nil {
		t.Fatalf("refreshLocalSnapshot: %v", err)
	}
	payload, ok := state.getSnapshot()
	if !ok {
		t.Fatal("expected snapshot")
	}
	if payload.SyncStatus["local"].Code != webSyncCodeOK {
		t.Fatalf("local code=%q, want=%q", payload.SyncStatus["local"].Code, webSyncCodeOK)
	}
	if payload.SyncStatus["local"].Reason != webSyncReasonLocalSyncCompleted {
		t.Fatalf("local reason=%q, want=%q", payload.SyncStatus["local"].Reason, webSyncReasonLocalSyncCompleted)
	}
	if _, exists := payload.SyncStatus["codespaces"]; !exists {
		t.Fatal("expected codespaces sync status in payload")
	}

	state.logsDir = filepath.Join(root, "missing")
	if err := state.refreshLocalSnapshot(); err != nil {
		t.Fatalf("refreshLocalSnapshot with missing logs dir: %v", err)
	}
	payload, ok = state.getSnapshot()
	if !ok {
		t.Fatal("expected snapshot after missing logs dir")
	}
	if payload.SyncStatus["local"].Code != webSyncCodeSkipped {
		t.Fatalf("local code=%q, want=%q", payload.SyncStatus["local"].Code, webSyncCodeSkipped)
	}
	if payload.SyncStatus["local"].Reason != webSyncReasonLocalLogsDirMissing {
		t.Fatalf("local reason=%q, want=%q", payload.SyncStatus["local"].Reason, webSyncReasonLocalLogsDirMissing)
	}
}

func TestRebuildSnapshotUsesConfiguredDateWindow(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	state.periodLabel = "today"
	state.dateRange = "2026-01-02 → 2026-01-02"
	state.dateFromQuery = "2026-01-02T00:00:00"
	state.dateToQuery = "2026-01-03T00:00:00"

	insertRecords(state.db, []Record{
		{
			Model:            "gpt-4.1",
			PromptTokens:     10,
			CompletionTokens: 5,
			IsUserTurn:       true,
			Timestamp:        "2026-01-01T10:00:00",
			SessionID:        "s-old",
			LogFile:          "old.log",
		},
		{
			Model:            "gpt-4.1",
			PromptTokens:     20,
			CompletionTokens: 7,
			IsUserTurn:       true,
			Timestamp:        "2026-01-02T10:00:00",
			SessionID:        "s-new",
			LogFile:          "new.log",
		},
	}, "local")

	state.rebuildSnapshot()
	payload, ok := state.getSnapshot()
	if !ok {
		t.Fatal("expected snapshot")
	}
	if payload.Period != "today" {
		t.Fatalf("period=%q, want=%q", payload.Period, "today")
	}
	if payload.DateRange == nil || *payload.DateRange != state.dateRange {
		t.Fatalf("date range=%v, want=%q", payload.DateRange, state.dateRange)
	}
	if payload.APICalls != 1 {
		t.Fatalf("api calls=%d, want=1", payload.APICalls)
	}
	if _, ok := payload.Daily["2026-01-01"]; ok {
		t.Fatalf("unexpected daily stats for filtered-out day")
	}
	if _, ok := payload.Daily["2026-01-02"]; !ok {
		t.Fatalf("missing daily stats for included day")
	}
}

func TestRebuildSnapshotBroadcastsPatchToSubscribers(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	updates, unsubscribe := state.subscribe()
	defer unsubscribe()

	state.rebuildSnapshot()
	patch := waitForPatch(t, updates, "rebuild snapshot patch")
	for _, selector := range []string{
		"data: selector #overview-summary",
		"data: selector #sync-status-region",
		"data: selector #model-summary-region",
		"data: selector #project-summary-region",
		"data: selector #daily-totals-region",
		"data: selector #stats-json",
		"data: selector #refresh-indicators-region",
	} {
		if !strings.Contains(patch, selector) {
			t.Fatalf("patch missing selector %q", selector)
		}
	}
}

func TestRefreshLocalSnapshotBroadcastsPatch(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	state := newTestWebState(t, logsDir)
	updates, unsubscribe := state.subscribe()
	defer unsubscribe()

	if err := state.refreshLocalSnapshot(); err != nil {
		t.Fatalf("refreshLocalSnapshot: %v", err)
	}
	patch := waitForPatch(t, updates, "local refresh patch")
	if !strings.Contains(patch, "event: datastar-patch-elements") {
		t.Fatalf("refresh patch missing datastar events")
	}
}

func TestSetSyncStatusBroadcastsPatch(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	state.rebuildSnapshot()
	updates, unsubscribe := state.subscribe()
	defer unsubscribe()

	state.setSyncStatus("local", webSyncCodeOK, webSyncReasonLocalSyncCompleted)

	patch := waitForPatch(t, updates, "sync status patch")
	if !strings.Contains(patch, "data: selector #sync-status-region") {
		t.Fatalf("sync status patch missing selector")
	}
	if !strings.Contains(patch, webSyncReasonLocalSyncCompleted) {
		t.Fatalf("sync status patch missing updated reason")
	}
}

func TestSyncCodespacesSnapshotBroadcastsPatch(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	state := newTestWebState(t, logsDir)
	updates, unsubscribe := state.subscribe()
	defer unsubscribe()

	binDir := t.TempDir()
	writeFakeGH(t, binDir, true)
	withPath(t, binDir)

	if err := state.syncCodespacesSnapshot(); err != nil {
		t.Fatalf("syncCodespacesSnapshot: %v", err)
	}
	patch := waitForPatch(t, updates, "codespaces sync patch")
	if !strings.Contains(patch, "event: datastar-patch-elements") {
		t.Fatalf("codespaces patch missing datastar events")
	}
}

func TestSyncCodespacesSnapshotAutoBroadcastsPatch(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	state := newTestWebState(t, logsDir)
	state.codespacesMode = "auto"
	updates, unsubscribe := state.subscribe()
	defer unsubscribe()

	binDir := t.TempDir()
	writeFakeGH(t, binDir, true)
	withPath(t, binDir)

	if err := state.syncCodespacesSnapshotAuto(); err != nil {
		t.Fatalf("syncCodespacesSnapshotAuto: %v", err)
	}
	patch := waitForPatch(t, updates, "codespaces auto-sync patch")
	if !strings.Contains(patch, "event: datastar-patch-elements") {
		t.Fatalf("codespaces auto-sync patch missing datastar events")
	}
}

func TestWriteActionErrorIncludesReason(t *testing.T) {
	rec := httptest.NewRecorder()
	writeActionError(rec, &webActionError{
		status:  http.StatusConflict,
		reason:  webSyncReasonInProgress,
		message: "sync already in progress",
	})

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want=%d", rec.Code, http.StatusConflict)
	}
	var body webActionErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Reason != webSyncReasonInProgress {
		t.Fatalf("reason=%q, want=%q", body.Reason, webSyncReasonInProgress)
	}
}

func TestClassifyCodespacesSyncFailureAutoUsesStaleAfterSuccess(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	state.codespacesHasSuccess = true
	state.codespacesLastSuccessAt = time.Date(2026, time.February, 18, 12, 0, 0, 0, time.UTC)

	code, reason := state.classifyCodespacesSyncFailure(errors.New("timed out"), true)
	if code != webSyncCodeStale {
		t.Fatalf("code=%q, want=%q", code, webSyncCodeStale)
	}
	if !strings.Contains(reason, webSyncReasonCodespacesStale) {
		t.Fatalf("reason=%q, want stale marker", reason)
	}
	if !strings.Contains(reason, "last_success=") {
		t.Fatalf("reason=%q, want last_success metadata", reason)
	}
}

func TestCodespacesRetryHelpersRespectBounds(t *testing.T) {
	base := normalizeCodespacesInterval(0)
	if base != defaultWebCodespacesInterval {
		t.Fatalf("normalize interval=%s, want=%s", base, defaultWebCodespacesInterval)
	}

	capDelay := computeCodespacesRetryCap(10 * time.Minute)
	if capDelay != maxWebCodespacesRetryBackoff {
		t.Fatalf("cap delay=%s, want=%s", capDelay, maxWebCodespacesRetryBackoff)
	}

	delay := nextCodespacesRetryDelay(5*time.Minute, capDelay)
	if delay != 10*time.Minute {
		t.Fatalf("delay=%s, want=10m0s", delay)
	}
	delay = nextCodespacesRetryDelay(20*time.Minute, capDelay)
	if delay != capDelay {
		t.Fatalf("delay=%s, want=%s", delay, capDelay)
	}
}

func TestWebMuxEndpointAvailability(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	state := newTestWebState(t, logsDir)
	if err := state.refreshLocalSnapshot(); err != nil {
		t.Fatalf("refreshLocalSnapshot: %v", err)
	}
	mux := newWebMux(state)

	homeRec := httptest.NewRecorder()
	mux.ServeHTTP(homeRec, httptest.NewRequest(http.MethodGet, "/", nil))
	if homeRec.Code != http.StatusOK {
		t.Fatalf("GET / status=%d", homeRec.Code)
	}
	if ct := homeRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET / content-type=%q", ct)
	}
	if !strings.Contains(homeRec.Body.String(), "Copilot Token Cost Dashboard") {
		t.Fatalf("GET / body missing dashboard title")
	}

	statsRec := httptest.NewRecorder()
	mux.ServeHTTP(statsRec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if statsRec.Code != http.StatusOK {
		t.Fatalf("GET /api/stats status=%d", statsRec.Code)
	}
	if ct := statsRec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("GET /api/stats content-type=%q", ct)
	}
	var payload statsPayload
	if err := json.Unmarshal(statsRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /api/stats: %v", err)
	}
	if payload.SyncStatus["local"].Code == "" {
		t.Fatalf("GET /api/stats missing local sync status")
	}

	healthRec := httptest.NewRecorder()
	mux.ServeHTTP(healthRec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if healthRec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status=%d", healthRec.Code)
	}
	if got := healthRec.Body.String(); got != "ok\n" {
		t.Fatalf("GET /healthz body=%q, want=%q", got, "ok\n")
	}
}

func TestWebMuxEventsEndpointMethodValidation(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	mux := newWebMux(state)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/events", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /events status=%d, want=%d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestWebMuxEventsEndpointStreamsBroadcastPatch(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	server := httptest.NewServer(newWebMux(state))
	defer server.Close()

	client := server.Client()
	client.Timeout = 3 * time.Second
	resp, err := client.Get(server.URL + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events status=%d, want=%d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("GET /events content-type=%q", ct)
	}
	if cacheControl := resp.Header.Get("Cache-Control"); !strings.Contains(strings.ToLower(cacheControl), "no-cache") {
		t.Fatalf("GET /events cache-control=%q", cacheControl)
	}
	if accel := resp.Header.Get("X-Accel-Buffering"); strings.ToLower(accel) != "no" {
		t.Fatalf("GET /events x-accel-buffering=%q, want=no", accel)
	}

	reader := bufio.NewReader(resp.Body)
	state.rebuildSnapshot()

	first := readSSEFrameContaining(t, reader, "events patch #1", "data: selector #overview-summary")
	if !strings.Contains(first, "event: datastar-patch-elements") {
		t.Fatalf("/events frame missing datastar patch event: %q", first)
	}

	state.rebuildSnapshot()

	second := readSSEFrameContaining(t, reader, "events patch #2", "data: selector #overview-summary")
	if !strings.Contains(second, "event: datastar-patch-elements") {
		t.Fatalf("/events second frame missing datastar patch event: %q", second)
	}
}

func TestWebMuxEventsEndpointReconnectReceivesNewPatch(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	server := httptest.NewServer(newWebMux(state))
	defer server.Close()

	client := server.Client()
	client.Timeout = 3 * time.Second

	firstResp, err := client.Get(server.URL + "/events")
	if err != nil {
		t.Fatalf("GET /events first connect: %v", err)
	}
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events first status=%d, want=%d", firstResp.StatusCode, http.StatusOK)
	}

	state.rebuildSnapshot()
	firstFrame := readSSEFrameContaining(t, bufio.NewReader(firstResp.Body), "events reconnect first patch", "data: selector #overview-summary")
	if !strings.Contains(firstFrame, "event: datastar-patch-elements") {
		t.Fatalf("/events first reconnect frame missing patch event: %q", firstFrame)
	}
	_ = firstResp.Body.Close()

	secondResp, err := client.Get(server.URL + "/events")
	if err != nil {
		t.Fatalf("GET /events second connect: %v", err)
	}
	defer func() { _ = secondResp.Body.Close() }()
	if secondResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events second status=%d, want=%d", secondResp.StatusCode, http.StatusOK)
	}

	state.rebuildSnapshot()
	secondFrame := readSSEFrameContaining(t, bufio.NewReader(secondResp.Body), "events reconnect second patch", "data: selector #overview-summary")
	if !strings.Contains(secondFrame, "event: datastar-patch-elements") {
		t.Fatalf("/events second reconnect frame missing patch event: %q", secondFrame)
	}
}

func TestWebMuxEventsEndpointEmitsHeartbeat(t *testing.T) {
	original := webEventsHeartbeatInterval
	webEventsHeartbeatInterval = 10 * time.Millisecond
	defer func() {
		webEventsHeartbeatInterval = original
	}()

	state := newTestWebState(t, t.TempDir())
	server := httptest.NewServer(newWebMux(state))
	defer server.Close()

	client := server.Client()
	client.Timeout = 3 * time.Second
	resp, err := client.Get(server.URL + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	frame := readSSEFrame(t, bufio.NewReader(resp.Body), "events heartbeat")
	if !strings.Contains(frame, "event: heartbeat") {
		t.Fatalf("/events frame missing heartbeat event: %q", frame)
	}
}

func TestWebMuxEventsEndpointEmitsRefreshIndicatorPatch(t *testing.T) {
	original := webIndicatorRefreshInterval
	webIndicatorRefreshInterval = 10 * time.Millisecond
	defer func() {
		webIndicatorRefreshInterval = original
	}()

	state := newTestWebState(t, t.TempDir())
	state.setLocalRefreshSchedule(30*time.Second, time.Now().Add(20*time.Second))
	server := httptest.NewServer(newWebMux(state))
	defer server.Close()

	client := server.Client()
	client.Timeout = 3 * time.Second
	resp, err := client.Get(server.URL + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	frame := readSSEFrameContaining(t, bufio.NewReader(resp.Body), "events indicators patch", "data: selector #refresh-indicators-region")
	if !strings.Contains(frame, "Local") {
		t.Fatalf("/events indicator frame missing Local row: %q", frame)
	}
}

func TestWebMuxAPIStatsUnavailableWithoutSnapshot(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	mux := newWebMux(state)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/stats status=%d, want=%d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestWebMuxRefreshActionEndpointNotAvailable(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	mux := newWebMux(state)

	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/actions/refresh", nil))
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("GET /actions/refresh status=%d, want=%d", getRec.Code, http.StatusNotFound)
	}

	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, httptest.NewRequest(http.MethodPost, "/actions/refresh", nil))
	if postRec.Code != http.StatusNotFound {
		t.Fatalf("POST /actions/refresh status=%d, want=%d", postRec.Code, http.StatusNotFound)
	}
}

func TestWebMuxSyncCodespacesActionBehaviorAndValidation(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	state := newTestWebState(t, logsDir)
	mux := newWebMux(state)

	methodRec := httptest.NewRecorder()
	mux.ServeHTTP(methodRec, httptest.NewRequest(http.MethodGet, "/actions/sync-codespaces", nil))
	if methodRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /actions/sync-codespaces status=%d, want=%d", methodRec.Code, http.StatusMethodNotAllowed)
	}

	state.codespacesMode = "auto"
	modeRec := httptest.NewRecorder()
	mux.ServeHTTP(modeRec, httptest.NewRequest(http.MethodPost, "/actions/sync-codespaces", nil))
	if modeRec.Code != http.StatusConflict {
		t.Fatalf("mode validation status=%d, want=%d", modeRec.Code, http.StatusConflict)
	}
	var modeBody webActionErrorResponse
	if err := json.Unmarshal(modeRec.Body.Bytes(), &modeBody); err != nil {
		t.Fatalf("decode mode validation body: %v", err)
	}
	if modeBody.Reason != "codespaces_mode_not_manual" {
		t.Fatalf("mode validation reason=%q", modeBody.Reason)
	}

	state.codespacesMode = "manual"
	binDir := t.TempDir()
	writeFakeGH(t, binDir, true)
	withPath(t, binDir)

	syncRec := httptest.NewRecorder()
	mux.ServeHTTP(syncRec, httptest.NewRequest(http.MethodPost, "/actions/sync-codespaces", nil))
	if syncRec.Code != http.StatusOK {
		t.Fatalf("POST /actions/sync-codespaces status=%d", syncRec.Code)
	}
	if ct := syncRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("POST /actions/sync-codespaces content-type=%q", ct)
	}
	if !strings.Contains(syncRec.Body.String(), "event: datastar-patch-elements") {
		t.Fatalf("POST /actions/sync-codespaces missing datastar patch event")
	}
	for _, selector := range []string{
		"data: selector #overview-summary",
		"data: selector #sync-status-region",
		"data: selector #model-summary-region",
		"data: selector #project-summary-region",
		"data: selector #daily-totals-region",
		"data: selector #stats-json",
		"data: selector #refresh-indicators-region",
	} {
		if !strings.Contains(syncRec.Body.String(), selector) {
			t.Fatalf("POST /actions/sync-codespaces missing selector %q", selector)
		}
	}
	payload, ok := state.getSnapshot()
	if !ok {
		t.Fatalf("snapshot missing after codespaces sync")
	}
	if payload.SyncStatus["codespaces"].Code != webSyncCodeOK {
		t.Fatalf("codespaces status code=%q, want=%q", payload.SyncStatus["codespaces"].Code, webSyncCodeOK)
	}
}

func sampleWebDashboardPayload() statsPayload {
	return statsPayload{
		Period:                  "last 7 days",
		LogFiles:                3,
		APICalls:                12,
		TotalCost:               1.25,
		TotalCostNoCache:        1.80,
		TotalPremiumRequestCost: 0.48,
		SyncStatus: map[string]syncSourceStatus{
			"local":      {Code: webSyncCodeOK, Reason: webSyncReasonLocalSyncCompleted, UpdatedAt: "2026-02-19T12:00:00Z"},
			"codespaces": {Code: webSyncCodeSkipped, Reason: webSyncReasonManualMode, UpdatedAt: "2026-02-19T12:00:01Z"},
		},
		Models: map[string]statsPayloadStats{
			"gpt-5":           {APICalls: 7, PremiumRequests: 7, Cost: 0.9, CostWithoutCache: 1.2, PremiumRequestCost: 0.28},
			"claude-sonnet-4": {APICalls: 5, PremiumRequests: 5, Cost: 0.35, CostWithoutCache: 0.6, PremiumRequestCost: 0.2},
		},
		Projects: map[string]statsPayloadStats{
			"token-consumption-copilot": {APICalls: 12, PremiumRequests: 12, Cost: 1.25, PremiumRequestCost: 0.48},
		},
		Daily: map[string]map[string]interface{}{
			"2026-02-18": {
				"gpt-5":                     statsPayloadStats{APICalls: 4, PremiumRequests: 4},
				"_total_cost":               0.5,
				"_total_cost_without_cache": 0.7,
			},
			"2026-02-19": {
				"claude-sonnet-4":           statsPayloadStats{APICalls: 8, PremiumRequests: 8},
				"_total_cost":               0.75,
				"_total_cost_without_cache": 1.1,
			},
		},
	}
}

func TestDashboardShellHTMLRendersOverviewTables(t *testing.T) {
	payload := sampleWebDashboardPayload()

	body := dashboardShellHTML(payload, true)
	expected := []string{
		"data-init=\"@get('/events')\"",
		"data-on:datastar-fetch=",
		"datastar.js",
		"id=\"refresh-indicators-region\"",
		"id=\"overview-summary\"",
		"id=\"sync-status-region\"",
		"Sync status",
		"id=\"sync-status-table\"",
		"id=\"model-summary-region\"",
		"Per-model summary",
		"id=\"model-summary-table\"",
		"<th>Model</th>",
		"gpt-5",
		"id=\"project-summary-region\"",
		"Per-project summary",
		"id=\"project-summary-table\"",
		"<th>Project</th>",
		"token-consumption-copilot",
		"id=\"daily-totals-region\"",
		"Daily totals",
		"id=\"daily-totals-table\"",
		"<th>Date</th>",
		"<th>Total</th>",
		"2026-02-18",
		"id=\"stats-json\"",
	}
	for _, needle := range expected {
		if !strings.Contains(body, needle) {
			t.Fatalf("dashboard body missing %q", needle)
		}
	}
	if strings.Contains(body, "No-Cache") {
		t.Fatalf("dashboard body unexpectedly contains No-Cache column")
	}
	if strings.Contains(body, "/actions/refresh") {
		t.Fatalf("dashboard body unexpectedly contains /actions/refresh control")
	}
}

func TestRenderRefreshIndicatorsManualModeAndCountdown(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	now := time.Date(2026, time.February, 19, 12, 0, 0, 0, time.UTC)
	state.setLocalRefreshSchedule(30*time.Second, now.Add(12*time.Second))

	body := state.renderRefreshIndicators(now)
	for _, needle := range []string{
		`id="refresh-indicators-region"`,
		"Local",
		"12s",
		"Codespaces",
		"Manual",
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("refresh indicators missing %q", needle)
		}
	}
	if strings.Contains(body, `Codespaces</span><span class="refresh-indicator-state">Manual</span><span class="refresh-indicator-countdown">`) {
		t.Fatalf("manual codespaces indicator unexpectedly contains countdown: %q", body)
	}
}

func TestRenderRefreshIndicatorsRunningKeepsCountdown(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	state.codespacesMode = "auto"
	now := time.Date(2026, time.February, 19, 12, 0, 0, 0, time.UTC)
	state.setLocalRefreshSchedule(30*time.Second, now.Add(9*time.Second))
	state.setCodespacesRefreshSchedule(5*time.Minute, now.Add(90*time.Second))
	state.syncStatus["local"] = newSyncSourceStatus(webSyncCodeSkipped, webSyncReasonInProgress)
	state.syncStatus["codespaces"] = newSyncSourceStatus(webSyncCodeSkipped, webSyncReasonInProgress)

	body := state.renderRefreshIndicators(now)
	if got := strings.Count(body, ">Running<"); got != 2 {
		t.Fatalf("running state count=%d, want=2; body=%q", got, body)
	}
	for _, needle := range []string{"9s", "1m 30s"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("running indicators missing countdown %q: %q", needle, body)
		}
	}
}

func TestBuildDashboardPatchIncludesRefreshIndicatorsPatch(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	patch, err := state.buildDashboardPatch(sampleWebDashboardPayload(), time.Date(2026, time.February, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("buildDashboardPatch: %v", err)
	}
	if !strings.Contains(patch, "data: selector #refresh-indicators-region") {
		t.Fatalf("dashboard patch missing refresh indicators selector")
	}
	if !strings.Contains(patch, "Codespaces") {
		t.Fatalf("dashboard patch missing refresh indicators content")
	}
}

func TestBuildRefreshPatchPatchesOverviewFragments(t *testing.T) {
	patch, err := buildRefreshPatch(sampleWebDashboardPayload())
	if err != nil {
		t.Fatalf("buildRefreshPatch: %v", err)
	}
	if got := strings.Count(patch, "event: datastar-patch-elements"); got != 6 {
		t.Fatalf("patch event count=%d, want=6", got)
	}
	if got := strings.Count(patch, "data: mode outer"); got != 6 {
		t.Fatalf("outer mode count=%d, want=6", got)
	}
	orderedSelectors := []string{
		"data: selector #overview-summary",
		"data: selector #sync-status-region",
		"data: selector #model-summary-region",
		"data: selector #project-summary-region",
		"data: selector #daily-totals-region",
		"data: selector #stats-json",
	}
	last := -1
	for _, selector := range orderedSelectors {
		idx := strings.Index(patch, selector)
		if idx == -1 {
			t.Fatalf("patch missing selector %q", selector)
		}
		if idx < last {
			t.Fatalf("selector %q appeared out of order", selector)
		}
		last = idx
	}
	for _, needle := range []string{
		`<table id="sync-status-table">`,
		`<table id="model-summary-table">`,
		`<table id="project-summary-table">`,
		`<table id="daily-totals-table">`,
		"&#34;period&#34;",
	} {
		if !strings.Contains(patch, needle) {
			t.Fatalf("patch missing fragment content %q", needle)
		}
	}
	if strings.Contains(patch, "No-Cache") {
		t.Fatalf("patch unexpectedly contains No-Cache column")
	}
}
