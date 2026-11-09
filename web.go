package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed webui.html
var webuiHTML []byte

// apiStatus is the JSON payload served by GET /api/status.
type apiStatus struct {
	Live             LiveStats     `json:"live"`
	Running          bool          `json:"running"`
	Paused           bool          `json:"paused"`
	LastError        string        `json:"last_error"`
	Runs             []RunSummary  `json:"runs"`
	DB               apiStatusDB   `json:"db"`
	Config           apiStatusCfg  `json:"config"`
	Telegram         apiTelegram   `json:"telegram"`
	Heartbeat        apiHeartbeat  `json:"heartbeat"`
	Update           UpdateStatus  `json:"update"`
	UptimeSec        int64         `json:"uptime_sec"`
	ProcessUptimeSec int64         `json:"process_uptime_sec"`
	RestartsCount    int           `json:"restarts_count"`
	ETASeconds       int64         `json:"eta_seconds"`
	Address          string        `json:"address"`
}

type apiStatusDB struct {
	Listings int64   `json:"listings"`
	History  int64   `json:"history"`
	SizeMB   float64 `json:"size_mb"`
	Path     string  `json:"path"`
}

type apiStatusCfg struct {
	SleepMinSeconds float64 `json:"sleep_min_seconds"`
	SleepMaxSeconds float64 `json:"sleep_max_seconds"`
	PageSize        int     `json:"page_size"`
	CycleHours      float64 `json:"cycle_hours"`
	WindowEnabled   bool    `json:"window_enabled"`
	WindowStart     int     `json:"window_start"`
	WindowEnd       int     `json:"window_end"`
	InWindow        bool    `json:"in_window"`
	CategoriesRent  int     `json:"categories_rent"`
	CategoriesSale  int     `json:"categories_sale"`
	DBPath          string  `json:"db_path"`
	URLEnrichment   bool    `json:"url_enrichment"`
	DryRun          bool    `json:"dry_run"`
}

type apiTelegram struct {
	Enabled  bool   `json:"enabled"`
	LastOK   bool   `json:"last_ok"`
	LastErr  string `json:"last_error"`
	LastSent string `json:"last_sent"`
}

type apiHeartbeat struct {
	LastUpdate string `json:"last_update"`
	AgeSec     int64  `json:"age_sec"`
	Stale      bool   `json:"stale"`
	AlertMin   int    `json:"alert_minutes"`
}

// webServer bundles the HTTP handlers around the controller + store.
type webServer struct {
	cfg   Config
	log   *Logger
	store *Store
	ctrl  *Controller
	start time.Time

	hbMu       sync.Mutex
	hbLastSeen time.Time // last time live stats were fresh (for stale alerting)
	hbAlerted  bool      // one alert per stale episode
}

func newWebServer(cfg Config, log *Logger, store *Store, ctrl *Controller) *webServer {
	return &webServer{cfg: cfg, log: log, store: store, ctrl: ctrl, start: time.Now()}
}

func (w *webServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.handleIndex)
	mux.HandleFunc("/api/status", w.auth(w.handleStatus))
	mux.HandleFunc("/api/control", w.auth(w.handleControl))
	mux.HandleFunc("/api/controls", w.auth(w.handleControl))
	mux.HandleFunc("/api/update", w.auth(w.handleUpdate))
	mux.HandleFunc("/api/log", w.auth(w.handleLog))
	mux.HandleFunc("/api/listing", w.auth(w.handleListing))
	mux.HandleFunc("/api/changes", w.auth(w.handleChanges))
	mux.HandleFunc("/api/analytics", w.auth(w.handleAnalytics))
	mux.HandleFunc("/api/export", w.auth(w.handleExport))
	return mux
}

// auth guards mutation endpoints when a web token is configured.
func (w *webServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if w.cfg.WebToken != "" && r.Header.Get("X-Auth-Token") != w.cfg.WebToken {
			writeJSON(rw, http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized (set X-Auth-Token)"})
			return
		}
		next(rw, r)
	}
}

