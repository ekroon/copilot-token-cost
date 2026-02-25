package syncservice

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"copilot-token-cost/internal/domain"
	"copilot-token-cost/internal/parsing"
	"copilot-token-cost/internal/storage"
)

type WorkspaceMeta struct {
	CWD    string
	Branch string
}

type CodespaceInfo struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	LastUsedAt string `json:"lastUsedAt"`
}

type CodespaceCopyResult struct {
	Idx        int
	Codespace  CodespaceInfo
	TmpDir     string
	LogsDir    string
	SessionDir string
	Copied     bool
}

type RuntimeDeps struct {
	ParseLogFileInRange   func(logPath string, minTimestamp string, maxTimestamp string) []domain.Record
	LoadSessionWorkspaces func(sessionDir string) map[string]WorkspaceMeta
	NormalizeModel        func(string) string
	PromptTextForStorage  func(*string) sql.NullString
	AddCommas             func(string) string
}

type Service struct {
	storage *storage.Service
	parser  *parsing.Service
	runtime RuntimeDeps
	logf    func(format string, args ...interface{})
}

func NewService(storageService *storage.Service, parser *parsing.Service) *Service {
	if parser == nil {
		parser = parsing.NewService()
	}
	return &Service{
		storage: storageService,
		parser:  parser,
		runtime: RuntimeDeps{
			NormalizeModel:       func(model string) string { return model },
			PromptTextForStorage: func(*string) sql.NullString { return sql.NullString{} },
			AddCommas:            func(s string) string { return s },
		},
		logf: func(format string, args ...interface{}) {
			fmt.Fprintf(os.Stderr, format, args...)
		},
	}
}

func (s *Service) SetRuntimeDeps(deps RuntimeDeps) {
	if deps.ParseLogFileInRange != nil {
		s.runtime.ParseLogFileInRange = deps.ParseLogFileInRange
	}
	if deps.LoadSessionWorkspaces != nil {
		s.runtime.LoadSessionWorkspaces = deps.LoadSessionWorkspaces
	}
	if deps.NormalizeModel != nil {
		s.runtime.NormalizeModel = deps.NormalizeModel
	}
	if deps.PromptTextForStorage != nil {
		s.runtime.PromptTextForStorage = deps.PromptTextForStorage
	}
	if deps.AddCommas != nil {
		s.runtime.AddCommas = deps.AddCommas
	}
}

func (s *Service) SetLogf(logf func(format string, args ...interface{})) {
	if logf == nil {
		s.logf = func(format string, args ...interface{}) {
			fmt.Fprintf(os.Stderr, format, args...)
		}
		return
	}
	s.logf = logf
}

func (s *Service) log(format string, args ...interface{}) {
	if s == nil {
		return
	}
	if s.logf == nil {
		fmt.Fprintf(os.Stderr, format, args...)
		return
	}
	s.logf(format, args...)
}

func (s *Service) Refresh(ctx context.Context) error {
	if s.storage == nil {
		return nil
	}
	return s.storage.Ping(ctx)
}

func (s *Service) ParseLogContent(content, source string) []domain.Record {
	return s.parser.ParseLogContent(content, source)
}

