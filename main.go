// AQAR-CRAWLER: slow 24-hour crawler that archives every AQAR property
// listing (rent + sale, all categories) into a single SQLite file.
//
// Usage:
//
//	aqar-crawler.exe                 plain log mode (24h cycles)
//	aqar-crawler.exe -tui            crawler + live dashboard in one process
//	aqar-crawler.exe -watch          dashboard only (crawler runs elsewhere)
//	aqar-crawler.exe -validate       verify the API is reachable, then exit
//
// Exit codes:
//
//	0 - clean stop (cycle done or graceful interrupt)
//	1 - fatal error
//	2 - API rejected our requests (check logs/crawler.log)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	fs := flag.NewFlagSet("aqar-crawler", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config file")
	once := fs.Bool("once", false, "run a single cycle and exit")
	validate := fs.Bool("validate", false, "only verify the API is reachable, then exit")
	reset := fs.Bool("reset", false, "clear resume state and start cycle 1 fresh")
	tuiMode := fs.Bool("tui", false, "run the crawler with a live dashboard")
	watchMode := fs.Bool("watch", false, "dashboard only; read stats from the database (crawler must be running elsewhere)")
	webMode := fs.Bool("web", false, "run the crawler + web control panel (http://web_host:web_port)")
	categoriesFlag := fs.String("categories", "", `override category sets, e.g. "rent:1,5,4;sale:3,2" or "36" (applies to rent)`)
	dryRunFlag := fs.Bool("dry-run", false, "fetch and log only; skip all database writes")
	compactFlag := fs.Bool("compact", false, "compact, deduplicate noise history, and compress the SQLite database in-place, then exit")
	inspectFlag := fs.Int64("inspect", 0, "inspect a single listing ID (metadata, version diffs, and full decompressed JSON), then exit")
	radarFlag := fs.Bool("radar", false, "print archive analytics and recent price drops/updates from the database, then exit")

	// Flag values are parsed into this default config first so we can detect
	// which flags were explicitly set, then overlaid on the file config.
	overrides := DefaultConfig()
	overrides.ApplyFlags(fs)
	fs.Parse(os.Args[1:])

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}
	// Explicit command-line flags win over the config file.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "db":
			cfg.DBPath = overrides.DBPath
		case "sleep-min":
			cfg.SleepMinSeconds = overrides.SleepMinSeconds
		case "sleep-max":
			cfg.SleepMaxSeconds = overrides.SleepMaxSeconds
		case "page-size":
			cfg.PageSize = overrides.PageSize
		case "cycle-hours":
			cfg.CycleHours = overrides.CycleHours
		case "req-time":
			cfg.ReqTime = overrides.ReqTime
		case "req-hash":
			cfg.ReqHash = overrides.ReqHash
		case "web-port":
			cfg.WebPort = overrides.WebPort
		}
	})

	if err := cfg.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: config: %v\n", err)
		os.Exit(1)
	}
	if *categoriesFlag != "" {
		if err := applyCategoriesFlag(&cfg, *categoriesFlag); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: -categories: %v\n", err)
			os.Exit(1)
		}
	}
	if *dryRunFlag {
		cfg.DryRun = true
	}

	log, err := NewLogger(cfg.LogDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}
	defer log.Close()

	if *compactFlag {
		sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		st, err := CompactDB(sigCtx, cfg.DBPath, log)
		if err != nil {
			log.Fatalf("database compaction failed: %v", err)
			os.Exit(1)
		}
		log.Infof("compaction summary: %.1f MB -> %.1f MB (%.1fx reduction) in %v",
			st.OldSizeMB, st.NewSizeMB, st.OldSizeMB/max(0.1, st.NewSizeMB), st.Duration.Round(time.Millisecond))
		os.Exit(0)
	}

	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("opening database: %v", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	if err := store.Migrate(ctx); err != nil {
		cancel()
		log.Fatalf("migrating database: %v", err)
		os.Exit(1)
	}
	cancel()

	if *inspectFlag > 0 {
		ictx, icancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer icancel()
		sum, err := store.InspectListing(ictx, *inspectFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Listing %d not found: %v\n", *inspectFlag, err)
			os.Exit(1)
		}
		hist, _ := store.ListingHistory(ictx, *inspectFlag, 50)
		fmt.Printf("=== Listing #%d (%s) ===\n", sum.ID, sum.CategoryName)
		fmt.Printf("Price: %.0f SAR | Area: %.1f m² | Beds: %d | Rooms: %d\n", sum.Price, sum.Area, sum.Beds, sum.Rooms)
		fmt.Printf("Title: %s\nAddress: %s\nURL: %s\n", sum.Title, sum.Address, sum.URL)
		fmt.Printf("First Seen: %s | Last Seen: %s (cycle %d) | Removed: %s\n", sum.FirstSeenAt, sum.LastSeenAt, sum.SeenCycle, sum.RemovedAt)
		fmt.Printf("Storage: %d bytes compressed -> %d bytes raw (%.1fx lossless compression)\n",
			sum.CompressedBytes, sum.RawBytes, float64(sum.RawBytes)/max(1.0, float64(sum.CompressedBytes)))
		fmt.Printf("History Snapshots (%d):\n", len(hist))
		var prev string
		for i := len(hist) - 1; i >= 0; i-- {
			h := hist[i]
			fmt.Printf("  - [%s] %s", h.FetchedAt, h.ChangeType)
			if h.DataJSON != "" {
				if prev != "" {
					diffs := DiffJSON(prev, h.DataJSON)
					for _, d := range diffs {
						fmt.Printf(" | %s: %v -> %v", d.Field, d.From, d.To)
					}
				}
				prev = h.DataJSON
			}
			fmt.Println()
		}
		os.Exit(0)
	}

	if *radarFlag {
		rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rcancel()
		_ = store.Checkpoint(rctx)
		an, err := store.ArchiveAnalytics(rctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Analytics failed: %v\n", err)
			_ = store.Close()
			os.Exit(1)
		}
		fmt.Printf("=== AQAR Archive Analytics (%s | %.1f MB) ===\n", cfg.DBPath, store.DBSizeMB())
		fmt.Printf("Listings: %d total (%d active, %d removed, %d with history updates)\n",
			an.TotalListings, an.ActiveListings, an.RemovedListings, an.UpdatedListings)
		fmt.Printf("History:  %d snapshots | Avg compressed listing blob: %d bytes\n\n", an.TotalHistory, an.AvgBlobBytes)
		fmt.Printf("=== Recent Price Drops ===\n")
		drops, _ := store.RecentChanges(rctx, 15, true)
		for _, d := range drops {
			fmt.Printf("  #%d [%s] %.0f -> %.0f SAR (%.1f%%) | %s | %s\n",
				d.ID, d.CategoryName, d.OldPrice, d.NewPrice, d.PricePct, d.ChangedAt, d.Title)
		}
		_ = store.Close()
		os.Exit(0)
	}

	if *reset {
		if err := store.ClearState(context.Background()); err != nil {
			log.Fatalf("clearing resume state: %v", err)
			os.Exit(1)
		}
		log.Infof("resume state cleared; starting fresh")
	}

	client := NewClient(cfg, log)

	notify := NewNotifier(cfg, log)
	if cfg.TelegramEnabled && !cfg.DryRun {
		notify.Verify()
	}

	if *validate {
		if err := client.CheckAPI(context.Background()); err != nil {
			if err == ErrAPIRejected {
				log.APIRejectedWarning()
				os.Exit(2)
			}
			log.Fatalf("api check failed: %v", err)
			os.Exit(1)
		}
		log.Infof("API reachable and responding. ready to crawl.")
		os.Exit(0)
	}

	if *tuiMode || *watchMode {
		log.SetQuiet(true) // bubbletea owns the terminal now
		code := runTUI(cfg, log, store, client, *tuiMode, *watchMode, *once, notify)
		os.Exit(code)
	}

	if *webMode {
		code := runWeb(cfg, log, store, client)
		os.Exit(code)
	}

	log.Infof("AQAR crawler starting | api=%s page_size=%d sleep=%v-%vs cycle=%vh db=%s",
		cfg.APIURL, cfg.PageSize, cfg.SleepMinSeconds, cfg.SleepMaxSeconds, cfg.CycleHours, cfg.DBPath)
	log.Infof("categories: %d rent, %d sale (total %d)",
		len(cfg.CategoriesRent), len(cfg.CategoriesSale), len(cfg.CategoriesRent)+len(cfg.CategoriesSale))

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stats := NewStats()

	crawler := NewCrawler(cfg, log, client, store)
	crawler.Stats = stats
	crawler.Once = *once

	err = crawler.Run(sigCtx)
	reason := "stopped cleanly"
	switch {
	case err == ErrAPIRejected:
		reason = "API rejected requests"
		log.APIRejectedWarning()
		os.Exit(2)
	case err != nil:
		reason = "crawler error: " + err.Error()
		log.Fatalf("crawler stopped with errors: %v", err)
		os.Exit(1)
	default:
		log.Infof("crawler stopped cleanly")
	}
	snapshot := stats.Snapshot()
	nctx, ncancel := context.WithTimeout(context.Background(), 10*time.Second)
	listings, _ := store.DBListingsCount(nctx)
	history, _ := store.DBHistoryCount(nctx)
	ncancel()
	notify.Stopped(snapshot.Cycle, reason, snapshot, listings, history, store.DBSizeMB())
	os.Exit(0)
}

