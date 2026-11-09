package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ExitCodeAutoUpdate signals the watchdog supervisor (run.bat / run-web.bat / run.sh)
// to relaunch the newly compiled binary immediately (0-second delay).
const ExitCodeAutoUpdate = 42

// UpdateStatus represents the live and historical state of the AutoUpdater,
// exposed via GET /api/update, POST /api/update, and embedded in GET /api/status.
type UpdateStatus struct {
	Enabled            bool     `json:"enabled"`
	Branch             string   `json:"branch"`
	CurrentCommit      string   `json:"current_commit"`
	RemoteCommit       string   `json:"remote_commit,omitempty"`
	UpToDate           bool     `json:"up_to_date"`
	InProgress         bool     `json:"in_progress"`
	RestartPending     bool     `json:"restart_pending"`
	HandoffPending     bool     `json:"handoff_pending,omitempty"`
	LastCheckAt        string   `json:"last_check_at,omitempty"`
	LastUpdateAt       string   `json:"last_update_at,omitempty"`
	LastAppliedCommit  string   `json:"last_applied_commit,omitempty"`
	LastChangedFiles   []string `json:"last_changed_files,omitempty"`
	LastMode           string   `json:"last_mode,omitempty"` // "hot_reload" | "binary_handoff" | "none" | "failed"
	BuildDurationMS    int64    `json:"build_duration_ms,omitempty"`
	HandoffStartedAt   string   `json:"handoff_started_at,omitempty"`
	HandoffCompletedAt string   `json:"handoff_completed_at,omitempty"`
	HandoffDurationMS  int64    `json:"handoff_duration_ms,omitempty"`
	LastError          string   `json:"last_error,omitempty"`
	TotalHotReloads    int      `json:"total_hot_reloads"`
	TotalBinarySwaps   int      `json:"total_binary_swaps"`
}

// AutoUpdater manages zero-downtime Git polling, Plan A in-memory config hot-reload,
// and Plan B background binary compilation + atomic swap + process handoff.
type AutoUpdater struct {
	mu              sync.Mutex
	repoDir         string
	stateDir        string
	configPath      string
	branch          string
	pollInterval    time.Duration
	status          UpdateStatus
	log             *Logger
	onHotReload     func() error
	onBinaryHandoff func(newCommit string)
}

func NewAutoUpdater(
	repoDir, stateDir, configPath, branch string,
	pollInterval time.Duration,
	log *Logger,
	onHotReload func() error,
	onBinaryHandoff func(newCommit string),
) *AutoUpdater {
	if repoDir == "" {
		repoDir = "."
	}
	if abs, err := filepath.Abs(repoDir); err == nil {
		repoDir = abs
	}
	if stateDir == "" {
		stateDir = "state"
	}
	if !filepath.IsAbs(stateDir) {
		stateDir = filepath.Join(repoDir, stateDir)
	}
	if configPath == "" {
		configPath = "config.json"
	}
	if branch == "" {
		branch = "main"
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Minute
	}
	_ = os.MkdirAll(stateDir, 0o755)

	u := &AutoUpdater{
		repoDir:         repoDir,
		stateDir:        stateDir,
		configPath:      configPath,
		branch:          branch,
		pollInterval:    pollInterval,
		log:             log,
		onHotReload:     onHotReload,
		onBinaryHandoff: onBinaryHandoff,
		status: UpdateStatus{
			Enabled:  true,
			Branch:   branch,
			UpToDate: true,
		},
	}

	u.status.CurrentCommit = u.gitRevParse("HEAD")
	u.status.RemoteCommit = u.status.CurrentCommit

	// Measure total process handoff duration if we just restarted from a Plan B swap.
	statusFile := filepath.Join(stateDir, "last_update.json")
	if raw, err := os.ReadFile(statusFile); err == nil {
		var prev UpdateStatus
		if json.Unmarshal(raw, &prev) == nil {
			u.status.LastUpdateAt = prev.LastUpdateAt
			u.status.LastAppliedCommit = prev.LastAppliedCommit
			u.status.LastChangedFiles = prev.LastChangedFiles
			u.status.LastMode = prev.LastMode
			u.status.BuildDurationMS = prev.BuildDurationMS
			u.status.HandoffStartedAt = prev.HandoffStartedAt
			u.status.HandoffCompletedAt = prev.HandoffCompletedAt
			u.status.HandoffDurationMS = prev.HandoffDurationMS
			u.status.TotalHotReloads = prev.TotalHotReloads
			u.status.TotalBinarySwaps = prev.TotalBinarySwaps

			if prev.HandoffPending && prev.HandoffStartedAt != "" {
				if t0, err := time.Parse(time.RFC3339Nano, prev.HandoffStartedAt); err == nil {
					now := time.Now().UTC()
					u.status.HandoffCompletedAt = now.Format(time.RFC3339Nano)
					u.status.HandoffDurationMS = now.Sub(t0).Milliseconds()
					u.status.HandoffPending = false
					u.status.RestartPending = false
					u.status.InProgress = false
					if u.log != nil {
						u.log.Infof("[AUTO-UPDATE-COMPLETE] binary handoff to commit %s finished in %dms",
							u.status.CurrentCommit, u.status.HandoffDurationMS)
					}
					u.saveStatusLocked()
				}
			}
		}
	}
	return u
}

