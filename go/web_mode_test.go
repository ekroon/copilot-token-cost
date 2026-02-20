package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"math"
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
	defer state.refreshMu.Unlock()
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
	if got := strings.Count(patch, "event: datastar-patch-elements"); got != 9 {
		t.Fatalf("patch event count=%d, want=9", got)
	}
	for _, selector := range []string{
		"data: selector #overview-summary",
		"data: selector #sync-status-region",
		"data: selector #daily-token-chart-region",
		"data: selector #model-summary-region",
		"data: selector #project-summary-region",
		"data: selector #daily-totals-region",
		"data: selector #daily-spend-region",
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
	if !strings.Contains(homeRec.Body.String(), `<a href="/daily-spend">View today's spend</a>`) {
		t.Fatalf("GET / body missing daily spend link")
	}

	dailySpendRec := httptest.NewRecorder()
	mux.ServeHTTP(dailySpendRec, httptest.NewRequest(http.MethodGet, "/daily-spend", nil))
	if dailySpendRec.Code != http.StatusOK {
		t.Fatalf("GET /daily-spend status=%d", dailySpendRec.Code)
	}
	if ct := dailySpendRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET /daily-spend content-type=%q", ct)
	}
	for _, needle := range []string{
		"Today's Copilot Spend",
		`id="daily-spend-region"`,
		`id="daily-spend-tokens"`,
		`id="daily-spend-cost"`,
		`id="daily-spend-weekly-average"`,
		`id="daily-spend-token-trend"`,
		`data-init="@get('/events')"`,
	} {
		if !strings.Contains(dailySpendRec.Body.String(), needle) {
			t.Fatalf("GET /daily-spend body missing %q", needle)
		}
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

func TestWebMuxEventsEndpointStreamsDailySpendPatchWithValidSSELines(t *testing.T) {
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

	reader := bufio.NewReader(resp.Body)
	state.rebuildSnapshot()

	frame := readSSEFrameContaining(t, reader, "events daily spend patch", "data: selector #daily-spend-region")
	if !strings.Contains(frame, "event: datastar-patch-elements") {
		t.Fatalf("/events daily spend frame missing datastar patch event: %q", frame)
	}
	if !strings.Contains(frame, "data: mode outer") {
		t.Fatalf("/events daily spend frame missing outer patch mode: %q", frame)
	}
	if got := strings.Count(frame, "data: selector #daily-spend-region"); got != 1 {
		t.Fatalf("/events daily spend selector count=%d, want=1", got)
	}
	for _, line := range strings.Split(frame, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "event: ") && !strings.HasPrefix(line, "data: ") {
			t.Fatalf("/events daily spend frame contains non-SSE line %q", line)
		}
	}
	for _, needle := range []string{
		`id="daily-spend-today-summary"`,
		`id="daily-spend-weekly-average"`,
		`class="daily-spend-top-panels"`,
		`class="daily-spend-trend-panels"`,
		`<figure id="daily-spend-token-trend" class="daily-spend-chart"><svg`,
		`<figure id="daily-spend-money-trend" class="daily-spend-chart"><svg`,
		`id="daily-spend-top-models-week-section"`,
	} {
		if !strings.Contains(frame, needle) {
			t.Fatalf("/events daily spend frame missing redesigned content %q", needle)
		}
	}
	for _, legacy := range []string{
		`<table id="daily-spend-token-trend"`,
		`<table id="daily-spend-money-trend"`,
	} {
		if strings.Contains(frame, legacy) {
			t.Fatalf("/events daily spend frame unexpectedly contains legacy trend table %q", legacy)
		}
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
	setTestPricing(t)

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
	if got := strings.Count(syncRec.Body.String(), "event: datastar-patch-elements"); got != 9 {
		t.Fatalf("POST /actions/sync-codespaces patch event count=%d, want=9", got)
	}
	for _, selector := range []string{
		"data: selector #overview-summary",
		"data: selector #sync-status-region",
		"data: selector #daily-token-chart-region",
		"data: selector #model-summary-region",
		"data: selector #project-summary-region",
		"data: selector #daily-totals-region",
		"data: selector #daily-spend-region",
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

func TestWebMuxProjectRowActionBehaviorAndValidation(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	state.snapshotMu.Lock()
	state.snapshot = sampleWebDashboardPayload()
	state.hasData = true
	state.snapshotMu.Unlock()
	mux := newWebMux(state)

	methodRec := httptest.NewRecorder()
	mux.ServeHTTP(methodRec, httptest.NewRequest(http.MethodGet, "/actions/project-row", nil))
	if methodRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /actions/project-row status=%d, want=%d", methodRec.Code, http.StatusMethodNotAllowed)
	}

	missingKeyRec := httptest.NewRecorder()
	mux.ServeHTTP(missingKeyRec, httptest.NewRequest(http.MethodPost, "/actions/project-row", nil))
	if missingKeyRec.Code != http.StatusBadRequest {
		t.Fatalf("POST /actions/project-row missing key status=%d, want=%d", missingKeyRec.Code, http.StatusBadRequest)
	}

	notFoundRec := httptest.NewRecorder()
	mux.ServeHTTP(notFoundRec, httptest.NewRequest(http.MethodPost, "/actions/project-row?row_key=missing&expand=true", nil))
	if notFoundRec.Code != http.StatusNotFound {
		t.Fatalf("POST /actions/project-row unknown key status=%d, want=%d", notFoundRec.Code, http.StatusNotFound)
	}

	rowKey := webStableRowKey("project", "token-consumption-copilot")
	expandRec := httptest.NewRecorder()
	mux.ServeHTTP(expandRec, httptest.NewRequest(http.MethodPost, "/actions/project-row?row_key="+rowKey+"&expand=true", nil))
	if expandRec.Code != http.StatusOK {
		t.Fatalf("POST /actions/project-row expand status=%d, want=%d", expandRec.Code, http.StatusOK)
	}
	if ct := expandRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("POST /actions/project-row expand content-type=%q", ct)
	}
	for _, needle := range []string{
		"event: datastar-patch-elements",
		"data: selector #" + webProjectSummaryRowID(rowKey),
		"data: selector #" + webProjectDetailRowID(rowKey),
		"project-model-row",
		`data-row-group="project"`,
		`data-expand-action="false"`,
		"/actions/project-row?row_key=" + rowKey + "&expand=false",
	} {
		if !strings.Contains(expandRec.Body.String(), needle) {
			t.Fatalf("POST /actions/project-row expand missing %q", needle)
		}
	}

	collapseRec := httptest.NewRecorder()
	mux.ServeHTTP(collapseRec, httptest.NewRequest(http.MethodPost, "/actions/project-row?row_key="+rowKey+"&expand=false", nil))
	if collapseRec.Code != http.StatusOK {
		t.Fatalf("POST /actions/project-row collapse status=%d, want=%d", collapseRec.Code, http.StatusOK)
	}
	if !strings.Contains(collapseRec.Body.String(), "/actions/project-row?row_key="+rowKey+"&expand=true") {
		t.Fatalf("POST /actions/project-row collapse missing next expand action")
	}
	if !strings.Contains(collapseRec.Body.String(), `data-expand-action="true"`) {
		t.Fatalf("POST /actions/project-row collapse missing data-expand-action=true")
	}
}

func TestWebMuxDayRowActionBehaviorAndValidation(t *testing.T) {
	state := newTestWebState(t, t.TempDir())
	state.snapshotMu.Lock()
	state.snapshot = sampleWebDashboardPayload()
	state.hasData = true
	state.snapshotMu.Unlock()
	mux := newWebMux(state)

	methodRec := httptest.NewRecorder()
	mux.ServeHTTP(methodRec, httptest.NewRequest(http.MethodGet, "/actions/day-row", nil))
	if methodRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /actions/day-row status=%d, want=%d", methodRec.Code, http.StatusMethodNotAllowed)
	}

	missingKeyRec := httptest.NewRecorder()
	mux.ServeHTTP(missingKeyRec, httptest.NewRequest(http.MethodPost, "/actions/day-row", nil))
	if missingKeyRec.Code != http.StatusBadRequest {
		t.Fatalf("POST /actions/day-row missing key status=%d, want=%d", missingKeyRec.Code, http.StatusBadRequest)
	}

	notFoundRec := httptest.NewRecorder()
	mux.ServeHTTP(notFoundRec, httptest.NewRequest(http.MethodPost, "/actions/day-row?row_key=missing&expand=true", nil))
	if notFoundRec.Code != http.StatusNotFound {
		t.Fatalf("POST /actions/day-row unknown key status=%d, want=%d", notFoundRec.Code, http.StatusNotFound)
	}

	rowKey := webStableRowKey("day", "2026-02-18")
	expandRec := httptest.NewRecorder()
	mux.ServeHTTP(expandRec, httptest.NewRequest(http.MethodPost, "/actions/day-row?row_key="+rowKey+"&expand=true", nil))
	if expandRec.Code != http.StatusOK {
		t.Fatalf("POST /actions/day-row expand status=%d, want=%d", expandRec.Code, http.StatusOK)
	}
	if ct := expandRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("POST /actions/day-row expand content-type=%q", ct)
	}
	for _, needle := range []string{
		"event: datastar-patch-elements",
		"data: selector #" + webDaySummaryRowID(rowKey),
		"data: selector #" + webDayDetailRowID(rowKey),
		"daily-model-row",
		`data-row-group="day"`,
		`data-expand-action="false"`,
		"/actions/day-row?row_key=" + rowKey + "&expand=false",
	} {
		if !strings.Contains(expandRec.Body.String(), needle) {
			t.Fatalf("POST /actions/day-row expand missing %q", needle)
		}
	}

	collapseRec := httptest.NewRecorder()
	mux.ServeHTTP(collapseRec, httptest.NewRequest(http.MethodPost, "/actions/day-row?row_key="+rowKey+"&expand=false", nil))
	if collapseRec.Code != http.StatusOK {
		t.Fatalf("POST /actions/day-row collapse status=%d, want=%d", collapseRec.Code, http.StatusOK)
	}
	if !strings.Contains(collapseRec.Body.String(), "/actions/day-row?row_key="+rowKey+"&expand=true") {
		t.Fatalf("POST /actions/day-row collapse missing next expand action")
	}
	if !strings.Contains(collapseRec.Body.String(), `data-expand-action="true"`) {
		t.Fatalf("POST /actions/day-row collapse missing data-expand-action=true")
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
			"gpt-5":           {APICalls: 7, UserTurns: 7, PremiumRequests: 7, Cost: 0.9, CostWithoutCache: 1.2, PremiumRequestCost: 0.28},
			"claude-sonnet-4": {APICalls: 5, UserTurns: 5, PremiumRequests: 5, Cost: 0.35, CostWithoutCache: 0.6, PremiumRequestCost: 0.2},
		},
		Projects: map[string]statsPayloadStats{
			"token-consumption-copilot": {APICalls: 12, UserTurns: 12, PremiumRequests: 12, Cost: 1.25, PremiumRequestCost: 0.48},
		},
		ProjectModels: map[string]map[string]statsPayloadStats{
			"token-consumption-copilot": {
				"gpt-5":           {APICalls: 7, UserTurns: 7, PremiumRequests: 7, Cost: 0.9, PremiumRequestCost: 0.28, PromptTokens: 5000, CompletionTokens: 2000},
				"claude-sonnet-4": {APICalls: 5, UserTurns: 5, PremiumRequests: 5, Cost: 0.35, PremiumRequestCost: 0.2, PromptTokens: 3000, CompletionTokens: 1000},
			},
		},
		Daily: map[string]map[string]interface{}{
			"2026-02-18": {
				"gpt-5":                     statsPayloadStats{APICalls: 4, UserTurns: 4, PremiumRequests: 4, PremiumRequestCost: 0.16, Cost: 0.5, PromptTokens: 2000, CompletionTokens: 800},
				"_total_cost":               0.5,
				"_total_cost_without_cache": 0.7,
			},
			"2026-02-19": {
				"claude-sonnet-4":           statsPayloadStats{APICalls: 8, UserTurns: 8, PremiumRequests: 8, PremiumRequestCost: 0.32, Cost: 0.75, PromptTokens: 6000, CompletionTokens: 2500},
				"_total_cost":               0.75,
				"_total_cost_without_cache": 1.1,
			},
		},
		Hourly: map[string]statsPayloadStats{
			"09": {APICalls: 5, PromptTokens: 2000, CompletionTokens: 500},
			"15": {APICalls: 8, PromptTokens: 1200, CompletionTokens: 800},
		},
	}
}