// applyCategoriesFlag parses "rent:1,5;sale:3,2" or a plain "1,5" list
// (applied to the rent set when no label is given).
func applyCategoriesFlag(cfg *Config, flagVal string) error {
	parse := func(s string) ([]int, error) {
		var ids []int
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			var n int
			if _, err := fmt.Sscanf(part, "%d", &n); err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid category id %q", part)
			}
			ids = append(ids, n)
		}
		return ids, nil
	}
	hadRent, hadSale := false, false
	var rent, sale []int
	for _, seg := range strings.Split(flagVal, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, "rent:") {
			ids, err := parse(strings.TrimPrefix(seg, "rent:"))
			if err != nil {
				return err
			}
			rent, hadRent = ids, true
		} else if strings.HasPrefix(seg, "sale:") {
			ids, err := parse(strings.TrimPrefix(seg, "sale:"))
			if err != nil {
				return err
			}
			sale, hadSale = ids, true
		} else {
			ids, err := parse(seg)
			if err != nil {
				return err
			}
			rent, hadRent = ids, true
		}
	}
	if hadRent {
		cfg.CategoriesRent = rent
	}
	if hadSale {
		cfg.CategoriesSale = sale
	}
	if len(cfg.CategoriesRent)+len(cfg.CategoriesSale) == 0 {
		return errors.New("no categories after parsing -categories")
	}
	return nil
}

