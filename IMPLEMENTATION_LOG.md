# Implementation & Resolution Log

## Summary of Actions
- **Bugs Squashed:** 6 (2 Critical, 1 Medium, 3 Low — all from `FULL_AUDIT_REPORTv2.md` Part 1)
- **Future Bugs Prevented:** 7 of 8 (Part 2; #8 documented but not implemented — see below)
- **Files Refactored:** 13 (12 modified + 1 new regression test file)
- **New Features Implemented:** 8

---

## Details of Changes

### `crawler.go` — CRITICAL fixes + new behavior
- **Bug 1 (Critical) — removed-pass guard:** the removed-detection pass now requires `totalSeen > 0 AND requests > 0` before it may run. A cycle that saw zero listings (API outage returning `count: 0`, config drift) is refused with a loud ERROR and the cycle is marked aborted instead of poisoning the archive. **Sandbox-proven fix:** `Test_MarkRemovedGuardZeroSeen` — 0 listings removed where the old code removed 500/500.
- **Bug 2 (Critical) — stale resume validation:** new `resumeCategoryExists()` checks the resume state's `CategoryID` against the configured category set, and the resume `PageSize` is validated against the current config (`CrawlState` now carries `PageSize`). Any mismatch → the cycle starts fresh with a prominent ERROR log, and the removed-pass can no longer fire on a zero-request cycle. **Sandbox-proven fix:** `Test_StaleResumeRefused` — cycle now makes 4 requests (was 0) and removes nothing.
- **Future Bug 3 — cycle numbering:** `SaveNextCycle`/`LoadNextCycle` persist the next cycle number in `crawl_state`; `Run()` falls back to it when no resume state exists. Restarts after a completed cycle no longer reset to #1. Test: `Test_CycleNumberPersistence`.
- **Future Bug 5 — WAL checkpoint:** `PRAGMA wal_checkpoint(TRUNCATE)` every 500 pages keeps the WAL sidecar bounded.
- **New: coverage reporting** — `reportCoverage()` logs unique-listing coverage per cycle (offset-drift visibility).
- **New: URL enrichment** — per-category call to the new `WebClient` after pagination (paced), storing canonical `aqar.fm` URLs; best-effort, never fatal.
- **New: dry-run** — `cfg.DryRun` skips all DB writes (no run rows, no ingest, no state) while still fetching and logging; the removed-pass is skipped with an explicit log line.
- **Stupid Code 6 — stale comments** fixed (pacing/Stats comments now describe the real behavior).

### `store.go` — ingest hardening + new queries
- **Future Bug 4 — duplicate IDs inside one page:** `IngestPage` now collapses duplicate ids (last occurrence wins) before hashing/inserting; a repeated id can no longer trip the UNIQUE constraint and kill the whole page transaction. Test: `Test_IngestDedupInPage`.
- **Stupid Code 1 — wasted per-page UPDATE:** removed the `UPDATE crawl_runs SET requests=requests+1 ...` block from `IngestPage` (it was overwritten by `FinishRun` on every path).
- **Stupid Code 2 — marshal→unmarshal round trip:** removed the per-listing `json.Unmarshal`; columns are now built directly from the original decoded map.
- **Stupid Code 3 — dead `Run` struct:** deleted.
- **New: `Checkpoint`** (WAL truncate), **`DBSizeMB`** now includes `-wal`/`-shm` (the size display no longer lies), **`SetListingURLs`**, **`ListingHistory`** + **`DiffJSON`** (field-level diff between versions, `location.*`/`user.*` flattened), **`ExportListings`** streaming CSV rows, **`SaveNextCycle`/`LoadNextCycle`**.

### `schema.go`
- **Migration 003:** `ALTER TABLE listings ADD COLUMN url TEXT` (canonical listing URLs).

### `client.go` — robustness
- **Bug 5 — invalid UTF-8 truncate:** `truncate()` now backs off to a rune boundary (`utf8.RuneStart`) so Arabic/emoji error text is always valid UTF-8. Test: `Test_TruncateKeepsUTF8`.
- **Bug 6 — `toFloat` int gap:** added `int/int8..64/uint/uint8..64` cases — in-process-built maps can no longer silently lose ids. Test: `Test_ToFloatHandlesInts`.
- **New: `WebClient`** — website GraphQL API client (`sa.aqar.fm`, `app-version: 1.0.0`) with `FetchListingPaths(category)` returning `id → path` for the URL enrichment feature.

### `controller.go` — notification race fix
- **Bug 3 (Medium):** `Stop()` now **blocks** until the crawler goroutine fully exits — the goroutine signals via a `done` channel **after** the Telegram stop notification completes. The stop warning can no longer be killed by the process exiting. Timeout guard: 30s.
- Misleading "Blocks until the crawler exits" comment is now true.

### `logger.go` — log lifecycle
- **Bug 4 — SetQuiet(false):** now restores the console+file MultiWriter; the `quiet` field is actually read. Test: `Test_LoggerSetQuietRestores`.
- **Future Bug 1 — unbounded log:** size-based rotation at 20 MB (`crawler.log` → `crawler.log.1`), checked after each write.
- **Stupid Code 8:** removed the `_ = time.Now()` no-op; `APIRejectedWarning` now routes through the logger writer (never corrupts the TUI screen in quiet mode).

### `notify.go` — reliable alerts
- **Future Bug 7 + Stupid Code 4:** `Send()` now retries (3 attempts with backoff), **logs every failure** (token never logged), and records `lastOK/lastError/lastAt` for the panel. New `Verify()` (Telegram `getMe` at startup), new `HeartbeatStale()` crash-alert, `Status()` for `/api/status`. Dead `Started()` removed.

### `main.go` — flags + shutdown
- **Stupid Code 5:** dead `nextCycleFromState` removed (replaced by `SaveNextCycle`/`LoadNextCycle`).
- **Stupid Code 12:** the magic `time.Sleep(2s)` shutdown dance replaced by blocking `ctrl.Stop()`.
- **New: `-categories` flag** (`rent:1,5,4;sale:3,2` or plain `36`), **`-dry-run` flag**.
- Startup: `notify.Verify()` logs Telegram bot health when enabled.

### `web.go` — panel v2
- **New: auth** — `web_token` config; all `/api/*` endpoints require `X-Auth-Token` when set (401 otherwise).
- **Future Bug 2:** `/api/status` prefers the crawler's exact cumulative counters (`TotalListings`/`TotalHistory`) over per-second full-table `COUNT(*)` scans.
- **Future Bug 1:** `/api/log` now seeks to the last 64 KB instead of reading + splitting the entire log file every poll.
- **New: heartbeat** — `/api/status` exposes `heartbeat {last_update, age_sec, stale, alert_minutes}`; a checker fires one Telegram crash-alert per stale episode (`telegram_heartbeat_alert_minutes`) — covers hard-killed crawlers that no in-process notification can catch.
- **New: `/api/listing?id=N`** — full version timeline with field-level diffs (via `DiffJSON`).
- **New: `/api/export?category=N`** — streaming CSV of the archive.
- Telegram delivery status surfaced in `/api/status`.

### `webui.html` — panel UI
- **Stupid Code 10:** the `(running ? "-" : "-")` no-op ternary replaced with a real "after cycle end" label.
- Token prompt flow (sessionStorage + retry on 401) for protected panels.
- Footer now shows heartbeat age (+ STALE flag), Telegram delivery status, and a DRY-RUN badge.

### `config.go` / `config.json`
- New config fields: `web_token`, `telegram_heartbeat_alert_minutes` (0 = off), `url_enrichment_enabled`, `dry_run` — all validated.
- `config.json` updated with the new keys.

### `crawler_test.go` (NEW — regression suite, 10 tests, no network)
Covers every squashed bug: `Test_MarkRemovedGuardZeroSeen`, `Test_StaleResumeRefused`, `Test_ToFloatHandlesInts`, `Test_TruncateKeepsUTF8`, `Test_CycleNumberPersistence`, `Test_IngestDedupInPage`, `Test_DiffJSON`, `Test_LoggerSetQuietRestores`, `Test_ApplyCategoriesFlag`, `Test_RandomBetweenRange`. Runs against an in-process fake GraphQL server.

---

## Verification
- `go build` — clean
- `go vet ./...` — clean
- `go test ./...` — all pass (10 new regression tests + existing TUI tests)
- Live smoke test: auth (401 without token / 200 with), `/api/status` telegram+heartbeat fields, `/api/export` CSV header, `/api/listing` diff endpoint, `/api/log` tail — all verified against a running instance
- Crawler restarted in `-web` mode (PID 66248, panel at :9090) with the fixes live; Telegram `getMe` verification OK

## Not implemented (documented)
- **Future Bug 8 (dashboard read-only connection):** the single-connection store serializes panel reads with crawler writes; WAL + `busy_timeout` currently make this tolerable. Recommended when the archive exceeds ~1M rows.
- **Idea 11 (deletion-policy toggle):** deferred — the archive intentionally never deletes.
