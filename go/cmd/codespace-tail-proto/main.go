package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const logsRoot = "/home/vscode/.copilot/logs"

type sshTarget struct {
	Alias       string
	Host        string
	Port        string
	User        string
	Identity    string
	ProxyCmd    string
	ConfigFile  string
	SourceLabel string
}

type staticAddr string

func (a staticAddr) Network() string { return "stdio" }
func (a staticAddr) String() string  { return string(a) }

type commandConn struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	cmd    *exec.Cmd

	mu     sync.Mutex
	closed bool
}

func (c *commandConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *commandConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

func (c *commandConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	_ = c.stdin.Close()
	_ = c.stdout.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}

func (c *commandConn) LocalAddr() net.Addr              { return staticAddr("proxy-local") }
func (c *commandConn) RemoteAddr() net.Addr             { return staticAddr("proxy-remote") }
func (c *commandConn) SetDeadline(time.Time) error      { return nil }
func (c *commandConn) SetReadDeadline(time.Time) error  { return nil }
func (c *commandConn) SetWriteDeadline(time.Time) error { return nil }

func expandHome(path string) string {
	if path == "" || !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/"))
}

func runOutput(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s %v failed: %s", name, args, msg)
	}
	return out, nil
}

func collectHostAliases(config string) []string {
	var aliases []string
	sc := bufio.NewScanner(strings.NewReader(config))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
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

func selectHostAlias(codespace, override string, aliases []string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	want := strings.ToLower(strings.TrimSpace(codespace))
	if want == "" {
		return "", errors.New("codespace is required")
	}
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
	if len(aliases) == 0 {
		return "", errors.New("no host aliases found in gh cs ssh --config output")
	}
	return "", fmt.Errorf("could not infer SSH host alias for codespace %q; use --ssh-host (known aliases: %s)", codespace, strings.Join(aliases, ", "))
}

func parseSSHResolvedConfig(output string) map[string][]string {
	values := map[string][]string{}
	sc := bufio.NewScanner(strings.NewReader(output))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		key := strings.ToLower(parts[0])
		val := strings.Join(parts[1:], " ")
		values[key] = append(values[key], val)
	}
	return values
}

func firstValue(values map[string][]string, key string) string {
	candidates := values[strings.ToLower(key)]
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

func resolveTarget(codespace, sshHostOverride string) (*sshTarget, error) {
	configText, err := runOutput("gh", "cs", "ssh", "-c", codespace, "--config")
	if err != nil {
		return nil, err
	}
	aliases := collectHostAliases(string(configText))
	alias, err := selectHostAlias(codespace, sshHostOverride, aliases)
	if err != nil {
		return nil, err
	}

	tempFile, err := os.CreateTemp("", "codespaces-ssh-config-*.tmp")
	if err != nil {
		return nil, err
	}
	if _, err := tempFile.Write(configText); err != nil {
		_ = tempFile.Close()
		return nil, err
	}
	if err := tempFile.Close(); err != nil {
		return nil, err
	}

	resolvedRaw, err := runOutput("ssh", "-F", tempFile.Name(), "-G", alias)
	if err != nil {
		_ = os.Remove(tempFile.Name())
		return nil, err
	}
	values := parseSSHResolvedConfig(string(resolvedRaw))
	target := &sshTarget{
		Alias:       alias,
		Host:        firstValue(values, "hostname"),
		Port:        firstValue(values, "port"),
		User:        firstValue(values, "user"),
		Identity:    expandHome(firstValue(values, "identityfile")),
		ProxyCmd:    firstValue(values, "proxycommand"),
		ConfigFile:  tempFile.Name(),
		SourceLabel: "codespace:" + codespace,
	}
	if target.Host == "" || target.Port == "" || target.User == "" || target.Identity == "" {
		_ = os.Remove(tempFile.Name())
		return nil, fmt.Errorf("resolved ssh config missing required fields for alias %q", alias)
	}
	return target, nil
}

func readSigner(identityPath string) (ssh.Signer, error) {
	key, err := os.ReadFile(identityPath)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse private key %s failed: %w", identityPath, err)
	}
	return signer, nil
}

func expandProxyCommand(cmdText string, target *sshTarget) string {
	out := cmdText
	out = strings.ReplaceAll(out, "%h", target.Host)
	out = strings.ReplaceAll(out, "%p", target.Port)
	out = strings.ReplaceAll(out, "%r", target.User)
	return out
}

