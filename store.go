package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteDriver = "sqlite"

type Store struct {
	db   *sql.DB
	ro   *sql.DB
	path string
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = "AQAR.db"
	}
	db, err := sql.Open(sqliteDriver, path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite: one writer avoids write-lock contention
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// WAL = fast writes and allows concurrent readers; cache_size=-32000 = 32MB RAM cache.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=15000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA cache_size=-32000",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}

	// Open a separate read-only connection pool so dashboard/TUI/web queries
	// never block behind crawler write transactions under WAL.
	ro := db
	if path != ":memory:" && !strings.HasPrefix(path, "file::memory:") {
		if rdb, err := sql.Open(sqliteDriver, path); err == nil {
			rdb.SetMaxOpenConns(4)
			rdb.SetMaxIdleConns(2)
			rdb.SetConnMaxLifetime(0)
			for _, p := range []string{
				"PRAGMA query_only=ON",
				"PRAGMA busy_timeout=15000",
				"PRAGMA temp_store=MEMORY",
				"PRAGMA cache_size=-16000",
			} {
				_, _ = rdb.ExecContext(ctx, p)
			}
			ro = rdb
		}
	}
	return &Store{db: db, ro: ro, path: path}, nil
}

func (s *Store) Close() error {
	if s.ro != nil && s.ro != s.db {
		_ = s.ro.Close()
	}
	return s.db.Close()
}

// Migrate applies any pending schema migrations.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	for i, migration := range migrations {
		version := i + 1
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&exists)
		if err != nil {
			return fmt.Errorf("checking migration %d: %w", version, err)
		}
		if exists > 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			tx.Rollback()
			return fmt.Errorf("applying migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, nowUTC()); err != nil {
			tx.Rollback()
			return fmt.Errorf("recording migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %d: %w", version, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// crawl_runs / crawl_state

func (s *Store) StartRun(ctx context.Context, cycle int) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO crawl_runs (cycle, started_at, status) VALUES (?, ?, 'running')`,
		cycle, nowUTC())
	if err != nil {
		return 0, fmt.Errorf("start run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("run id: %w", err)
	}
	return id, nil
}

func (s *Store) FinishRun(ctx context.Context, runID int64, status string, requests, pages, seen, fresh, updated, removed, errs int, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE crawl_runs SET finished_at = ?, status = ?,
			requests = ?, pages = ?, listings_seen = ?, listings_new = ?,
			listings_updated = ?, listings_removed = ?, errors = ?, last_error = ?
		WHERE id = ?`,
		nowUTC(), status, requests, pages, seen, fresh, updated, removed, errs, lastErr, runID)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

type CrawlState struct {
	Cycle      int
	SetIndex   int
	CategoryID int
	From       int
	PageSize   int
}

func (s *Store) SaveState(ctx context.Context, st CrawlState) error {
	data, _ := json.Marshal(st)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO crawl_state (key, value) VALUES ('resume', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(data))
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// SaveNextCycle persists the cycle number a restart should begin with when no
// resume state exists (prevents cycle numbering from resetting to 1).
func (s *Store) SaveNextCycle(ctx context.Context, cycle int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO crawl_state (key, value) VALUES ('next_cycle', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, fmt.Sprint(cycle))
	if err != nil {
		return fmt.Errorf("save next cycle: %w", err)
	}
	return nil
}

func (s *Store) LoadNextCycle(ctx context.Context) (int, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM crawl_state WHERE key = 'next_cycle'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("load next cycle: %w", err)
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, false, fmt.Errorf("parse next cycle: %w", err)
	}
	return n, true, nil
}

func (s *Store) LoadState(ctx context.Context) (CrawlState, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM crawl_state WHERE key = 'resume'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return CrawlState{}, false, nil
	}
	if err != nil {
		return CrawlState{}, false, fmt.Errorf("load state: %w", err)
	}
	var st CrawlState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return CrawlState{}, false, fmt.Errorf("parse state: %w", err)
	}
	return st, true, nil
}

