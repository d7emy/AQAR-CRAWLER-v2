# Thoughts & Progress

## Project context

Reverse-engineered the AQAR (عقار) iOS app API from a Burp Suite capture (136 requests, `requests-in-burp`). Goal: a property listing website/crawler for all property types (rent + sale). No maps, no chat, no user accounts in scope.

## High-value technical findings (from traffic analysis)

- **Primary API:** `POST https://pro-v3.aqar.app/graphql` — Strapi-style GraphQL (query Search.find/count/related, Listing, User, Info, ResidentialIndicators, DistrictBroker). Secondary: `sa-services.aqar.app/graphql` (map services, not needed), `geocode.aqar.app/api?lat=&lng=` (reverse geocode: city/direction/district IDs + names).
- **Req-Hash:** mandatory header; 40-hex (SHA-1 style). Proven empirically: hash depends ONLY on Req-Time + a secret baked into the app binary (identical Req-Time → identical hash regardless of body; fresh timestamp + captured hash → 400; captured timestamp + captured hash replays fine with any body). Not crackable from traffic alone. Strategy: reuse captured (Req-Time, Req-Hash) pairs; re-capture fresh pairs from the app when they stop working.
- **Req-Device-Token:** NOT validated (fake UUID accepted). App-Version/Req-App/Type/User-Agent appear cosmetic.
- **Search:** `find(where: {...}, polygon: [...], from, size, premium: true, show_project_units: true)`. Polygon is OPTIONAL (nationwide query works). `from/size` offset pagination; size up to 1000 verified; deep offsets work (from=28800 ok). `where` supports eq/in/gte/lte (verified: price {lte}, beds {eq}, furnished {eq}, district_id {eq}). Count chips via `Search.main:count` with aliases main/all/unverified_owner/verified_owner/direct_from_owners/unique_listing.
- **Category IDs (from Info query):** rent set = [1,5,4,13,11,8,16,15,19,14,18,17,23,27,29,31,33,35,37,39,41,44,46,49,51,53,55,56,58]; sale set = [3,2,6,7,9,10,12,20,22,24,25,26,28,30,32,34,36,38,40,42,43,45,47,48,50,52,54,57]. Full name mapping stored in analysis.
- **Image CDN:** `https://images.aqar.fm/webp/{width}x0/props/{filename}` (listings), `/webp/{width}x0/users/{filename}` (users), `/webp/original/props/{filename}` (full). assets.aqar.fm serves icons only.
- **Language:** Accept-Language ar/en switches localized content; many fields are bilingual (msg_ar/msg_en pairs). App is Arabic-first, RTL.
- **Prices:** numeric SAR; formatting client-side (no currency field).
- **MojStats** in Info query maps MOJ categories ↔ AQAR categories (license data).

## User decisions

- No maps anywhere. Listings only, all property types.
- No chat, no favorites/saved searches/user features.
- English responses requested.

## Build session - first smoke test

- Go crawler built (flat package main, all files in repo root per user preference; no internal/ folders). SQLite via modernc.org/sqlite (pure Go, no CGO - builds fine on Windows 10 / Server 2022). Deps: modernc.org/sqlite only.
- Schema: listings (current state + indexed search columns + data_json blob), listing_history (append-only archive: new/update/removed rows with fetched_at), crawl_runs, crawl_state (resume point), schema_migrations.
- Req-Hash handling: reuse captured pair; -validate flag; exit code 2 + loud console warning when pair dies; crawler stops immediately.
- Smoke test surfaced 2 bugs (fixed in next build):
  1. Flags parsed AFTER config load, so -config/-once were ignored (run used default config, all 57 categories, 45s interval).
  2. Response parsing used path 'data.Search.main' but the decoder already unwraps 'data' -> count/find always failed with 'missing Search.main'. (CheckCredentials passed only because it ignores the parsed value.)
- The buggy run still behaved politely: ~57 requests at 45s spacing, no errors from the API side.

## Build session - smoke test pass 2 (after fixes)

- Fixed both smoke-test bugs: flags now parse BEFORE config load (-config/-once work); response paths unwrap 'data' correctly. Also made config loading strip UTF-8 BOM (PowerShell/Notepad edit safety).
- Full cycle verified on category 1 (rent apartments): 61 requests, 29,305 listings seen, 24,799 new + 1 update archived, run row recorded, WAL mode active, resume state works. Second cycle (page-size 1000): +3,734 new, +517 updates. DB: listings=28,533, history=29,051 rows. ~436 MB for 28.5k full snapshots (~15 KB/listing) - expect ~4-7 GB for the full Saudi dataset.
- NOTABLE: live data churns constantly (cat 1 total moved 29,301 -> 29,562 -> 29,561 within an hour; hundreds of listings changed between cycles). Offset pagination on a shifting sort causes some page duplicates and a few missed listings per cycle - acceptable for an archive (next cycle picks them up). Duplicates are deduped by id; identical snapshots are not re-archived.

