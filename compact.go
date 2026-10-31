package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
)

// CompactStats reports the before/after metrics of a database compaction pass.
type CompactStats struct {
	OldSizeMB          float64
	NewSizeMB          float64
	ListingsTotal      int64
	ZombiesHealed      int64
	HistoryOldRows     int64
	HistoryNewRows     int64
	NoiseUpdatesPurged int64
	FalseRemovalsPurged int64
	Duration           time.Duration
}

// CompactDB rebuilds the SQLite archive into a tightly-packed, dictionary-
// compressed database file. It preserves 100% of all listings (with full JSON
// compressed via AQZ1 dictionary-deflate), heals zombie removed_at flags caused
// by earlier touch bugs, strips pure-noise history rows (backend empty-array
// jitter and volatile hit-counter ticks) and false pagination-drift bounce
// removals, and atomically replaces the database file after integrity verification.
func CompactDB(ctx context.Context, dbPath string, log *Logger) (CompactStats, error) {
	start := time.Now()
	var st CompactStats

	if dbPath == "" {
		dbPath = "AQAR.db"
	}

	// Measure initial size (including -wal and -shm).
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if fi, err := os.Stat(p); err == nil {
			st.OldSizeMB += float64(fi.Size()) / (1 << 20)
		}
	}

	// Step 1: Open source DB briefly in read-write mode to checkpoint any WAL.
	if srcRW, err := OpenStore(dbPath); err == nil {
		_ = srcRW.Checkpoint(ctx)
		_ = srcRW.Close()
	}

	srcDB, err := sql.Open(sqliteDriver, "file:"+dbPath+"?mode=ro")
	if err != nil {
		return st, fmt.Errorf("open source db: %w", err)
	}
	defer func() {
		if srcDB != nil {
			_ = srcDB.Close()
		}
	}()
	srcDB.SetMaxOpenConns(1)
	_, _ = srcDB.ExecContext(ctx, "PRAGMA temp_store=MEMORY; PRAGMA cache_size=-64000;")

	_ = srcDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM listings").Scan(&st.ListingsTotal)
	_ = srcDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM listing_history").Scan(&st.HistoryOldRows)

	var maxCompletedCycle int
	_ = srcDB.QueryRowContext(ctx, "SELECT COALESCE(MAX(cycle), 0) FROM crawl_runs WHERE status = 'completed'").Scan(&maxCompletedCycle)

	if log != nil {
		log.Infof("compaction starting on %s (%.1f MB | %d listings | %d history rows | max completed cycle=%d)",
			dbPath, st.OldSizeMB, st.ListingsTotal, st.HistoryOldRows, maxCompletedCycle)
	}

	compactPath := dbPath + ".compact"
	_ = os.Remove(compactPath)
	_ = os.Remove(compactPath + "-wal")
	_ = os.Remove(compactPath + "-shm")

	dstDB, err := sql.Open(sqliteDriver, compactPath)
	if err != nil {
		return st, fmt.Errorf("create compact db: %w", err)
	}
	defer func() {
		if dstDB != nil {
			_ = dstDB.Close()
		}
	}()
	dstDB.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA page_size=4096",
		"PRAGMA journal_mode=OFF",
		"PRAGMA synchronous=OFF",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA cache_size=-128000",
	} {
		if _, err := dstDB.ExecContext(ctx, pragma); err != nil {
			return st, fmt.Errorf("dst %s: %w", pragma, err)
		}
	}

	// Step 2: Create tables in dstDB WITHOUT secondary indexes for fast bulk load.
	schemaSQL := `
	CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
	CREATE TABLE listings (
		id            INTEGER PRIMARY KEY,
		data_json     TEXT    NOT NULL,
		data_hash     TEXT    NOT NULL,
		category      INTEGER,
		price         REAL,
		meter_price   REAL,
		area          REAL,
		beds          INTEGER,
		rooms         INTEGER,
		furnished     INTEGER,
		lat           REAL,
		lng           REAL,
		province_id   INTEGER,
		city_id       INTEGER,
		direction_id  INTEGER,
		district_id   INTEGER,
		premium       INTEGER,
		boosted       INTEGER,
		status        INTEGER,
		published     INTEGER,
		create_time   INTEGER,
		refresh       INTEGER,
		user_id       INTEGER,
		user_name     TEXT,
		first_seen_at TEXT NOT NULL,
		last_seen_at  TEXT NOT NULL,
		last_change_at TEXT NOT NULL,
		removed_at    TEXT,
		seen_cycle    INTEGER NOT NULL DEFAULT 0,
		url           TEXT
	);
	CREATE TABLE listing_history (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		listing_id  INTEGER NOT NULL,
		change_type TEXT    NOT NULL,
		data_json   TEXT,
		data_hash   TEXT,
		fetched_at  TEXT NOT NULL
	);
	CREATE TABLE crawl_runs (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		cycle           INTEGER NOT NULL,
		started_at      TEXT NOT NULL,
		finished_at     TEXT,
		status          TEXT NOT NULL,
		requests        INTEGER NOT NULL DEFAULT 0,
		pages           INTEGER NOT NULL DEFAULT 0,
		listings_seen   INTEGER NOT NULL DEFAULT 0,
		listings_new    INTEGER NOT NULL DEFAULT 0,
		listings_updated INTEGER NOT NULL DEFAULT 0,
		listings_removed INTEGER NOT NULL DEFAULT 0,
		errors          INTEGER NOT NULL DEFAULT 0,
		last_error      TEXT
	);
	CREATE TABLE crawl_state (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE live_stats (
		id         INTEGER PRIMARY KEY CHECK (id = 1),
		data_json  TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);`
	if _, err := dstDB.ExecContext(ctx, schemaSQL); err != nil {
		return st, fmt.Errorf("create compact schema: %w", err)
	}

	// Copy metadata tables (schema_migrations, crawl_runs, crawl_state, live_stats).
	if err := copyMetadataTables(ctx, srcDB, dstDB); err != nil {
		return st, fmt.Errorf("copy metadata: %w", err)
	}

	workers := max(2, runtime.NumCPU())

	// Step 3: Stream, heal, and compress `listings` in parallel batches.
	// Track each listing's final removed_at (empty if active) so step 4 knows
	// whether a listing is currently removed.
	removedAtByID := make(map[int64]string, 64000)

	if err := compactListingsTable(ctx, srcDB, dstDB, maxCompletedCycle, workers, &st, removedAtByID, log); err != nil {
		return st, fmt.Errorf("compact listings: %w", err)
	}

	// Step 4: Stream, deduplicate noise, and compress `listing_history`.
	if err := compactHistoryTable(ctx, srcDB, dstDB, workers, &st, removedAtByID, log); err != nil {
		return st, fmt.Errorf("compact history: %w", err)
	}

	// Update live_stats totals to reflect the compacted history row count.
	var lsRaw string
	if dstDB.QueryRowContext(ctx, "SELECT data_json FROM live_stats WHERE id = 1").Scan(&lsRaw) == nil && lsRaw != "" {
		var ls LiveStats
		if json.Unmarshal([]byte(lsRaw), &ls) == nil {
			ls.TotalListings = st.ListingsTotal
			ls.TotalHistory = st.HistoryNewRows
			if b, err := json.Marshal(ls); err == nil {
				_, _ = dstDB.ExecContext(ctx, "UPDATE live_stats SET data_json = ? WHERE id = 1", string(b))
			}
		}
	}

	// Step 5: Build secondary indexes on tightly-packed tables.
	if log != nil {
		log.Infof("compaction: building indexes...")
	}
	indexSQL := `
	CREATE INDEX idx_listings_category  ON listings(category);
	CREATE INDEX idx_listings_district  ON listings(district_id);
	CREATE INDEX idx_listings_city      ON listings(city_id);
	CREATE INDEX idx_listings_price     ON listings(price);
	CREATE INDEX idx_listings_removed   ON listings(removed_at);
	CREATE INDEX idx_listings_seencycle ON listings(seen_cycle);
	CREATE INDEX idx_history_listing    ON listing_history(listing_id, fetched_at);
	ANALYZE;`
	if _, err := dstDB.ExecContext(ctx, indexSQL); err != nil {
		return st, fmt.Errorf("create indexes: %w", err)
	}

	// Step 6: Verify row count and integrity before swapping.
	var newListingsCount int64
	if err := dstDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM listings").Scan(&newListingsCount); err != nil {
		return st, fmt.Errorf("verify listings count: %w", err)
	}
	if newListingsCount != st.ListingsTotal {
		return st, fmt.Errorf("listings count mismatch: old=%d new=%d", st.ListingsTotal, newListingsCount)
	}
	var integrity string
	if err := dstDB.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return st, fmt.Errorf("integrity check failed: %s (%v)", integrity, err)
	}

	// Switch compact DB back to production WAL mode before closing.
	_, _ = dstDB.ExecContext(ctx, "PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA wal_checkpoint(TRUNCATE);")
	_ = dstDB.Close()
	dstDB = nil
	_ = srcDB.Close()
	srcDB = nil

	// Step 7: Atomic swap (keep a temporary backup name during rename).
	backupPath := dbPath + ".bak"
	_ = os.Remove(backupPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	if err := os.Rename(dbPath, backupPath); err != nil {
		return st, fmt.Errorf("rename old db to .bak: %w", err)
	}
	if err := os.Rename(compactPath, dbPath); err != nil {
		_ = os.Rename(backupPath, dbPath)
		return st, fmt.Errorf("rename compact db to %s: %w", dbPath, err)
	}
	_ = os.Remove(backupPath)
	_ = os.Remove(compactPath + "-wal")
	_ = os.Remove(compactPath + "-shm")

	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if fi, err := os.Stat(p); err == nil {
			st.NewSizeMB += float64(fi.Size()) / (1 << 20)
		}
	}
	st.Duration = time.Since(start)

	if log != nil {
		log.Infof("compaction complete in %v: %.1f MB -> %.1f MB (saved %.1f MB, %.1fx smaller) | listings=%d (zombies healed=%d) | history=%d -> %d (purged %d noise updates, %d false removals)",
			st.Duration.Round(time.Millisecond), st.OldSizeMB, st.NewSizeMB, st.OldSizeMB-st.NewSizeMB,
			st.OldSizeMB/max(0.1, st.NewSizeMB),
			st.ListingsTotal, st.ZombiesHealed, st.HistoryOldRows, st.HistoryNewRows,
			st.NoiseUpdatesPurged, st.FalseRemovalsPurged)
	}
	return st, nil
}

