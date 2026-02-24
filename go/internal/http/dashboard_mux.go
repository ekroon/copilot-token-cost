package httplayer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"copilot-token-cost/internal/web"
)

type DashboardMuxOptions struct {
	HeartbeatInterval time.Duration
	IndicatorInterval time.Duration
	Logf              func(format string, args ...interface{})
}

type actionErrorResponse struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

func NewDashboardMux(runtime *web.DashboardRuntime, opts DashboardMuxOptions) *http.ServeMux {
	mux := http.NewServeMux()
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, args ...interface{}) {
			fmt.Fprintf(os.Stderr, format, args...)
		}
	}
	heartbeatInterval := opts.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = 25 * time.Second
	}
	indicatorInterval := opts.IndicatorInterval
	if indicatorInterval <= 0 {
		indicatorInterval = time.Second
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if !handleMethod(w, r, http.MethodGet) {
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := w.Write([]byte(runtime.RenderHomePage(time.Now()))); err != nil {
			logf("failed to write / response: %v\n", err)
		}
	})
	mux.HandleFunc("/details", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodGet) {
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := w.Write([]byte(runtime.RenderDetailsPage(time.Now()))); err != nil {
			logf("failed to write /details response: %v\n", err)
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
		updates, unsubscribe := runtime.Subscribe()
		defer unsubscribe()

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		heartbeat := time.NewTicker(heartbeatInterval)
		defer heartbeat.Stop()
		indicators := time.NewTicker(indicatorInterval)
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
					logf("failed to write /events patch: %v\n", err)
					return
				}
				flusher.Flush()
			case t := <-heartbeat.C:
				if _, err := fmt.Fprintf(w, "event: heartbeat\ndata: %s\n\n", t.UTC().Format(time.RFC3339Nano)); err != nil {
					logf("failed to write /events heartbeat: %v\n", err)
					return
				}
				flusher.Flush()
			case t := <-indicators.C:
				patch := runtime.RefreshIndicatorsPatch(t)
				if _, err := w.Write([]byte(patch)); err != nil {
					logf("failed to write /events indicators patch: %v\n", err)
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
		payload, ok := runtime.Snapshot()
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
			logf("failed to write /healthz response: %v\n", err)
		}
	})
	mux.HandleFunc("/actions/project-row", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodPost) {
			return
		}
		patch, err := runtime.ProjectRowPatch(r.URL.Query().Get("row_key"), parseExpandAction(r.URL.Query().Get("expand")))
		if err != nil {
			writeActionError(w, err)
			return
		}
		writePatchResponse(w, patch, "failed to write /actions/project-row response: %v\n", logf)
	})
	mux.HandleFunc("/actions/day-row", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodPost) {
			return
		}
		patch, err := runtime.DayRowPatch(r.URL.Query().Get("row_key"), parseExpandAction(r.URL.Query().Get("expand")))
		if err != nil {
			writeActionError(w, err)
			return
		}
		writePatchResponse(w, patch, "failed to write /actions/day-row response: %v\n", logf)
	})
	mux.HandleFunc("/actions/sync-codespaces", func(w http.ResponseWriter, r *http.Request) {
		if !handleMethod(w, r, http.MethodPost) {
			return
		}
		patch, err := runtime.SyncCodespacesPatch(time.Now())
		if err != nil {
			writeActionError(w, err)
			return
		}
		writePatchResponse(w, patch, "failed to write /actions/sync-codespaces response: %v\n", logf)
	})
	return mux
}

func writePatchResponse(w http.ResponseWriter, patch, logFormat string, logf func(format string, args ...interface{})) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if _, err := w.Write([]byte(patch)); err != nil {
		logf(logFormat, err)
	}
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
	_, _ = w.Write(body)
}

func writeActionError(w http.ResponseWriter, actionErr *web.ActionError) {
	if actionErr == nil {
		writeJSONResponse(w, http.StatusInternalServerError, actionErrorResponse{
			Error:  "unknown action error",
			Reason: "unknown_error",
		})
		return
	}
	writeJSONResponse(w, actionErr.Status, actionErrorResponse{
		Error:  actionErr.Message,
		Reason: actionErr.Reason,
	})
}

func parseExpandAction(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
