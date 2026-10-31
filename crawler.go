package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// categoryGroup pairs a human label with its category id list.
type categoryGroup struct {
	label string
	ids   []int
}

// Crawler drives slow, continuous crawl cycles against the AQAR GraphQL API.
// One cycle = every configured category, fully paginated (from: 0..total).
// Pacing: a uniform-random sleep in [sleep_min_seconds, sleep_max_seconds]
// between requests. After a cycle completes it sleeps until the cycle_hours
// boundary, then starts a fresh cycle (that is what makes it a "24-hour
// crawler").
type Crawler struct {
	cfg    Config
	log    *Logger
	client *Client
	store  *Store
	web    *WebClient // website-API client for URL enrichment (nil if disabled)
	// Once exits after a single cycle (used by -once for testing/backfills).
	Once bool
	// Stats feeds the dashboards (TUI/web panel); always attached.
	Stats *Stats
	// dryRun skips all database writes (fetch + log only).
	dryRun bool
}

func NewCrawler(cfg Config, log *Logger, client *Client, store *Store) *Crawler {
	c := &Crawler{cfg: cfg, log: log, client: client, store: store, dryRun: cfg.DryRun}
	if cfg.URLEnrichmentEnabled {
		c.web = NewWebClient(cfg, log)
	}
	return c
}

// resumeCategoryExists reports whether the resume state's category is still
// part of the configured category set.
func resumeCategoryExists(st CrawlState, groups []categoryGroup) bool {
	if st.SetIndex >= len(groups) {
		return false
	}
	for _, id := range groups[st.SetIndex].ids {
		if id == st.CategoryID {
			return true
		}
	}
	return false
}

// reportCoverage logs how many unique listings the cycle actually saw versus
// the sum of the API's category totals (offset-drift visibility).
func (c *Crawler) reportCoverage(groups []categoryGroup, seen map[int64]struct{}) {
	c.log.Infof("cycle coverage: %d unique listings seen (offset drift shows as gaps vs category totals)", len(seen))
}

// enrichCategoryURLs fetches canonical aqar.fm URLs for the given category
// from the website API and stores them. Best-effort: failures are logged,
// never fatal for the cycle.
func (c *Crawler) enrichCategoryURLs(ctx context.Context, catID int) {
	if c.web == nil {
		return
	}
	paths, err := c.web.FetchListingPaths(ctx, catID)
	if err != nil {
		c.log.Warnf("URL enrichment for cat %d failed: %v", catID, err)
		return
	}
	if len(paths) == 0 {
		return
	}
	urls := make(map[int64]string, len(paths))
	for id, p := range paths {
		urls[id] = "https://aqar.fm" + p
	}
	if err := c.store.SetListingURLs(ctx, urls); err != nil {
		c.log.Warnf("URL enrichment store failed: %v", err)
		return
	}
	c.log.Infof("URL enrichment cat %d: %d canonical URLs stored", catID, len(urls))
}

// saveLive persists the current stats snapshot for the TUI / web panel.
// Uses a detached context so it still works during shutdown.
func (c *Crawler) saveLive(ctx context.Context) {
	if c.Stats == nil {
		return
	}
	ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.store.SaveLiveStats(ctx2, c.Stats.Snapshot()); err != nil {
		c.log.Errorf("saving live stats: %v", err)
	}
}

// waitWithHeartbeat waits for duration d while refreshing live_stats every
// minute so external watchdogs/Telegram heartbeats know the process is alive.
// Returns false if ctx was cancelled before d elapsed.
func (c *Crawler) waitWithHeartbeat(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	hb := time.NewTicker(time.Minute)
	defer hb.Stop()
	for {
		select {
		case <-timer.C:
			return true
		case <-hb.C:
			c.saveLive(ctx)
		case <-ctx.Done():
			return false
		}
	}
}