func (s *Service) SyncLogsToDB(logsDir, sessionDir string, force bool, source string, minTime, maxTime *time.Time) int {
	if s.storage == nil || s.runtime.ParseLogFileInRange == nil || s.runtime.LoadSessionWorkspaces == nil {
		return 0
	}

	existing := s.storage.CountAPICallsBySource(source)
	matches, _ := filepath.Glob(filepath.Join(logsDir, "process-*.log"))
	sort.Strings(matches)
	if len(matches) > 0 {
		s.log("  🔎 Scanning %d log files (%s)\n", len(matches), source)
	}

	if force {
		s.storage.DeleteParsedLogsBySource(source)
		s.log("  🔄 Force re-sync (%s): re-parsing %d log files (keeping %s existing records)\n", source, len(matches), s.runtime.AddCommas(strconv.Itoa(existing)))
	}

	totalInserted := 0
	parsedCount := 0
	minTimestamp := ""
	maxTimestamp := ""
	if minTime != nil {
		minTimestamp = minTime.Format("2006-01-02T15:04:05")
	}
	if maxTime != nil {
		maxTimestamp = maxTime.Format("2006-01-02T15:04:05")
	}

	parsedMtimeByFile := map[string]float64{}
	if !force {
		parsedMtimeByFile = s.storage.ParsedMtimeByFile(source)
	}

	var syncTx *storage.LogSyncTx
	for _, logPath := range matches {
		filename := filepath.Base(logPath)
		info, err := os.Lstat(logPath)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if !force {
			modTime := info.ModTime()
			if minTime != nil && modTime.Before(*minTime) {
				continue
			}
			if maxTime != nil && !modTime.Before(*maxTime) {
				continue
			}
		}
		mtime := float64(info.ModTime().UnixMilli()) / 1000.0
		if !force {
			if parsedMtime, ok := parsedMtimeByFile[filename]; ok && parsedMtime == mtime {
				continue
			}
		}
		if syncTx == nil {
			syncTx, err = s.storage.BeginLogSyncTx(source, s.runtime.NormalizeModel, s.runtime.PromptTextForStorage)
			if err != nil {
				return 0
			}
		}
		records := s.runtime.ParseLogFileInRange(logPath, minTimestamp, maxTimestamp)
		for _, r := range records {
			syncTx.InsertRecord(r)
		}
		syncTx.MarkLogParsed(filename, mtime, len(records))
		totalInserted += len(records)
		parsedCount++
		if force {
			s.log("  📄 [%d/%d] %s (%d records)\n", parsedCount, len(matches), filename, len(records))
		}
	}

	if syncTx != nil {
		if err := syncTx.Commit(); err != nil {
			syncTx.Rollback()
			return 0
		}
	}

	if parsedCount > 0 {
		workspaces := s.runtime.LoadSessionWorkspaces(sessionDir)
		for sessionID, meta := range workspaces {
			s.storage.UpsertSessionWorkspace(sessionID, meta.CWD, meta.Branch, source)
		}
	}

	if parsedCount > 0 {
		totalNow := s.storage.CountAPICallsBySource(source)
		newRecords := totalNow - existing
		s.log("  ✅ Synced %d log files (%s): %s new records (%s total)\n", parsedCount, source, s.runtime.AddCommas(strconv.Itoa(newRecords)), s.runtime.AddCommas(strconv.Itoa(totalNow)))
	}

	return totalInserted
}

func (s *Service) ListCodespaces(includeStopped bool) []CodespaceInfo {
	s.log("  🔄 Codespaces: listing...\n")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "cs", "list", "--json", "name,state,lastUsedAt", "--limit", "1000")
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			s.log("  ⚠️ Codespaces sync skipped: listing timed out\n")
			return nil
		}
		s.log("  ⚠️ Codespaces sync skipped: failed to list codespaces\n")
		return nil
	}
	var all []CodespaceInfo
	if err := json.Unmarshal(out, &all); err != nil {
		s.log("  ⚠️ Codespaces sync skipped: invalid JSON from gh cs list\n")
		return nil
	}
	allowed := map[string]bool{"Available": true}
	if includeStopped {
		allowed["Shutdown"] = true
	}
	var filtered []CodespaceInfo
	for _, cs := range all {
		if cs.Name != "" && allowed[cs.State] {
			filtered = append(filtered, cs)
		}
	}
	s.log("  📦 Codespaces: %d to sync\n", len(filtered))
	return filtered
}

func IsCodespaceStartThrottleError(msg string) bool {
	if msg == "" {
		return false
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "too many codespaces starting") ||
		(strings.Contains(lower, "http 400") && strings.Contains(lower, "codespaces"))
}

func CodespaceThrottleBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 4 {
		attempt = 4
	}
	return time.Duration(1<<attempt) * time.Second
}

func SummarizeSyncCommandStderr(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return ""
	}
	oneLine := strings.Join(strings.Fields(trimmed), " ")
	if len(oneLine) > 240 {
		return oneLine[:240] + "..."
	}
	return oneLine
}

func IsTarFileChangedWarning(err error, stderr string) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && strings.Contains(stderr, "file changed as we read it") {
		return true
	}
	return false
}

func FormatSshTarFailure(sshErr, tarErr error, sshStderr, tarStderr string) string {
	var parts []string
	if sshErr != nil {
		parts = append(parts, fmt.Sprintf("ssh error: %v", sshErr))
	}
	if tarErr != nil {
		parts = append(parts, fmt.Sprintf("extract error: %v", tarErr))
	}
	if msg := SummarizeSyncCommandStderr(sshStderr); msg != "" {
		parts = append(parts, "ssh stderr: "+msg)
	}
	if msg := SummarizeSyncCommandStderr(tarStderr); msg != "" {
		parts = append(parts, "extract stderr: "+msg)
	}
	return strings.Join(parts, "; ")
}