// writeFileAtomic writes data to a PID-stamped temp file, fsyncs, closes, and
// renames over path with Windows sharing-violation retry backoff.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	_ = f.Sync()
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		if renameErr = os.Rename(tmp, path); renameErr == nil {
			return nil
		}
		time.Sleep(time.Duration(15*(attempt+1)) * time.Millisecond)
	}
	_ = os.Remove(tmp)
	return renameErr
}

func (u *AutoUpdater) saveStatusLocked() {
	data, err := json.MarshalIndent(u.status, "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(filepath.Join(u.stateDir, "last_update.json"), data, 0o644)
}

// Status returns a thread-safe snapshot of the current UpdateStatus.
func (u *AutoUpdater) Status() UpdateStatus {
	if u == nil {
		return UpdateStatus{}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.status
	if len(u.status.LastChangedFiles) > 0 {
		st.LastChangedFiles = append([]string(nil), u.status.LastChangedFiles...)
	}
	return st
}

// StartLoop polls origin/<branch> in the background at u.pollInterval.
func (u *AutoUpdater) StartLoop(ctx context.Context) {
	if u == nil {
		return
	}
	ticker := time.NewTicker(u.pollInterval)
	defer ticker.Stop()

	select {
	case <-time.After(45 * time.Second):
		u.CheckAndApply(false)
	case <-ctx.Done():
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.CheckAndApply(false)
		}
	}
}

// CheckAndApply fetches origin/<branch>, inspects changed files, and executes
// either Plan A (0ms hot-reload) or Plan B (<1s binary hot-handoff).
func (u *AutoUpdater) CheckAndApply(force bool) UpdateStatus {
	u.mu.Lock()
	if u.status.InProgress || u.status.RestartPending {
		st := u.status
		u.mu.Unlock()
		return st
	}
	u.status.InProgress = true
	u.status.LastCheckAt = time.Now().UTC().Format(time.RFC3339)
	u.status.LastError = ""
	u.mu.Unlock()

	finishWithErr := func(msg string) UpdateStatus {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.status.InProgress = false
		u.status.LastError = msg
		u.status.LastMode = "failed"
		u.saveStatusLocked()
		if u.log != nil {
			u.log.Warnf("[AUTO-UPDATE] %s", msg)
		}
		return u.status
	}

	// 1. Fetch remote branch
	if out, err := u.runGit(60*time.Second, "fetch", "origin", u.branch, "--quiet"); err != nil {
		return finishWithErr(fmt.Sprintf("git fetch failed: %v (%s)", err, strings.TrimSpace(out)))
	}

	localSHA := u.gitRevParse("HEAD")
	remoteSHA := u.gitRevParse("origin/" + u.branch)
	if localSHA == "" || remoteSHA == "" {
		return finishWithErr("failed to resolve local or remote git commit SHA")
	}

	u.mu.Lock()
	u.status.CurrentCommit = localSHA
	u.status.RemoteCommit = remoteSHA
	u.mu.Unlock()

	// Already up to date and not forced
	if localSHA == remoteSHA && !force {
		u.mu.Lock()
		u.status.UpToDate = true
		u.status.InProgress = false
		if u.status.LastMode == "" {
			u.status.LastMode = "none"
		}
		st := u.status
		u.mu.Unlock()
		return st
	}

	// 2. Inspect changed files between HEAD and origin/<branch>
	diffOut, err := u.runGit(15*time.Second, "diff", "--name-only", "HEAD..origin/"+u.branch)
	if err != nil {
		return finishWithErr(fmt.Sprintf("git diff failed: %v", err))
	}

	var changedFiles []string
	needsBinaryRebuild := force
	for _, line := range strings.Split(strings.TrimSpace(diffOut), "\n") {
		f := strings.TrimSpace(line)
		if f == "" {
			continue
		}
		changedFiles = append(changedFiles, f)
		lower := strings.ToLower(f)
		// Go source, module manifests, and embedded webui.html require binary rebuild.
		if strings.HasSuffix(lower, ".go") ||
			strings.HasSuffix(lower, "go.mod") ||
			strings.HasSuffix(lower, "go.sum") ||
			strings.HasSuffix(lower, "webui.html") {
			needsBinaryRebuild = true
		}
	}

	if u.log != nil {
		u.log.Infof("[AUTO-UPDATE] update detected: %s -> %s (files=%d, rebuild=%v, force=%v)",
			localSHA, remoteSHA, len(changedFiles), needsBinaryRebuild, force)
	}

	// 3. Backup server-local config.json (contains live Telegram token, proxy credentials, etc.)
	cfgPath := u.configPath
	if !filepath.IsAbs(cfgPath) {
		cfgPath = filepath.Join(u.repoDir, cfgPath)
	}
	localCfgBackup, hadLocalCfg := readFileBytes(cfgPath)

	// 4. Advance working tree to origin/<branch>
	if out, err := u.runGit(20*time.Second, "reset", "--hard", "origin/"+u.branch); err != nil {
		if hadLocalCfg {
			_ = writeFileAtomic(cfgPath, localCfgBackup, 0o644)
		}
		return finishWithErr(fmt.Sprintf("git reset failed: %v (%s)", err, strings.TrimSpace(out)))
	}

	// 5. Restore server-local config.json so local runtime credentials are never clobbered
	if hadLocalCfg {
		_ = writeFileAtomic(cfgPath, localCfgBackup, 0o644)
	}

	// PLAN A: 0ms In-Memory Hot-Reload (only config/docs/scripts changed)
	if !needsBinaryRebuild {
		t0 := time.Now()
		if u.onHotReload != nil {
			if err := u.onHotReload(); err != nil {
				return finishWithErr(fmt.Sprintf("hot-reload failed: %v", err))
			}
		}
		dur := time.Since(t0).Milliseconds()
		u.mu.Lock()
		u.status.CurrentCommit = remoteSHA
		u.status.RemoteCommit = remoteSHA
		u.status.UpToDate = true
		u.status.InProgress = false
		u.status.LastUpdateAt = time.Now().UTC().Format(time.RFC3339)
		u.status.LastAppliedCommit = remoteSHA
		u.status.LastChangedFiles = changedFiles
		u.status.LastMode = "hot_reload"
		u.status.BuildDurationMS = dur
		u.status.TotalHotReloads++
		u.saveStatusLocked()
		st := u.status
		u.mu.Unlock()

		if u.log != nil {
			u.log.Infof("[AUTO-UPDATE] Plan A hot-reload complete (%s -> %s) in %dms (0ms downtime)",
				localSHA, remoteSHA, dur)
		}
		return st
	}

	// PLAN B: Background Compile + Smoke Test + Atomic Binary Swap + Sub-Second Handoff
	t0 := time.Now()
	curBin, newBin, oldBin := u.binaryNames()
	_ = os.Remove(newBin)

	if u.log != nil {
		u.log.Infof("[AUTO-UPDATE] Plan B: compiling %s in background (-p 2) while crawler continues running...", filepath.Base(newBin))
	}
	buildCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	buildCmd := exec.CommandContext(buildCtx, "go", "build", "-p", "2", "-o", newBin, ".")
	buildCmd.Dir = u.repoDir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		_ = os.Remove(newBin)
		return finishWithErr(fmt.Sprintf("go build failed: %v (%s)", err, strings.TrimSpace(string(out))))
	}
	buildDur := time.Since(t0).Milliseconds()

	// Pre-swap smoke test (-self-test)
	testCtx, testCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer testCancel()
	testCmd := exec.CommandContext(testCtx, newBin, "-self-test", "-config", cfgPath)
	testCmd.Dir = u.repoDir
	if out, err := testCmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "SELF-TEST OK") {
		_ = os.Remove(newBin)
		return finishWithErr(fmt.Sprintf("new binary self-test failed: %v (%s)", err, strings.TrimSpace(string(out))))
	}

	// Atomic binary swap:
	// Windows allows renaming a currently running .exe, freeing the original filename
	// for the newly compiled binary.
	_ = os.Remove(oldBin)
	hasOldBackup := false
	if _, err := os.Stat(curBin); err == nil {
		if err := os.Rename(curBin, oldBin); err != nil {
			_ = os.Remove(newBin)
			return finishWithErr(fmt.Sprintf("rename current binary to .old failed: %v", err))
		}
		hasOldBackup = true
	}
	if err := os.Rename(newBin, curBin); err != nil {
		if hasOldBackup {
			_ = os.Rename(oldBin, curBin)
		}
		return finishWithErr(fmt.Sprintf("rename .new binary to current failed: %v", err))
	}

	// Write update_restart marker for watchdog script
	restartMarker := filepath.Join(u.stateDir, "update_restart")
	_ = writeFileAtomic(restartMarker, []byte(remoteSHA+"\n"), 0o644)

	nowUTC := time.Now().UTC()
	u.mu.Lock()
	u.status.CurrentCommit = remoteSHA
	u.status.RemoteCommit = remoteSHA
	u.status.UpToDate = true
	u.status.InProgress = false
	u.status.RestartPending = true
	u.status.HandoffPending = true
	u.status.HandoffStartedAt = nowUTC.Format(time.RFC3339Nano)
	u.status.HandoffCompletedAt = ""
	u.status.HandoffDurationMS = 0
	u.status.LastUpdateAt = nowUTC.Format(time.RFC3339)
	u.status.LastAppliedCommit = remoteSHA
	u.status.LastChangedFiles = changedFiles
	u.status.LastMode = "binary_handoff"
	u.status.BuildDurationMS = buildDur
	u.status.TotalBinarySwaps++
	u.saveStatusLocked()
	st := u.status
	u.mu.Unlock()

	if u.log != nil {
		u.log.Infof("[AUTO-UPDATE] Plan B binary compiled & verified in %dms; triggering sub-second handoff to %s",
			buildDur, remoteSHA)
	}

	if u.onBinaryHandoff != nil {
		u.onBinaryHandoff(remoteSHA)
	}

	return st
}