func TestBuildWebDailySpendDataShapesTodayRollingTrendsAndTopRows(t *testing.T) {
	payload := statsPayload{
		Projects: map[string]statsPayloadStats{
			"proj-alpha": {PremiumRequestCost: 0.65, Cost: 0.92},
			"proj-beta":  {PremiumRequestCost: 1.20, Cost: 1.60},
			"proj-gamma": {PremiumRequestCost: 0.05, Cost: 0.10},
			"proj-delta": {PremiumRequestCost: 0.60, Cost: 0.70},
		},
		ProjectModels: map[string]map[string]statsPayloadStats{
			"proj-alpha": {"model-a": {PremiumRequestCost: 1}},
			"proj-beta":  {"model-b": {PremiumRequestCost: 1}},
			"proj-gamma": {"model-c": {PremiumRequestCost: 1}},
			"proj-delta": {"model-d": {PremiumRequestCost: 1}},
		},
		Daily: map[string]map[string]interface{}{
			"2026-02-12": {
				"model-z": statsPayloadStats{
					APICalls:           99,
					PromptTokens:       999,
					CompletionTokens:   999,
					PremiumRequestCost: 9.99,
					Cost:               9.99,
					CostWithoutCache:   9.99,
				},
			},
			"2026-02-13": {
				"model-a": statsPayloadStats{APICalls: 1, UserTurns: 1, PromptTokens: 10, CompletionTokens: 1, PremiumRequestCost: 0.10, Cost: 0.20, CostWithoutCache: 0.30},
			},
			"2026-02-14": {
				"model-b": statsPayloadStats{APICalls: 2, UserTurns: 1, PromptTokens: 20, CompletionTokens: 2, PremiumRequestCost: 0.20, Cost: 0.30, CostWithoutCache: 0.40},
			},
			"2026-02-15": {
				"model-c": statsPayloadStats{APICalls: 3, UserTurns: 0, PromptTokens: 30, CompletionTokens: 3, PremiumRequestCost: 0.05, Cost: 0.10, CostWithoutCache: 0.15},
			},
			"2026-02-16": {
				"model-a": statsPayloadStats{APICalls: 4, UserTurns: 2, PromptTokens: 40, CompletionTokens: 4, PremiumRequestCost: 0.40, Cost: 0.50, CostWithoutCache: 0.60},
			},
			"2026-02-17": {
				"model-b": statsPayloadStats{APICalls: 5, UserTurns: 2, PromptTokens: 50, CompletionTokens: 5, PremiumRequestCost: 0.30, Cost: 0.40, CostWithoutCache: 0.45},
			},
			"2026-02-18": {
				"model-d": statsPayloadStats{APICalls: 6, UserTurns: 4, PromptTokens: 60, CompletionTokens: 6, PremiumRequestCost: 0.60, Cost: 0.70, CostWithoutCache: 0.80},
			},
			"2026-02-19": {
				"model-b": statsPayloadStats{APICalls: 7, UserTurns: 3, PromptTokens: 70, CompletionTokens: 7, PremiumRequestCost: 0.70, Cost: 0.90, CostWithoutCache: 1.10},
				"model-a": statsPayloadStats{APICalls: 1, UserTurns: 1, PromptTokens: 15, CompletionTokens: 5, PremiumRequestCost: 0.15, Cost: 0.22, CostWithoutCache: 0.30},
			},
		},
	}

	data := buildWebDailySpendData(payload, time.Date(2026, time.February, 19, 12, 0, 0, 0, time.UTC))
	if data.Day != "2026-02-19" {
		t.Fatalf("day=%q, want=2026-02-19", data.Day)
	}

	if data.Today.InputTokens != 85 || data.Today.OutputTokens != 12 {
		t.Fatalf("today tokens=%d/%d, want=85/12", data.Today.InputTokens, data.Today.OutputTokens)
	}
	assertFloatEqual(t, data.Today.PremiumSpend, 0.85)
	assertFloatEqual(t, data.Today.APISpend, 1.12)
	if math.Abs(data.Today.APISpend-1.40) < 1e-9 {
		t.Fatalf("today api spend incorrectly matched no-cache total: got=%v", data.Today.APISpend)
	}

	assertFloatEqual(t, data.Rolling7DayAverage.InputTokens, 295.0/7.0)
	assertFloatEqual(t, data.Rolling7DayAverage.OutputTokens, 33.0/7.0)
	assertFloatEqual(t, data.Rolling7DayAverage.PremiumSpend, 2.5/7.0)
	assertFloatEqual(t, data.Rolling7DayAverage.APISpend, 3.32/7.0)

	if len(data.TokenTrend) != 7 || len(data.MoneyTrend) != 7 {
		t.Fatalf("trend lengths=%d/%d, want=7/7", len(data.TokenTrend), len(data.MoneyTrend))
	}
	if data.TokenTrend[0].Day != "2026-02-13" || data.TokenTrend[6].Day != "2026-02-19" {
		t.Fatalf("token trend days=%q..%q, want=2026-02-13..2026-02-19", data.TokenTrend[0].Day, data.TokenTrend[6].Day)
	}
	assertFloatEqual(t, data.MoneyTrend[6].APISpend, 1.12)
	assertFloatEqual(t, data.MoneyTrend[6].PremiumSpend, 0.85)

	if len(data.TopModelsToday) != 2 {
		t.Fatalf("top models today len=%d, want=2", len(data.TopModelsToday))
	}
	if data.TopModelsToday[0].Name != "model-b" || data.TopModelsToday[1].Name != "model-a" {
		t.Fatalf("top models today order=%v, want=[model-b model-a]", []string{data.TopModelsToday[0].Name, data.TopModelsToday[1].Name})
	}
	if data.TopModelsToday[0].PromptCount != 3 || data.TopModelsToday[0].InputTokens != 70 || data.TopModelsToday[0].OutputTokens != 7 {
		t.Fatalf("top model today row=%+v", data.TopModelsToday[0])
	}
	assertFloatEqual(t, data.TopModelsToday[0].PremiumSpend, 0.70)
	assertFloatEqual(t, data.TopModelsToday[0].APISpend, 0.90)
	if math.Abs(data.TopModelsToday[0].APISpend-1.10) < 1e-9 {
		t.Fatalf("top model today api spend incorrectly matched no-cache value: got=%v", data.TopModelsToday[0].APISpend)
	}

	if len(data.TopModelsRolling7) != 3 {
		t.Fatalf("top models rolling len=%d, want=3", len(data.TopModelsRolling7))
	}
	if got := []string{data.TopModelsRolling7[0].Name, data.TopModelsRolling7[1].Name, data.TopModelsRolling7[2].Name}; got[0] != "model-b" || got[1] != "model-a" || got[2] != "model-d" {
		t.Fatalf("top models rolling order=%v, want=[model-b model-a model-d]", got)
	}

	if len(data.TopProjectsToday) != 2 {
		t.Fatalf("top projects today len=%d, want=2", len(data.TopProjectsToday))
	}
	if data.TopProjectsToday[0].Name != "proj-beta" || data.TopProjectsToday[1].Name != "proj-alpha" {
		t.Fatalf("top projects today order=%v, want=[proj-beta proj-alpha]", []string{data.TopProjectsToday[0].Name, data.TopProjectsToday[1].Name})
	}
	assertFloatEqual(t, data.TopProjectsToday[0].PremiumSpend, 0.70)
	assertFloatEqual(t, data.TopProjectsToday[0].APISpend, 0.90)
	if math.Abs(data.TopProjectsToday[0].APISpend-1.10) < 1e-9 {
		t.Fatalf("top project today api spend incorrectly matched no-cache value: got=%v", data.TopProjectsToday[0].APISpend)
	}
	if data.TopProjectsToday[0].PromptCount != 3 {
		t.Fatalf("top project today prompt_count=%d, want=3", data.TopProjectsToday[0].PromptCount)
	}

	if len(data.TopProjectsRolling7) != 3 {
		t.Fatalf("top projects rolling len=%d, want=3", len(data.TopProjectsRolling7))
	}
	if got := []string{data.TopProjectsRolling7[0].Name, data.TopProjectsRolling7[1].Name, data.TopProjectsRolling7[2].Name}; got[0] != "proj-beta" || got[1] != "proj-alpha" || got[2] != "proj-delta" {
		t.Fatalf("top projects rolling order=%v, want=[proj-beta proj-alpha proj-delta]", got)
	}
}

