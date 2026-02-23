package main

import (
	"path/filepath"
	"testing"
)

func TestReconcileTrackedFilesHandlesAppearAndDisappear(t *testing.T) {
	tracked := map[string]*trackedTailFile{}
	codespace := "demo-space"
	localBase := "mirrors"

	a := "/home/vscode/.copilot/logs/process-a.log"
	b := "/home/vscode/.copilot/logs/process-b.log"
	source := "codespace:demo-space"
	checkpoints := map[string]tailCheckpoint{}

	added, removed := reconcileTrackedFiles(tracked, checkpoints, source, []string{a}, codespace, localBase, true)
	if len(added) != 1 || added[0] != a || len(removed) != 0 {
		t.Fatalf("first reconcile unexpected added=%v removed=%v", added, removed)
	}
	if got := tracked[a]; got == nil || !got.FullCopyPending {
		t.Fatalf("expected tracked file %s with FullCopyPending=true", a)
	}

	added, removed = reconcileTrackedFiles(tracked, checkpoints, source, []string{a, b}, codespace, localBase, true)
	if len(added) != 1 || added[0] != b || len(removed) != 0 {
		t.Fatalf("second reconcile unexpected added=%v removed=%v", added, removed)
	}

	added, removed = reconcileTrackedFiles(tracked, checkpoints, source, []string{b}, codespace, localBase, true)
	if len(added) != 0 || len(removed) != 1 || removed[0] != a {
		t.Fatalf("third reconcile unexpected added=%v removed=%v", added, removed)
	}
	if _, ok := tracked[a]; ok {
		t.Fatalf("expected %s to be removed from tracked map", a)
	}

	added, removed = reconcileTrackedFiles(tracked, checkpoints, source, []string{a, b}, codespace, localBase, true)
	if len(added) != 1 || added[0] != a || len(removed) != 0 {
		t.Fatalf("reappear reconcile unexpected added=%v removed=%v", added, removed)
	}
	if got := tracked[a]; got == nil || !got.FullCopyPending {
		t.Fatalf("expected reappeared file %s to require full copy", a)
	}
}

func TestDeriveLocalPathForRemoteMultiFileUsesDirectory(t *testing.T) {
	got := deriveLocalPathForRemote("mirrors", "demo-space", "/home/vscode/.copilot/logs/process-a.log", true)
	want := filepath.Join("mirrors", "demo-space-process-a.log")
	if got != want {
		t.Fatalf("deriveLocalPathForRemote()=%q, want %q", got, want)
	}
}

func TestIsProcessLogFile(t *testing.T) {
	if !isProcessLogFile("process-123.log") {
		t.Fatal("expected process-123.log to match")
	}
	if isProcessLogFile("telemetry.log") {
		t.Fatal("did not expect telemetry.log to match")
	}
}