func (w *webServer) handleIndex(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Write(webuiHTML)
}

func (w *webServer) handleStatus(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	live := w.ctrl.Stats().Snapshot()
	running, paused, lastErr := w.ctrl.StatusSnapshot()
	totalUp, procUp, restarts := w.ctrl.CumulativeUptimeSec()

	out := apiStatus{
		Live:             live,
		Running:          running,
		Paused:           paused,
		LastError:        lastErr,
		Update:           w.ctrl.Updater().Status(),
		UptimeSec:        totalUp,
		ProcessUptimeSec: procUp,
		RestartsCount:    restarts,
		Address:          fmt.Sprintf("http://%s", r.Host),
	}
	// Prefer the crawler's exact cumulative counters over full-table COUNT(*)
	// scans (the panel polls every second).
	if live.TotalListings > 0 {
		out.DB.Listings = live.TotalListings
	} else if n, err := w.store.DBListingsCount(ctx); err == nil {
		out.DB.Listings = n
	}
	if live.TotalHistory > 0 {
		out.DB.History = live.TotalHistory
	} else if h, err := w.store.DBHistoryCount(ctx); err == nil {
		out.DB.History = h
	}
	out.DB.SizeMB = w.store.DBSizeMB()
	out.DB.Path = w.cfg.DBPath
	if runs, err := w.store.LastRuns(ctx, 10); err == nil {
		out.Runs = runs
	}
	out.Config = apiStatusCfg{
		SleepMinSeconds: w.cfg.SleepMinSeconds,
		SleepMaxSeconds: w.cfg.SleepMaxSeconds,
		PageSize:        w.cfg.PageSize,
		CycleHours:      w.cfg.CycleHours,
		WindowEnabled:   w.cfg.CrawlWindowEnabled,
		WindowStart:     w.cfg.CrawlStartHour,
		WindowEnd:       w.cfg.CrawlEndHour,
		InWindow:        isInWindow(time.Now(), w.cfg.CrawlStartHour, w.cfg.CrawlEndHour),
		CategoriesRent:  len(w.cfg.CategoriesRent),
		CategoriesSale:  len(w.cfg.CategoriesSale),
		DBPath:          w.cfg.DBPath,
		URLEnrichment:   w.cfg.URLEnrichmentEnabled,
		DryRun:          w.cfg.DryRun,
	}
	out.ETASeconds = etaSeconds(live, w.cfg.PageSize, w.cfg.SleepMinSeconds, w.cfg.SleepMaxSeconds)

	if ok, lastErr, lastAt := w.ctrl.notify.Status(); ok || lastErr != "" || w.cfg.TelegramEnabled {
		out.Telegram = apiTelegram{Enabled: w.cfg.TelegramEnabled, LastOK: ok, LastErr: lastErr}
		if !lastAt.IsZero() {
			out.Telegram.LastSent = lastAt.UTC().Format(time.RFC3339)
		}
	}

	// heartbeat freshness (live_stats updated_at), for crash detection
	if hb, t, ok, err := w.store.LoadLiveStats(ctx); err == nil && ok {
		age := time.Since(t)
		out.Heartbeat = apiHeartbeat{
			LastUpdate: t.UTC().Format(time.RFC3339),
			AgeSec:     int64(age.Seconds()),
			Stale:      age > 30*time.Second,
			AlertMin:   w.cfg.TelegramHeartbeatMin,
		}
		_ = hb
		w.checkHeartbeat(running, t, age)
	}

	writeJSON(rw, http.StatusOK, out)
}