func TestBuildWebDailySpendDataUsesDateRangeEndForHistoricalWindows(t *testing.T) {
	dateRange := "2026-02-10 → 2026-02-12"
	payload := statsPayload{
		DateRange: &dateRange,
		Daily: map[string]map[string]interface{}{
			"2026-02-10": {
				"model-a": statsPayloadStats{PromptTokens: 10, CompletionTokens: 1, PremiumRequestCost: 0.10, Cost: 0.20},
			},
			"2026-02-11": {
				"model-a": statsPayloadStats{PromptTokens: 20, CompletionTokens: 2, PremiumRequestCost: 0.20, Cost: 0.40},
			},
			"2026-02-12": {
				"model-a": statsPayloadStats{PromptTokens: 30, CompletionTokens: 3, PremiumRequestCost: 0.30, Cost: 0.60},
			},
		},
	}

	data := buildWebDailySpendData(payload, time.Date(2026, time.February, 20, 12, 0, 0, 0, time.UTC))
	if data.Day != "2026-02-12" {
		t.Fatalf("day=%q, want=2026-02-12", data.Day)
	}
	if data.WindowDays != 3 {
		t.Fatalf("window_days=%d, want=3", data.WindowDays)
	}
	if len(data.TokenTrend) != 3 || data.TokenTrend[0].Day != "2026-02-10" || data.TokenTrend[2].Day != "2026-02-12" {
		t.Fatalf("token trend window=%v, want 2026-02-10..2026-02-12", data.TokenTrend)
	}
	if data.Today.InputTokens != 30 || data.Today.OutputTokens != 3 {
		t.Fatalf("today tokens=%d/%d, want=30/3", data.Today.InputTokens, data.Today.OutputTokens)
	}
	assertFloatEqual(t, data.Rolling7DayAverage.InputTokens, 60.0/3.0)
	assertFloatEqual(t, data.Rolling7DayAverage.OutputTokens, 6.0/3.0)
	assertFloatEqual(t, data.Rolling7DayAverage.PremiumSpend, 0.60/3.0)
	assertFloatEqual(t, data.Rolling7DayAverage.APISpend, 1.20/3.0)
}

