package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func exportJSONL(db *sql.DB, outputPath string) {
	f, err := os.Create(outputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating export file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	apiCols := apiCallColumns(db, "main")
	promptTextExpr := "NULL"
	if apiCols["prompt_text"] {
		promptTextExpr = "prompt_text"
	}
	rows, _ := db.Query("SELECT model, model_normalized, prompt_tokens, completion_tokens, " +
		"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
		"timestamp, session_id, log_file, source, " + promptTextExpr + " FROM api_calls")
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var model, modelNorm, source string
			var pt, ct, cct, crt, isUT int
			var ts, sid, lf, promptText sql.NullString
			rows.Scan(&model, &modelNorm, &pt, &ct, &cct, &crt, &isUT, &ts, &sid, &lf, &source, &promptText)
			promptTextValue := interface{}(nil)
			if promptText.Valid {
				promptTextValue = promptText.String
			}
			rec := map[string]interface{}{
				"type": "api_call", "model": model, "model_normalized": modelNorm,
				"prompt_tokens": pt, "completion_tokens": ct,
				"cache_creation_tokens": cct, "cache_read_tokens": crt,
				"is_user_turn": isUT, "timestamp": ts.String,
				"session_id": sid.String, "log_file": lf.String, "source": source, "prompt_text": promptTextValue,
			}
			b, _ := json.Marshal(rec)
			w.Write(b)
			w.WriteByte('\n')
		}
	}

	rows2, _ := db.Query("SELECT session_id, cwd, source, branch FROM session_workspaces")
	if rows2 != nil {
		defer rows2.Close()
		for rows2.Next() {
			var sid, cwd, source string
			var branch sql.NullString
			rows2.Scan(&sid, &cwd, &source, &branch)
			branchValue := interface{}(nil)
			if branch.Valid {
				branchValue = branch.String
			}
			rec := map[string]interface{}{
				"type": "session_workspace", "session_id": sid, "cwd": cwd, "source": source, "branch": branchValue,
			}
			b, _ := json.Marshal(rec)
			w.Write(b)
			w.WriteByte('\n')
		}
	}
}

func importJSONL(db *sql.DB, inputPath, sourceOverride string) int {
	f, err := os.Open(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening import file: %v\n", err)
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	apiCols := apiCallColumns(db, "main")
	hasPromptText := apiCols["prompt_text"]
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		rtype, _ := obj["type"].(string)
		src := sourceOverride
		if src == "" {
			if s, ok := obj["source"].(string); ok && s != "" {
				src = s
			} else {
				src = "local"
			}
		}
		if rtype == "api_call" {
			isUT := 0
			if v, ok := obj["is_user_turn"].(float64); ok && v != 0 {
				isUT = 1
			}
			var promptText interface{}
			if v, ok := obj["prompt_text"].(string); ok {
				promptText = v
			}
			if hasPromptText {
				db.Exec("INSERT OR IGNORE INTO api_calls "+
					"(model, model_normalized, prompt_tokens, completion_tokens, "+
					"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
					"timestamp, session_id, log_file, source, prompt_text) "+
					"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
					obj["model"], obj["model_normalized"],
					int(obj["prompt_tokens"].(float64)), int(obj["completion_tokens"].(float64)),
					int(obj["cache_creation_tokens"].(float64)), int(obj["cache_read_tokens"].(float64)),
					isUT, obj["timestamp"], obj["session_id"], obj["log_file"], src, promptText)
			} else {
				db.Exec("INSERT OR IGNORE INTO api_calls "+
					"(model, model_normalized, prompt_tokens, completion_tokens, "+
					"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
					"timestamp, session_id, log_file, source) "+
					"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
					obj["model"], obj["model_normalized"],
					int(obj["prompt_tokens"].(float64)), int(obj["completion_tokens"].(float64)),
					int(obj["cache_creation_tokens"].(float64)), int(obj["cache_read_tokens"].(float64)),
					isUT, obj["timestamp"], obj["session_id"], obj["log_file"], src)
			}
		} else if rtype == "session_workspace" {
			var branch interface{}
			if b, ok := obj["branch"].(string); ok && strings.TrimSpace(b) != "" {
				branch = b
			}
			db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source, branch) VALUES (?, ?, ?, ?)",
				obj["session_id"], obj["cwd"], src, branch)
		}
		count++
	}
	return count
}