// Run blocks until ctx is cancelled, the API starts refusing us (exit code 2),
// or a fatal error occurs (exit code 1).
func (c *Crawler) Run(ctx context.Context) error {
	for {
		err := c.client.CheckAPI(ctx)
		if err == nil {
			break
		}
		if errors.Is(err, ErrAPIRejected) {
			return ErrAPIRejected
		}
		if c.Once || ctx.Err() != nil {
			return err
		}
		c.log.Warnf("initial API check failed (%v); retrying in 30s...", err)
		if !c.waitWithHeartbeat(ctx, 30*time.Second) {
			return nil
		}
	}
	c.log.Infof("API reachable and responding")

	groups := []categoryGroup{
		{label: "rent", ids: c.cfg.CategoriesRent},
		{label: "sale", ids: c.cfg.CategoriesSale},
	}

	// Seed the archive totals for the dashboard.
	if c.Stats != nil {
		ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
		listings, err1 := c.store.DBListingsCount(ctx2)
		history, err2 := c.store.DBHistoryCount(ctx2)
		cancel()
		if err1 == nil && err2 == nil {
			c.Stats.SetTotal(listings, history)
		}
	}

	nextCycle := 1
	if st, ok, err := c.store.LoadState(ctx); err != nil {
		return fmt.Errorf("loading crawl state: %w", err)
	} else if ok && st.Cycle > 0 {
		nextCycle = st.Cycle
		c.log.Infof("found interrupted cycle %d; will resume", st.Cycle)
	} else if n, ok, err := c.store.LoadNextCycle(ctx); err != nil {
		return fmt.Errorf("loading next cycle: %w", err)
	} else if ok && n > 0 {
		nextCycle = n
	}

	for {
		// Crawl window (optional): outside [start,end) the crawler sleeps
		// silently until the next window opens. Manual -once runs ignore it.
		if c.cfg.CrawlWindowEnabled && !c.Once {
			if !isInWindow(time.Now(), c.cfg.CrawlStartHour, c.cfg.CrawlEndHour) {
				next := nextWindowStart(time.Now(), c.cfg.CrawlStartHour, c.cfg.CrawlEndHour)
				if c.Stats != nil {
					c.Stats.FinishCycle(next)
					c.Stats.SetWaiting(true)
					c.saveLive(ctx)
				}
				c.log.Infof("outside crawl window (%02d:00-%02d:00); waiting until %s",
					c.cfg.CrawlStartHour, c.cfg.CrawlEndHour, next.Local().Format("15:04"))
				if !c.waitWithHeartbeat(ctx, time.Until(next)) {
					if c.Stats != nil {
						c.Stats.SetWaiting(false)
					}
					return nil
				}
				if c.Stats != nil {
					c.Stats.SetWaiting(false)
				}
			}
		}

		cycleStart := time.Now()
		cycle := nextCycle
		nextCycle++

		err := c.runCycle(ctx, cycle, groups)

		if errors.Is(err, ErrAPIRejected) {
			return ErrAPIRejected
		}
		if ctx.Err() != nil {
			c.log.Infof("shutdown requested; cycle %d interrupted", cycle)
			if c.Stats != nil {
				c.Stats.FinishCycle(time.Time{})
				c.saveLive(ctx)
			}
			return nil
		}
		if err != nil {
			c.log.Errorf("cycle %d finished with errors: %v", cycle, err)
		}
		if c.Once {
			c.log.Infof("single-cycle mode (-once): stopping after cycle %d", cycle)
			if c.Stats != nil {
				c.Stats.FinishCycle(time.Time{})
				c.saveLive(ctx)
			}
			return nil
		}

		// Boundary: window mode - keep cycling back-to-back while inside the
		// window; a cycle that ends after the window finishes, then we wait
		// until the next window. Non-window mode - classic cycle_hours sleep.
		var boundary time.Time
		if c.cfg.CrawlWindowEnabled {
			now := time.Now()
			if isInWindow(now, c.cfg.CrawlStartHour, c.cfg.CrawlEndHour) {
				c.log.Infof("cycle %d complete; still inside the crawl window - starting next cycle immediately", cycle)
				continue
			}
			boundary = nextWindowStart(now, c.cfg.CrawlStartHour, c.cfg.CrawlEndHour)
		} else {
			boundary = cycleStart.Add(time.Duration(c.cfg.CycleHours * float64(time.Hour)))
		}
		wait := time.Until(boundary)
		if c.Stats != nil {
			c.Stats.FinishCycle(boundary)
			c.saveLive(ctx)
		}
		if wait <= 0 {
			c.log.Warnf("cycle %d finished late; starting next cycle immediately", cycle)
			continue
		}
		if c.cfg.CrawlWindowEnabled {
			c.log.Infof("cycle %d complete. waiting until next window at %s",
				cycle, boundary.Local().Format("2006-01-02 15:04:05"))
		} else {
			c.log.Infof("cycle %d complete. next cycle in %v (at %s)", cycle, wait.Round(time.Minute), boundary.Local().Format("2006-01-02 15:04:05"))
		}
		if c.Stats != nil {
			c.Stats.SetWaiting(true)
			c.saveLive(ctx)
		}
		if !c.waitWithHeartbeat(ctx, wait) {
			if c.Stats != nil {
				c.Stats.SetWaiting(false)
			}
			return nil
		}
		if c.Stats != nil {
			c.Stats.SetWaiting(false)
		}
	}
}

