# AQAR-CRAWLER-v2

A high-efficiency, continuous 24/7 real-estate watchdog and archiver for Saudi Arabia's **AQAR** platform (`pro-v3.aqar.app` + `sa.aqar.fm`), written in Go with pure-Go SQLite (`modernc.org/sqlite`).

It crawls all 57 property categories (29 rent + 28 sale) across the Kingdom, stores **100% of every listing's GraphQL payload (all 115+ fields)** using custom preset-dictionary DEFLATE compression (`AQZ1`), tracks genuine price/status/content changes over time, and serves both a terminal UI (`-tui` / `-watch`) and a live web control panel (`-web`).

---

## Key v2 Architecture & Efficiency Upgrades

1. **100% Lossless Full-Payload Compression (`AQZ1` Preset-Dictionary DEFLATE)**
   - Every listing's full raw JSON (~5.8 KB across 115+ fields, nested `location`, `user`, `videos`, `rent_attributes`, etc.) is compressed with `compress/flate` (`BestCompression`) primed with a deterministic 32 KB domain-specific dictionary (`aqarFlateDict` in `compress.go`).
   - Achieves **~7.3x–7.6x per-record compression** (~800 bytes/listing vs ~5,800 bytes raw) with **zero data loss** — `DecompressJSON` restores the exact JSON byte-for-byte on read (`ListingHistory`, `ExportListings`, `GetListingJSON`, and web diffing).

2. **Zero-Allocation Canonical Content Hashing & Noise Elimination**
   - `CanonicalContentHashBytes` computes a SHA-256 content hash over canonical JSON while normalizing AQAR backend replica jitter (`imgs_desc` and `bookable_unit_plans` randomly alternating between `null`, `[]`, and `[""]`) and volatile engagement/bump counters (`refresh`, `whatsapp_requested_count`, `whatsapp_responded_count`, `bookable_unit_interested_users_count`, `sov_campaign_id`, `user.paid`, `user.review`).
   - Volatile counters and latest compressed `data_json` are still updated in-place on `listings` every cycle (`touch`), while `listing_history` only records genuine property changes (`new`, real `update`, and `removed`).
   - `removed` history events record `data_json = NULL` instead of duplicating the 5.8 KB payload.

3. **Multi-Cycle Removal Grace Period & Zombie-Removal Healing**
   - `removed_grace_cycles` (default `2`) prevents single-cycle API pagination offset drift from falsely flagging active listings as removed and re-adding them next cycle.
   - Re-seeing a previously removed listing immediately clears `removed_at = NULL`.
   - Built-in archive drop guard refuses mass-removal passes if a cycle sees an anomalously small fraction of the active archive.

4. **Hardened 24/7 Watchdog Operation**
   - **Automatic Proxy-to-Direct Fallback:** When `proxy_url` is configured and `proxy_fallback_direct` is enabled, any proxy outage automatically falls back to direct connection for 15 minutes before re-probing the proxy.
   - **Consecutive Category Failure Circuit Breaker:** Aborts an unhealthy cycle after 5 consecutive category failures instead of stalling for 90+ minutes across all 57 categories.
   - **Continuous Heartbeat Keepalive:** Refreshes `live_stats` every 60 seconds even while sleeping between cycles so watchdog monitors and Telegram stale-heartbeat alerts stay accurate 24/7.
   - **Separate Read/Write & Read-Only SQLite Pools:** Dashboard polling and CSV exports run against a dedicated read-only pool (`?mode=ro`) so they never contend with page ingestion transactions.

5. **In-Place Database Compaction (`-compact`)**
   - `aqar-crawler.exe -compact` streams, heals zombie `removed_at` flags, purges historical replica-jitter noise and false bounce removals, compresses all kept JSON records in parallel across all CPU cores, verifies row counts and `PRAGMA quick_check`, and atomically swaps the compacted SQLite file.
   - **Production Benchmark on `AQAR.db`:** Shrunk the live archive from **16,573.4 MB (17.38 GB) to 680.0 MB (24.4x smaller — 15.89 GB saved)** while retaining **100% of all 178,320 listings** (full 115+ GraphQL fields byte-for-byte), healing **59,907** zombie `removed_at` records, and distilling `listing_history` from **2,126,974** rows down to **301,215** genuine history events (purging 1,243,601 replica-jitter/counter noise updates and 582,158 false pagination-drift removals).

---

## Quick Start

### Build
```powershell
go build -o aqar-crawler.exe .
```

### Modes
```powershell
# Validate API connectivity (and proxy fallback) then exit
.\aqar-crawler.exe -validate

# Run 24/7 crawler + Web Control Panel (http://127.0.0.1:9090)
.\aqar-crawler.exe -web

# Run 24/7 crawler with interactive Terminal UI
.\aqar-crawler.exe -tui

# Watch live stats in Terminal UI while crawler runs elsewhere
.\aqar-crawler.exe -watch

# Run a single full cycle and exit
.\aqar-crawler.exe -once

# Compact, deduplicate noise history, and compress an existing AQAR.db in-place
.\aqar-crawler.exe -compact

# Inspect a specific listing's metadata, compression ratio, and version diff timeline
.\aqar-crawler.exe -inspect 6347338
```

### Configuration
On first run, `config.json` is generated automatically (see [`config.example.json`](config.example.json)).


---

## Zero-Downtime Self-Update & Remote CLI

When running via `run.bat`, `run.ps1`, or `run.sh`, the server automatically polls `origin/main` every 2 minutes and exposes `POST /api/update`:
- **Plan A (`hot_reload`)**: Reloads configuration/non-code updates in `<5ms` with `0ms` downtime.
- **Plan B (`binary_handoff`)**: Compiles `aqar-crawler.new.exe` in the background while crawling continues, runs `-self-test`, swaps binaries, checkpoints mid-cycle state, and restarts in `<1s` via exit code `42`.

```powershell
# Remote status check
.\aqar-crawler.exe -remote 192.168.0.137:9090 -remote-cmd status

# Trigger immediate Git pull & update
.\aqar-crawler.exe -remote 192.168.0.137:9090 -remote-cmd update

# Tail remote logs
.\aqar-crawler.exe -remote 192.168.0.137:9090 -remote-cmd log
```