func importSQLiteDB(db *sql.DB, otherDBPath, sourceOverride string) int {
	db.Exec("ATTACH DATABASE ? AS import_db", otherDBPath)
	targetAPICallCols := apiCallColumns(db, "main")
	importAPICallCols := apiCallColumns(db, "import_db")
	importPromptTextExpr := "NULL"
	if importAPICallCols["prompt_text"] {
		importPromptTextExpr = "prompt_text"
	}
	importCols := sessionWorkspaceColumns(db, "import_db")
	sourceExpr := "'local'"
	if importCols["source"] {
		sourceExpr = "COALESCE(source, 'local')"
	}
	branchExpr := "NULL"
	if importCols["branch"] {
		branchExpr = "branch"
	}
	var count int64
	if sourceOverride != "" {
		if targetAPICallCols["prompt_text"] {
			db.Exec("INSERT OR IGNORE INTO api_calls "+
				"(model, model_normalized, prompt_tokens, completion_tokens, "+
				"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
				"timestamp, session_id, log_file, source, prompt_text) "+
				"SELECT model, model_normalized, prompt_tokens, completion_tokens, "+
				"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
				"timestamp, session_id, log_file, ?, "+importPromptTextExpr+" FROM import_db.api_calls", sourceOverride)
		} else {
			db.Exec("INSERT OR IGNORE INTO api_calls "+
				"(model, model_normalized, prompt_tokens, completion_tokens, "+
				"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
				"timestamp, session_id, log_file, source) "+
				"SELECT model, model_normalized, prompt_tokens, completion_tokens, "+
				"cache_creation_tokens, cache_read_tokens, is_user_turn, "+
				"timestamp, session_id, log_file, ? FROM import_db.api_calls", sourceOverride)
		}
		db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source, branch) "+
			"SELECT session_id, cwd, ?, "+branchExpr+" FROM import_db.session_workspaces", sourceOverride)
		db.Exec("INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) "+
			"SELECT log_file, mtime, ?, record_count, parsed_at FROM import_db.parsed_logs", sourceOverride)
	} else {
		if targetAPICallCols["prompt_text"] {
			db.Exec("INSERT OR IGNORE INTO api_calls " +
				"(model, model_normalized, prompt_tokens, completion_tokens, " +
				"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
				"timestamp, session_id, log_file, source, prompt_text) " +
				"SELECT model, model_normalized, prompt_tokens, completion_tokens, " +
				"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
				"timestamp, session_id, log_file, source, " + importPromptTextExpr + " FROM import_db.api_calls")
		} else {
			db.Exec("INSERT OR IGNORE INTO api_calls " +
				"(model, model_normalized, prompt_tokens, completion_tokens, " +
				"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
				"timestamp, session_id, log_file, source) " +
				"SELECT model, model_normalized, prompt_tokens, completion_tokens, " +
				"cache_creation_tokens, cache_read_tokens, is_user_turn, " +
				"timestamp, session_id, log_file, source FROM import_db.api_calls")
		}
		db.Exec("INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source, branch) " +
			"SELECT session_id, cwd, " + sourceExpr + ", " + branchExpr + " FROM import_db.session_workspaces")
		db.Exec("INSERT OR REPLACE INTO parsed_logs SELECT * FROM import_db.parsed_logs")
	}
	db.QueryRow("SELECT changes()").Scan(&count)
	db.Exec("DETACH DATABASE import_db")
	return int(count)
}
