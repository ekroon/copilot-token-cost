package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

type ActionError struct {
	Status  int
	Reason  string
	Message string
}

func (e *ActionError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

type DashboardRuntime struct {
	RenderHomePageFn            func(now time.Time) string
	RenderDetailsPageFn         func(now time.Time) string
	SubscribeFn                 func() (<-chan string, func())
	BuildRefreshIndicatorsPatch func(now time.Time) string
	SnapshotFn                  func() (interface{}, bool)
	SetRowExpandedFn            func(group, rowKey string, expanded bool)
	BuildProjectRowPatchFn      func(snapshot interface{}, rowKey string, expand bool) (string, error)
	BuildDayRowPatchFn          func(snapshot interface{}, rowKey string, expand bool) (string, error)
	SyncCodespacesFn            func() *ActionError
	BuildDashboardPatchFn       func(snapshot interface{}, now time.Time) (string, error)
}

func (r *DashboardRuntime) RenderHomePage(now time.Time) string {
	if r == nil || r.RenderHomePageFn == nil {
		return ""
	}
	return r.RenderHomePageFn(now)
}

func (r *DashboardRuntime) RenderDetailsPage(now time.Time) string {
	if r == nil || r.RenderDetailsPageFn == nil {
		return ""
	}
	return r.RenderDetailsPageFn(now)
}

func (r *DashboardRuntime) Subscribe() (<-chan string, func()) {
	if r == nil || r.SubscribeFn == nil {
		closed := make(chan string)
		close(closed)
		return closed, func() {}
	}
	return r.SubscribeFn()
}

func (r *DashboardRuntime) RefreshIndicatorsPatch(now time.Time) string {
	if r == nil || r.BuildRefreshIndicatorsPatch == nil {
		return ""
	}
	return r.BuildRefreshIndicatorsPatch(now)
}

func (r *DashboardRuntime) Snapshot() (interface{}, bool) {
	if r == nil || r.SnapshotFn == nil {
		return nil, false
	}
	return r.SnapshotFn()
}

func (r *DashboardRuntime) ProjectRowPatch(rowKey string, expand bool) (string, *ActionError) {
	rowKey = strings.TrimSpace(rowKey)
	if rowKey == "" {
		return "", &ActionError{
			Status:  http.StatusBadRequest,
			Reason:  "row_key_required",
			Message: "project row action failed: row_key is required",
		}
	}
	snapshot, ok := r.Snapshot()
	if !ok {
		return "", &ActionError{
			Status:  http.StatusServiceUnavailable,
			Reason:  "snapshot_unavailable",
			Message: "project row action failed: snapshot unavailable",
		}
	}
	if r.SetRowExpandedFn != nil {
		r.SetRowExpandedFn("project", rowKey, expand)
	}
	if r.BuildProjectRowPatchFn == nil {
		return "", &ActionError{
			Status:  http.StatusInternalServerError,
			Reason:  "project_row_patch_unavailable",
			Message: "project row action failed: patch builder unavailable",
		}
	}
	patch, err := r.BuildProjectRowPatchFn(snapshot, rowKey, expand)
	if err != nil {
		return "", &ActionError{
			Status:  http.StatusNotFound,
			Reason:  "project_row_not_found",
			Message: "project row action failed: unknown row_key",
		}
	}
	return patch, nil
}

func (r *DashboardRuntime) DayRowPatch(rowKey string, expand bool) (string, *ActionError) {
	rowKey = strings.TrimSpace(rowKey)
	if rowKey == "" {
		return "", &ActionError{
			Status:  http.StatusBadRequest,
			Reason:  "row_key_required",
			Message: "day row action failed: row_key is required",
		}
	}
	snapshot, ok := r.Snapshot()
	if !ok {
		return "", &ActionError{
			Status:  http.StatusServiceUnavailable,
			Reason:  "snapshot_unavailable",
			Message: "day row action failed: snapshot unavailable",
		}
	}
	if r.SetRowExpandedFn != nil {
		r.SetRowExpandedFn("day", rowKey, expand)
	}
	if r.BuildDayRowPatchFn == nil {
		return "", &ActionError{
			Status:  http.StatusInternalServerError,
			Reason:  "day_row_patch_unavailable",
			Message: "day row action failed: patch builder unavailable",
		}
	}
	patch, err := r.BuildDayRowPatchFn(snapshot, rowKey, expand)
	if err != nil {
		return "", &ActionError{
			Status:  http.StatusNotFound,
			Reason:  "day_row_not_found",
			Message: "day row action failed: unknown row_key",
		}
	}
	return patch, nil
}

func (r *DashboardRuntime) SyncCodespacesPatch(now time.Time) (string, *ActionError) {
	if r == nil || r.SyncCodespacesFn == nil {
		return "", &ActionError{
			Status:  http.StatusInternalServerError,
			Reason:  "codespaces_sync_failed",
			Message: "codespaces sync failed: sync function unavailable",
		}
	}
	if err := r.SyncCodespacesFn(); err != nil {
		return "", err
	}
	snapshot, ok := r.Snapshot()
	if !ok {
		return "", &ActionError{
			Status:  http.StatusInternalServerError,
			Reason:  "snapshot_unavailable",
			Message: "codespaces sync failed: snapshot unavailable",
		}
	}
	if r.BuildDashboardPatchFn == nil {
		return "", &ActionError{
			Status:  http.StatusInternalServerError,
			Reason:  "refresh_patch_failed",
			Message: "refresh patch build failed: patch builder unavailable",
		}
	}
	patch, err := r.BuildDashboardPatchFn(snapshot, now)
	if err != nil {
		return "", &ActionError{
			Status:  http.StatusInternalServerError,
			Reason:  "refresh_patch_failed",
			Message: fmt.Sprintf("%v", err),
		}
	}
	return patch, nil
}