func dialViaProxy(target *sshTarget) (net.Conn, error) {
	expanded := expandProxyCommand(target.ProxyCmd, target)
	cmd := exec.Command("sh", "-c", expanded)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start proxy command failed: %w", err)
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		fmt.Fprintf(os.Stderr, "proxy command stderr: %s\n", msg)
	}
	return &commandConn{stdin: stdin, stdout: stdout, cmd: cmd}, nil
}

func dialTarget(target *sshTarget) (net.Conn, error) {
	if proxy := strings.TrimSpace(strings.ToLower(target.ProxyCmd)); proxy != "" && proxy != "none" {
		return dialViaProxy(target)
	}
	return net.DialTimeout("tcp", net.JoinHostPort(target.Host, target.Port), 20*time.Second)
}

func connectSSHAndSFTP(target *sshTarget) (*ssh.Client, *sftp.Client, error) {
	signer, err := readSigner(target.Identity)
	if err != nil {
		return nil, nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            target.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // prototype only
		Timeout:         20 * time.Second,
	}
	rawConn, err := dialTarget(target)
	if err != nil {
		return nil, nil, err
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(rawConn, net.JoinHostPort(target.Host, target.Port), cfg)
	if err != nil {
		_ = rawConn.Close()
		return nil, nil, err
	}
	sshClient := ssh.NewClient(clientConn, chans, reqs)
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, nil, err
	}
	return sshClient, sftpClient, nil
}

func isProcessLogFile(name string) bool {
	return strings.HasPrefix(name, "process") && strings.HasSuffix(name, ".log")
}

func listRemoteProcessLogs(client *sftp.Client) ([]string, error) {
	entries, err := client.ReadDir(logsRoot)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if isProcessLogFile(entry.Name()) {
			paths = append(paths, filepath.Join(logsRoot, entry.Name()))
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no process*.log files found in %s", logsRoot)
	}
	sort.Strings(paths)
	return paths, nil
}

func fullCopy(client *sftp.Client, remotePath, localPath string) (int64, error) {
	remoteFile, err := client.Open(remotePath)
	if err != nil {
		return 0, err
	}
	defer remoteFile.Close()
	localFile, err := os.Create(localPath)
	if err != nil {
		return 0, err
	}
	defer localFile.Close()
	return io.Copy(localFile, remoteFile)
}

type trackedTailFile struct {
	Source                string
	RemotePath            string
	LocalPath             string
	Offset                int64
	FullCopyPending       bool
	LastHash              string
	LastMtime             string
	LastError             string
	LastChunkAt           string
	LastFullCopyAt        string
	LastDefensiveRecopyAt string
	ConnectionState       string
}

