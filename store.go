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
	"path/filepath"
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
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
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
		_, _ = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	}
	// Heal any run row falsely marked aborted by the pre-fix in-memory cycleSeen check after a hot-swap.
	_, _ = s.db.ExecContext(ctx, `
		UPDATE crawl_runs
		SET status = 'completed', errors = 0, last_error = ''
		WHERE status = 'aborted' AND errors = 1 AND listings_seen >= 50000
		  AND last_error LIKE '%REFUSING removed-detection pass%'`)
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
	_, err := s.db.ExecContext(ctx, `DELETE FROM crawl_state WHERE key IN ('resume', 'session_checkpoint')`)
	return err
}

// ResumeRun reopens an existing crawl_runs row when a cycle resumes after a
// hot-handoff or graceful restart so the cycle finishes as a single unified
// run instead of splitting into an "aborted" row. Falls back to StartRun if
// the prior row does not exist.
func (s *Store) ResumeRun(ctx context.Context, runID int64, cycle int) (int64, error) {
	if runID > 0 {
		res, err := s.db.ExecContext(ctx,
			`UPDATE crawl_runs SET status = 'running', finished_at = NULL, last_error = NULL WHERE id = ? AND cycle = ?`,
			runID, cycle)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				return runID, nil
			}
		}
	}
	return s.StartRun(ctx, cycle)
}

// SessionCheckpoint persists in-flight cycle counters, category progress,
// and cumulative uptime/restart counters across process restarts (such as
// Plan B <1s binary hot-handoffs).
type SessionCheckpoint struct {
	RunID           int64     `json:"run_id"`
	Cycle           int       `json:"cycle"`
	CycleStartedAt  string    `json:"cycle_started_at"`
	SetIndex        int       `json:"set_index"`
	CategoryID      int       `json:"category_id"`
	From            int       `json:"from"`
	PageSize        int       `json:"page_size"`
	Requests        int       `json:"requests"`
	Pages           int       `json:"pages"`
	TotalSeen       int       `json:"total_seen"`
	TotalNew        int       `json:"total_new"`
	TotalUpdated    int       `json:"total_updated"`
	CycleErrors     int       `json:"cycle_errors"`
	ActiveCat       int       `json:"active_cat"`
	Categories      []CatStat `json:"categories,omitempty"`
	PriorUptimeSecs int64     `json:"prior_uptime_secs"`
	RestartsCount   int       `json:"restarts_count"`
	Reason          string    `json:"reason"`
	SavedAt         string    `json:"saved_at"`
}

