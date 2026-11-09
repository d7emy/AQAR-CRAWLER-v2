package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

// Controller owns the crawler lifecycle so the web panel can start, stop,
// pause, resume, and self-update it at will. One Stats instance is shared
// across restarts so the dashboard keeps continuity (archive totals persist).
type Controller struct {
	cfg        Config
	configPath string
	log        *Logger
	client     *Client
	store      *Store
	notify     *Notifier
	updater    *AutoUpdater

	mu                 sync.Mutex
	stats              *Stats
	cancel             context.CancelFunc
	done               chan struct{} // closed when the crawler goroutine fully exits
	running            bool
	lastErr            string
	suppressStopNotify bool
	exitForUpdate      bool
	startedAt          time.Time
	priorUptimeSecs    int64
	restartsCount      int
	handoffCh          chan string
	shutdownCh         chan struct{}
}

func NewController(cfg Config, log *Logger, client *Client, store *Store) *Controller {
	c := &Controller{
		cfg:        cfg,
		configPath: "config.json",
		log:        log,
		client:     client,
		store:      store,
		notify:     NewNotifier(cfg, log),
		stats:      NewStats(),
		done:       make(chan struct{}),
		startedAt:  time.Now(),
		handoffCh:  make(chan string, 1),
		shutdownCh: make(chan struct{}, 1),
	}

	// Restore cumulative session uptime and restart counter if resuming after a handoff
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if cp, ok, err := store.LoadSessionCheckpoint(ctx, cfg.StateDir); err == nil && ok {
		c.priorUptimeSecs = cp.PriorUptimeSecs
		c.restartsCount = cp.RestartsCount + 1
	}
	cancel()

	interval := time.Duration(cfg.AutoUpdateIntervalSecs) * time.Second
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	c.updater = NewAutoUpdater(".", cfg.StateDir, c.configPath, cfg.AutoUpdateBranch, interval, log,
		c.ReloadConfig,
		c.TriggerBinaryHandoff,
	)
	return c
}

// SetConfigPath updates the config file path used by Plan A hot-reloads and Plan B self-tests.
func (c *Controller) SetConfigPath(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if path != "" {
		c.configPath = path
		if c.updater != nil {
			c.updater.configPath = path
		}
	}
}

// Updater returns the attached AutoUpdater.
func (c *Controller) Updater() *AutoUpdater {
	return c.updater
}

// ReloadConfig hot-reloads config.json in memory (<5ms, 0ms downtime) for Plan A updates.
func (c *Controller) ReloadConfig() error {
	c.mu.Lock()
	path := c.configPath
	c.mu.Unlock()
	if path == "" {
		path = "config.json"
	}
	newCfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	if err := newCfg.validate(); err != nil {
		return err
	}
	c.mu.Lock()
	c.cfg = newCfg
	c.notify = NewNotifier(newCfg, c.log)
	c.mu.Unlock()
	c.log.Infof("controller: hot-reloaded configuration from %s (0ms downtime)", path)
	return nil
}

// TriggerBinaryHandoff schedules a sub-second process handoff after a 350ms
// delay so the active /api/update HTTP 200 JSON response flushes cleanly to the caller.
func (c *Controller) TriggerBinaryHandoff(newCommit string) {
	c.mu.Lock()
	c.suppressStopNotify = true
	c.exitForUpdate = true
	c.mu.Unlock()

	go func() {
		time.Sleep(350 * time.Millisecond)
		select {
		case c.handoffCh <- newCommit:
		default:
		}
	}()
}

// ExitForUpdate reports whether the controller is shutting down for a Plan B binary handoff (exit code 42).
func (c *Controller) ExitForUpdate() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitForUpdate
}

// CumulativeUptimeSec returns total uptime across self-update restarts plus current process uptime.
func (c *Controller) CumulativeUptimeSec() (totalSec int64, processSec int64, restarts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	proc := int64(time.Since(c.startedAt).Seconds())
	return c.priorUptimeSecs + proc, proc, c.restartsCount
}