func (s *Store) ClearState(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM crawl_state WHERE key = 'resume'`)
	return err
}

// ---------------------------------------------------------------------------
// listing ingestion

// IngestionStats is the per-page delta report.
type IngestionStats struct {
	New     int
	Updated int
	Seen    int // total distinct listings on the page
}

// IngestPage archives one page of listings (duplicates by id are collapsed,
// last occurrence wins - protects against API pages that repeat listings).
// Full JSON is compressed losslessly via CompressJSON (AQZ1 dictionary-deflate).
// Change detection compares CanonicalContentHash against listings.data_hash so
// backend array jitter (null/[]/[""]) and volatile hit counters (refresh,
// whatsapp_*) update the current listing without spamming listing_history.
func (s *Store) IngestPage(ctx context.Context, _ int64, cycle int, listings []map[string]any) (IngestionStats, error) {
	if len(listings) == 0 {
		return IngestionStats{}, nil
	}
	fetchedAt := nowUTC()

	type item struct {
		id         int64
		hash       string
		rawHash    string
		blob       []byte
		m          map[string]any // original decoded map (no re-marshal)
		changeType string         // "new" | "update" | "unchanged"
	}
	// collapse duplicate ids (last occurrence wins) while keeping first-seen
	// order for stable processing
	byID := make(map[int64]item, len(listings))
	order := make([]int64, 0, len(listings))
	for _, l := range listings {
		id := int64(toFloat(l["id"]))
		if id <= 0 {
			continue
		}
		raw, err := json.Marshal(l) // keys are sorted by encoding/json -> canonical
		if err != nil {
			return IngestionStats{}, fmt.Errorf("canonical marshal listing %d: %w", id, err)
		}
		rawSum := sha256.Sum256(raw)
		rawHash := hex.EncodeToString(rawSum[:])
		contentHash := CanonicalContentHashBytes(raw)
		blob := CompressJSON(raw)
		if _, exists := byID[id]; !exists {
			order = append(order, id)
		}
		byID[id] = item{id: id, hash: contentHash, rawHash: rawHash, blob: blob, m: l}
	}
	items := make([]item, 0, len(order))
	for _, id := range order {
		items = append(items, byID[id])
	}
	if len(items) == 0 {
		return IngestionStats{}, nil
	}

	// Fetch current hashes for these ids in one query.
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.id)
	}
	current := make(map[int64]string, len(ids))
	{
		placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, "SELECT id, data_hash FROM listings WHERE id IN ("+placeholders+")", args...)
		if err != nil {
			return IngestionStats{}, fmt.Errorf("querying current hashes: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var h string
			if err := rows.Scan(&id, &h); err != nil {
				return IngestionStats{}, fmt.Errorf("scan hash row: %w", err)
			}
			current[id] = h
		}
		if err := rows.Err(); err != nil {
			return IngestionStats{}, fmt.Errorf("iterating hash rows: %w", err)
		}
	}

	stats := IngestionStats{Seen: len(items)}
	for i := range items {
		cur, known := current[items[i].id]
		switch {
		case !known:
			items[i].changeType = "new"
			stats.New++
		case cur != items[i].hash && cur != items[i].rawHash:
			items[i].changeType = "update"
			stats.Updated++
		default:
			items[i].changeType = "unchanged"
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IngestionStats{}, fmt.Errorf("begin ingest tx: %w", err)
	}
	defer tx.Rollback()

	ins, err := tx.PrepareContext(ctx, `
		INSERT INTO listings (
			id, data_json, data_hash, category, price, meter_price, area, beds, rooms,
			furnished, lat, lng, province_id, city_id, direction_id, district_id,
			premium, boosted, status, published, create_time, refresh, user_id, user_name,
			first_seen_at, last_seen_at, last_change_at, seen_cycle
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return IngestionStats{}, fmt.Errorf("prepare insert: %w", err)
	}
	defer ins.Close()

	upd, err := tx.PrepareContext(ctx, `
		UPDATE listings SET
			data_json = ?, data_hash = ?, category = ?, price = ?, meter_price = ?,
			area = ?, beds = ?, rooms = ?, furnished = ?, lat = ?, lng = ?,
			province_id = ?, city_id = ?, direction_id = ?, district_id = ?,
			premium = ?, boosted = ?, status = ?, published = ?, create_time = ?,
			refresh = ?, user_id = ?, user_name = ?,
			last_seen_at = ?, last_change_at = ?, removed_at = NULL, seen_cycle = ?
		WHERE id = ?`)
	if err != nil {
		return IngestionStats{}, fmt.Errorf("prepare update: %w", err)
	}
	defer upd.Close()

	// touch refreshes the full compressed snapshot (preserving latest volatile
	// counters like refresh/whatsapp_requested_count), migrates data_hash to
	// CanonicalContentHash, and clears any stale removed_at flag.
	touch, err := tx.PrepareContext(ctx,
		`UPDATE listings SET data_json = ?, data_hash = ?, refresh = ?, last_seen_at = ?, seen_cycle = ?, removed_at = NULL WHERE id = ?`)
	if err != nil {
		return IngestionStats{}, fmt.Errorf("prepare touch: %w", err)
	}
	defer touch.Close()

	hist, err := tx.PrepareContext(ctx,
		`INSERT INTO listing_history (listing_id, change_type, data_json, data_hash, fetched_at)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return IngestionStats{}, fmt.Errorf("prepare history: %w", err)
	}
	defer hist.Close()

	for _, it := range items {
		v := buildListingValues(it.id, it.blob, it.hash, it.m, fetchedAt, cycle)
		switch it.changeType {
		case "new":
			if _, err := ins.ExecContext(ctx, v.insertArgs()...); err != nil {
				return IngestionStats{}, fmt.Errorf("insert listing %d: %w", it.id, err)
			}
			if _, err := hist.ExecContext(ctx, it.id, "new", it.blob, it.hash, fetchedAt); err != nil {
				return IngestionStats{}, fmt.Errorf("history insert %d: %w", it.id, err)
			}
		case "update":
			if _, err := upd.ExecContext(ctx, v.updateArgs()...); err != nil {
				return IngestionStats{}, fmt.Errorf("update listing %d: %w", it.id, err)
			}
			if _, err := hist.ExecContext(ctx, it.id, "update", it.blob, it.hash, fetchedAt); err != nil {
				return IngestionStats{}, fmt.Errorf("history update %d: %w", it.id, err)
			}
		default: // unchanged
			if _, err := touch.ExecContext(ctx, it.blob, it.hash, v.refresh, fetchedAt, cycle, it.id); err != nil {
				return IngestionStats{}, fmt.Errorf("touch listing %d: %w", it.id, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return IngestionStats{}, fmt.Errorf("commit ingest tx: %w", err)
	}
	return stats, nil
}

// listingValues holds the columns shared by the INSERT and UPDATE statements
// below; the arg-slicing helpers keep both statements in lockstep.
type listingValues struct {
	id                                                                    int64
	blob                                                                  []byte
	hash                                                                  string
	category, price, meterPrice, area, beds, rooms, furnished, lat, lng   any
	provinceID, cityID, directionID, districtID, premium, boosted, status any
	published, createTime, refresh, userID, userName                      any
	fetchedAt                                                             string
	cycle                                                                 int
}

func buildListingValues(id int64, blob []byte, hash string, m map[string]any, fetchedAt string, cycle int) listingValues {
	return listingValues{
		id: id, blob: blob, hash: hash,
		category: intOrNil(m, "category"), price: numOrNil(m, "price"),
		meterPrice: numOrNil(m, "meter_price"), area: numOrNil(m, "area"),
		beds: intOrNil(m, "beds"), rooms: intOrNil(m, "rooms"),
		furnished: intOrNil(m, "furnished"), lat: numOrNil(m, "location", "lat"),
		lng: numOrNil(m, "location", "lng"), provinceID: intOrNil(m, "province_id"),
		cityID: intOrNil(m, "city_id"), directionID: intOrNil(m, "direction_id"),
		districtID: intOrNil(m, "district_id"), premium: intOrNil(m, "premium"),
		boosted: intOrNil(m, "boosted"), status: intOrNil(m, "status"),
		published: intOrNil(m, "published"), createTime: intOrNil(m, "create_time"),
		refresh: intOrNil(m, "refresh"), userID: intOrNil(m, "user_id"),
		userName: strOrNil(m, "user", "name"),
		fetchedAt: fetchedAt, cycle: cycle,
	}
}

// insertArgs matches the 28-column INSERT statement.
func (v listingValues) insertArgs() []any {
	return []any{
		v.id, v.blob, v.hash,
		v.category, v.price, v.meterPrice, v.area, v.beds, v.rooms, v.furnished,
		v.lat, v.lng, v.provinceID, v.cityID, v.directionID, v.districtID,
		v.premium, v.boosted, v.status, v.published, v.createTime, v.refresh,
		v.userID, v.userName,
		v.fetchedAt, v.fetchedAt, v.fetchedAt, v.cycle,
	}
}

// updateArgs matches the UPDATE statement (data_json..user_name, last_seen_at,
// last_change_at, seen_cycle, WHERE id). first_seen_at is preserved.
func (v listingValues) updateArgs() []any {
	return []any{
		v.blob, v.hash,
		v.category, v.price, v.meterPrice, v.area, v.beds, v.rooms, v.furnished,
		v.lat, v.lng, v.provinceID, v.cityID, v.directionID, v.districtID,
		v.premium, v.boosted, v.status, v.published, v.createTime, v.refresh,
		v.userID, v.userName,
		v.fetchedAt, v.fetchedAt, v.cycle, v.id,
	}
}

// MarkRemoved flags listings not seen in the completed cycle (graceCycles=1).
func (s *Store) MarkRemoved(ctx context.Context, cycle int) (int, error) {
	return s.MarkRemovedWithGrace(ctx, cycle, 1)
}

// MarkRemovedWithGrace flags listings that have been absent for at least
// graceCycles consecutive completed cycles (seen_cycle <= cycle - graceCycles)
// and appends a compact 'removed' event row (data_json = NULL, avoiding
// redundant snapshot duplication).
func (s *Store) MarkRemovedWithGrace(ctx context.Context, cycle, graceCycles int) (int, error) {
	if graceCycles < 1 {
		graceCycles = 1
	}
	maxSeenCycle := cycle - graceCycles
	if maxSeenCycle < 1 {
		return 0, nil
	}
	removedAt := nowUTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin remove tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO listing_history (listing_id, change_type, data_json, data_hash, fetched_at)
		SELECT id, 'removed', NULL, NULL, ?
		FROM listings
		WHERE removed_at IS NULL AND seen_cycle <= ?`,
		removedAt, maxSeenCycle); err != nil {
		return 0, fmt.Errorf("insert remove history: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE listings SET removed_at = ?
		WHERE removed_at IS NULL AND seen_cycle <= ?`,
		removedAt, maxSeenCycle)
	if err != nil {
		return 0, fmt.Errorf("update removed flag: %w", err)
	}
	n, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit remove tx: %w", err)
	}
	return int(n), nil
}

// ---------------------------------------------------------------------------
// live stats + dashboard queries (used by the TUI and -watch mode)

// SaveLiveStats upserts the single live_stats row.
func (s *Store) SaveLiveStats(ctx context.Context, ls LiveStats) error {
	data, err := json.Marshal(ls)
	if err != nil {
		return fmt.Errorf("marshal live stats: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO live_stats (id, data_json, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET data_json = excluded.data_json, updated_at = excluded.updated_at`,
		string(data), nowUTC())
	if err != nil {
		return fmt.Errorf("save live stats: %w", err)
	}
	return nil
}

// LoadLiveStats reads the live_stats row (updated time + whether it exists).
func (s *Store) LoadLiveStats(ctx context.Context) (LiveStats, time.Time, bool, error) {
	var raw, updatedAt string
	err := s.ro.QueryRowContext(ctx, `SELECT data_json, updated_at FROM live_stats WHERE id = 1`).Scan(&raw, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveStats{}, time.Time{}, false, nil
	}
	if err != nil {
		return LiveStats{}, time.Time{}, false, fmt.Errorf("load live stats: %w", err)
	}
	var ls LiveStats
	if err := json.Unmarshal([]byte(raw), &ls); err != nil {
		return LiveStats{}, time.Time{}, false, fmt.Errorf("parse live stats: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return LiveStats{}, time.Time{}, false, fmt.Errorf("parse live stats time: %w", err)
	}
	return ls, t, true, nil
}

// RunSummary is one crawl cycle for the history panel.
type RunSummary struct {
	Cycle     int    `json:"cycle"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at"`
	Requests  int    `json:"requests"`
	Seen      int    `json:"seen"`
	Fresh     int    `json:"fresh"`
	Updated   int    `json:"updated"`
	Removed   int    `json:"removed"`
	Errors    int    `json:"errors"`
}

// LastRuns returns the most recent crawl runs (newest first).
func (s *Store) LastRuns(ctx context.Context, limit int) ([]RunSummary, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT cycle, status, started_at, requests, listings_seen, listings_new,
		       listings_updated, listings_removed, errors
		FROM crawl_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()
	var out []RunSummary
	for rows.Next() {
		var r RunSummary
		if err := rows.Scan(&r.Cycle, &r.Status, &r.StartedAt, &r.Requests, &r.Seen,
			&r.Fresh, &r.Updated, &r.Removed, &r.Errors); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}
	return out, nil
}

func (s *Store) DBListingsCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM listings`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count listings: %w", err)
	}
	return n, nil
}

func (s *Store) DBActiveListingsCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM listings WHERE removed_at IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active listings: %w", err)
	}
	return n, nil
}

func (s *Store) DBHistoryCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM listing_history`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count history: %w", err)
	}
	return n, nil
}