func (s *Store) SaveSessionCheckpoint(ctx context.Context, stateDir string, cp SessionCheckpoint) error {
	if cp.SavedAt == "" {
		cp.SavedAt = nowUTC()
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session checkpoint: %w", err)
	}
	if stateDir != "" {
		_ = writeFileAtomic(filepath.Join(stateDir, "session_checkpoint.json"), data, 0o644)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO crawl_state (key, value) VALUES ('session_checkpoint', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(data))
	if err != nil {
		return fmt.Errorf("save session checkpoint: %w", err)
	}
	return nil
}

func (s *Store) LoadSessionCheckpoint(ctx context.Context, stateDir string) (SessionCheckpoint, bool, error) {
	if stateDir != "" {
		if raw, err := os.ReadFile(filepath.Join(stateDir, "session_checkpoint.json")); err == nil {
			var cp SessionCheckpoint
			if json.Unmarshal(raw, &cp) == nil && cp.Cycle >= 0 {
				return cp, true, nil
			}
		}
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM crawl_state WHERE key = 'session_checkpoint'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionCheckpoint{}, false, nil
	}
	if err != nil {
		return SessionCheckpoint{}, false, fmt.Errorf("load session checkpoint: %w", err)
	}
	var cp SessionCheckpoint
	if err := json.Unmarshal([]byte(raw), &cp); err != nil {
		return SessionCheckpoint{}, false, fmt.Errorf("parse session checkpoint: %w", err)
	}
	return cp, true, nil
}

func (s *Store) ClearSessionCheckpoint(ctx context.Context, stateDir string) error {
	if stateDir != "" {
		_ = os.Remove(filepath.Join(stateDir, "session_checkpoint.json"))
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM crawl_state WHERE key = 'session_checkpoint'`)
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
		refresh    int64
		whatsapp   int64
		raw        []byte
		blob       []byte
		m          map[string]any // original decoded map (no re-marshal)
		changeType string         // "new" | "update" | "touch_blob" | "touch_meta"
	}
	// Step 1: Collapse duplicate ids in-page first (last occurrence wins) before
	// marshaling or hashing so duplicates cost zero CPU.
	rawByID := make(map[int64]map[string]any, len(listings))
	order := make([]int64, 0, len(listings))
	for _, l := range listings {
		id := int64(toFloat(l["id"]))
		if id <= 0 {
			continue
		}
		if _, exists := rawByID[id]; !exists {
			order = append(order, id)
		}
		rawByID[id] = l
	}
	if len(order) == 0 {
		return IngestionStats{}, nil
	}

	// Step 2: Canonical marshal + zero-alloc CanonicalContentHashBytes.
	// Defer CompressJSON until we know whether the listing actually needs a
	// blob write (new, update, or bumped refresh), saving ~90% CPU and ~80%
	// SQLite WAL write amplification on unchanged listings.
	items := make([]item, 0, len(order))
	ids := make([]int64, 0, len(order))
	for _, id := range order {
		l := rawByID[id]
		raw, err := json.Marshal(l) // keys are sorted by encoding/json -> canonical
		if err != nil {
			return IngestionStats{}, fmt.Errorf("canonical marshal listing %d: %w", id, err)
		}
		contentHash := CanonicalContentHashBytes(raw)
		ref := int64(toFloat(l["refresh"]))
		wa := int64(toFloat(l["whatsapp_requested_count"]))
		items = append(items, item{id: id, hash: contentHash, refresh: ref, whatsapp: wa, raw: raw, m: l})
		ids = append(ids, id)
	}

	// Step 3: Fetch current hash & refresh for these ids (chunked to stay well
	// below SQLite's host parameter limit regardless of page_size).
		type curState struct {
			hash     string
			refresh  int64
			whatsapp int64
		}
		current := make(map[int64]curState, len(ids))
		const queryChunk = 450
		for start := 0; start < len(ids); start += queryChunk {
			chunk := ids[start:min(len(ids), start+queryChunk)]
			placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
			args := make([]any, len(chunk))
			for i, id := range chunk {
				args[i] = id
			}
			rows, err := s.db.QueryContext(ctx,
				"SELECT id, data_hash, COALESCE(refresh, 0), COALESCE(whatsapp_count, 0) FROM listings WHERE id IN ("+placeholders+")",
				args...)
			if err != nil {
				return IngestionStats{}, fmt.Errorf("querying current hashes: %w", err)
			}
			for rows.Next() {
				var id, ref, wa int64
				var h string
				if err := rows.Scan(&id, &h, &ref, &wa); err != nil {
					rows.Close()
					return IngestionStats{}, fmt.Errorf("scan hash row: %w", err)
				}
				current[id] = curState{hash: h, refresh: ref, whatsapp: wa}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return IngestionStats{}, fmt.Errorf("iterating hash rows: %w", err)
			}
			rows.Close()
		}

		stats := IngestionStats{Seen: len(items)}
		for i := range items {
			cur, known := current[items[i].id]
			if !known {
				items[i].changeType = "new"
				items[i].blob = CompressJSON(items[i].raw)
				items[i].raw = nil
				stats.New++
				continue
			}
			if cur.hash != items[i].hash {
				// Lazy fallback check against legacy raw SHA-256 (for uncompacted v1 DBs)
				rawSum := sha256.Sum256(items[i].raw)
				rawHash := hex.EncodeToString(rawSum[:])
				if cur.hash != rawHash {
					items[i].changeType = "update"
					items[i].blob = CompressJSON(items[i].raw)
					items[i].raw = nil
					stats.Updated++
					continue
				}
				// Legacy hash matched -> migrate hash & compressed blob in-place without history row
				items[i].changeType = "touch_blob"
				items[i].blob = CompressJSON(items[i].raw)
				items[i].raw = nil
				continue
			}
			if cur.refresh != items[i].refresh || cur.whatsapp != items[i].whatsapp {
				// Substantive content unchanged, but refresh or whatsapp lead count changed -> update blob in-place
				items[i].changeType = "touch_blob"
				items[i].blob = CompressJSON(items[i].raw)
				items[i].raw = nil
				continue
			}
		// 100% unchanged (same canonical hash and same refresh) -> metadata-only touch!
		items[i].changeType = "touch_meta"
		items[i].raw = nil
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
			first_seen_at, last_seen_at, last_change_at, seen_cycle,
			initial_price, price_changes, whatsapp_count, property_age, street_width, street_direction, rent_period
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, ?,0,?,?,?,?,?)`)
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
			last_seen_at = ?, last_change_at = ?, removed_at = NULL, seen_cycle = ?,
			initial_price = COALESCE(initial_price, price, ?),
			price_changes = CASE WHEN price IS NOT NULL AND ? IS NOT NULL AND price != ? THEN COALESCE(price_changes, 0) + 1 ELSE COALESCE(price_changes, 0) END,
			whatsapp_count = ?, property_age = ?, street_width = ?, street_direction = ?, rent_period = ?
		WHERE id = ?`)
	if err != nil {
		return IngestionStats{}, fmt.Errorf("prepare update: %w", err)
	}
	defer upd.Close()

	// touchBlob refreshes the compressed snapshot and refresh timestamp when an
	// unchanged listing was bumped or migrated.
	touchBlob, err := tx.PrepareContext(ctx,
		`UPDATE listings SET data_json = ?, data_hash = ?, refresh = ?, whatsapp_count = ?, last_seen_at = ?, seen_cycle = ?, removed_at = NULL WHERE id = ?`)
	if err != nil {
		return IngestionStats{}, fmt.Errorf("prepare touchBlob: %w", err)
	}
	defer touchBlob.Close()

	// touchMeta updates only last_seen_at, seen_cycle, whatsapp_count, and clears
	// removed_at when neither canonical content nor refresh changed (zero blob rewrite).
	touchMeta, err := tx.PrepareContext(ctx,
		`UPDATE listings SET last_seen_at = ?, seen_cycle = ?, whatsapp_count = ?, removed_at = NULL WHERE id = ?`)
	if err != nil {
		return IngestionStats{}, fmt.Errorf("prepare touchMeta: %w", err)
	}
	defer touchMeta.Close()

	hist, err := tx.PrepareContext(ctx,
		`INSERT INTO listing_history (listing_id, change_type, data_json, data_hash, fetched_at)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return IngestionStats{}, fmt.Errorf("prepare history: %w", err)
	}
	defer hist.Close()

	for _, it := range items {
		switch it.changeType {
		case "new":
			v := buildListingValues(it.id, it.blob, it.hash, it.m, fetchedAt, cycle)
			if _, err := ins.ExecContext(ctx, v.insertArgs()...); err != nil {
				return IngestionStats{}, fmt.Errorf("insert listing %d: %w", it.id, err)
			}
			if _, err := hist.ExecContext(ctx, it.id, "new", it.blob, it.hash, fetchedAt); err != nil {
				return IngestionStats{}, fmt.Errorf("history insert %d: %w", it.id, err)
			}
		case "update":
			v := buildListingValues(it.id, it.blob, it.hash, it.m, fetchedAt, cycle)
			if _, err := upd.ExecContext(ctx, v.updateArgs()...); err != nil {
				return IngestionStats{}, fmt.Errorf("update listing %d: %w", it.id, err)
			}
			if _, err := hist.ExecContext(ctx, it.id, "update", it.blob, it.hash, fetchedAt); err != nil {
				return IngestionStats{}, fmt.Errorf("history update %d: %w", it.id, err)
			}
		case "touch_blob":
			var refArg any
			if it.refresh > 0 {
				refArg = int(it.refresh)
			}
			if _, err := touchBlob.ExecContext(ctx, it.blob, it.hash, refArg, it.whatsapp, fetchedAt, cycle, it.id); err != nil {
				return IngestionStats{}, fmt.Errorf("touchBlob listing %d: %w", it.id, err)
			}
		default: // touch_meta
			if _, err := touchMeta.ExecContext(ctx, fetchedAt, cycle, it.whatsapp, it.id); err != nil {
				return IngestionStats{}, fmt.Errorf("touchMeta listing %d: %w", it.id, err)
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
	id                                                                      int64
	blob                                                                    []byte
	hash                                                                    string
	category, price, meterPrice, area, beds, rooms, furnished, lat, lng     any
	provinceID, cityID, directionID, districtID, premium, boosted, status   any
	published, createTime, refresh, userID, userName                        any
	whatsappCount, propertyAge, streetWidth, streetDirection, rentPeriod    any
	fetchedAt                                                               string
	cycle                                                                   int
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
		userName:        strOrNil(m, "user", "name"),
		whatsappCount:   int64(toFloat(m["whatsapp_requested_count"])),
		propertyAge:     intOrNil(m, "age"),
		streetWidth:     intOrNil(m, "street_width"),
		streetDirection: intOrNil(m, "street_direction"),
		rentPeriod:      intOrNil(m, "rent_period"),
		fetchedAt:       fetchedAt, cycle: cycle,
	}
}

// insertArgs matches the 34-parameter INSERT statement.
func (v listingValues) insertArgs() []any {
	return []any{
		v.id, v.blob, v.hash,
		v.category, v.price, v.meterPrice, v.area, v.beds, v.rooms, v.furnished,
		v.lat, v.lng, v.provinceID, v.cityID, v.directionID, v.districtID,
		v.premium, v.boosted, v.status, v.published, v.createTime, v.refresh,
		v.userID, v.userName,
		v.fetchedAt, v.fetchedAt, v.fetchedAt, v.cycle,
		v.price, v.whatsappCount, v.propertyAge, v.streetWidth, v.streetDirection, v.rentPeriod,
	}
}

// updateArgs matches the UPDATE statement.
func (v listingValues) updateArgs() []any {
	return []any{
		v.blob, v.hash,
		v.category, v.price, v.meterPrice, v.area, v.beds, v.rooms, v.furnished,
		v.lat, v.lng, v.provinceID, v.cityID, v.directionID, v.districtID,
		v.premium, v.boosted, v.status, v.published, v.createTime, v.refresh,
		v.userID, v.userName,
		v.fetchedAt, v.fetchedAt, v.cycle,
		v.price, v.price, v.price,
		v.whatsappCount, v.propertyAge, v.streetWidth, v.streetDirection, v.rentPeriod,
		v.id,
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

func (s *Store) DBSeenInCycleCount(ctx context.Context, cycle int) (int64, error) {
	var n int64
	err := s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM listings WHERE seen_cycle = ?`, cycle).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count seen_cycle listings: %w", err)
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

// ListingSummary holds the current metadata and decompressed full JSON for a
// single listing, used by the Web Inspector and CLI -inspect flag.
type ListingSummary struct {
	ID              int64   `json:"id"`
	Category        int     `json:"category"`
	CategoryName    string  `json:"category_name"`
	Price           float64 `json:"price"`
	MeterPrice      float64 `json:"meter_price"`
	Area            float64 `json:"area"`
	Beds            int     `json:"beds"`
	Rooms           int     `json:"rooms"`
	Title           string  `json:"title"`
	Address         string  `json:"address"`
	URL             string  `json:"url"`
	FirstSeenAt     string  `json:"first_seen_at"`
	LastSeenAt      string  `json:"last_seen_at"`
	LastChangeAt    string  `json:"last_change_at"`
	RemovedAt       string  `json:"removed_at,omitempty"`
	SeenCycle       int     `json:"seen_cycle"`
	CompressedBytes int     `json:"compressed_bytes"`
	RawBytes        int     `json:"raw_bytes"`
	FullJSON        string  `json:"full_json"`
}

// InspectListing retrieves a listing's full metadata and decompressed JSON.
func (s *Store) InspectListing(ctx context.Context, id int64) (ListingSummary, error) {
	var out ListingSummary
	var rawBlob []byte
	var cat, beds, rooms sql.NullInt64
	var price, mprice, area sql.NullFloat64
	var urlStr, remAt sql.NullString

	err := s.ro.QueryRowContext(ctx, `
		SELECT id, category, price, meter_price, area, beds, rooms, url,
		       first_seen_at, last_seen_at, last_change_at, removed_at, seen_cycle, data_json
		FROM listings WHERE id = ?`, id).Scan(
		&out.ID, &cat, &price, &mprice, &area, &beds, &rooms, &urlStr,
		&out.FirstSeenAt, &out.LastSeenAt, &out.LastChangeAt, &remAt, &out.SeenCycle, &rawBlob,
	)
	if err != nil {
		return out, err
	}
	out.Category = int(cat.Int64)
	out.CategoryName = categoryNames[out.Category]
	out.Price = price.Float64
	out.MeterPrice = mprice.Float64
	out.Area = area.Float64
	out.Beds = int(beds.Int64)
	out.Rooms = int(rooms.Int64)
	out.URL = urlStr.String
	out.RemovedAt = remAt.String
	out.CompressedBytes = len(rawBlob)

	if plain, err := DecompressJSON(rawBlob); err == nil {
		out.FullJSON = plain
		out.RawBytes = len(plain)
		var meta struct {
			Title     string `json:"title"`
			UserTitle string `json:"user_title"`
			Address   string `json:"address"`
		}
		if json.Unmarshal([]byte(plain), &meta) == nil {
			out.Title = meta.Title
			if out.Title == "" {
				out.Title = meta.UserTitle
			}
			out.Address = meta.Address
		}
	}
	return out, nil
}

// PriceChangeEvent represents a recent listing update (especially price drops
// and increases) detected across crawl cycles.
type PriceChangeEvent struct {
	ID            int64    `json:"id"`
	Category      int      `json:"category"`
	CategoryName  string   `json:"category_name"`
	Title         string   `json:"title"`
	Address       string   `json:"address"`
	Area          float64  `json:"area"`
	OldPrice      float64  `json:"old_price"`
	NewPrice      float64  `json:"new_price"`
	PriceDelta    float64  `json:"price_delta"`
	PricePct      float64  `json:"price_pct"`
	ChangedFields []string `json:"changed_fields"`
	ChangedAt     string   `json:"changed_at"`
	URL           string   `json:"url,omitempty"`
	Active        bool     `json:"active"`
}

// RecentChanges returns the most recently updated listings in the archive,
// walking listing_history in reverse primary-key order (O(1) index scan) and
// comparing each listing's latest two snapshots to surface price drops, price
// hikes, and modified fields.
func (s *Store) RecentChanges(ctx context.Context, limit int, onlyDrops bool) ([]PriceChangeEvent, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	scanLimit := limit * 3
	if onlyDrops {
		scanLimit = limit * 8
	}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT h.listing_id, COALESCE(l.category, 0), COALESCE(l.price, 0), COALESCE(l.area, 0),
		       COALESCE(l.url, ''), h.fetched_at, l.removed_at IS NULL
		FROM listing_history h
		JOIN listings l ON l.id = h.listing_id
		WHERE h.change_type = 'update'
		ORDER BY h.id DESC
		LIMIT ?`, scanLimit)
	if err != nil {
		return nil, fmt.Errorf("query recent changes: %w", err)
	}
	type cand struct {
		id        int64
		cat       int
		price     float64
		area      float64
		url       string
		changedAt string
		active    bool
	}
	var cands []cand
	seenID := make(map[int64]struct{}, scanLimit)
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.cat, &c.price, &c.area, &c.url, &c.changedAt, &c.active); err == nil {
			if _, dup := seenID[c.id]; !dup {
				seenID[c.id] = struct{}{}
				cands = append(cands, c)
			}
		}
	}
	rows.Close()

	out := make([]PriceChangeEvent, 0, limit)
	for _, c := range cands {
		if len(out) >= limit {
			break
		}
		// Fetch the 2 most recent non-removed snapshots for this listing.
		hrows, err := s.ro.QueryContext(ctx, `
			SELECT data_json, fetched_at FROM listing_history
			WHERE listing_id = ? AND change_type IN ('new', 'update') AND data_json IS NOT NULL
			ORDER BY id DESC LIMIT 2`, c.id)
		if err != nil {
			continue
		}
		var blobs [][]byte
		var latestAt string
		for hrows.Next() {
			var b []byte
			var ft string
			if hrows.Scan(&b, &ft) == nil && len(b) > 0 {
				if latestAt == "" {
					latestAt = ft
				}
				blobs = append(blobs, b)
			}
		}
		hrows.Close()
		if len(blobs) < 2 {
			continue
		}
		newBytes, err1 := DecompressJSONBytes(blobs[0])
		oldBytes, err2 := DecompressJSONBytes(blobs[1])
		if err1 != nil || err2 != nil {
			continue
		}
		var newSnap, oldSnap struct {
			Price     float64 `json:"price"`
			Title     string  `json:"title"`
			UserTitle string  `json:"user_title"`
			Address   string  `json:"address"`
			Status    int     `json:"status"`
			Content   string  `json:"content"`
			Area      float64 `json:"area"`
		}
		if json.Unmarshal(newBytes, &newSnap) != nil || json.Unmarshal(oldBytes, &oldSnap) != nil {
			continue
		}
		delta := newSnap.Price - oldSnap.Price
		if onlyDrops && delta >= 0 {
			continue
		}
		var pct float64
		if oldSnap.Price > 0 {
			pct = (delta / oldSnap.Price) * 100
		}
		var fields []string
		if newSnap.Price != oldSnap.Price {
			fields = append(fields, "price")
		}
		if newSnap.Title != oldSnap.Title || newSnap.UserTitle != oldSnap.UserTitle {
			fields = append(fields, "title")
		}
		if newSnap.Content != oldSnap.Content {
			fields = append(fields, "content")
		}
		if newSnap.Status != oldSnap.Status {
			fields = append(fields, "status")
		}
		if newSnap.Area != oldSnap.Area {
			fields = append(fields, "area")
		}
		if len(fields) == 0 {
			diffs := DiffJSON(string(oldBytes), string(newBytes))
			for _, d := range diffs {
				fields = append(fields, d.Field)
				if len(fields) >= 4 {
					break
				}
			}
		}
		title := newSnap.Title
		if title == "" {
			title = newSnap.UserTitle
		}
		if title == "" {
			title = newSnap.Address
		}
		out = append(out, PriceChangeEvent{
			ID:            c.id,
			Category:      c.cat,
			CategoryName:  categoryNames[c.cat],
			Title:         title,
			Address:       newSnap.Address,
			Area:          c.area,
			OldPrice:      oldSnap.Price,
			NewPrice:      newSnap.Price,
			PriceDelta:    delta,
			PricePct:      pct,
			ChangedFields: fields,
			ChangedAt:     latestAt,
			URL:           c.url,
			Active:        c.active,
		})
	}
	return out, nil
}

// CategoryAnalytics summarizes archive counts and pricing for one category.
type CategoryAnalytics struct {
	CategoryID   int     `json:"category_id"`
	CategoryName string  `json:"category_name"`
	Active       int64   `json:"active"`
	Removed      int64   `json:"removed"`
	AvgPrice     float64 `json:"avg_price"`
	AvgMeter     float64 `json:"avg_meter"`
}

// ArchiveAnalyticsSummary provides high-level market & storage metrics.
type ArchiveAnalyticsSummary struct {
	TotalListings   int64               `json:"total_listings"`
	ActiveListings  int64               `json:"active_listings"`
	RemovedListings int64               `json:"removed_listings"`
	UpdatedListings int64               `json:"updated_listings"`
	TotalHistory    int64               `json:"total_history"`
	AvgBlobBytes    int                 `json:"avg_blob_bytes"`
	Categories      []CategoryAnalytics `json:"categories"`
}

// ArchiveAnalytics computes active/removed breakdowns and category market stats.
func (s *Store) ArchiveAnalytics(ctx context.Context) (ArchiveAnalyticsSummary, error) {
	var out ArchiveAnalyticsSummary
	_ = s.ro.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN removed_at IS NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN removed_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		       CAST(COALESCE(AVG(LENGTH(data_json)), 0) AS INTEGER)
		FROM listings`).Scan(
		&out.TotalListings, &out.ActiveListings, &out.RemovedListings, &out.AvgBlobBytes,
	)
	_ = s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM listing_history`).Scan(&out.TotalHistory)
	_ = s.ro.QueryRowContext(ctx, `SELECT COUNT(DISTINCT listing_id) FROM listing_history WHERE change_type = 'update'`).Scan(&out.UpdatedListings)

	rows, err := s.ro.QueryContext(ctx, `
		SELECT COALESCE(category, 0),
		       SUM(CASE WHEN removed_at IS NULL THEN 1 ELSE 0 END) AS act,
		       SUM(CASE WHEN removed_at IS NOT NULL THEN 1 ELSE 0 END) AS rem,
		       COALESCE(AVG(CASE WHEN removed_at IS NULL AND price > 0 THEN price END), 0),
		       COALESCE(AVG(CASE WHEN removed_at IS NULL AND meter_price > 0 THEN meter_price END), 0)
		FROM listings
		WHERE category IS NOT NULL
		GROUP BY category
		ORDER BY act DESC
		LIMIT 20`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c CategoryAnalytics
		if err := rows.Scan(&c.CategoryID, &c.Active, &c.Removed, &c.AvgPrice, &c.AvgMeter); err == nil {
			c.CategoryName = categoryNames[c.CategoryID]
			out.Categories = append(out.Categories, c)
		}
	}
	return out, rows.Err()
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

// SetListingURLs stores canonical aqar.fm URLs (from the web API enrichment),
// skipping rows whose url is already identical to avoid redundant WAL writes.
func (s *Store) SetListingURLs(ctx context.Context, urls map[int64]string) error {
	if len(urls) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin url tx: %w", err)
	}
	defer tx.Rollback()
	upd, err := tx.PrepareContext(ctx, `UPDATE listings SET url = ? WHERE id = ? AND (url IS NULL OR url != ?)`)
	if err != nil {
		return fmt.Errorf("prepare url update: %w", err)
	}
	defer upd.Close()
	for id, u := range urls {
		if _, err := upd.ExecContext(ctx, u, id, u); err != nil {
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
		if dataBytes, err := DecompressJSONBytes(rawBlob); err == nil && len(dataBytes) > 0 {
			var addrOnly struct {
				Address string `json:"address"`
			}
			if json.Unmarshal(dataBytes, &addrOnly) == nil {
				r.Address = addrOnly.Address
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
