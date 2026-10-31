package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Notifier sends Telegram messages when the crawler stops or when the
// heartbeat goes stale. Failures are retried and logged; the last delivery
// status is exposed via /api/status. The bot token lives in config.json and
// must never be logged or shared.
type Notifier struct {
	cfg    Config
	log    *Logger
	client *http.Client

	mu        sync.Mutex
	lastOK    bool
	lastError string
	lastAt    time.Time
}

func NewNotifier(cfg Config, log *Logger) *Notifier {
	return &Notifier{cfg: cfg, log: log, client: &http.Client{Timeout: 15 * time.Second}}
}

func (n *Notifier) Enabled() bool { return n != nil && n.cfg.TelegramEnabled }

// Send posts a plain-text message to the configured chat with 2 retries.
// Failures are logged (never the token) and recorded for the panel.
func (n *Notifier) Send(text string) {
	if !n.Enabled() {
		return
	}
	body, err := json.Marshal(map[string]string{
		"chat_id": n.cfg.TelegramChatID,
		"text":    text,
	})
	if err != nil {
		n.fail("marshal", err)
		return
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.TelegramBotToken)
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			n.fail("build request", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := n.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				n.mu.Lock()
				n.lastOK = true
				n.lastError = ""
				n.lastAt = time.Now()
				n.mu.Unlock()
				return
			}
			err = fmt.Errorf("telegram status %d", resp.StatusCode)
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		n.fail(fmt.Sprintf("attempt %d", attempt), err)
	}
}

func (n *Notifier) fail(stage string, err error) {
	n.mu.Lock()
	n.lastOK = false
	n.lastError = fmt.Sprintf("%s: %v", stage, err)
	n.lastAt = time.Now()
	n.mu.Unlock()
	if n.log != nil {
		n.log.Errorf("telegram send failed (%s): %v", stage, err)
	}
}

// Verify checks the bot token against the Telegram API (getMe) at startup.
func (n *Notifier) Verify() {
	if !n.Enabled() {
		return
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", n.cfg.TelegramBotToken)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		n.log.Warnf("telegram verify: build request failed")
		return
	}
	resp, err := n.client.Do(req)
	if err != nil {
		n.log.Warnf("telegram verify: %v (alerts may not work)", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		n.log.Infof("telegram bot verified OK")
	} else {
		n.log.Warnf("telegram verify: HTTP %d (alerts may not work)", resp.StatusCode)
	}
}

// Status returns the last delivery result for the panel.
func (n *Notifier) Status() (ok bool, lastErr string, lastAt time.Time) {
	if !n.Enabled() {
		return false, "disabled", time.Time{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastOK, n.lastError, n.lastAt
}

// Stopped builds the stop message with the reason and final stats.
func (n *Notifier) Stopped(cycle int, reason string, ls LiveStats, dbListings, dbHistory int64, dbMB float64) {
	if !n.Enabled() {
		return
	}
	msg := fmt.Sprintf(
		"🛑 AQAR crawler STOPPED\nReason: %s\n\nCycle: #%d | requests: %d | seen: %d | new: %d | updated: %d | removed: %d | errors: %d\nArchive: %d listings | %d history rows | %.1f MB",
		reason, cycle, ls.Requests, ls.Seen, ls.Fresh, ls.Updated, ls.Removed, ls.Errors,
		dbListings, dbHistory, dbMB)
	n.Send(msg)
}

// HeartbeatStale sends a crash-detection alert (crawler supposed to be
// running but its live_stats heartbeat went stale).
func (n *Notifier) HeartbeatStale(minutes int) {
	if !n.Enabled() {
		return
	}
	n.Send(fmt.Sprintf("⚠️ AQAR crawler heartbeat STALE for %d+ minutes - the crawler may have crashed (no graceful stop was captured).", minutes))
}