// checkHeartbeat fires the stale-crawler Telegram alert once per stale
// episode (a hard-killed crawler leaves no graceful stop event).
func (w *webServer) checkHeartbeat(running bool, last time.Time, age time.Duration) {
	if w.cfg.TelegramHeartbeatMin <= 0 {
		return
	}
	w.hbMu.Lock()
	defer w.hbMu.Unlock()
	threshold := time.Duration(w.cfg.TelegramHeartbeatMin) * time.Minute
	if running || age < threshold {
		// crawler running or heartbeat fresh -> reset the episode
		if age < 30*time.Second {
			w.hbAlerted = false
		}
		return
	}
	if !w.hbAlerted {
		w.hbAlerted = true
		w.ctrl.notify.HeartbeatStale(w.cfg.TelegramHeartbeatMin)
	}
}

// etaSeconds estimates how long the current cycle still needs at the average
// sleep pace, based on the remaining pages per category.
func etaSeconds(live LiveStats, pageSize int, sleepMin, sleepMax float64) int64 {
	if !live.Running || pageSize <= 0 {
		return 0
	}
	pages := 0
	for _, c := range live.Categories {
		if c.Done || c.Errored {
			continue
		}
		remaining := c.Total - c.From
		if remaining < 0 {
			remaining = 0
		}
		pages += int(math.Ceil(float64(remaining) / float64(pageSize)))
	}
	avg := (sleepMin + sleepMax) / 2
	return int64(float64(pages) * avg)
}

type controlRequest struct {
	Action string `json:"action"`
}

func (w *webServer) handleControl(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(rw, http.StatusMethodNotAllowed, map[string]any{"ok": false, "message": "use POST"})
		return
	}
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	msg, err := w.ctrl.DoControl(req.Action)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "message": msg})
}

func (w *webServer) handleUpdate(rw http.ResponseWriter, r *http.Request) {
	if w.ctrl.Updater() == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]any{"ok": false, "message": "auto-updater not initialized"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(rw, http.StatusOK, w.ctrl.Updater().Status())
	case http.MethodPost:
		force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
		st := w.ctrl.Updater().CheckAndApply(force)
		writeJSON(rw, http.StatusOK, st)
	default:
		writeJSON(rw, http.StatusMethodNotAllowed, map[string]any{"ok": false, "message": "use GET or POST"})
	}
}

// handleLog tails the crawler log file efficiently: it seeks to the last
// ~64KB instead of reading (and splitting) the whole file on every poll.
func (w *webServer) handleLog(rw http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	path := filepath.Join(w.cfg.LogDir, "crawler.log")
	f, err := os.Open(path)
	if err != nil {
		writeJSON(rw, http.StatusOK, map[string]any{"lines": []string{}})
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeJSON(rw, http.StatusOK, map[string]any{"lines": []string{}})
		return
	}
	const tailBlock = 64 << 10
	start := int64(0)
	if fi.Size() > tailBlock {
		start = fi.Size() - tailBlock
	}
	buf := make([]byte, fi.Size()-start)
	_, _ = f.ReadAt(buf, start)
	// drop the first partial line, then tail
	s := string(buf)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 && start > 0 {
		s = s[idx+1:]
	}
	writeJSON(rw, http.StatusOK, map[string]any{"lines": tailLines(s, limit)})
}

func tailLines(s string, n int) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// handleListing serves the current listing summary, full decompressed JSON,
// and chronological version timeline + field-level diffs for one id.
func (w *webServer) handleListing(rw http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "message": "missing ?id="})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	summary, serr := w.store.InspectListing(ctx, id)
	history, err := w.store.ListingHistory(ctx, id, 50)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if serr != nil && len(history) == 0 {
		writeJSON(rw, http.StatusNotFound, map[string]any{"ok": false, "message": fmt.Sprintf("listing %d not found in archive", id)})
		return
	}
	type row struct {
		FetchedAt  string      `json:"fetched_at"`
		ChangeType string      `json:"change_type"`
		Diff       []FieldDiff `json:"diff,omitempty"`
	}
	rows := make([]row, 0, len(history))
	var prevJSON string
	for i := len(history) - 1; i >= 0; i-- { // chronological (oldest -> newest)
		cur := history[i]
		rr := row{FetchedAt: cur.FetchedAt, ChangeType: cur.ChangeType}
		if cur.DataJSON != "" {
			if prevJSON != "" {
				rr.Diff = DiffJSON(prevJSON, cur.DataJSON)
			}
			prevJSON = cur.DataJSON
		}
		rows = append(rows, rr)
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":      true,
		"id":      id,
		"summary": summary,
		"rows":    rows,
	})
}