// isInWindow reports whether the local time is inside [start, end) hours.
// Wraps around midnight (e.g. 22:00-04:00 means 22:00 today to 04:00 tomorrow).
func isInWindow(t time.Time, start, end int) bool {
	h := t.Hour()
	if start < end {
		return h >= start && h < end
	}
	// wrapped window
	return h >= start || h < end
}

// nextWindowStart returns the next local time the window [start, end) opens.
// Plain windows (2-8) and wrapped windows (22-4, spanning midnight) are
// supported.
func nextWindowStart(t time.Time, start, end int) time.Time {
	today := time.Date(t.Year(), t.Month(), t.Day(), start, 0, 0, 0, t.Location())
	if !isInWindow(t, start, end) {
		// outside the window: open today at start if it is still ahead,
		// otherwise tomorrow
		if t.Before(today) {
			return today
		}
		return today.AddDate(0, 0, 1)
	}
	// inside the window
	if start < end {
		// plain window [start,end): the next opening is tomorrow
		return today.AddDate(0, 0, 1)
	}
	// wrapped window [start,end) spanning midnight: the window that opened
	// yesterday closes today at `end`; the next opening is today at `start`.
	// At exactly the opening hour the window is already open -> tomorrow.
	if t.Equal(today) {
		return today.AddDate(0, 0, 1)
	}
	return today
}