func (u *AutoUpdater) binaryNames() (curBin, newBin, oldBin string) {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	base := u.repoDir
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	curBin = filepath.Join(base, "aqar-crawler"+ext)
	newBin = filepath.Join(base, "aqar-crawler.new"+ext)
	oldBin = filepath.Join(base, "aqar-crawler.old"+ext)
	return
}

func (u *AutoUpdater) gitRevParse(ref string) string {
	out, err := u.runGit(10*time.Second, "rev-parse", "--short", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (u *AutoUpdater) runGit(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = u.repoDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func readFileBytes(path string) ([]byte, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return b, true
}

// runSelfTest validates binary startup, configuration parsing, GraphQL templates,
// and AQZ1 preset-dictionary compression/decompression round-trip before swap.
func runSelfTest(configPath string) error {
	defCfg := DefaultConfig()
	if err := defCfg.validate(); err != nil {
		return fmt.Errorf("default config validation failed: %w", err)
	}
	if configPath != "" {
		if _, err := os.Stat(configPath); err == nil {
			cfg, err := LoadConfig(configPath)
			if err != nil {
				return fmt.Errorf("loading config %s: %w", configPath, err)
			}
			if err := cfg.validate(); err != nil {
				return fmt.Errorf("validating config %s: %w", configPath, err)
			}
		}
	}
	if !strings.Contains(countQuery, "Search") || !strings.Contains(findQuery, "Search") {
		return fmt.Errorf("GraphQL query templates corrupted")
	}

	// Verify AQZ1 dictionary compression & canonical hash round-trip
	sample := `{"id":6347338,"category":1,"price":45000,"area":150,"beds":3,"city_id":21,"district_id":402,"title":"شقة للإيجار"}`
	compressed := CompressJSON([]byte(sample))
	if !IsCompressedJSON(compressed) {
		return fmt.Errorf("AQZ1 compression magic header missing")
	}
	decompressed, err := DecompressJSON(compressed)
	if err != nil || decompressed != sample {
		return fmt.Errorf("AQZ1 decompression round-trip mismatch: %v", err)
	}
	if h := CanonicalContentHashBytes([]byte(sample)); len(h) != 64 {
		return fmt.Errorf("canonical content hash invalid length: %d", len(h))
	}

	fmt.Println("SELF-TEST OK")
	return nil
}

// normalizeRemoteURL turns "1.2.3.4", "1.2.3.4:9090", or "http://1.2.3.4:9090"
// into a base URL without trailing slash.
func normalizeRemoteURL(addr string, defaultPort int) string {
	addr = strings.TrimSpace(addr)
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	addr = strings.TrimRight(addr, "/")
	// Add defaultPort if no port is specified in host part
	schemeIdx := strings.Index(addr, "://")
	hostPart := addr[schemeIdx+3:]
	if !strings.Contains(hostPart, ":") {
		if defaultPort <= 0 {
			defaultPort = 9090
		}
		addr = fmt.Sprintf("%s:%d", addr, defaultPort)
	}
	return addr
}

// runRemoteCLI executes a remote management command against a running AQAR crawler
// instance over HTTP (-remote <host:port> -remote-cmd <cmd>).
func runRemoteCLI(remoteAddr, cmd, token string, defaultPort int) error {
	base := normalizeRemoteURL(remoteAddr, defaultPort)
	hc := &http.Client{Timeout: 3 * time.Minute}

	doReq := func(method, path string, body any) (int, []byte, error) {
		var rdr io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return 0, nil, err
			}
			rdr = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, base+path, rdr)
		if err != nil {
			return 0, nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("X-Auth-Token", token)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		return resp.StatusCode, raw, err
	}

	cmd = strings.ToLower(strings.TrimSpace(cmd))
	switch cmd {
	case "", "status":
		code, raw, err := doReq(http.MethodGet, "/api/status", nil)
		if err != nil {
			return fmt.Errorf("remote status failed: %w", err)
		}
		if code != http.StatusOK {
			return fmt.Errorf("remote status HTTP %d: %s", code, strings.TrimSpace(string(raw)))
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, raw, "", "  ") == nil {
			fmt.Println(pretty.String())
		} else {
			fmt.Println(string(raw))
		}
		return nil

	case "update", "force-update":
		path := "/api/update"
		if cmd == "force-update" {
			path = "/api/update?force=1"
		}
		code, raw, err := doReq(http.MethodPost, path, nil)
		if err == nil && code == http.StatusNotFound {
			// Fallback to /api/control if talking to an older build
			code, raw, err = doReq(http.MethodPost, "/api/control", map[string]string{"action": "update"})
		}
		if err != nil {
			return fmt.Errorf("remote %s failed: %w", cmd, err)
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, raw, "", "  ") == nil {
			fmt.Println(pretty.String())
		} else {
			fmt.Println(string(raw))
		}
		if code >= 400 {
			return fmt.Errorf("remote %s returned HTTP %d", cmd, code)
		}
		return nil

	case "start", "stop", "pause", "resume", "shutdown":
		code, raw, err := doReq(http.MethodPost, "/api/control", map[string]string{"action": cmd})
		if err != nil {
			return fmt.Errorf("remote %s failed: %w", cmd, err)
		}
		fmt.Println(strings.TrimSpace(string(raw)))
		if code >= 400 {
			return fmt.Errorf("remote %s returned HTTP %d", cmd, code)
		}
		return nil

	case "log", "logs":
		code, raw, err := doReq(http.MethodGet, "/api/log?lines=80", nil)
		if err != nil {
			return fmt.Errorf("remote log failed: %w", err)
		}
		if code != http.StatusOK {
			return fmt.Errorf("remote log HTTP %d: %s", code, strings.TrimSpace(string(raw)))
		}
		var resp struct {
			Lines []string `json:"lines"`
		}
		if json.Unmarshal(raw, &resp) == nil {
			for _, line := range resp.Lines {
				fmt.Println(line)
			}
		} else {
			fmt.Println(string(raw))
		}
		return nil

	default:
		return fmt.Errorf("unknown -remote-cmd %q (use: status|update|force-update|start|stop|pause|resume|shutdown|log)", cmd)
	}
}
