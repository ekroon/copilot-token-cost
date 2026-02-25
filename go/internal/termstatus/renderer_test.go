package termstatus

import (
	"bytes"
	"strings"
	"testing"
)

func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "", want: ModeCompact},
		{in: "compact", want: ModeCompact},
		{in: "COMPACT", want: ModeCompact},
		{in: "verbose", want: ModeVerbose},
		{in: "errors", want: ModeErrors},
		{in: "unknown", want: ""},
	}
	for _, tc := range cases {
		if got := NormalizeMode(tc.in); got != tc.want {
			t.Fatalf("NormalizeMode(%q)=%q, want=%q", tc.in, got, tc.want)
		}
	}
}

func TestRendererCompactTTYUsesSingleStatusLine(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, ModeCompact, true)

	r.Progressf("scan")
	r.Progressf("sync")
	r.Infof("startup complete")

	log := out.String()
	if !strings.Contains(log, "\r\033[2Kscan") {
		t.Fatalf("compact tty output missing first progress write: %q", log)
	}
	if !strings.Contains(log, "\r\033[2Ksync") {
		t.Fatalf("compact tty output missing overwrite progress write: %q", log)
	}
	if !strings.Contains(log, "startup complete\n") {
		t.Fatalf("compact tty output missing info line: %q", log)
	}
}

func TestRendererErrorsModeSuppressesProgress(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, ModeErrors, false)

	r.Progressf("scan")
	r.Infof("listening")
	r.Errorf("copy failed")

	log := out.String()
	if strings.Contains(log, "scan") {
		t.Fatalf("errors mode should suppress progress: %q", log)
	}
	if !strings.Contains(log, "listening\n") || !strings.Contains(log, "copy failed\n") {
		t.Fatalf("errors mode should keep info/error lines: %q", log)
	}
}

func TestRendererCompactNonTTYDeduplicatesRapidProgress(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, ModeCompact, false)

	r.Progressf("same message")
	r.Progressf("same message")

	log := out.String()
	if strings.Count(log, "same message\n") != 1 {
		t.Fatalf("expected one deduplicated progress line, got: %q", log)
	}
}