## MAJOR DISCOVERY - Req-Hash is NOT validated

- The Req-Hash/Req-Time headers are completely decorative. Requests WITHOUT them work fine (verified with valid GraphQL queries: count + find both return 200).
- My earlier 'hash rejected -> 400' conclusion was wrong: those 400s came from a malformed test query (count with a subfield selection), not hash validation.
- Changes: headers now sent only if configured (optional); failure sentinel renamed ErrHashInvalid -> ErrAPIRejected (any persistent API refusal: 400-empty, 401/403); warning message now generic ('API refusing requests'), still stops with exit code 2 as the user requested.
- User requirement honored: if the API ever starts rejecting us, the crawler stops immediately, prints a loud warning, and exits 2.

## User decisions (build)

- Go backend, all .go files in repo root (no internal/ folders).
- SQLite single-file DB (modernc.org/sqlite, pure Go - no CGO, runs on Win10 + Server 2022).
- 24h slow crawler: default 45s between page requests (+-10% jitter), one full cycle per 24h, -once for single pass.
- Archive model: listings (current) + listing_history (append-only new/update/removed rows with fetch timestamp). Nothing is ever deleted.
- No search/frontend yet, but schema has indexed columns (category, price, district, city, area, beds...) for a future UI.

## TUI dashboard added

- New flags: -tui (crawler + dashboard in one process) and -watch (dashboard only, reads stats from the DB while the crawler runs elsewhere - useful on Server 2022 where the crawler can run as a background task).
- Dependencies added: charmbracelet/bubbletea + lipgloss (pure Go, Windows-friendly). Real-time facts:
  - Header: cycle number, RUNNING/PAUSED/IDLE status, DB size.
  - Counters: listings (cumulative), new/updated/removed (current cycle), requests, pages, seen, history rows, errors.
  - Cycle line: started time, uptime, next-cycle countdown; red banner when the API refused / last error.
  - Category progress: all 57 categories with per-category total + progress bar + done/ERR state, active row highlighted, j/k scroll, g/G top/bottom.
  - History panel: last 8 crawl runs with per-run counters.
  - Keys: [p] pause/resume requests (TUI mode), [q] quit (graceful stop, resume state saved).
- Stats plumbing: crawler updates an in-memory Stats struct (mutex) + persists a compact live_stats row (single UPSERT per page) so -watch works cross-process. Catches ErrAPIRejected -> stops + exit 2 as before.
- Testing: headless render unit test (seeded DB) + live 12s run of -tui verified (cycle 0 IDLE -> cycle 1 RUNNING -> category total fetched -> first page ingested, DB size growing).

## Category names verified + TUI stop instructions

- User asked 'are you sure about the property type?' - verified the hardcoded categoryNames map against the captured Info response: 57/57 IDs correct, only 6 cosmetic differences (abbreviations like 'Bank/ATM' vs 'Bank / ATM', 'Hospital' vs 'Hospital / health center', 'Comm. tower' vs 'Communication tower'). Fixed to match the API exactly and rebuilt.
- User couldn't stop the crawler. No process was running when checked. Documented stop methods in run.bat: TUI mode q / Ctrl+C, plain mode Ctrl+C, force kill taskkill /IM aqar-crawler.exe /F. Force-kill is safe - resume state is saved after every page.

## AQAR-CRAWLER-v1 folder discovered and mined (user: 'cheat from it')

- v1 = old crawler (June 2023) for the aqar.fm WEBSITE API: https://sa.aqar.fm/graphql - a DIFFERENT backend from the app API (pro-v3.aqar.app) used by v2. Web API: operationName+variables+query envelope, no Req-Hash header (supports the finding that Req-Hash is decorative).
- v1 has 4 POST operations: findListings, getAllCities, getAllDirections, getAllDistricts. Crawled per-district (city_id+district_id, size 20) with hardcoded city/district maps (var.go, cities.txt).
- v1 categories.txt: 23 categories with Arabic names -> cross-validated my app category map: 22/23 IDs identical (only v1 id 21 'شقة-مفروشة' furnished apartment has no app equivalent - became booking category 101).
- v1 decoded numeric enums the app API leaves opaque:
  - rent_period: 1=daily يومي, 2=monthly شهري, 3=yearly سنوي
  - property type field: 1=residential سكني, 2=commercial تجاري, 3=both
  - street_direction: 1-8 compass + 9=three streets + 10=four streets
- v1 web listings have fields the app API lacks: title, uri/path (web slugs), native (ads), user.phone.
- Keep v1 folder untouched (reference only). v2 supersedes it (nationwide queries, 115 fields, SQLite archive, TUI).