func copyMetadataTables(ctx context.Context, src, dst *sql.DB) error {
	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// schema_migrations
	mrows, err := src.QueryContext(ctx, "SELECT version, applied_at FROM schema_migrations")
	if err == nil {
		for mrows.Next() {
			var v int
			var a string
			if mrows.Scan(&v, &a) == nil {
				_, _ = tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", v, a)
			}
		}
		mrows.Close()
	}

	// crawl_runs (clean up any orphaned 'running' statuses from old crashes)
	rrows, err := src.QueryContext(ctx, `
		SELECT id, cycle, started_at, finished_at, status, requests, pages,
		       listings_seen, listings_new, listings_updated, listings_removed, errors, last_error
		FROM crawl_runs ORDER BY id ASC`)
	if err == nil {
		for rrows.Next() {
			var id, cycle, req, pg, seen, nw, upd, rem, errs int64
			var start, status string
			var fin, lerr sql.NullString
			if rrows.Scan(&id, &cycle, &start, &fin, &status, &req, &pg, &seen, &nw, &upd, &rem, &errs, &lerr) == nil {
				if status == "running" {
					status = "aborted"
					if !lerr.Valid || lerr.String == "" {
						lerr = sql.NullString{String: "interrupted", Valid: true}
					}
				}
				_, _ = tx.ExecContext(ctx, `
					INSERT INTO crawl_runs (id, cycle, started_at, finished_at, status, requests, pages,
						listings_seen, listings_new, listings_updated, listings_removed, errors, last_error)
					VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					id, cycle, start, fin, status, req, pg, seen, nw, upd, rem, errs, lerr)
			}
		}
		rrows.Close()
	}

	// crawl_state
	srows, err := src.QueryContext(ctx, "SELECT key, value FROM crawl_state")
	if err == nil {
		for srows.Next() {
			var k, v string
			if srows.Scan(&k, &v) == nil {
				_, _ = tx.ExecContext(ctx, "INSERT INTO crawl_state (key, value) VALUES (?, ?)", k, v)
			}
		}
		srows.Close()
	}

	// live_stats
	lrows, err := src.QueryContext(ctx, "SELECT id, data_json, updated_at FROM live_stats")
	if err == nil {
		for lrows.Next() {
			var id int
			var dj, up string
			if lrows.Scan(&id, &dj, &up) == nil {
				_, _ = tx.ExecContext(ctx, "INSERT INTO live_stats (id, data_json, updated_at) VALUES (?, ?, ?)", id, dj, up)
			}
		}
		lrows.Close()
	}

	return tx.Commit()
}

type rawListingRow struct {
	id                                                                    int64
	rawData                                                               []byte
	dataHash                                                              string
	category, beds, rooms, furnished                                      sql.NullInt64
	price, meterPrice, area, lat, lng                                     sql.NullFloat64
	provinceID, cityID, directionID, districtID                           sql.NullInt64
	premium, boosted, status, published, createTime, refresh, userID      sql.NullInt64
	userName, url                                                         sql.NullString
	firstSeenAt, lastSeenAt, lastChangeAt                                 string
	removedAt                                                             sql.NullString
	seenCycle                                                             int
	// computed by worker:
	compBlob []byte
	newHash  string
}

func compactListingsTable(
	ctx context.Context,
	src, dst *sql.DB,
	maxCompletedCycle int,
	workers int,
	st *CompactStats,
	removedAtByID map[int64]string,
	log *Logger,
) error {
	// Check whether `url` column exists in source DB.
	hasURL := true
	if r, err := src.QueryContext(ctx, "SELECT url FROM listings LIMIT 1"); err != nil {
		hasURL = false
	} else {
		_ = r.Close()
	}
	q := `SELECT id, data_json, data_hash, category, price, meter_price, area, beds, rooms,
		furnished, lat, lng, province_id, city_id, direction_id, district_id,
		premium, boosted, status, published, create_time, refresh, user_id, user_name,
		first_seen_at, last_seen_at, last_change_at, removed_at, seen_cycle`
	if hasURL {
		q += `, url`
	}
	q += ` FROM listings ORDER BY id ASC`

	rows, err := src.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()

	const batchSize = 2500
	batch := make([]rawListingRow, 0, batchSize)
	processed := int64(0)

	flushBatch := func(items []rawListingRow) error {
		// Parallel compress + canonical hash across workers
		var wg sync.WaitGroup
		chunk := (len(items) + workers - 1) / workers
		for w := 0; w < workers; w++ {
			lo := w * chunk
			if lo >= len(items) {
				break
			}
			hi := min(len(items), lo+chunk)
			wg.Add(1)
			go func(slice []rawListingRow) {
				defer wg.Done()
				for i := range slice {
					var plainBytes []byte
					if IsCompressedJSON(slice[i].rawData) {
						if plainStr, err := DecompressJSON(slice[i].rawData); err == nil && plainStr != "" {
							plainBytes = []byte(plainStr)
						} else {
							plainBytes = slice[i].rawData
						}
						slice[i].compBlob = slice[i].rawData
					} else {
						plainBytes = slice[i].rawData
						slice[i].compBlob = CompressJSON(plainBytes)
					}
					slice[i].newHash = CanonicalContentHashBytes(plainBytes)
					slice[i].rawData = nil // free raw slice immediately
				}
			}(items[lo:hi])
		}
		wg.Wait()

		tx, err := dst.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		ins, err := tx.PrepareContext(ctx, `
			INSERT INTO listings (
				id, data_json, data_hash, category, price, meter_price, area, beds, rooms,
				furnished, lat, lng, province_id, city_id, direction_id, district_id,
				premium, boosted, status, published, create_time, refresh, user_id, user_name,
				first_seen_at, last_seen_at, last_change_at, removed_at, seen_cycle, url
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()

		for i := range items {
			it := &items[i]
			// Heal zombie removed_at: if seen in the latest completed cycle, it is NOT removed.
			if maxCompletedCycle > 0 && it.seenCycle >= maxCompletedCycle && it.removedAt.Valid {
				it.removedAt = sql.NullString{}
				st.ZombiesHealed++
			}
			if it.removedAt.Valid && it.removedAt.String != "" {
				removedAtByID[it.id] = it.removedAt.String
			}
			if _, err := ins.ExecContext(ctx,
				it.id, it.compBlob, it.newHash,
				it.category, it.price, it.meterPrice, it.area, it.beds, it.rooms,
				it.furnished, it.lat, it.lng, it.provinceID, it.cityID, it.directionID, it.districtID,
				it.premium, it.boosted, it.status, it.published, it.createTime, it.refresh, it.userID, it.userName,
				it.firstSeenAt, it.lastSeenAt, it.lastChangeAt, it.removedAt, it.seenCycle, it.url,
			); err != nil {
				return fmt.Errorf("insert compacted listing %d: %w", it.id, err)
			}
		}
		return tx.Commit()
	}

	for rows.Next() {
		var r rawListingRow
		var err error
		if hasURL {
			err = rows.Scan(
				&r.id, &r.rawData, &r.dataHash, &r.category, &r.price, &r.meterPrice, &r.area, &r.beds, &r.rooms,
				&r.furnished, &r.lat, &r.lng, &r.provinceID, &r.cityID, &r.directionID, &r.districtID,
				&r.premium, &r.boosted, &r.status, &r.published, &r.createTime, &r.refresh, &r.userID, &r.userName,
				&r.firstSeenAt, &r.lastSeenAt, &r.lastChangeAt, &r.removedAt, &r.seenCycle, &r.url,
			)
		} else {
			err = rows.Scan(
				&r.id, &r.rawData, &r.dataHash, &r.category, &r.price, &r.meterPrice, &r.area, &r.beds, &r.rooms,
				&r.furnished, &r.lat, &r.lng, &r.provinceID, &r.cityID, &r.directionID, &r.districtID,
				&r.premium, &r.boosted, &r.status, &r.published, &r.createTime, &r.refresh, &r.userID, &r.userName,
				&r.firstSeenAt, &r.lastSeenAt, &r.lastChangeAt, &r.removedAt, &r.seenCycle,
			)
		}
		if err != nil {
			return fmt.Errorf("scan listing: %w", err)
		}
		batch = append(batch, r)
		if len(batch) >= batchSize {
			if err := flushBatch(batch); err != nil {
				return err
			}
			processed += int64(len(batch))
			batch = batch[:0]
			if log != nil && processed%25000 == 0 {
				log.Infof("compaction: listings %d / %d compressed", processed, st.ListingsTotal)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(batch) > 0 {
		if err := flushBatch(batch); err != nil {
			return err
		}
	}
	return nil
}

type rawHistRow struct {
	listingID  int64
	changeType string
	rawData    []byte
	fetchedAt  string
	// computed by worker:
	plainBytes []byte
	contentHash string
	compBlob   []byte
}

func compactHistoryTable(
	ctx context.Context,
	src, dst *sql.DB,
	workers int,
	st *CompactStats,
	removedAtByID map[int64]string,
	log *Logger,
) error {
	// Skip reading the 3.51 GB of redundant data_json on 'removed' rows directly in SQL!
	rows, err := src.QueryContext(ctx, `
		SELECT listing_id, change_type,
		       CASE WHEN change_type = 'removed' THEN NULL ELSE data_json END,
		       fetched_at
		FROM listing_history
		ORDER BY id ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	// lastHash tracks the most recent kept CanonicalContentHash per listing_id.
	lastHash := make(map[int64]string, st.ListingsTotal+1024)
	// lastRemAt tracks the latest 'removed' timestamp for listings that are still
	// genuinely removed at the end of the timeline.
	lastRemAt := make(map[int64]string, len(removedAtByID)+1024)

	const batchSize = 4000
	batch := make([]rawHistRow, 0, batchSize)
	scanned := int64(0)

	processBatch := func(items []rawHistRow) error {
		// Step A: In parallel, compute CanonicalContentHash for non-removed rows.
		var wg sync.WaitGroup
		chunk := (len(items) + workers - 1) / workers
		for w := 0; w < workers; w++ {
			lo := w * chunk
			if lo >= len(items) {
				break
			}
			hi := min(len(items), lo+chunk)
			wg.Add(1)
			go func(slice []rawHistRow) {
				defer wg.Done()
				for i := range slice {
					if slice[i].changeType == "removed" || len(slice[i].rawData) == 0 {
						continue
					}
					var plainBytes []byte
					if IsCompressedJSON(slice[i].rawData) {
						if plainStr, err := DecompressJSON(slice[i].rawData); err == nil && plainStr != "" {
							plainBytes = []byte(plainStr)
						} else {
							plainBytes = slice[i].rawData
						}
					} else {
						plainBytes = slice[i].rawData
					}
					slice[i].plainBytes = plainBytes
					slice[i].contentHash = CanonicalContentHashBytes(plainBytes)
				}
			}(items[lo:hi])
		}
		wg.Wait()

		// Step B: Sequentially filter out noise updates and false bounce removals.
		keepIdx := make([]int, 0, len(items)/4+16)
		for i := range items {
			it := &items[i]
			switch it.changeType {
			case "removed":
				if _, isCurrentlyRemoved := removedAtByID[it.listingID]; isCurrentlyRemoved {
					lastRemAt[it.listingID] = it.fetchedAt
				} else {
					st.FalseRemovalsPurged++
				}
			case "new":
				delete(lastRemAt, it.listingID)
				lastHash[it.listingID] = it.contentHash
				keepIdx = append(keepIdx, i)
			case "update":
				if _, hadPendingRem := lastRemAt[it.listingID]; hadPendingRem {
					delete(lastRemAt, it.listingID)
					st.FalseRemovalsPurged++
				}
				prev, exists := lastHash[it.listingID]
				if exists && prev == it.contentHash {
					// Pure noise update (array jitter or volatile hit counter) -> skip!
					st.NoiseUpdatesPurged++
					it.rawData = nil
					it.plainBytes = nil
					continue
				}
				lastHash[it.listingID] = it.contentHash
				keepIdx = append(keepIdx, i)
			}
		}

		if len(keepIdx) == 0 {
			return nil
		}

		// Step C: In parallel, compress ONLY the kept rows!
		kChunk := (len(keepIdx) + workers - 1) / workers
		for w := 0; w < workers; w++ {
			lo := w * kChunk
			if lo >= len(keepIdx) {
				break
			}
			hi := min(len(keepIdx), lo+kChunk)
			wg.Add(1)
			go func(indices []int) {
				defer wg.Done()
				for _, idx := range indices {
					it := &items[idx]
					if IsCompressedJSON(it.rawData) {
						it.compBlob = it.rawData
					} else if len(it.plainBytes) > 0 {
						it.compBlob = CompressJSON(it.plainBytes)
					}
					it.rawData = nil
					it.plainBytes = nil
				}
			}(keepIdx[lo:hi])
		}
		wg.Wait()

		// Step D: Write kept rows to dstDB in a single transaction.
		tx, err := dst.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		ins, err := tx.PrepareContext(ctx, `
			INSERT INTO listing_history (listing_id, change_type, data_json, data_hash, fetched_at)
			VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer ins.Close()

		for _, idx := range keepIdx {
			it := &items[idx]
			if _, err := ins.ExecContext(ctx, it.listingID, it.changeType, it.compBlob, it.contentHash, it.fetchedAt); err != nil {
				return fmt.Errorf("insert compacted history for %d: %w", it.listingID, err)
			}
			st.HistoryNewRows++
		}
		return tx.Commit()
	}

	for rows.Next() {
		var r rawHistRow
		if err := rows.Scan(&r.listingID, &r.changeType, &r.rawData, &r.fetchedAt); err != nil {
			return fmt.Errorf("scan history: %w", err)
		}
		batch = append(batch, r)
		if len(batch) >= batchSize {
			if err := processBatch(batch); err != nil {
				return err
			}
			scanned += int64(len(batch))
			batch = batch[:0]
			if log != nil && scanned%100000 == 0 {
				log.Infof("compaction: history scanned %d / %d (kept=%d, noise_purged=%d, false_rem_purged=%d)",
					scanned, st.HistoryOldRows, st.HistoryNewRows, st.NoiseUpdatesPurged, st.FalseRemovalsPurged)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(batch) > 0 {
		if err := processBatch(batch); err != nil {
			return err
		}
	}

	// Finally, insert one compact 'removed' event row (data_json = NULL) for each
	// listing that is genuinely removed at the end of the timeline.
	if len(removedAtByID) > 0 {
		tx, err := dst.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		ins, err := tx.PrepareContext(ctx, `
			INSERT INTO listing_history (listing_id, change_type, data_json, data_hash, fetched_at)
			VALUES (?, 'removed', NULL, NULL, ?)`)
		if err != nil {
			return err
		}
		defer ins.Close()

		for id, remAt := range removedAtByID {
			ts := remAt
			if hts, ok := lastRemAt[id]; ok && hts != "" {
				ts = hts
			}
			if _, err := ins.ExecContext(ctx, id, ts); err != nil {
				return err
			}
			st.HistoryNewRows++
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	return nil
}