func collectHostAliases(config string) []string {
	var aliases []string
	for _, raw := range strings.Split(config, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(line), "host ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, alias := range fields[1:] {
			if strings.ContainsAny(alias, "*?") {
				continue
			}
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

func selectHostAlias(codespace string, aliases []string) (string, error) {
	if len(aliases) == 0 {
		return "", errors.New("no host aliases found in gh cs ssh --config output")
	}
	want := strings.ToLower(strings.TrimSpace(codespace))
	for _, alias := range aliases {
		if strings.EqualFold(alias, want) {
			return alias, nil
		}
	}
	for _, alias := range aliases {
		if strings.Contains(strings.ToLower(alias), want) {
			return alias, nil
		}
	}
	return "", fmt.Errorf("could not infer SSH host alias for codespace %q", codespace)
}

func sshControlPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory failed: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", errors.New("resolve home directory failed: empty home path")
	}
	sshDir := filepath.Join(home, ".ssh")
	if mkErr := os.MkdirAll(sshDir, 0o700); mkErr != nil {
		return "", mkErr
	}
	return filepath.Join(sshDir, "copilot-token-cost-%C"), nil
}

func buildCodespaceSSHTarCommand(ctx context.Context, codespace string) (*exec.Cmd, func(), error) {
	cfgCmd := exec.CommandContext(ctx, "gh", "cs", "ssh", "-c", codespace, "--config")
	cfgCmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	var cfgErr bytes.Buffer
	cfgCmd.Stderr = &cfgErr
	configText, err := cfgCmd.Output()
	if err != nil {
		detail := SummarizeSyncCommandStderr(cfgErr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, nil, fmt.Errorf("gh cs ssh --config failed: %s", detail)
	}
	alias, err := selectHostAlias(codespace, collectHostAliases(string(configText)))
	if err != nil {
		return nil, nil, err
	}
	configFile, err := os.CreateTemp("", "codespaces-ssh-config-*.tmp")
	if err != nil {
		return nil, nil, err
	}
	if _, err := configFile.Write(configText); err != nil {
		_ = configFile.Close()
		_ = os.Remove(configFile.Name())
		return nil, nil, err
	}
	if err := configFile.Close(); err != nil {
		_ = os.Remove(configFile.Name())
		return nil, nil, err
	}
	controlPath, err := sshControlPath()
	if err != nil {
		_ = os.Remove(configFile.Name())
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, "ssh",
		"-F", configFile.Name(),
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=15m",
		"-o", "ControlPath="+controlPath,
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		alias,
		"tar", "czf", "-", "-C", "/home/vscode", ".copilot/logs", ".copilot/session-state",
	)
	cleanup := func() {
		_ = os.Remove(configFile.Name())
	}
	return cmd, cleanup, nil
}

func (s *Service) CopyCodespaceData(cs CodespaceInfo, idx, total int, stoppedStartLimiter chan struct{}) CodespaceCopyResult {
	res := CodespaceCopyResult{
		Idx:       idx,
		Codespace: cs,
	}
	shouldStop := cs.State == "Shutdown"
	tmpDir, err := os.MkdirTemp("", "copilot-cs-")
	if err != nil {
		return res
	}
	res.TmpDir = tmpDir

	if shouldStop && stoppedStartLimiter != nil {
		stoppedStartLimiter <- struct{}{}
		defer func() { <-stoppedStartLimiter }()
	}

	if shouldStop {
		defer func() {
			stopStart := time.Now()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer stopCancel()
			stopCmd := exec.CommandContext(stopCtx, "gh", "cs", "stop", "-c", cs.Name)
			stopCmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
			_ = stopCmd.Run()
			s.log("  🛑 Stopping %s... (%.1fs)\n", cs.Name, time.Since(stopStart).Seconds())
		}()
	}

	stage := filepath.Join(tmpDir, cs.Name)
	_ = os.MkdirAll(stage, 0o755)
	s.log("  📦 [%d/%d] Copying %s...\n", idx+1, total, cs.Name)
	cpStart := time.Now()
	cpCtx, cpCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cpCancel()

	copied := false
	sshTarCmd, cleanupSSHConfig, sshBuildErr := buildCodespaceSSHTarCommand(cpCtx, cs.Name)
	if cleanupSSHConfig != nil {
		defer cleanupSSHConfig()
	}
	if sshBuildErr != nil {
		s.log("  ℹ️ SSH reuse unavailable for %s: %v; using gh cs ssh directly\n", cs.Name, sshBuildErr)
		sshTarCmd = exec.CommandContext(cpCtx, "gh", "cs", "ssh", "-c", cs.Name, "--",
			"tar", "czf", "-", "-C", "/home/vscode", ".copilot/logs", ".copilot/session-state")
		sshTarCmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	} else {
		s.log("  🔐 SSH reuse enabled for %s (ControlPersist=15m)\n", cs.Name)
	}
	var sshErrBuf bytes.Buffer
	sshTarCmd.Stderr = &sshErrBuf
	if pipe, pipeErr := sshTarCmd.StdoutPipe(); pipeErr == nil {
		tarExtract := exec.CommandContext(cpCtx, "tar", "xzf", "-", "-C", stage)
		tarExtract.Stdin = pipe
		var tarErrBuf bytes.Buffer
		tarExtract.Stderr = &tarErrBuf
		if sshErr := sshTarCmd.Start(); sshErr == nil {
			if tarErr := tarExtract.Start(); tarErr == nil {
				tarWaitErr := tarExtract.Wait()
				sshWaitErr := sshTarCmd.Wait()
				sshOK := sshWaitErr == nil || IsTarFileChangedWarning(sshWaitErr, sshErrBuf.String())
				if sshOK && tarWaitErr == nil {
					if sshWaitErr != nil {
						s.log("  ✅ Copied %s via ssh+tar (%.1fs) (tar warned: file changed as we read it)\n", cs.Name, time.Since(cpStart).Seconds())
					} else {
						s.log("  ✅ Copied %s via ssh+tar (%.1fs)\n", cs.Name, time.Since(cpStart).Seconds())
					}
					copied = true
				} else {
					detail := FormatSshTarFailure(sshWaitErr, tarWaitErr, sshErrBuf.String(), tarErrBuf.String())
					if detail != "" {
						s.log("  ⚠️ ssh+tar failed for %s (%.1fs): %s; falling back to gh cs cp\n", cs.Name, time.Since(cpStart).Seconds(), detail)
					} else {
						s.log("  ⚠️ ssh+tar failed for %s (%.1fs), falling back to gh cs cp\n", cs.Name, time.Since(cpStart).Seconds())
					}
				}
			}
		}
	}

	if !copied {
		const maxThrottleRetries = 3
		for attempt := 1; attempt <= maxThrottleRetries; attempt++ {
			cpStart = time.Now()
			cpCmd := exec.CommandContext(cpCtx, "gh", "cs", "cp", "-e", "-r", "-c", cs.Name, "remote:/home/vscode/.copilot", stage)
			cpCmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
			var cpErrBuf bytes.Buffer
			cpCmd.Stdout = io.Discard
			cpCmd.Stderr = &cpErrBuf
			cpErr := cpCmd.Run()
			if cpErr == nil {
				s.log("  ✅ Copied %s (%.1fs)\n", cs.Name, time.Since(cpStart).Seconds())
				copied = true
				break
			}
			if cpCtx.Err() == context.DeadlineExceeded {
				s.log("  ⚠️ Failed to copy %s: timed out after %.1fs\n", cs.Name, time.Since(cpStart).Seconds())
				return res
			}
			msg := strings.TrimSpace(cpErrBuf.String())
			if IsCodespaceStartThrottleError(msg) && attempt < maxThrottleRetries {
				wait := CodespaceThrottleBackoff(attempt)
				s.log("  ⏳ Start throttled for %s, retrying copy in %.0fs (%d/%d)\n", cs.Name, wait.Seconds(), attempt+1, maxThrottleRetries)
				select {
				case <-time.After(wait):
					continue
				case <-cpCtx.Done():
					s.log("  ⚠️ Failed to copy %s: timed out while waiting to retry\n", cs.Name)
					return res
				}
			}
			if strings.Contains(msg, "No such file or directory") {
				s.log("  ⚠️ Skipping %s: /home/vscode/.copilot not found\n", cs.Name)
			} else {
				if msg == "" {
					msg = "gh cs cp failed"
				}
				s.log("  ⚠️ Failed to copy %s: %s (%.1fs)\n", cs.Name, msg, time.Since(cpStart).Seconds())
			}
			return res
		}
		if !copied {
			return res
		}
	}

	copilotDir := filepath.Join(stage, ".copilot")
	if _, err := os.Stat(filepath.Join(copilotDir, "logs")); err != nil {
		alt := filepath.Join(stage, "home", "vscode", ".copilot")
		if _, altErr := os.Stat(filepath.Join(alt, "logs")); altErr == nil {
			copilotDir = alt
		}
	}
	logsDir := filepath.Join(copilotDir, "logs")
	sessionDir := filepath.Join(copilotDir, "session-state")
	if _, err := os.Stat(logsDir); err != nil {
		s.log("  ⚠️ Skipping %s: no .copilot/logs in copied data\n", cs.Name)
		return res
	}

	fileCount, totalBytes := DirStats(logsDir)
	s.log("  📊 %s: %d log files, %s copied\n", cs.Name, fileCount, HumanSize(totalBytes))

	res.LogsDir = logsDir
	res.SessionDir = sessionDir
	res.Copied = true
	return res
}

func HumanSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func DirStats(root string) (int, int64) {
	var count int
	var total int64
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil {
			count++
			total += info.Size()
		}
		return nil
	})
	return count, total
}

