package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTailStateStoreUpsertAndLoad(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := openTailStateStore(dbPath)
	if err != nil {
		t.Fatalf("openTailStateStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cp := tailCheckpoint{
		Source:                "codespace:demo",
		LogFile:               "/home/vscode/.copilot/logs/process-a.log",
		LastOffset:            123,
		LastSize:              123,
		LastHash:              "abc123",
		ConnectionState:       "connected",
		LastChunkAt:           "2026-01-01T00:00:00Z",
		LastFullCopyAt:        "2026-01-01T00:00:00Z",
		LastDefensiveRecopyAt: "2026-01-01T00:00:00Z",
	}
	if err := store.Upsert(cp); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	gotMap, err := store.LoadBySource("codespace:demo")
	if err != nil {
		t.Fatalf("LoadBySource: %v", err)
	}
	got, ok := gotMap[cp.LogFile]
	if !ok {
		t.Fatalf("missing checkpoint for %s", cp.LogFile)
	}
	if got.LastOffset != cp.LastOffset || got.LastHash != cp.LastHash {
		t.Fatalf("unexpected checkpoint: %+v", got)
	}
}

func TestSampleHashFromReadAtDetectsChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.log")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	hash1, _, err := localSampleHash(path)
	if err != nil {
		t.Fatalf("localSampleHash #1: %v", err)
	}
	if err := os.WriteFile(path, []byte("hello world changed\n"), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	hash2, _, err := localSampleHash(path)
	if err != nil {
		t.Fatalf("localSampleHash #2: %v", err)
	}
	if hash1 == hash2 {
		t.Fatalf("expected hash to change after content change: %s", hash1)
	}
}

func TestTailStateStoreMarkDisconnected(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := openTailStateStore(dbPath)
	if err != nil {
		t.Fatalf("openTailStateStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	source := "codespace:demo"
	logFile := "/home/vscode/.copilot/logs/process-a.log"
	if err := store.MarkDisconnected(source, logFile, "file_disappeared"); err != nil {
		t.Fatalf("MarkDisconnected: %v", err)
	}
	gotMap, err := store.LoadBySource(source)
	if err != nil {
		t.Fatalf("LoadBySource: %v", err)
	}
	got := gotMap[logFile]
	if got.ConnectionState != "disconnected" {
		t.Fatalf("connection_state=%q, want disconnected", got.ConnectionState)
	}
	if got.LastError != "file_disappeared" {
		t.Fatalf("last_error=%q, want file_disappeared", got.LastError)
	}
}
