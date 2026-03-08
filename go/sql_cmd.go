package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func runSQL(args []string) {
	fs := flag.NewFlagSet("sql", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON array of objects")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: copilot-token-cost sql [--json] \"SQL QUERY\"")
		fmt.Fprintln(os.Stderr, "       echo \"SQL QUERY\" | copilot-token-cost sql [--json]")
		fmt.Fprintln(os.Stderr, "\nRun a read-only SQL query against the copilot-tokens database.")
		fmt.Fprintln(os.Stderr, "\nExamples:")
		fmt.Fprintln(os.Stderr, "  copilot-token-cost sql \"SELECT COUNT(*) FROM api_calls\"")
		fmt.Fprintln(os.Stderr, "  copilot-token-cost sql --json \"SELECT model_normalized, COUNT(*) as n FROM api_calls GROUP BY 1\"")
		fmt.Fprintln(os.Stderr, "  copilot-token-cost sql \"SELECT DISTINCT cwd FROM session_workspaces\"")
	}
	fs.Parse(args)

	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading stdin: %v\n", err)
			os.Exit(1)
		}
		query = strings.TrimSpace(string(b))
	}
	if query == "" {
		fs.Usage()
		os.Exit(1)
	}

	dbPath := getDBPath()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	db.Exec("PRAGMA query_only = ON")

	rows, err := db.Query(query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Query error: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	if *jsonOut {
		printSQLJSON(cols, rows)
	} else {
		printSQLTable(cols, rows)
	}
}