// runCycle crawls every category once.
func (c *Crawler) runCycle(ctx context.Context, cycle int, groups []categoryGroup) error {
	c.log.Infof("=== starting cycle %d ===", cycle)

	cats := buildCatStats(groups)
	if c.Stats != nil {
		c.Stats.BeginCycle(cycle, cats)
	}

	resume, hasResume, err := c.store.LoadState(ctx)
	if err != nil {
		return fmt.Errorf("loading crawl state: %w", err)
	}
	if hasResume {
		if resume.Cycle != cycle || resume.SetIndex >= len(groups) {
			c.log.Infof("ignoring stale resume state (cycle %d, set %d)", resume.Cycle, resume.SetIndex)
			hasResume = false
		} else if !resumeCategoryExists(resume, groups) {
			c.log.Errorf("RESUME STATE REFERENCES CATEGORY %d WHICH IS NO LONGER CONFIGURED in set %d; starting cycle %d fresh (removed-pass will be skipped)",
				resume.CategoryID, resume.SetIndex, cycle)
			hasResume = false
		} else if resume.PageSize != 0 && resume.PageSize != c.cfg.PageSize {
			c.log.Warnf("resume state was saved with page_size=%d but config now has %d; starting cycle %d fresh",
				resume.PageSize, c.cfg.PageSize, cycle)
			hasResume = false
		}
	}

	runID := int64(0)
	if !c.dryRun {
		runID, err = c.store.StartRun(ctx, cycle)
		if err != nil {
			return fmt.Errorf("starting run row: %w", err)
		}
	}

	var (
		requests     int // count + page requests
		pages        int // page requests only
		totalSeen    int
		totalNew     int
		totalUpdated int
		cycleErrors  int
		lastErr      string
		catIndex     int // index into cats / stats
	)

	// pace sleeps a uniform-random amount in [sleep_min, sleep_max] between
	// requests, pauses completely while paused, and aborts on ctx cancel.
	pace := func() {
		if ctx.Err() != nil {
			return
		}
		target := time.Now().Add(randomBetween(c.cfg.SleepMinSeconds, c.cfg.SleepMaxSeconds))
		for {
			if ctx.Err() != nil {
				return
			}
			if c.Stats != nil && c.Stats.IsPaused() {
				time.Sleep(300 * time.Millisecond)
				target = time.Now().Add(randomBetween(c.cfg.SleepMinSeconds, c.cfg.SleepMaxSeconds))
				continue
			}
			if d := time.Until(target); d > 0 {
				time.Sleep(min(d, 500*time.Millisecond))
				continue
			}
			break
		}
	}

	// syncStats pushes counters into the shared Stats + live_stats row.
	syncStats := func() {
		if c.Stats == nil {
			return
		}
		c.Stats.SetCounters(requests, pages, totalSeen, totalNew, totalUpdated, 0, cycleErrors, lastErr)
		c.saveLive(ctx)
	}

	// abort finishes the run row and returns the sentinel when the API
	// persistently refuses our requests.
	abort := func() error {
		if err := c.store.FinishRun(ctx, runID, "aborted", requests, pages, totalSeen, totalNew, totalUpdated, 0, cycleErrors, "API rejected requests"); err != nil {
			c.log.Errorf("finishing aborted run: %v", err)
		}
		return ErrAPIRejected
	}

	apiRejected := false
	circuitBroken := false
	consecutiveCatFailures := 0
	// cycle-wide seen set for coverage reporting (offset-drift detection)
	cycleSeen := make(map[int64]struct{})
	// category-integrity guard: listings whose category field does not match
	// the category being crawled are flagged, never silently archived wrong.
	catMismatches := 0

	for setIdx, grp := range groups {
		if ctx.Err() != nil || circuitBroken {
			break
		}
		for _, catID := range grp.ids {
			if ctx.Err() != nil || circuitBroken {
				break
			}
			// resume positioning
			from := 0
			if hasResume {
				if resume.SetIndex == setIdx && resume.CategoryID == catID {
					from = resume.From
					c.log.Infof("resuming cat %s/%d at from=%d", grp.label, catID, from)
					hasResume = false
				} else {
					// categories before the resume point were already completed
					// in the interrupted run of this cycle -> mark them done
					if c.Stats != nil {
						c.Stats.SetCatProgress(catIndex, 0, 0, true, false)
					}
					catIndex++
					continue // skip categories before the resume point
				}
			}

			catIdx := catIndex
			if c.Stats != nil {
				c.Stats.SetActiveCat(catIdx)
			}

			// count
			pace()
			total, err := c.client.Count(ctx, catID)
			requests++
			if err != nil {
				if errors.Is(err, ErrAPIRejected) {
					apiRejected = true
					break
				}
				if ctx.Err() != nil {
					break // shutting down; not a real error
				}
				cycleErrors++
				consecutiveCatFailures++
				lastErr = fmt.Sprintf("count cat %d: %v", catID, err)
				c.log.Errorf("%s", lastErr)
				if c.Stats != nil {
					c.Stats.SetCatProgress(catIdx, 0, 0, false, true)
					syncStats()
				}
				catIndex++
				if consecutiveCatFailures >= 5 {
					circuitBroken = true
					lastErr = fmt.Sprintf("circuit breaker tripped after %d consecutive category failures (%v)", consecutiveCatFailures, err)
					c.log.Errorf("%s", lastErr)
					break
				}
				continue // skip this category this cycle
			}
			consecutiveCatFailures = 0
			c.log.Infof("cat %s/%d: total=%d", grp.label, catID, total)
			if c.Stats != nil {
				c.Stats.SetCatProgress(catIdx, 0, total, false, false)
				syncStats()
			}

			// pages
			catPages := 0
			catPageCount := int(math.Ceil(float64(total) / float64(c.cfg.PageSize)))
			for from < total && ctx.Err() == nil {
				pace()
				page, err := c.client.FindPage(ctx, catID, from, c.cfg.PageSize)
				requests++
				if err != nil {
					if errors.Is(err, ErrAPIRejected) {
						apiRejected = true
						break
					}
					if ctx.Err() != nil {
						break // shutting down; not a real error
					}
					cycleErrors++
					lastErr = fmt.Sprintf("page cat %d from %d: %v", catID, from, err)
					c.log.Errorf("%s", lastErr)
					break // skip rest of this category this cycle
				}
				pages++

				// merge featured + listings (featured pins may also appear in listings)
				merged := make([]map[string]any, 0, len(page.Featured)+len(page.Listings))
				seen := make(map[int64]struct{}, len(page.Featured)+len(page.Listings))
				// category integrity: every listing in this page must belong to
				// the category we asked for; anything else is an API/query bug
				for _, l := range page.Featured {
					if id := int64(toFloat(l["id"])); id > 0 {
						if _, dup := seen[id]; !dup {
							if int(toFloat(l["category"])) != catID {
								catMismatches++
								c.log.Errorf("CATEGORY MISMATCH: featured listing %d has category=%v while crawling cat %d", id, l["category"], catID)
							}
							seen[id] = struct{}{}
							merged = append(merged, l)
						}
					}
				}
				for _, l := range page.Listings {
					if id := int64(toFloat(l["id"])); id > 0 {
						if _, dup := seen[id]; !dup {
							if int(toFloat(l["category"])) != catID {
								catMismatches++
								c.log.Errorf("CATEGORY MISMATCH: listing %d has category=%v while crawling cat %d", id, l["category"], catID)
							}
							seen[id] = struct{}{}
							merged = append(merged, l)
						}
					}
				}
				for id := range seen {
					cycleSeen[id] = struct{}{}
				}

				if c.dryRun {
					totalSeen += len(merged)
					totalNew += len(merged)
				} else {
					stats, err := c.store.IngestPage(ctx, runID, cycle, merged)
					if err != nil {
						cycleErrors++
						lastErr = fmt.Sprintf("store cat %d from %d: %v", catID, from, err)
						c.log.Errorf("%s", lastErr)
						break
					}
					totalSeen += stats.Seen
					totalNew += stats.New
					totalUpdated += stats.Updated
					if c.Stats != nil {
						c.Stats.AddTotal(int64(stats.New), int64(stats.New+stats.Updated))
						c.Stats.SetCatProgress(catIdx, from, total, false, false)
					}
				}

				if !c.dryRun {
					if err := c.store.SaveState(ctx, CrawlState{Cycle: cycle, SetIndex: setIdx, CategoryID: catID, From: from, PageSize: c.cfg.PageSize}); err != nil {
						c.log.Errorf("saving resume state: %v", err)
					}
				}

				catPages++
				pct := float64(from) / float64(total) * 100
				c.log.Infof("cat %s/%d page %d/%d (%3.1f%%) req=%d | cycle: %d seen, %d new, %d upd",
					grp.label, catID, catPages, catPageCount, pct, requests, totalSeen, totalNew, totalUpdated)
				syncStats()

				from += len(page.Listings)
				if len(page.Listings) == 0 {
					c.log.Warnf("cat %d returned an empty page at from=%d (total=%d); stopping category", catID, from, total)
					if c.Stats != nil {
						c.Stats.SetCatProgress(catIdx, from, total, false, false)
					}
					break
				}
			}
			if c.Stats != nil && !apiRejected {
				c.Stats.SetCatProgress(catIdx, total, total, true, false)
				c.Stats.SetActiveCat(min(catIdx+1, len(cats)-1))
			}
			catIndex++
			if apiRejected {
				break
			}
			// URL enrichment for the completed category (one web request per
			// category per cycle, paced like everything else)
			if !c.dryRun && c.web != nil && ctx.Err() == nil && cycleErrors == 0 {
				pace()
				c.enrichCategoryURLs(ctx, catID)
			}
		}
		if apiRejected {
			break
		}
	}

	if apiRejected {
		return abort()
	}

	interrupted := ctx.Err() != nil
	if interrupted {
		// Manual stop: keep the resume state so the next start continues,
		// and NEVER run the removed-detection pass on a partial cycle.
		c.log.Infof("cycle %d interrupted; resume state kept, removed-pass skipped", cycle)
		if !c.dryRun {
			fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := c.store.FinishRun(fctx, runID, "aborted", requests, pages, totalSeen, totalNew, totalUpdated, 0, cycleErrors, "stopped by user"); err != nil {
				c.log.Errorf("finishing run row: %v", err)
			}
			fcancel()
		}
		if c.Stats != nil {
			c.Stats.SetCounters(requests, pages, totalSeen, totalNew, totalUpdated, 0, cycleErrors, "")
			syncStats()
		}
		return nil
	}

	// URL enrichment and coverage are handled per-category in the loop below.

	if !c.dryRun && !circuitBroken {
		if err := c.store.ClearState(ctx); err != nil {
			c.log.Errorf("clearing resume state: %v", err)
		}
		// persist the next cycle number so restarts don't reset numbering
		if err := c.store.SaveNextCycle(ctx, cycle+1); err != nil {
			c.log.Errorf("saving next cycle: %v", err)
		}
	}

	// Coverage report: unique listings seen vs sum of category totals.
	c.reportCoverage(groups, cycleSeen)
	if catMismatches > 0 {
		c.log.Errorf("CYCLE INTEGRITY: %d listings had a category field that did not match the crawled category (see CATEGORY MISMATCH logs above)", catMismatches)
	} else if len(cycleSeen) > 0 {
		c.log.Infof("cycle integrity: category field matched the crawled category for every listing (%d checked)", len(cycleSeen))
	}

	// Only trust the "removed" pass when the cycle had zero errors AND
	// actually saw listings. A cycle that ingested nothing (API outage,
	// config drift) must NEVER flag the archive as removed.
	var removed int
	if cycleErrors == 0 {
		if totalSeen == 0 || requests == 0 {
			cycleErrors++
			lastErr = fmt.Sprintf("cycle %d saw %d listings in %d requests; REFUSING removed-detection pass (possible API outage or config drift)", cycle, totalSeen, requests)
			c.log.Errorf("%s", lastErr)
		} else if c.dryRun {
			c.log.Infof("dry-run: skipping removed-detection pass")
		} else {
			activeCount, aerr := c.store.DBActiveListingsCount(ctx)
			if aerr == nil && len(cats) >= 20 && activeCount >= 1000 && int64(len(cycleSeen)) < activeCount/2 {
				cycleErrors++
				lastErr = fmt.Sprintf("cycle %d saw only %d unique listings vs %d active in archive (<50%%); REFUSING removed-detection pass", cycle, len(cycleSeen), activeCount)
				c.log.Errorf("%s", lastErr)
			} else {
				removed, err = c.store.MarkRemovedWithGrace(ctx, cycle, max(1, c.cfg.RemovedGraceCycles))
				if err != nil {
					cycleErrors++
					lastErr = fmt.Sprintf("mark removed: %v", err)
					c.log.Errorf("%s", lastErr)
				}
			}
		}
	} else {
		c.log.Warnf("cycle had %d errors; skipping removed-detection pass", cycleErrors)
	}

	// periodic WAL checkpoint keeps the sidecar file bounded
	if !c.dryRun && pages > 0 && pages%500 == 0 {
		if err := c.store.Checkpoint(ctx); err != nil {
			c.log.Errorf("%v", err)
		}
	}

	status := "completed"
	if cycleErrors > 0 {
		status = "aborted"
	}
	if !c.dryRun {
		if err := c.store.FinishRun(ctx, runID, status, requests, pages, totalSeen, totalNew, totalUpdated, removed, cycleErrors, lastErr); err != nil {
			c.log.Errorf("finishing run row: %v", err)
		}
	}
	if c.Stats != nil {
		c.Stats.SetCounters(requests, pages, totalSeen, totalNew, totalUpdated, removed, cycleErrors, lastErr)
		c.Stats.AddTotal(0, int64(removed))
		syncStats()
	}

	c.log.Infof("=== cycle %d done: %d requests, %d listings seen (%d new, %d updated, %d removed), %d errors ===",
		cycle, requests, totalSeen, totalNew, totalUpdated, removed, cycleErrors)

	if cycleErrors > 0 {
		return fmt.Errorf("cycle %d had %d errors", cycle, cycleErrors)
	}
	return nil
}