func TestRenderWebDailySpendRegionUsesSelectedDayLabelsForHistoricalRange(t *testing.T) {
	dateRange := "2026-02-10 → 2026-02-12"
	payload := statsPayload{
		DateRange: &dateRange,
		Daily: map[string]map[string]interface{}{
			"2026-02-11": {
				"model-a": statsPayloadStats{PromptTokens: 10, CompletionTokens: 1, PremiumRequestCost: 0.10, Cost: 0.20},
			},
		},
	}

	body := renderWebDailySpendRegion(payload, true, time.Date(2026, time.February, 20, 12, 0, 0, 0, time.UTC))
	for _, needle := range []string{
		"<h2>Daily Spend</h2>",
		"Selected-day summary",
		"Rolling average (3 days including selected day)",
		"Top projects on selected day",
		"Top projects in rolling window",
		"Top models on selected day",
		"Top models in rolling window",
		"No usage recorded for the selected day.",
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("daily spend body missing %q", needle)
		}
	}
	if strings.Contains(body, "No usage recorded yet for today.") {
		t.Fatalf("daily spend body should not use today empty-state text for historical range")
	}
}

func TestWebDailySpendTopRowsRankByHybridScore(t *testing.T) {
	assertFloatEqual(t, webDailySpendHybridScore(10, 100, 50), 52)

	modelRows := webDailySpendTopRowsFromStatsMap(map[string]statsPayloadStats{
		"high-premium": {PremiumRequestCost: 10, Cost: 1, PromptTokens: 10, CompletionTokens: 0},
		"high-tokens":  {PremiumRequestCost: 1, Cost: 2, PromptTokens: 100, CompletionTokens: 0},
	})
	if len(modelRows) < 2 {
		t.Fatalf("model rows len=%d, want>=2", len(modelRows))
	}
	if modelRows[0].Name != "high-tokens" {
		t.Fatalf("top model=%q, want=%q", modelRows[0].Name, "high-tokens")
	}

	projectRows := webDailySpendTopProjectRows(
		statsPayload{
			ProjectModels: map[string]map[string]statsPayloadStats{
				"project-high-premium": {"model-a": {PremiumRequestCost: 1}},
				"project-high-tokens":  {"model-b": {PremiumRequestCost: 1}},
			},
		},
		map[string]statsPayloadStats{
			"model-a": {APICalls: 1, PremiumRequestCost: 10, Cost: 1, PromptTokens: 10, CompletionTokens: 0},
			"model-b": {APICalls: 1, PremiumRequestCost: 1, Cost: 2, PromptTokens: 100, CompletionTokens: 0},
		},
	)
	if len(projectRows) < 2 {
		t.Fatalf("project rows len=%d, want>=2", len(projectRows))
	}
	if projectRows[0].Name != "project-high-tokens" {
		t.Fatalf("top project=%q, want=%q", projectRows[0].Name, "project-high-tokens")
	}
}