func appendDelta(client *sftp.Client, remotePath, localPath string, offset int64) (int64, error) {
	remoteFile, err := client.Open(remotePath)
	if err != nil {
		return 0, err
	}
	defer remoteFile.Close()
	if _, err := remoteFile.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	localFile, err := os.OpenFile(localPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer localFile.Close()
	return io.Copy(localFile, remoteFile)
}

func deriveLocalPathForRemote(localPath, codespace, remotePath string, multiFile bool) string {
	if !multiFile {
		return deriveLocalPath(localPath, codespace, remotePath)
	}
	base := fmt.Sprintf("%s-%s", codespace, filepath.Base(remotePath))
	if strings.TrimSpace(localPath) == "" {
		return base
	}
	return filepath.Join(localPath, base)
}

func reconcileTrackedFiles(tracked map[string]*trackedTailFile, checkpoints map[string]tailCheckpoint, source string, discovered []string, codespace, localPath string, multiFile bool) (added, removed []string) {
	seen := make(map[string]bool, len(discovered))
	for _, remotePath := range discovered {
		seen[remotePath] = true
		if _, ok := tracked[remotePath]; ok {
			continue
		}
		cp := checkpoints[remotePath]
		tracked[remotePath] = &trackedTailFile{
			Source:                source,
			RemotePath:            remotePath,
			LocalPath:             deriveLocalPathForRemote(localPath, codespace, remotePath, multiFile),
			Offset:                cp.LastOffset,
			FullCopyPending:       true,
			LastHash:              cp.LastHash,
			LastMtime:             cp.LastMtime,
			LastError:             cp.LastError,
			LastChunkAt:           cp.LastChunkAt,
			LastFullCopyAt:        cp.LastFullCopyAt,
			LastDefensiveRecopyAt: cp.LastDefensiveRecopyAt,
			ConnectionState:       cp.ConnectionState,
		}
		added = append(added, remotePath)
	}
	for remotePath := range tracked {
		if seen[remotePath] {
			continue
		}
		delete(tracked, remotePath)
		removed = append(removed, remotePath)
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func syncTrackedFile(client *sftp.Client, file *trackedTailFile, store *tailStateStore) error {
	nowTS := time.Now().UTC().Format(time.RFC3339Nano)
	info, err := client.Stat(file.RemotePath)
	if err != nil {
		file.ConnectionState = "degraded"
		file.LastError = err.Error()
		_ = store.Upsert(tailCheckpoint{
			Source:          file.Source,
			LogFile:         file.RemotePath,
			LastOffset:      file.Offset,
			LastHash:        file.LastHash,
			ConnectionState: file.ConnectionState,
			LastError:       file.LastError,
		})
		return err
	}
	file.ConnectionState = "connected"
	file.LastError = ""
	remoteSize := info.Size()
	file.LastMtime = info.ModTime().UTC().Format(time.RFC3339Nano)
	fullCopied := false
	defensiveRecopy := false
	wroteBytes := int64(0)
	if file.FullCopyPending || remoteSize < file.Offset {
		size, err := fullCopy(client, file.RemotePath, file.LocalPath)
		if err != nil {
			return err
		}
		file.Offset = size
		file.FullCopyPending = false
		fullCopied = true
		file.LastFullCopyAt = nowTS
		fmt.Printf("full copy: %s -> %s (%d bytes)\n", file.RemotePath, file.LocalPath, size)
	} else if remoteSize > file.Offset {
		written, err := appendDelta(client, file.RemotePath, file.LocalPath, file.Offset)
		if err != nil {
			return err
		}
		wroteBytes = written
		file.Offset += written
		fmt.Printf("tail append: %s +%d bytes (offset=%d)\n", file.RemotePath, written, file.Offset)
	}

	localHash, localSize, localErr := localSampleHash(file.LocalPath)
	if localErr == nil {
		remoteHash, remoteErr := remoteSampleHash(client, file.RemotePath, file.Offset)
		if remoteErr == nil && localHash != "" && remoteHash != "" && localHash != remoteHash {
			resetSize, copyErr := fullCopy(client, file.RemotePath, file.LocalPath)
			if copyErr == nil {
				file.Offset = resetSize
				if h, _, hErr := localSampleHash(file.LocalPath); hErr == nil {
					localHash = h
				}
				defensiveRecopy = true
				file.LastDefensiveRecopyAt = nowTS
				file.LastFullCopyAt = nowTS
				fmt.Printf("defensive recopy: digest mismatch for %s, reset to %d bytes\n", file.RemotePath, resetSize)
			}
		}
	}
	if localErr == nil {
		file.LastHash = localHash
		if file.Offset == 0 {
			file.Offset = localSize
		}
	}
	if wroteBytes > 0 || fullCopied || defensiveRecopy {
		file.LastChunkAt = nowTS
	}

	_ = store.Upsert(tailCheckpoint{
		Source:                file.Source,
		LogFile:               file.RemotePath,
		LastOffset:            file.Offset,
		LastSize:              localSize,
		LastMtime:             file.LastMtime,
		LastHash:              file.LastHash,
		ConnectionState:       file.ConnectionState,
		LastError:             file.LastError,
		LastChunkAt:           file.LastChunkAt,
		LastFullCopyAt:        file.LastFullCopyAt,
		LastDefensiveRecopyAt: file.LastDefensiveRecopyAt,
	})
	return nil
}

func deriveLocalPath(localPath, codespace, remotePath string) string {
	if strings.TrimSpace(localPath) != "" {
		return localPath
	}
	base := filepath.Base(remotePath)
	return fmt.Sprintf("%s-%s", codespace, base)
}

func sortedTrackedKeys(tracked map[string]*trackedTailFile) []string {
	keys := make([]string, 0, len(tracked))
	for key := range tracked {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func runTailSession(client *sftp.Client, source, codespace, remoteFile, localPath string, interval time.Duration, until time.Time, tracked map[string]*trackedTailFile, forceFullCopy bool, store *tailStateStore, checkpoints map[string]tailCheckpoint) error {
	multiFile := strings.TrimSpace(remoteFile) == ""
	syncOnce := func() error {
		var discovered []string
		var err error
		if multiFile {
			discovered, err = listRemoteProcessLogs(client)
			if err != nil {
				return err
			}
		} else {
			discovered = []string{remoteFile}
		}
		added, removed := reconcileTrackedFiles(tracked, checkpoints, source, discovered, codespace, localPath, multiFile)
		for _, file := range added {
			fmt.Printf("discovered new log file: %s\n", file)
		}
		for _, file := range removed {
			fmt.Printf("log file disappeared, stopping sync: %s\n", file)
			_ = store.MarkDisconnected(source, file, "file_disappeared")
		}
		if forceFullCopy {
			for _, file := range tracked {
				file.FullCopyPending = true
			}
			forceFullCopy = false
		}
		for _, key := range sortedTrackedKeys(tracked) {
			if err := syncTrackedFile(client, tracked[key], store); err != nil {
				if os.IsNotExist(err) && multiFile {
					delete(tracked, key)
					fmt.Printf("log file disappeared during sync: %s\n", key)
					_ = store.MarkDisconnected(source, key, "file_disappeared")
					continue
				}
				return err
			}
		}
		return nil
	}

	if err := syncOnce(); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for time.Now().Before(until) {
		<-ticker.C
		if err := syncOnce(); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	var (
		codespace     = flag.String("codespace", "", "Codespace name")
		sshHost       = flag.String("ssh-host", "", "Explicit SSH host alias from gh cs ssh --config")
		remoteFile    = flag.String("remote-file", "", "Remote log file path (default: discover and tail all process*.log files)")
		localFile     = flag.String("local-file", "", "Single local mirror file path when --remote-file is set; otherwise local directory for mirrored files")
		stateDB       = flag.String("state-db", "", "SQLite state DB path (default: copilot-token-cost state DB)")
		pollInterval  = flag.Duration("poll-interval", 2*time.Second, "Polling interval for tail reads")
		runFor        = flag.Duration("run-for", 45*time.Second, "Prototype run duration")
		reconnectOnce = flag.Bool("reconnect-once", true, "Force one reconnect cycle (full recopy on reconnect)")
	)
	flag.Parse()

	if strings.TrimSpace(*codespace) == "" {
		fmt.Fprintln(os.Stderr, "--codespace is required")
		os.Exit(1)
	}
	if *pollInterval <= 0 {
		fmt.Fprintln(os.Stderr, "--poll-interval must be > 0")
		os.Exit(1)
	}
	if *runFor <= 0 {
		fmt.Fprintln(os.Stderr, "--run-for must be > 0")
		os.Exit(1)
	}

	target, err := resolveTarget(*codespace, *sshHost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve target failed: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(target.ConfigFile)

	fmt.Printf("resolved target alias=%s host=%s port=%s user=%s proxy=%t\n",
		target.Alias, target.Host, target.Port, target.User, strings.TrimSpace(strings.ToLower(target.ProxyCmd)) != "" && strings.TrimSpace(strings.ToLower(target.ProxyCmd)) != "none")

	source := "codespace:" + strings.TrimSpace(*codespace)
	store, err := openTailStateStore(*stateDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open state DB failed: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()
	checkpoints, err := store.LoadBySource(source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load state checkpoints failed: %v\n", err)
		os.Exit(1)
	}

	endTime := time.Now().Add(*runFor)
	reconnectAt := time.Time{}
	if *reconnectOnce {
		reconnectAt = time.Now().Add(*runFor / 2)
	}
	remoteOverride := strings.TrimSpace(*remoteFile)
	tracked := map[string]*trackedTailFile{}

	for {
		sshClient, sftpClient, err := connectSSHAndSFTP(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
			os.Exit(1)
		}

		sessionUntil := endTime
		if !reconnectAt.IsZero() && reconnectAt.Before(endTime) {
			sessionUntil = reconnectAt
		}
		err = runTailSession(sftpClient, source, *codespace, remoteOverride, *localFile, *pollInterval, sessionUntil, tracked, true, store, checkpoints)
		_ = sftpClient.Close()
		_ = sshClient.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "session error: %v\n", err)
			os.Exit(1)
		}

		if reconnectAt.IsZero() || time.Now().After(endTime) {
			break
		}
		fmt.Println("forcing reconnect cycle; next session starts with full recopy")
		reconnectAt = time.Time{}
	}

	fmt.Printf("prototype completed in %s\n", strconv.FormatFloat(runFor.Seconds(), 'f', 1, 64)+"s")
}
