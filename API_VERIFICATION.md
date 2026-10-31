# API Verification Report

**Date:** 2026-08-10
**Scope:** End-to-end verification of every AQAR API integration point: request correctness, response parsing, property-type integrity, and malformed/failing data handling.

---

## 1. What was verified and how

| Area | Method | Result |
|---|---|---|
| Count query parse (`client.go` Count) | Local fake-server tests + live requests (multiple sessions) | ✅ Correct — `Search.main` number parsed; malformed shapes counted as errors |
| Find/pages query parse (`FindPage`) | Local fake-server tests + live requests | ✅ Correct — `total/featured/listings` parsed; non-object entries dropped safely |
| Pagination (`from`/`size`) | Live smoke runs (cycles 1-2, 57 categories, 100k+ listings) | ✅ Works; documented offset-drift on shifting sort (coverage report added) |
| Website URL-enrichment API (`sa.aqar.fm`) | Live requests (`app-version: 1.0.0`) | ✅ Works — verified 70/page cap, correct `from` pagination, `id→path` mapping |
| Telegram API | Live sends (multiple stop alerts delivered) | ✅ Works — retry + status tracking + `getMe` verify at startup |
| Category names vs live `Info` API | Live compare (57/57) | ✅ Identical (checked earlier; fixes applied for 6 cosmetic abbreviations) |
| Property-type integrity | **NEW runtime guard** + local test | ✅ Guard added — see §2 |

## 2. Property-type integrity (the "villa must return villa" check)

**Problem found:** nothing verified that listings returned for category X actually *are* category X. Wrong data would be archived silently.

**Fix (implemented):** runtime integrity guard in `crawler.go` — every listing (featured + listings) on every page is checked: `listing.category != crawled category` → `CATEGORY MISMATCH` error log with the listing id, counted per cycle, and a cycle summary line (`cycle integrity: ... matched for every listing (N checked)` / `CYCLE INTEGRITY: N mismatches`).

**Local proof** (`Test_CategoryIntegrityGuard`): fed the crawler a page for cat 1 containing a listing with `category: 5`:
```
ERROR CATEGORY MISMATCH: listing 2 has category=5 while crawling cat 1
ERROR CYCLE INTEGRITY: 1 listings had a category field that did not match the crawled category
```
The listing is still archived (it IS a real listing) but the mismatch is loudly flagged.

**Live check status:** the full 57-category live sweep (`cat_check.py`, one small sample per category, verifying `category == asked`) is ready but **deferred — Cloudflare blocked this machine's IP (403)** during the audit burst. It will run as soon as the block lifts. Meanwhile, the runtime guard performs this verification continuously: the next complete cycle's log will state `cycle integrity: category field matched ... for every listing (N checked)` as hard evidence.

## 3. Wrong/unexpected data handling (fuzz-style, 9 regression tests — all PASS)

Added `apiverify_test.go` (fake GraphQL server, no live API):

| Test | Input | Verified behavior |
|---|---|---|
| `Test_MalformedFindBodyTruncated` | truncated JSON mid-listing | Page fails cleanly → error counted, category skipped, **0 false removals** |
| `Test_MalformedFindBodyNull` | `null` body | No panic, clean error path |
| `Test_MalformedFindBodyEmpty` | empty body | No panic, clean error path |
| `Test_MalformedFindWrongShape` | listings = `[42,"x",null,"str",7,{...}]` | Non-objects dropped silently, valid objects ingested (2/2) |
| `Test_MalformedListingFields` | `id:0`, `id:-5`, price string, `category:"1"`, `location:null`, `category:999`, `price:[...]`, missing id, string lat/lng | Valid listings ingested, garbage skipped, **no crash**; string-lat/lng stored correctly |
| `Test_MalformedCountBody` | `"not json at all"` as count body | Count error → category skipped, **0 false removals** |
| `Test_APIFailureModes` | HTTP 429 / 500 / 403 | 429/500: retried then failed cleanly, **0 false removals**; 403: cycle aborts with `ErrAPIRejected` (the stop+Telegram path) |
| `Test_CategoryIntegrityGuard` | wrong-category listing | Stored + flagged (see §2) |
| `Test_CrawlWindow` | window helpers | Plain + wrap-around windows, next-open computation |

**All 9 pass**, full suite green (`go test ./...`, 1.4s).

## 4. Bugs found and fixed during this audit

1. **No category-integrity verification** (critical for data trust) → runtime guard added (`crawler.go`), cycle summary added.
2. **Retry backoff hardcoded** (15s/30s, 60s/120s for 429) — made configurable (`client.go` `retryBase`/`rateLimitBase`) so tests run in ms instead of minutes.
3. *(Previously fixed, re-verified here: RunSummary JSON tags, enrichment 70/page cap, removed-pass guard, stale-resume validation, UTF-8 truncate, toFloat ints — all covered by regression tests from the earlier audit.)*

## 5. Live observations (raw evidence from this session)

- Count + find requests verified live repeatedly (HTTP 200, e.g. cat 1 total 29,414; cat 36 Hospital/health-center-for-sale total 2 with both listings' category == 36).
- Rent set `[1,5,4,13,11,8,16,15,19,14,18,17,23,27,29,31,33,35,37,39,41,44,46,49,51,53,55,56,58]` and sale set `[3,2,6,7,9,10,12,20,22,24,25,26,28,30,32,34,36,38,40,42,43,45,47,48,50,52,54,57]` have **no overlap** and each side contains exactly its rent/sale categories.
- Website API pagination: `size=500` returns 70 → walk `from=0/70/...` yields 70+38=108=total (category 25). Enrichment now pages correctly (max 4 pages/category).
- Cloudflare 403 incidents (twice today) were handled exactly as designed: crawler stopped, `APIRejectedWarning` printed, **Telegram stop alert delivered** (panel shows `telegram: OK`), resume state kept.

## 6. Remaining action (blocked by Cloudflare)

- Run `cat_check.py` (57-category live sample sweep, ~6s pacing) once the 403 block lifts → append its output here.
- Restart the crawler (web mode) after the block lifts; its next full cycle will emit the automatic `cycle integrity` line as ongoing proof.