func TestBuildWebDailySpendDataGraphHopperWeeklyVisibilityAndPromptCounts(t *testing.T) {
	payload := statsPayload{
		ProjectModels: map[string]map[string]statsPayloadStats{
			"proj-alpha":   {"model-a": {PremiumRequestCost: 1}},
			"graph-hopper": {"model-gh": {PremiumRequestCost: 1}},
			"proj-beta":    {"model-b": {PremiumRequestCost: 1}},
			"proj-ignored": {"model-z": {PremiumRequestCost: 1}},
		},
		Daily: map[string]map[string]interface{}{
			"2026-02-19": {
				"model-a":  statsPayloadStats{APICalls: 100, UserTurns: 9, PromptTokens: 0, CompletionTokens: 0, PremiumRequestCost: 0.4, Cost: 100},
				"model-gh": statsPayloadStats{APICalls: 20, UserTurns: 0, PromptTokens: 200, CompletionTokens: 0, PremiumRequestCost: 0.2, Cost: 10},
				"model-b":  statsPayloadStats{APICalls: 80, UserTurns: 7, PromptTokens: 0, CompletionTokens: 0, PremiumRequestCost: 0.3, Cost: 80},
				"model-z":  statsPayloadStats{APICalls: 70, UserTurns: 5, PromptTokens: 0, CompletionTokens: 0, PremiumRequestCost: 0.3, Cost: 70},
			},
		},
	}

	data := buildWebDailySpendData(payload, time.Date(2026, time.February, 19, 12, 0, 0, 0, time.UTC))
	if len(data.TopProjectsRolling7) != 3 {
		t.Fatalf("top projects rolling len=%d, want=3", len(data.TopProjectsRolling7))
	}
	if got := []string{data.TopProjectsRolling7[0].Name, data.TopProjectsRolling7[1].Name, data.TopProjectsRolling7[2].Name}; got[0] != "proj-alpha" || got[1] != "graph-hopper" || got[2] != "proj-beta" {
		t.Fatalf("top projects rolling order=%v, want=[proj-alpha graph-hopper proj-beta]", got)
	}
	if len(data.TopModelsRolling7) < 2 {
		t.Fatalf("top models rolling len=%d, want>=2", len(data.TopModelsRolling7))
	}
	if data.TopModelsRolling7[1].Name != "model-gh" {
		t.Fatalf("top models rolling second=%q, want=model-gh", data.TopModelsRolling7[1].Name)
	}
	if data.TopModelsRolling7[1].PromptCount != 0 {
		t.Fatalf("autopilot model prompt_count=%d, want=0", data.TopModelsRolling7[1].PromptCount)
	}
	if data.TopProjectsRolling7[1].PromptCount != 0 {
		t.Fatalf("graph-hopper prompt_count=%d, want=0", data.TopProjectsRolling7[1].PromptCount)
	}
}