// runTUI starts the dashboard and (optionally) the crawler itself.
func runTUI(cfg Config, log *Logger, store *Store, client *Client, tuiMode, watchMode, once bool, notify *Notifier) int {
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var crawler *Crawler
	var stats *Stats
	done := make(chan error, 1)

	if tuiMode {
		stats = NewStats()
		crawler = NewCrawler(cfg, log, client, store)
		crawler.Stats = stats
		crawler.Once = once
		ctx, cancel := context.WithCancel(sigCtx)
		defer cancel()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			done <- crawler.Run(ctx)
		}()
		model := NewTUIModel(stats, store, cfg, false, once)
		if _, err := tea.NewProgram(model, tea.WithAltScreen()).Run(); err != nil {
			log.Fatalf("tui error: %v", err)
			cancel()
			wg.Wait()
			return 1
		}
		cancel()
		wg.Wait()
	} else {
		model := NewTUIModel(nil, store, cfg, true, false)
		if _, err := tea.NewProgram(model, tea.WithAltScreen()).Run(); err != nil {
			log.Fatalf("tui error: %v", err)
			return 1
		}
		return 0
	}

	err := <-done
	reason := "stopped cleanly"
	switch {
	case err == ErrAPIRejected:
		reason = "API rejected requests"
		log.APIRejectedWarning()
		return 2
	case err != nil:
		reason = "crawler error: " + err.Error()
		log.Errorf("crawler stopped with errors: %v", err)
	}
	snapshot := stats.Snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	listings, _ := store.DBListingsCount(ctx)
	history, _ := store.DBHistoryCount(ctx)
	cancel()
	notify.Stopped(snapshot.Cycle, reason, snapshot, listings, history, store.DBSizeMB())
	if err != nil && err != ErrAPIRejected {
		return 1
	}
	return 0
}

// runWeb starts the web control panel and the crawler (controlled from the
// browser: start/stop/pause/resume). Blocks until Ctrl+C.
func runWeb(cfg Config, log *Logger, store *Store, client *Client) int {
	log.Infof("AQAR crawler web mode | db=%s panel=%s:%d",
		cfg.DBPath, cfg.WebHost, cfg.WebPort)

	ctrl := NewController(cfg, log, client, store)
	if err := ctrl.Start(); err != nil {
		log.Fatalf("starting crawler: %v", err)
		return 1
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serveWeb(sigCtx, cfg, log, store, ctrl); err != nil {
		log.Fatalf("%v", err)
		_ = ctrl.Stop()
		return 1
	}

	log.Infof("shutting down; stopping crawler (resume state kept)")
	_ = ctrl.Stop() // blocks until the crawler goroutine exits, incl. Telegram alert
	if lastErr := ctrl.LastError(); lastErr != "" {
		log.Errorf("last crawler error: %s", lastErr)
		if strings.Contains(lastErr, "API rejected") {
			log.APIRejectedWarning()
			return 2
		}
	}
	return 0
}
