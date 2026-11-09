package main

// Database schema. Versioned via schema_migrations; the list below is
// append-only — never edit an applied migration, add a new one.
//
// listings        : current snapshot per listing + indexed searchable columns.
// listing_history : append-only archive; one row per detected change, plus a
//                   'removed' row when a listing disappears from the API.
// crawl_runs      : one row per cycle, with counters for monitoring.
// crawl_state     : resume point so an interrupted cycle can continue.

var migrations = []string{
	// 001: initial schema
	`CREATE TABLE IF NOT EXISTS listings (
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
		seen_cycle    INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_listings_category  ON listings(category);
	CREATE INDEX IF NOT EXISTS idx_listings_district  ON listings(district_id);
	CREATE INDEX IF NOT EXISTS idx_listings_city      ON listings(city_id);
	CREATE INDEX IF NOT EXISTS idx_listings_price     ON listings(price);
	CREATE INDEX IF NOT EXISTS idx_listings_removed   ON listings(removed_at);
	CREATE INDEX IF NOT EXISTS idx_listings_seencycle ON listings(seen_cycle);

	CREATE TABLE IF NOT EXISTS listing_history (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		listing_id  INTEGER NOT NULL,
		change_type TEXT    NOT NULL,
		data_json   TEXT,
		data_hash   TEXT,
		fetched_at  TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_history_listing ON listing_history(listing_id, fetched_at);

	CREATE TABLE IF NOT EXISTS crawl_runs (
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

	CREATE TABLE IF NOT EXISTS crawl_state (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	`,

	// 002: live stats row so a separate watch process (-watch) can render
	// the TUI dashboard against a crawler running elsewhere.
	`CREATE TABLE IF NOT EXISTS live_stats (
		id         INTEGER PRIMARY KEY CHECK (id = 1),
		data_json  TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	`,

	// 003: canonical aqar.fm URL per listing (web API enrichment).
	`ALTER TABLE listings ADD COLUMN url TEXT;
	`,

	// 004: market intelligence & valuation model columns (initial_price,
	// price_changes, whatsapp_count, property_age, street_width,
	// street_direction, rent_period).
	`ALTER TABLE listings ADD COLUMN initial_price REAL;
	ALTER TABLE listings ADD COLUMN price_changes INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE listings ADD COLUMN whatsapp_count INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE listings ADD COLUMN property_age INTEGER;
	ALTER TABLE listings ADD COLUMN street_width INTEGER;
	ALTER TABLE listings ADD COLUMN street_direction INTEGER;
	ALTER TABLE listings ADD COLUMN rent_period INTEGER;
	UPDATE listings SET initial_price = price WHERE initial_price IS NULL;
	`,
}
