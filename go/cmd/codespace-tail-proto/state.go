package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	_ "modernc.org/sqlite"
)

const tailStateSchema = `
CREATE TABLE IF NOT EXISTS codespace_tail_offsets (
    source TEXT NOT NULL,
    log_file TEXT NOT NULL,
    last_offset INTEGER NOT NULL DEFAULT 0,
    last_size INTEGER NOT NULL DEFAULT 0,
    last_mtime TEXT,
    last_hash TEXT,
    connection_state TEXT NOT NULL DEFAULT 'disconnected',
    last_error TEXT,
    last_chunk_at TEXT,
    last_full_copy_at TEXT,
    last_defensive_recopy_at TEXT,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (source, log_file)
);
CREATE INDEX IF NOT EXISTS idx_codespace_tail_offsets_source ON codespace_tail_offsets(source);
`

type tailCheckpoint struct {
	Source                string
	LogFile               string
	LastOffset            int64
	LastSize              int64
	LastMtime             string
	LastHash              string
	ConnectionState       string
	LastError             string
	LastChunkAt           string
	LastFullCopyAt        string
	LastDefensiveRecopyAt string
	UpdatedAt             string
}

type tailStateStore struct {
	db *sql.DB
}

func defaultTailStateDBPath() string {
	xdgStateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if xdgStateHome == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			xdgStateHome = filepath.Join(home, ".local", "state")
		}
	}
	if xdgStateHome == "" {
		xdgStateHome = filepath.Join(".local", "state")
	}
	path := filepath.Join(xdgStateHome, "copilot-token-cost", "copilot-tokens.db")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	return path
}

func openTailStateStore(dbPath string) (*tailStateStore, error) {
	if strings.TrimSpace(dbPath) == "" {
		dbPath = defaultTailStateDBPath()
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(tailStateSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &tailStateStore{db: db}, nil
}

func (s *tailStateStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *tailStateStore) LoadBySource(source string) (map[string]tailCheckpoint, error) {
	rows, err := s.db.Query(
		`SELECT source,log_file,last_offset,last_size,last_mtime,last_hash,connection_state,last_error,last_chunk_at,last_full_copy_at,last_defensive_recopy_at,updated_at
		 FROM codespace_tail_offsets WHERE source = ?`,
		source,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]tailCheckpoint{}
	for rows.Next() {
		var cp tailCheckpoint
		var mtime, hash, state, lastErr, chunkAt, fullAt, defensiveAt sql.NullString
		if err := rows.Scan(&cp.Source, &cp.LogFile, &cp.LastOffset, &cp.LastSize, &mtime, &hash, &state, &lastErr, &chunkAt, &fullAt, &defensiveAt, &cp.UpdatedAt); err != nil {
			return nil, err
		}
		cp.LastMtime = mtime.String
		cp.LastHash = hash.String
		cp.ConnectionState = state.String
		cp.LastError = lastErr.String
		cp.LastChunkAt = chunkAt.String
		cp.LastFullCopyAt = fullAt.String
		cp.LastDefensiveRecopyAt = defensiveAt.String
		out[cp.LogFile] = cp
	}
	return out, nil
}

func (s *tailStateStore) Upsert(cp tailCheckpoint) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if strings.TrimSpace(cp.ConnectionState) == "" {
		cp.ConnectionState = "connected"
	}
	_, err := s.db.Exec(
		`INSERT INTO codespace_tail_offsets (
			source,log_file,last_offset,last_size,last_mtime,last_hash,connection_state,last_error,last_chunk_at,last_full_copy_at,last_defensive_recopy_at,updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source,log_file) DO UPDATE SET
			last_offset=excluded.last_offset,
			last_size=excluded.last_size,
			last_mtime=excluded.last_mtime,
			last_hash=excluded.last_hash,
			connection_state=excluded.connection_state,
			last_error=excluded.last_error,
			last_chunk_at=excluded.last_chunk_at,
			last_full_copy_at=excluded.last_full_copy_at,
			last_defensive_recopy_at=excluded.last_defensive_recopy_at,
			updated_at=excluded.updated_at`,
		cp.Source,
		cp.LogFile,
		cp.LastOffset,
		cp.LastSize,
		nullableString(cp.LastMtime),
		nullableString(cp.LastHash),
		cp.ConnectionState,
		nullableString(cp.LastError),
		nullableString(cp.LastChunkAt),
		nullableString(cp.LastFullCopyAt),
		nullableString(cp.LastDefensiveRecopyAt),
		now,
	)
	return err
}

func (s *tailStateStore) MarkDisconnected(source, logFile, reason string) error {
	cp := tailCheckpoint{
		Source:          source,
		LogFile:         logFile,
		ConnectionState: "disconnected",
		LastError:       strings.TrimSpace(reason),
	}
	return s.Upsert(cp)
}

func nullableString(value string) sql.NullString {
	v := strings.TrimSpace(value)
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func sampleHashFromReadAt(size int64, readAt func([]byte, int64) (int, error)) (string, error) {
	if size < 0 {
		size = 0
	}
	const sampleWindow int64 = 64 * 1024
	hasher := sha256.New()
	writeChunk := func(offset, length int64) error {
		if length <= 0 {
			return nil
		}
		buf := make([]byte, length)
		n, err := readAt(buf, offset)
		if err != nil && err != io.EOF {
			return err
		}
		if n < 0 || int64(n) > length {
			return fmt.Errorf("invalid read length %d", n)
		}
		if _, err := hasher.Write(buf[:n]); err != nil {
			return err
		}
		return nil
	}

	if size <= sampleWindow {
		if err := writeChunk(0, size); err != nil {
			return "", err
		}
	} else {
		if err := writeChunk(0, sampleWindow); err != nil {
			return "", err
		}
		if err := writeChunk(size-sampleWindow, sampleWindow); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func localSampleHash(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	hash, err := sampleHashFromReadAt(info.Size(), file.ReadAt)
	if err != nil {
		return "", 0, err
	}
	return hash, info.Size(), nil
}

func remoteSampleHash(client *sftp.Client, remotePath string, size int64) (string, error) {
	file, err := client.Open(remotePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return sampleHashFromReadAt(size, file.ReadAt)
}