## Web control panel added (-web mode)

- New mode: qar-crawler.exe -web = Go HTTP server + embedded dashboard (webui.html via go:embed) + crawler controlled from the browser. run-web.bat added.
- Endpoints: GET / (dashboard), GET /api/status (live stats + DB counts + runs history + config + ETA), POST /api/control {action: start|stop|pause|resume}, GET /api/log (log tail). Polls every 1s from the page.
- New controller.go: CrawlerController owns the crawler lifecycle (shared Stats across restarts); stop keeps resume state, start resumes where it stopped. Panel buttons: Pause/Resume/Stop/Start.
- Crawler made stop-fast: pace() and all loops are now ctx-aware (no 40-minute unwind on stop), manual stop keeps resume state and SKIPS the removed-detection pass (critical - a partial cycle must never flag listings as removed).
- Default web_port changed 8080 -> 9090: 8080 is taken by Burp Suite on this machine (bind failure killed the first test run). web_host/web_port in config.json (127.0.0.1 default; change web_host to 0.0.0.0 for LAN/Server 2022 access).
- Verified live: status/counts/ETA render, pause/resume/stop/start round-trip via API, bogus action -> 400, log tail works, graceful shutdown persists state.

## Pacing + Telegram notifications

- Pacing changed from 45s fixed +-10% jitter to a uniform-random sleep between 10.5s and 14.5s per request (config: sleep_min_seconds/sleep_max_seconds, flags -sleep-min/-sleep-max). ETA on the panel uses the average (12.5s).
- Telegram bot added (notify.go): sends a message ONLY when the crawler stops for any reason (manual stop, API rejection, error) with reason + cycle stats + archive totals. No start/progress messages (user requested warnings only). Config: telegram_enabled/bot_token/chat_id in config.json. Verified: test message delivered (msg_id=113).
- Note: the bot token is in config.json - do not commit/share it.
- NOTE: a hard process kill (taskkill /F, power loss, crash) cannot send Telegram from a dead process - the stop message only fires for in-process exits.

## Full code audit completed (FULL_AUDIT_REPORTv2.md)

- 18 files reviewed, 20 sandbox tests executed (sandbox deleted after; repo pristine).
- CRITICAL BUGS (both proven in sandbox): the removed-detection pass (MarkRemoved) runs after any zero-error cycle even when NOTHING was ingested - (a) an API outage returning count=0 marks the ENTIRE archive removed (500/500 proven), (b) stale resume state (category removed from config) skips the whole cycle with 0 requests and does the same (300/300 proven). FIX: guard with totalSeen > 0 + validate resume category; never let a zero-request cycle reach MarkRemoved.
- MEDIUM: web-mode Telegram stop warning is lost - runWeb sleeps only 2s then os.Exit while the notify goroutine needs DB queries + HTTP (race proven). FIX: Controller.Stop must block on the crawler goroutine.
- LOW proven: SetQuiet(false) never restores console; truncate() emits invalid UTF-8 (Arabic/emoji); toFloat() drops Go int types (latent); duplicate id in one page -> UNIQUE constraint kills whole page (latent).
- 8 future bugs: unbounded log + full-file re-read every 3s by /api/log, COUNT(*) table scans every 1s on /api/status, cycle numbering resets to 1 after crashes, WAL growth/ignored in size display, config drift unvalidated, plaintext telegram token + silent send failures, single-connection contention with dashboard readers.
- 12 stupid-code items: dead Run struct/Started()/nextCycleFromState, per-page crawl_runs UPDATE overwritten by FinishRun (proven useless), marshal->unmarshal round-trip per listing, lying comments (RequestIntervalSeconds, Stop 'blocks'), two truncate functions, no-op ternary in webui, fragile fs.Visit flag merge.
- 12 ideas: guarded removed-pass policy, config-drift validation, heartbeat dead-crawler Telegram alerts, listing URL enrichment via sa.aqar.fm web API (verified working, app-version 1.0.0), panel auth token, pagination coverage tracker, per-listing diff view, CSV export, WAL checkpointing, notifier retry+status, deletion-policy toggle, -categories/-dry-run flags.

## Audit fixes implemented (see IMPLEMENTATION_LOG.md)

- ALL 6 audit bugs fixed and regression-tested in-repo (crawler_test.go, 10 tests, fake GraphQL server, no network):
  - Critical: removed-pass now requires totalSeen>0 AND requests>0; stale-resume states (missing CategoryID, changed PageSize) start fresh with a loud error - the archive can no longer be mass-flagged removed by an outage or config drift.
  - Medium: Controller.Stop() now blocks until the crawler goroutine exits (Telegram stop alert is never lost on shutdown).
  - Low: SetQuiet(false) restores console; truncate() is UTF-8 safe; toFloat() handles all Go int types.