// GetListingJSON fetches and transparently decompresses the full JSON snapshot
// for a single listing ID.
func (s *Store) GetListingJSON(ctx context.Context, id int64) (string, error) {
	var raw []byte
	if err := s.ro.QueryRowContext(ctx, `SELECT data_json FROM listings WHERE id = ?`, id).Scan(&raw); err != nil {
		return "", err
	}
	return DecompressJSON(raw)
}

// DBSizeMB returns the on-disk size of the SQLite database, including the
// WAL and SHM sidecar files (the WAL can hold most of the data on a 24/7
// writer, so ignoring it lies about real usage).
func (s *Store) DBSizeMB() float64 {
	var total int64
	for _, p := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return float64(total) / (1 << 20)
}

// Checkpoint truncates the WAL so the main .db file reflects all data.
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}
	return nil
}

// SetListingURLs stores canonical aqar.fm URLs (from the web API enrichment).
func (s *Store) SetListingURLs(ctx context.Context, urls map[int64]string) error {
	if len(urls) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin url tx: %w", err)
	}
	defer tx.Rollback()
	upd, err := tx.PrepareContext(ctx, `UPDATE listings SET url = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare url update: %w", err)
	}
	defer upd.Close()
	for id, u := range urls {
		if _, err := upd.ExecContext(ctx, u, id); err != nil {
			return fmt.Errorf("set url %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit url tx: %w", err)
	}
	return nil
}

// HistoryRow is one archived version of a listing.
type HistoryRow struct {
	FetchedAt  string
	ChangeType string
	DataJSON   string
}

// ListingHistory returns the version timeline for one listing (newest first).
func (s *Store) ListingHistory(ctx context.Context, id int64, limit int) ([]HistoryRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT fetched_at, change_type, data_json FROM listing_history
		WHERE listing_id = ? ORDER BY id DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("query listing history: %w", err)
	}
	defer rows.Close()
	var out []HistoryRow
	for rows.Next() {
		var h HistoryRow
		var raw []byte
		if err := rows.Scan(&h.FetchedAt, &h.ChangeType, &raw); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		if len(raw) > 0 {
			h.DataJSON, _ = DecompressJSON(raw)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate history: %w", err)
	}
	return out, nil
}

// FieldDiff is one changed field between two listing versions.
type FieldDiff struct {
	Field string `json:"field"`
	From  any    `json:"from"`
	To    any    `json:"to"`
}

// DiffJSON computes top-level + location/user field differences between two
// listing snapshots (older, newer).
func DiffJSON(oldJSON, newJSON string) []FieldDiff {
	var diffs []FieldDiff
	var oldM, newM map[string]any
	if err := json.Unmarshal([]byte(oldJSON), &oldM); err != nil {
		return nil
	}
	if err := json.Unmarshal([]byte(newJSON), &newM); err != nil {
		return nil
	}
	// flatten nested objects we care about
	flatten := func(m map[string]any) map[string]any {
		out := make(map[string]any, len(m)+8)
		for k, v := range m {
			switch k {
			case "location", "user":
				if sub, ok := v.(map[string]any); ok {
					for sk, sv := range sub {
						out[k+"."+sk] = sv
					}
					continue
				}
			}
			out[k] = v
		}
		return out
	}
	of, nf := flatten(oldM), flatten(newM)
	for k, nv := range nf {
		ov, existed := of[k]
		if !existed {
			diffs = append(diffs, FieldDiff{Field: k, From: nil, To: nv})
			continue
		}
		if !jsonEqual(ov, nv) {
			diffs = append(diffs, FieldDiff{Field: k, From: ov, To: nv})
		}
	}
	for k, ov := range of {
		if _, existed := nf[k]; !existed {
			diffs = append(diffs, FieldDiff{Field: k, From: ov, To: nil})
		}
	}
	return diffs
}

func jsonEqual(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return bytes.Equal(aj, bj)
}

// ExportRow is one listing row for the CSV export.
type ExportRow struct {
	ID           int64
	Category     sql.NullInt64
	Price        sql.NullFloat64
	Area         sql.NullFloat64
	Beds         sql.NullInt64
	Rooms        sql.NullInt64
	Furnished    sql.NullInt64
	CityID       sql.NullInt64
	DistrictID   sql.NullInt64
	Lat          sql.NullFloat64
	Lng          sql.NullFloat64
	Address      string
	URL          sql.NullString
	FirstSeenAt  string
	LastSeenAt   string
	LastChangeAt string
	RemovedAt    sql.NullString
}

// ExportListings streams all current listings (optionally one category).
func (s *Store) ExportListings(ctx context.Context, category int, cb func(ExportRow) error) error {
	q := `SELECT id, category, price, area, beds, rooms, furnished, city_id,
		district_id, lat, lng, data_json, url, first_seen_at, last_seen_at,
		last_change_at, removed_at FROM listings`
	var args []any
	if category > 0 {
		q += ` WHERE category = ?`
		args = append(args, category)
	}
	q += ` ORDER BY id`
	rows, err := s.ro.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("export query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r ExportRow
		var rawBlob []byte
		if err := rows.Scan(&r.ID, &r.Category, &r.Price, &r.Area, &r.Beds, &r.Rooms,
			&r.Furnished, &r.CityID, &r.DistrictID, &r.Lat, &r.Lng, &rawBlob,
			&r.URL, &r.FirstSeenAt, &r.LastSeenAt, &r.LastChangeAt, &r.RemovedAt); err != nil {
			return fmt.Errorf("export scan: %w", err)
		}
		if dataJSON, err := DecompressJSON(rawBlob); err == nil && dataJSON != "" {
			var m map[string]any
			if json.Unmarshal([]byte(dataJSON), &m) == nil {
				if a, ok := m["address"].(string); ok {
					r.Address = a
				}
			}
		}
		if err := cb(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("export iterate: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func intOrNil(m map[string]any, path ...string) any {
	v, ok := jsonAny(m, path...)
	if !ok {
		return nil
	}
	f := toFloat(v)
	return int(f)
}

func numOrNil(m map[string]any, path ...string) any {
	v, ok := jsonAny(m, path...)
	if !ok {
		return nil
	}
	if s, isStr := v.(string); isStr {
		if s == "" {
			return nil
		}
		var f float64
		if _, err := fmt.Sscanf(s, "%f", &f); err == nil {
			return f
		}
		return nil
	}
	return toFloat(v)
}

func strOrNil(m map[string]any, path ...string) any {
	v, ok := jsonAny(m, path...)
	if !ok {
		return nil
	}
	if s, isStr := v.(string); isStr && s != "" {
		return s
	}
	return nil
}
