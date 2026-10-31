package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Controller owns the crawler lifecycle so the web panel can start, stop,
// pause and resume it at will. One Stats instance is shared across restarts
// so the dashboard keeps continuity (archive totals persist in Stats).
type Controller struct {
	cfg    Config
	log    *Logger
	client *Client
	store  *Store
	notify *Notifier

	mu      sync.Mutex
	stats   *Stats
	cancel  context.CancelFunc
	done    chan struct{} // closed when the crawler goroutine fully exits
	running bool
	lastErr string
}

func NewController(cfg Config, log *Logger, client *Client, store *Store) *Controller {
	return &Controller{
		cfg: cfg, log: log, client: client, store: store,
		notify: NewNotifier(cfg, log),
		stats:  NewStats(),
		done:   make(chan struct{}),
	}
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
	c.cancel = cancel
	c.running = true
	c.lastErr = ""
	c.done = make(chan struct{})
	c.log.Infof("controller: starting crawler")
	go func() {
		err := crawler.Run(ctx)
		c.mu.Lock()
		c.running = false
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
		c.notifyStopped(reason) // runs to completion before the goroutine exits
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

// DoControl dispatches a panel action.
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
	default:
		return "", fmt.Errorf("unknown action %q (start|stop|pause|resume)", action)
	}
}
