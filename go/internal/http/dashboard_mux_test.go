package httplayer

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	webstack "copilot-token-cost/internal/web"
)

func TestEventsStreamSendsInitialSnapshotPatch(t *testing.T) {
	updates := make(chan string)
	defer close(updates)

	runtime := &webstack.DashboardRuntime{
		SubscribeFn: func() (<-chan string, func()) {
			return updates, func() {}
		},
		SnapshotFn: func() (interface{}, bool) {
			return "snapshot", true
		},
		BuildDashboardPatchFn: func(snapshot interface{}, now time.Time) (string, error) {
			if snapshot != "snapshot" {
				t.Fatalf("snapshot=%v", snapshot)
			}
			return "event: datastar-patch-elements\ndata: selector #overview-summary\ndata: mode outer\ndata: elements <p>fresh</p>\n\n", nil
		},
	}

	server := httptest.NewServer(NewDashboardMux(runtime, DashboardMuxOptions{
		HeartbeatInterval: time.Hour,
		IndicatorInterval: time.Hour,
		Logf:              t.Logf,
	}))
	defer server.Close()

	resp, err := http.Get(server.URL + "/events")
	if err != nil {
		t.Fatalf("GET /events failed: %v", err)
	}
	defer resp.Body.Close()

	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("status=%d, want=%d", got, want)
	}

	reader := bufio.NewReader(resp.Body)
	patch, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading initial event line failed: %v", err)
	}
	if !strings.Contains(patch, "event: datastar-patch-elements") {
		t.Fatalf("expected initial dashboard patch, got %q", patch)
	}

	var body strings.Builder
	body.WriteString(patch)
	for !strings.Contains(body.String(), "\n\n") {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading initial patch body failed: %v", err)
		}
		body.WriteString(line)
	}
	if !strings.Contains(body.String(), "selector #overview-summary") {
		t.Fatalf("expected overview patch in initial event, got %q", body.String())
	}
}