func TestBuildWebDailySpendDataUsesProjectScopedDailyAggregation(t *testing.T) {
	payload := statsPayload{
		ProjectModels: map[string]map[string]statsPayloadStats{
			"MainVault": {"model-x": {PremiumRequestCost: 99}},
			"Scratch":   {"model-x": {PremiumRequestCost: 1}},
		},
		Daily: map[string]map[string]interface{}{
			"2026-02-18": {
				"model-x": statsPayloadStats{APICalls: 1, UserTurns: 1, PromptTokens: 100, CompletionTokens: 0, PremiumRequestCost: 0.10, Cost: 1.00},
			},
			"2026-02-19": {
				"model-x": statsPayloadStats{APICalls: 1, UserTurns: 1, PromptTokens: 1000, CompletionTokens: 0, PremiumRequestCost: 1.00, Cost: 10.00},
			},
		},
		DailyProjects: map[string]map[string]statsPayloadStats{
			"2026-02-18": {
				"MainVault": {APICalls: 1, UserTurns: 1, PromptTokens: 100, CompletionTokens: 0, PremiumRequestCost: 0.10, Cost: 1.00},
			},
			"2026-02-19": {
				"Scratch": {APICalls: 1, UserTurns: 1, PromptTokens: 1000, CompletionTokens: 0, PremiumRequestCost: 1.00, Cost: 10.00},
			},
		},
	}

	data := buildWebDailySpendData(payload, time.Date(2026, time.February, 19, 12, 0, 0, 0, time.UTC))
	if len(data.TopProjectsToday) != 1 {
		t.Fatalf("top projects today len=%d, want=1", len(data.TopProjectsToday))
	}
	if data.TopProjectsToday[0].Name != "Scratch" {
		t.Fatalf("top project today=%q, want=Scratch", data.TopProjectsToday[0].Name)
	}
	if data.TopProjectsToday[0].InputTokens != 1000 || data.TopProjectsToday[0].PromptCount != 1 {
		t.Fatalf("top project today row=%+v", data.TopProjectsToday[0])
	}
	assertFloatEqual(t, data.TopProjectsToday[0].APISpend, 10.00)
	assertFloatEqual(t, data.TopProjectsToday[0].PremiumSpend, 1.00)

	if len(data.TopProjectsRolling7) != 2 {
		t.Fatalf("top projects rolling len=%d, want=2", len(data.TopProjectsRolling7))
	}
	if got := []string{data.TopProjectsRolling7[0].Name, data.TopProjectsRolling7[1].Name}; got[0] != "Scratch" || got[1] != "MainVault" {
		t.Fatalf("top projects rolling order=%v, want=[Scratch MainVault]", got)
	}
}

func TestDashboardShellHTMLRendersOverviewTables(t *testing.T) {
	payload := sampleWebDashboardPayload()
	projectKey := webStableRowKey("project", "token-consumption-copilot")
	dayKey := webStableRowKey("day", "2026-02-18")

	body := dashboardShellHTML(payload, true)
	expected := []string{
		"data-init=\"@get('/events')\"",
		"data-on:datastar-fetch=",
		"datastar.js",
		"id=\"refresh-indicators-region\"",
		`<a href="/daily-spend">View today's spend</a>`,
		"id=\"overview-summary\"",
		"id=\"sync-status-region\"",
		"sync-status-compact",
		"id=\"sync-status-table\"",
		"id=\"model-summary-region\"",
		"Per-model summary",
		"id=\"model-summary-table\"",
		"<th>Model</th>",
		"<th>API%</th>",
		"gpt-5",
		"id=\"project-summary-region\"",
		"Per-project summary",
		"id=\"project-summary-table\"",
		"<th>Project</th>",
		"token-consumption-copilot",
		`id="` + webProjectSummaryRowID(projectKey) + `"`,
		`data-row-group="project"`,
		`data-expand-action="true"`,
		"/actions/project-row?row_key=" + projectKey + "&expand=true",
		"id=\"daily-totals-region\"",
		"Daily totals",
		"id=\"daily-totals-table\"",
		`id="daily-token-chart-region"`,
		"<th>Date</th>",
		"<th>Total</th>",
		"2026-02-18",
		`id="` + webDaySummaryRowID(dayKey) + `"`,
		`data-row-group="day"`,
		"/actions/day-row?row_key=" + dayKey + "&expand=true",
		"copilot-token-cost:web:expanded-project-rows",
		"copilot-token-cost:web:expanded-day-rows",
		"tr.expandable-row[data-row-group][data-row-key][data-expand-action]",
		"/actions/project-row",
		"/actions/day-row",
		"&expand=true",
		"model-indent",
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
	if strings.Contains(body, `id="hourly-usage-table"`) {
		t.Fatalf("dashboard body unexpectedly contains legacy hourly table")
	}
	for _, needle := range []string{`id="hourly-heatmap"`, `id="hourly-heatmap-grid"`, "data-hourly-heatmap"} {
		if strings.Contains(body, needle) {
			t.Fatalf("dashboard body unexpectedly contains hourly heatmap hook %q", needle)
		}
	}
	if strings.Contains(body, "data-signals:__p") || strings.Contains(body, "data-signals:__d") {
		t.Fatalf("dashboard body unexpectedly contains ephemeral row toggle signals")
	}
	if strings.Contains(body, "data-show=\"$__") {
		t.Fatalf("dashboard body unexpectedly contains signal-based row visibility toggles")
	}
}

func TestRenderWebDailySpendTokenTrendTableUsesDistinctDualAxisScales(t *testing.T) {
	body := renderWebDailySpendTokenTrendTable("daily-spend-token-trend", []webDailySpendTokenTrendPoint{
		{Day: "2026-02-18", InputTokens: 3000, OutputTokens: 30},
		{Day: "2026-02-19", InputTokens: 1500, OutputTokens: 15},
	})
	for _, needle := range []string{
		`<figure id="daily-spend-token-trend" class="daily-spend-chart"><svg`,
		"Input tokens (left axis)",
		"Output tokens (right axis)",
		`<text x="4" y="15.00" font-size="10" fill="#6b7280">3.0K</text>`,
		`<text x="4" y="105.00" font-size="10" fill="#6b7280">1.5K</text>`,
		`<text x="636.00" y="15.00" font-size="10" fill="#6b7280" text-anchor="end">30</text>`,
		`<text x="636.00" y="105.00" font-size="10" fill="#6b7280" text-anchor="end">15</text>`,
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("token trend chart missing dual-axis marker %q", needle)
		}
	}
	for _, wrongScale := range []string{
		`<text x="636.00" y="15.00" font-size="10" fill="#6b7280" text-anchor="end">3.0K</text>`,
		`<text x="636.00" y="105.00" font-size="10" fill="#6b7280" text-anchor="end">1.5K</text>`,
	} {
		if strings.Contains(body, wrongScale) {
			t.Fatalf("token trend chart unexpectedly reused left-axis scale on right axis %q", wrongScale)
		}
	}
}