- Future bugs prevented: log rotation (20MB) + seek-based /api/log tail; /api/status uses cached counters instead of per-second COUNT(*); next_cycle persisted (no more cycle-number resets); IngestPage dedupes duplicate ids per page; WAL checkpoint every 500 pages + DB size now includes -wal/-shm; notifier retries/logs failures + getMe verify at startup.
- New features: URL enrichment (canonical aqar.fm URLs via sa.aqar.fm web API, url column migration 003), panel auth (web_token), heartbeat crash-alert (telegram_heartbeat_alert_minutes), /api/listing diff view, /api/export CSV, -categories + -dry-run flags, cycle coverage reporting, telegram status in panel.
- Crawler restarted in -web mode with fixes live; archive had ~99,300 listings at restart.

## Crawl window feature (user request, options chosen in opencode)

- New scheduling mode replacing the '24h wait after cycle' idea: crawl only inside a window (default 02:00-08:00 local time).
- User decisions: machine-local time + config fields (crawl_window_enabled toggle for on/off; crawl_start_hour/crawl_end_hour); manual start outside window = wait silently; crash during day = resume unfinished cycle at next window; NO hard cap (finish the cycle even past 8am, then wait until next 2am); pace back to 10.5-14.5s; user handles Windows power plan (no sleep-code).
- Implementation: isInWindow/nextWindowStart helpers (plain + wrap-around windows like 22-4, tested); Run loop waits silently outside window; inside window = back-to-back cycles (no gap); cycle finishing after window end waits until next open; -once/-dry-run bypass the window.
- LiveStats gained 'waiting' state; panel pill shows WAITING + countdown, new 'Crawl window' info row (02:00-08:00 (OPEN)); TUI shows WAITING.
- Verified live: in_window=True, crawling at 04:54. Also killed stale old-binary process (PID 49080) that was holding port 9090 with a pre-window build.

## API verification pass (improved prompt executed)

- Added runtime CATEGORY INTEGRITY GUARD in crawler.go: every listing's category field is checked against the crawled category; mismatches log CATEGORY MISMATCH + cycle summary (CYCLE INTEGRITY: N mismatches). The crawler is now the permanent live verifier (next full cycle produces the evidence line).
- Added apiverify_test.go: 9 regression tests (malformed/truncated/null bodies, wrong-shape listings, garbage field types, 429/500/403 failure modes, category guard, window helpers) - ALL PASS, full suite green.
- Made retry backoff configurable (client.go retryBase/rateLimitBase) so tests run in ms.
- Live 57-category sweep script ready (cat_check.py) but DEFERRED: Cloudflare blocked the IP (403) mid-audit; the crawler stopped with the Telegram alert (3rd real-world proof of the stop+alert pipeline). Will run the sweep + restart the crawler when the block lifts.
- Report written: API_VERIFICATION.md.


## User request - Efficiency, full-data compression, 24/7 watchdog overhaul, and GitHub publish

- User wants the project thoroughly explored and overhauled for high efficiency, 24/7 watchdog reliability, and maximum data compression while preserving 100% of the full listing data.
- Current state observed: `AQAR.db` has ballooned to ~17.38 GB; previous AI left major storage and operational inefficiencies.
- After completing the overhaul, create a new GitHub repository named `AQAR-CRAWLER-v2` (matching the folder name) and push the project (excluding secrets/sensitive tokens and massive local artifacts).


## v2 Architecture & Compaction Overhaul Completed
- Implemented `compress.go` with `AQZ1` preset-dictionary DEFLATE (`compress/flate` BestCompression + 32 KB `aqarFlateDict`) achieving ~7.3x-7.6x lossless compression on full AQAR listing JSON blobs, plus zero-allocation `CanonicalContentHashBytes` to filter out AQAR backend replica jitter (`imgs_desc`/`bookable_unit_plans` flipping between `null`, `[]`, and `[""]`) and volatile hit counters (`refresh`, `whatsapp_*`, `sov_campaign_id`, `user.paid`, `user.review`).
- Upgraded `store.go` to separate write (`db`) and read-only (`ro`) SQLite connection pools, compress all stored JSON blobs, clear `removed_at = NULL` and refresh volatile fields on `touch`, store `NULL` payload on `removed` history events, and support `MarkRemovedWithGrace`.
- Implemented `compact.go` (`-compact` CLI flag) to rebuild, heal zombie `removed_at` flags, strip historical noise updates and false pagination-drift bounce removals, and parallel-compress the existing archive with full `PRAGMA quick_check` verification.
- Hardened `client.go` and `crawler.go` for 24/7 watchdog operation: automatic proxy-to-direct fallback (`proxy_fallback_direct`), 5-category consecutive failure circuit breaker, continuous 60s heartbeat updates during inter-cycle waits, and archive-drop guard before `MarkRemoved`.