// handleChanges serves the Price-Drop & Update Radar feed.
func (w *webServer) handleChanges(rw http.ResponseWriter, r *http.Request) {
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	onlyDrops := r.URL.Query().Get("drops") == "1" || r.URL.Query().Get("drops") == "true"
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	items, err := w.store.RecentChanges(ctx, limit, onlyDrops)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "items": items})
}

// handleAnalytics serves archive-wide active/removed breakdown & market stats.
func (w *webServer) handleAnalytics(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	an, err := w.store.ArchiveAnalytics(ctx)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "analytics": an})
}

// handleExport streams the archive as CSV (optionally one category).
func (w *webServer) handleExport(rw http.ResponseWriter, r *http.Request) {
	category := 0
	if v := r.URL.Query().Get("category"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			category = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	rw.Header().Set("Content-Type", "text/csv; charset=utf-8")
	rw.Header().Set("Content-Disposition", `attachment; filename="aqar_listings.csv"`)
	cw := csv.NewWriter(rw)
	defer cw.Flush()
	_ = cw.Write([]string{"id", "category", "price", "area", "beds", "rooms", "furnished",
		"city_id", "district_id", "lat", "lng", "address", "url", "first_seen_at",
		"last_seen_at", "last_change_at", "removed_at"})

	err := w.store.ExportListings(ctx, category, func(row ExportRow) error {
		return cw.Write([]string{
			strconv.FormatInt(row.ID, 10),
			nullInt(row.Category),
			nullFloat(row.Price),
			nullFloat(row.Area),
			nullInt(row.Beds),
			nullInt(row.Rooms),
			nullInt(row.Furnished),
			nullInt(row.CityID),
			nullInt(row.DistrictID),
			nullFloat(row.Lat),
			nullFloat(row.Lng),
			row.Address,
			nullStr(row.URL),
			row.FirstSeenAt, row.LastSeenAt, row.LastChangeAt,
			nullStr(row.RemovedAt),
		})
	})
	if err != nil {
		w.log.Errorf("export failed: %v", err)
	}
}

func nullInt(v sql.NullInt64) string {
	if v.Valid {
		return strconv.FormatInt(v.Int64, 10)
	}
	return ""
}

func nullFloat(v sql.NullFloat64) string {
	if v.Valid {
		return strconv.FormatFloat(v.Float64, 'f', -1, 64)
	}
	return ""
}

func nullStr(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

// serveWeb runs the control panel server; blocks until ctx is cancelled or
// a self-update binary handoff / intentional API shutdown occurs.
func serveWeb(ctx context.Context, cfg Config, log *Logger, store *Store, ctrl *Controller) error {
	addr := net.JoinHostPort(cfg.WebHost, strconv.Itoa(cfg.WebPort))
	srv := &http.Server{Addr: addr, Handler: newWebServer(cfg, log, store, ctrl).handler()}

	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	if cfg.AutoUpdateEnabled && ctrl.Updater() != nil {
		go ctrl.Updater().StartLoop(loopCtx)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Infof("control panel listening on http://%s", addr)
		errCh <- srv.ListenAndServe()
	}()

	shutdownServer := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}

	select {
	case <-ctx.Done():
		shutdownServer()
		return nil
	case newCommit := <-ctrl.handoffCh:
		log.Infof("[AUTO-UPDATE] shutting down web server for binary handoff to %s", newCommit)
		shutdownServer()
		return nil
	case <-ctrl.shutdownCh:
		log.Infof("intentional shutdown requested via API")
		shutdownServer()
		return nil
	case err := <-errCh:
		return fmt.Errorf("web server: %w", err)
	}
}