// Start launches the crawler loop (no-op if already running).
func (c *Controller) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return errors.New("crawler is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	crawler := NewCrawler(c.cfg, c.log, c.client, c.store)
	crawler.Stats = c.stats
	crawler.PriorUptimeSecs = c.priorUptimeSecs
	crawler.RestartsCount = c.restartsCount
	c.cancel = cancel
	c.running = true
	c.lastErr = ""
	c.suppressStopNotify = false
	c.done = make(chan struct{})
	c.log.Infof("controller: starting crawler")
	go func() {
		err := crawler.Run(ctx)
		c.mu.Lock()
		c.running = false
		suppress := c.suppressStopNotify
		reason := "stopped"
		if err != nil {
			if errors.Is(err, ErrAPIRejected) {
				c.lastErr = "API rejected requests (Req-Hash/anti-bot); check logs"
				reason = "API rejected requests"
			} else {
				c.lastErr = err.Error()
				reason = "crawler error: " + err.Error()
			}
			c.log.Errorf("controller: crawler stopped with error: %v", err)
		} else {
			c.log.Infof("controller: crawler stopped cleanly")
		}
		c.mu.Unlock()
		if !suppress {
			c.notifyStopped(reason) // runs to completion before the goroutine exits
		}
		close(c.done)
	}()
	return nil
}

// notifyStopped sends the Telegram stop message with current stats.
func (c *Controller) notifyStopped(reason string) {
	if !c.notify.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listings, err1 := c.store.DBListingsCount(ctx)
	history, err2 := c.store.DBHistoryCount(ctx)
	if err1 != nil || err2 != nil {
		listings, history = 0, 0
	}
	c.notify.Stopped(c.stats.Snapshot().Cycle, reason, c.stats.Snapshot(),
		listings, history, c.store.DBSizeMB())
}

// Stop gracefully cancels the crawler; it keeps its resume state so the next
// Start continues where it left off. BLOCKS until the crawler goroutine has
// fully exited (including the Telegram stop notification attempt), so the
// process can exit immediately after without losing the alert.
func (c *Controller) Stop() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return errors.New("crawler is not running")
	}
	cancel := c.cancel
	done := c.done
	c.mu.Unlock()

	c.log.Infof("controller: stopping crawler (resume state will be kept)")
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		c.log.Errorf("controller: crawler did not exit within 30s of Stop()")
	}
	return nil
}

// Pause halts requests between pages (the crawler keeps running, just idle).
func (c *Controller) Pause() {
	c.stats.SetPaused(true)
	c.log.Infof("controller: crawler paused")
}

// Resume unpauses request pacing.
func (c *Controller) Resume() {
	c.stats.SetPaused(false)
	c.log.Infof("controller: crawler resumed")
}

// IsRunning reports whether the crawler loop is active right now.
func (c *Controller) IsRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// LastError returns the error from the most recent crawler run, if any.
func (c *Controller) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// Stats returns the shared stats instance.
func (c *Controller) Stats() *Stats { return c.stats }

// StatusSnapshot is everything the web panel needs about the controller.
func (c *Controller) StatusSnapshot() (running bool, paused bool, lastErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running, c.stats.IsPaused(), c.lastErr
}

// DoControl dispatches a panel or remote API action.
func (c *Controller) DoControl(action string) (string, error) {
	switch action {
	case "start":
		if err := c.Start(); err != nil {
			return "", err
		}
		return "crawler started", nil
	case "stop":
		if err := c.Stop(); err != nil {
			return "", err
		}
		return "crawler stopping (resume point saved)", nil
	case "pause":
		c.Pause()
		return "crawler paused", nil
	case "resume":
		c.Resume()
		return "crawler resumed", nil
	case "update", "force-update":
		if c.updater == nil {
			return "", errors.New("auto-updater not initialized")
		}
		st := c.updater.CheckAndApply(action == "force-update")
		if st.LastError != "" {
			return "", errors.New(st.LastError)
		}
		return fmt.Sprintf("update complete: mode=%s commit=%s", st.LastMode, st.CurrentCommit), nil
	case "shutdown":
		stateDir := c.cfg.StateDir
		if stateDir == "" {
			stateDir = "state"
		}
		_ = writeFileAtomic(filepath.Join(stateDir, "stopfile"), []byte("shutdown\n"), 0o644)
		go func() {
			time.Sleep(250 * time.Millisecond)
			select {
			case c.shutdownCh <- struct{}{}:
			default:
			}
		}()
		return "intentional shutdown initiated (stopfile written)", nil
	default:
		return "", fmt.Errorf("unknown action %q (start|stop|pause|resume|update|shutdown)", action)
	}
}