func (s *Service) ListRemoteLogFiles(csName string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "cs", "ssh", "-c", csName, "--",
		"ls", "/home/vscode/.copilot/logs/")
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.TrimSpace(line)
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

func (s *Service) KnownLogFiles(source string) map[string]bool {
	if s.storage == nil {
		return map[string]bool{}
	}
	return s.storage.KnownLogFiles(source)
}

func (s *Service) SyncCodespacesToDBTick(includeStopped bool, force bool) (int, error) {
	if s.storage == nil {
		return 0, fmt.Errorf("storage not configured")
	}
	codespaces := s.ListCodespaces(includeStopped)
	if codespaces == nil {
		return 0, fmt.Errorf("failed to list codespaces")
	}
	if len(codespaces) == 0 {
		return 0, nil
	}
	var pending []CodespaceInfo
	for _, cs := range codespaces {
		if cs.State != "Available" && cs.LastUsedAt != "" && s.storage.GetCodespaceLastUsed(cs.Name) == cs.LastUsedAt {
			s.log("  ⏭️  Skipping %s (shutdown, unchanged lastUsedAt)\n", cs.Name)
			continue
		}
		if !force && cs.State != "Available" {
			source := "codespace:" + cs.Name
			known := s.KnownLogFiles(source)
			if len(known) > 0 {
				remoteFiles, err := s.ListRemoteLogFiles(cs.Name)
				if err == nil && len(remoteFiles) > 0 {
					allKnown := true
					for _, f := range remoteFiles {
						if !known[f] {
							allKnown = false
							break
						}
					}
					if allKnown {
						s.log("  ⏭️  Skipping %s copy: all %d log files already synced\n", cs.Name, len(remoteFiles))
						continue
					}
				}
			}
		}
		pending = append(pending, cs)
	}
	if len(pending) == 0 {
		return 0, nil
	}

	var stoppedPending int
	for _, cs := range pending {
		if cs.State == "Shutdown" {
			stoppedPending++
		}
	}
	var stoppedStartLimiter chan struct{}
	if stoppedPending > 0 {
		stoppedStartLimiter = make(chan struct{}, 1)
		s.log("  🧯 Stopped Codespaces startup parallelism: 1 (%d stopped)\n", stoppedPending)
	}

	workers := 4
	s.log("  🚚 Codespaces copy parallelism: %d workers (%d pending)\n", workers, len(pending))
	jobs := make(chan int, len(pending))
	results := make(chan CodespaceCopyResult, len(pending))
	for w := 0; w < workers; w++ {
		go func() {
			for idx := range jobs {
				results <- s.CopyCodespaceData(pending[idx], idx, len(pending), stoppedStartLimiter)
			}
		}()
	}
	for i := 0; i < len(pending); i++ {
		jobs <- i
	}
	close(jobs)

	ordered := make([]CodespaceCopyResult, len(pending))
	for i := 0; i < len(pending); i++ {
		res := <-results
		ordered[res.Idx] = res
	}

	total := 0
	failedCopies := 0
	for _, res := range ordered {
		if res.Copied {
			syncForce := force || res.Codespace.State == "Available"
			total += s.SyncLogsToDB(res.LogsDir, res.SessionDir, syncForce, "codespace:"+res.Codespace.Name, nil, nil)
			s.storage.UpsertCodespaceSyncState(res.Codespace.Name, res.Codespace.LastUsedAt)
		} else {
			failedCopies++
		}
		if res.TmpDir != "" {
			_ = os.RemoveAll(res.TmpDir)
		}
	}
	if failedCopies > 0 {
		return total, fmt.Errorf("codespaces sync incomplete: %d of %d copies failed", failedCopies, len(pending))
	}
	return total, nil
}

func (s *Service) SyncCodespacesToDB(includeStopped bool, force bool) int {
	total, _ := s.SyncCodespacesToDBTick(includeStopped, force)
	return total
}