func TestRenderWebDailySpendMoneyTrendTableRemainsSingleAxis(t *testing.T) {
	body := renderWebDailySpendMoneyTrendTable("daily-spend-money-trend", []webDailySpendMoneyTrendPoint{
		{Day: "2026-02-18", PremiumSpend: 0.75, APISpend: 1.25},
		{Day: "2026-02-19", PremiumSpend: 0.25, APISpend: 0.50},
	})
	for _, needle := range []string{
		`<figure id="daily-spend-money-trend" class="daily-spend-chart"><svg`,
		"Premium spend",
		"API spend",
		`<text x="4" y="15.00" font-size="10" fill="#6b7280">$1.25</text>`,
		`<text x="4" y="105.00" font-size="10" fill="#6b7280">$0.625</text>`,
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("money trend chart missing expected single-axis content %q", needle)
		}
	}
	for _, unexpected := range []string{
		"Input tokens (left axis)",
		"Output tokens (right axis)",
		`text-anchor="end">$`,
	} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("money trend chart unexpectedly changed %q", unexpected)
		}
	}
}

func TestDailySpendShellHTMLRendersCoreSections(t *testing.T) {
	payload := sampleWebDashboardPayload()
	body := dailySpendShellHTML(payload, true, time.Date(2026, time.February, 19, 12, 0, 0, 0, time.UTC))
	for _, needle := range []string{
		"Today's Copilot Spend",
		`id="daily-spend-region"`,
		`id="daily-spend-tokens"`,
		`id="daily-spend-output-tokens"`,
		`id="daily-spend-cost"`,
		`id="daily-spend-api-spend"`,
		`id="daily-spend-weekly-average"`,
		`id="daily-spend-weekly-input-tokens"`,
		`id="daily-spend-weekly-output-tokens"`,
		`id="daily-spend-weekly-premium-spend"`,
		`id="daily-spend-weekly-api-spend"`,
		`class="daily-spend-top-panels"`,
		`id="daily-spend-token-trend"`,
		`id="daily-spend-money-trend"`,
		`id="daily-spend-top-projects-today"`,
		`id="daily-spend-top-projects-week"`,
		`id="daily-spend-top-models-today"`,
		`id="daily-spend-top-models-week"`,
		"Today summary",
		"Weekly average (rolling 7 days including today)",
		"Token trend",
		"Input tokens (left axis)",
		"Output tokens (right axis)",
		"Money trend",
		"Top projects today",
		"Top projects this week",
		"Top models today",
		"Top models this week",
		"6.0K",
		"2.5K",
		"$0.320",
		"$0.750",
		"1.1K",
		"471",
		"$0.069",
		"$0.179",
		"gpt-5",
		"token-consumption-copilot",
		"<th>Name</th><th>Input tokens</th><th>Output tokens</th><th>Premium spend</th><th>API spend</th><th>Prompt count</th>",
		"<tr><td>token-consumption-copilot</td><td>6.0K</td><td>2.5K</td><td>$0.320</td><td>$0.750</td><td>8</td></tr>",
		"<tr><td>gpt-5</td><td>2.0K</td><td>800</td><td>$0.160</td><td>$0.500</td><td>4</td></tr>",
		`data-init="@get('/events')"`,
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("daily spend body missing %q", needle)
		}
	}
	if got := strings.Count(body, `class="daily-spend-metrics"`); got != 2 {
		t.Fatalf("daily spend metric grid count=%d, want=2", got)
	}
	if got := strings.Count(body, `<article class="daily-spend-metric">`); got != 8 {
		t.Fatalf("daily spend metric card count=%d, want=8", got)
	}
	for _, needle := range []string{
		`.daily-spend-top-panels { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 1rem; }`,
		`.daily-spend-metrics { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0.75rem; }`,
		`.daily-spend-metric { border: 1px solid #d1d5db; border-radius: 0.6rem; padding: 0.8rem; background: #f9fafb; min-height: 6.2rem;`,
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("daily spend body missing compact card layout CSS %q", needle)
		}
	}
	for _, needle := range []string{
		`<figure id="daily-spend-token-trend" class="daily-spend-chart"><svg`,
		`<figure id="daily-spend-money-trend" class="daily-spend-chart"><svg`,
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("daily spend body missing chart markup %q", needle)
		}
	}
	for _, legacy := range []string{
		`<table id="daily-spend-token-trend"`,
		`<table id="daily-spend-money-trend"`,
	} {
		if strings.Contains(body, legacy) {
			t.Fatalf("daily spend body unexpectedly contains legacy trend table %q", legacy)
		}
	}
	orderedTodayCards := []string{
		`id="daily-spend-tokens"`,
		`id="daily-spend-output-tokens"`,
		`id="daily-spend-cost"`,
		`id="daily-spend-api-spend"`,
	}
	last := -1
	for _, cardID := range orderedTodayCards {
		idx := strings.Index(body, cardID)
		if idx == -1 {
			t.Fatalf("daily spend body missing today card %q", cardID)
		}
		if idx < last {
			t.Fatalf("today cards order incorrect at %q", cardID)
		}
		last = idx
	}
	orderedWeeklyCards := []string{
		`id="daily-spend-weekly-input-tokens"`,
		`id="daily-spend-weekly-output-tokens"`,
		`id="daily-spend-weekly-premium-spend"`,
		`id="daily-spend-weekly-api-spend"`,
	}
	last = -1
	for _, cardID := range orderedWeeklyCards {
		idx := strings.Index(body, cardID)
		if idx == -1 {
			t.Fatalf("daily spend body missing weekly card %q", cardID)
		}
		if idx < last {
			t.Fatalf("weekly cards order incorrect at %q", cardID)
		}
		last = idx
	}
	orderedSections := []string{
		`id="daily-spend-today-summary"`,
		`id="daily-spend-weekly-average"`,
		`id="daily-spend-token-trend-section"`,
		`id="daily-spend-money-trend-section"`,
		`id="daily-spend-top-projects-today-section"`,
		`id="daily-spend-top-projects-week-section"`,
		`id="daily-spend-top-models-today-section"`,
		`id="daily-spend-top-models-week-section"`,
	}
	last = -1
	for _, sectionID := range orderedSections {
		idx := strings.Index(body, sectionID)
		if idx == -1 {
			t.Fatalf("daily spend body missing section %q", sectionID)
		}
		if idx < last {
			t.Fatalf("daily spend section order incorrect at %q", sectionID)
		}
		last = idx
	}
	if strings.Contains(body, `id="daily-spend-gamification"`) {
		t.Fatalf("daily spend body unexpectedly contains gamification layout")
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
	patch, err := buildRefreshPatch(sampleWebDashboardPayload(), time.Date(2026, time.February, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("buildRefreshPatch: %v", err)
	}
	if got := strings.Count(patch, "event: datastar-patch-elements"); got != 8 {
		t.Fatalf("patch event count=%d, want=8", got)
	}
	if got := strings.Count(patch, "data: mode outer"); got != 8 {
		t.Fatalf("outer mode count=%d, want=8", got)
	}
	orderedSelectors := []string{
		"data: selector #overview-summary",
		"data: selector #sync-status-region",
		"data: selector #daily-token-chart-region",
		"data: selector #model-summary-region",
		"data: selector #project-summary-region",
		"data: selector #daily-totals-region",
		"data: selector #daily-spend-region",
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
		`id="daily-spend-region"`,
		"&#34;period&#34;",
	} {
		if !strings.Contains(patch, needle) {
			t.Fatalf("patch missing fragment content %q", needle)
		}
	}
	if strings.Contains(patch, "No-Cache") {
		t.Fatalf("patch unexpectedly contains No-Cache column")
	}
	if strings.Contains(patch, "hourly-heatmap") || strings.Contains(patch, "data-hourly-heatmap") {
		t.Fatalf("patch unexpectedly contains hourly heatmap hooks")
	}
}

func TestBuildRefreshPatchDailySpendFrameUsesValidSSELines(t *testing.T) {
	patch, err := buildRefreshPatch(sampleWebDashboardPayload(), time.Date(2026, time.February, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("buildRefreshPatch: %v", err)
	}
	var dailySpendFrame string
	for _, frame := range strings.Split(patch, "\n\n") {
		if strings.Contains(frame, "data: selector #daily-spend-region") {
			dailySpendFrame = frame
			break
		}
	}
	if dailySpendFrame == "" {
		t.Fatalf("patch missing daily spend frame")
	}
	for _, line := range strings.Split(dailySpendFrame, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "event: ") && !strings.HasPrefix(line, "data: ") {
			t.Fatalf("daily spend frame contains non-SSE line %q", line)
		}
	}
	for _, needle := range []string{
		`class="daily-spend-top-panels"`,
		`id="daily-spend-weekly-average"`,
		`id="daily-spend-money-trend-section"`,
		`id="daily-spend-top-projects-week-section"`,
		`id="daily-spend-cost"`,
		`id="daily-spend-weekly-input-tokens"`,
		`id="daily-spend-weekly-api-spend"`,
		`id="daily-spend-today-summary"`,
		`id="daily-spend-top-models-week-section"`,
		`class="daily-spend-trend-panels"`,
		`<figure id="daily-spend-token-trend" class="daily-spend-chart"><svg`,
		`<figure id="daily-spend-money-trend" class="daily-spend-chart"><svg`,
		`<tr><td>token-consumption-copilot</td><td>8.0K</td><td>3.3K</td><td>$0.480</td><td>$1.25</td><td>12</td></tr>`,
		`<tr><td>claude-sonnet-4</td><td>6.0K</td><td>2.5K</td><td>$0.320</td><td>$0.750</td><td>8</td></tr>`,
	} {
		if !strings.Contains(dailySpendFrame, needle) {
			t.Fatalf("daily spend frame missing redesigned content %q", needle)
		}
	}
	for _, legacy := range []string{
		`<table id="daily-spend-token-trend"`,
		`<table id="daily-spend-money-trend"`,
	} {
		if strings.Contains(dailySpendFrame, legacy) {
			t.Fatalf("daily spend frame unexpectedly contains legacy trend table %q", legacy)
		}
	}
}
