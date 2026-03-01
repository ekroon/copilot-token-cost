package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func createTestSQLDB(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	db, err := sql.Open("sqlite", f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec("CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT, value REAL)")
	db.Exec("INSERT INTO items (name, value) VALUES ('alpha', 1.5), ('beta', NULL)")
	return f.Name()
}

func TestRunSQLReadonly(t *testing.T) {
	dbPath := createTestSQLDB(t)
	db, _ := sql.Open("sqlite", dbPath)
	defer db.Close()
	db.Exec("PRAGMA query_only = ON")

	_, err := db.Exec("INSERT INTO items (name, value) VALUES ('hack', 0)")
	if err == nil {
		t.Fatal("expected readonly error, got nil")
	}
	if !strings.Contains(err.Error(), "readonly") && !strings.Contains(err.Error(), "attempt to write") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrintSQLTable(t *testing.T) {
	dbPath := createTestSQLDB(t)
	db, _ := sql.Open("sqlite", dbPath)
	defer db.Close()

	rows, _ := db.Query("SELECT id, name, value FROM items ORDER BY id")
	defer rows.Close()
	cols, _ := rows.Columns()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	printSQLTable(cols, rows)
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	out := buf.String()

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (header + 2 rows), got %d: %q", len(lines), out)
	}
	if lines[0] != "id\tname\tvalue" {
		t.Fatalf("unexpected header: %q", lines[0])
	}
	if !strings.Contains(lines[1], "alpha") {
		t.Fatalf("expected alpha in row 1: %q", lines[1])
	}
	if !strings.Contains(lines[2], "NULL") {
		t.Fatalf("expected NULL for beta's value: %q", lines[2])
	}
}

func TestPrintSQLJSON(t *testing.T) {
	dbPath := createTestSQLDB(t)
	db, _ := sql.Open("sqlite", dbPath)
	defer db.Close()

	rows, _ := db.Query("SELECT id, name, value FROM items ORDER BY id")
	defer rows.Close()
	cols, _ := rows.Columns()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	printSQLJSON(cols, rows)
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)

	var result []map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
	if result[0]["name"] != "alpha" {
		t.Fatalf("expected alpha, got %v", result[0]["name"])
	}
	if result[1]["value"] != nil {
		t.Fatalf("expected nil for beta's value, got %v", result[1]["value"])
	}
}

func TestPrintSQLJSONEmpty(t *testing.T) {
	dbPath := createTestSQLDB(t)
	db, _ := sql.Open("sqlite", dbPath)
	defer db.Close()

	rows, _ := db.Query("SELECT id, name FROM items WHERE id = -1")
	defer rows.Close()
	cols, _ := rows.Columns()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	printSQLJSON(cols, rows)
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)

	var result []map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty array, got %d items", len(result))
	}
}
