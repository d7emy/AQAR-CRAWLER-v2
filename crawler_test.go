package main

// Regression tests for the audit findings (FULL_AUDIT_REPORTv2.md).
// They run against an in-process fake GraphQL server - no network needed.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAPI is an in-process GraphQL server for the real Client/Crawler code.
type fakeAPI struct {
	counts   map[int]int
	pages    func(cat, from, size int) []map[string]any
	failFind bool
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Query string `json:"query"` }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "main:count") {
			var cat int
			_, _ = fmt.Sscanf(req.Query, "{ Search { main:count(where: {category: {eq: %d},published: {eq: 1}}) } }", &cat)
			writeTestJSON(w, map[string]any{"data": map[string]any{"Search": map[string]any{"main": f.counts[cat]}}})
			return
		}
		if f.failFind {
			http.Error(w, "", http.StatusBadRequest)
			return
		}
		var cat, from, size int
		_, _ = fmt.Sscanf(req.Query, "{ Search { main: find(where: {category: {eq: %d},published: {eq: 1}}, from: %d, size: %d,",
			&cat, &from, &size)
		writeTestJSON(w, map[string]any{"data": map[string]any{"Search": map[string]any{
			"main": map[string]any{"total": f.counts[cat], "featured": []any{}, "listings": f.pages(cat, from, size)},
		}}})
	})
	return mux
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// genTestListings builds n fake listings and round-trips them through JSON so
// numbers arrive as float64 exactly like the production decoder produces.
func genTestListings(startID, n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{
			"id": startID + i, "category": 1, "price": 100000 + i,
			"location": map[string]any{"lat": 24.8, "lng": 46.6},
		})
	}
	raw, _ := json.Marshal(out)
	var real []map[string]any
	_ = json.Unmarshal(raw, &real)
	return real
}

func newTestCrawler(t *testing.T, counts map[int]int, pages func(cat, from, size int) []map[string]any) (*Crawler, *Store, *httptest.Server) {
	t.Helper()
	store, err := OpenStore(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SleepMinSeconds, cfg.SleepMaxSeconds = 0.001, 0.002
	cfg.URLEnrichmentEnabled = false
	fake := &fakeAPI{counts: counts, pages: pages}
	srv := httptest.NewServer(fake.handler())
	cfg.APIURL = srv.URL + "/graphql"
	log, err := NewLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close(); store.Close(); log.Close() })
	stats := NewStats()
	c := NewCrawler(cfg, log, NewClient(cfg, log), store)
	c.Stats = stats
	return c, store, srv
}

func seedTestArchive(t *testing.T, store *Store, cycle, n int) {
	t.Helper()
	runID, err := store.StartRun(context.Background(), cycle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IngestPage(context.Background(), runID, cycle, genTestListings(1, n)); err != nil {
		t.Fatal(err)
	}
}

// Test_MarkRemovedGuardZeroSeen: a cycle that saw zero listings must NOT
// mark anything removed (audit Bug 1).
func Test_MarkRemovedGuardZeroSeen(t *testing.T) {
	c, store, _ := newTestCrawler(t, map[int]int{1: 0}, func(cat, from, size int) []map[string]any { return nil })
	seedTestArchive(t, store, 1, 500)

	groups := []categoryGroup{{label: "rent", ids: []int{1}}}
	err := c.runCycle(context.Background(), 2, groups)
	if err == nil {
		t.Fatalf("expected the zero-seen cycle to be flagged as anomalous")
	}
	var removed int
	_ = store.db.QueryRow("SELECT COUNT(*) FROM listings WHERE removed_at IS NOT NULL").Scan(&removed)
	if removed != 0 {
		t.Fatalf("BUG REMAINS: %d listings falsely removed after zero-seen cycle", removed)
	}
	var hist int
	_ = store.db.QueryRow("SELECT COUNT(*) FROM listing_history WHERE change_type='removed'").Scan(&hist)
	if hist != 0 {
		t.Fatalf("BUG REMAINS: %d false 'removed' history rows", hist)
	}
}

// Test_StaleResumeRefused: resume state referencing a category that no longer
// exists must start fresh (requests > 0) instead of skipping the whole cycle
// and mass-removing the archive (audit Bug 2).
func Test_StaleResumeRefused(t *testing.T) {
	// fake API returns ids 1..6 across two categories - exactly what we seed
	c, store, _ := newTestCrawler(t, map[int]int{1: 3, 4: 3}, func(cat, from, size int) []map[string]any {
		if cat == 1 {
			return genTestListings(1, 3)
		}
		return genTestListings(4, 3)
	})
	seedTestArchive(t, store, 1, 6) // ids 1..6 from the previous cycle

	ctx := context.Background()
	if err := store.SaveState(ctx, CrawlState{Cycle: 2, SetIndex: 0, CategoryID: 5, From: 1000, PageSize: 500}); err != nil {
		t.Fatal(err)
	}
	groups := []categoryGroup{{label: "rent", ids: []int{1, 4}}}
	if err := c.runCycle(ctx, 2, groups); err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	var requests int
	_ = store.db.QueryRow("SELECT requests FROM crawl_runs ORDER BY id DESC LIMIT 1").Scan(&requests)
	if requests == 0 {
		t.Fatal("BUG REMAINS: stale resume made the cycle skip everything (0 requests)")
	}
	var removed int
	_ = store.db.QueryRow("SELECT COUNT(*) FROM listings WHERE removed_at IS NOT NULL").Scan(&removed)
	if removed != 0 {
		t.Fatalf("listings were removed even though the fresh cycle saw them all: %d", removed)
	}
}

// Test_ToFloatHandlesInts: toFloat must accept Go int types (audit Bug 6).
func Test_ToFloatHandlesInts(t *testing.T) {
	if toFloat(int(7)) != 7 || toFloat(int64(7)) != 7 || toFloat(uint64(7)) != 7 {
		t.Fatal("toFloat still drops integer types")
	}
}

// Test_TruncateKeepsUTF8: truncate must not split runes (audit Bug 5).
func Test_TruncateKeepsUTF8(t *testing.T) {
	s := strings.Repeat("ع", 100)
	for _, n := range []int{9, 11, 13, 1000} {
		got := truncate(s, n)
		if got != strings.ToValidUTF8(got, "") {
			t.Fatalf("truncate(%d) produced invalid UTF-8: %q", n, got)
		}
	}
}

// Test_CycleNumberPersistence: next cycle number survives a restart
// (audit Future Bug 3).
func Test_CycleNumberPersistence(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.SaveNextCycle(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := store.LoadNextCycle(ctx); err != nil || !ok || n != 4 {
		t.Fatalf("next cycle persistence broken: n=%d ok=%v err=%v", n, ok, err)
	}
}

// Test_IngestDedupInPage: duplicate ids inside one page must not blow up the
// transaction (audit Future Bug 4).
func Test_IngestDedupInPage(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page := genTestListings(7, 1)
	dup := genTestListings(7, 1)
	both := append(page, dup...)
	stats, err := store.IngestPage(ctx, 0, 1, both)
	if err != nil {
		t.Fatalf("BUG REMAINS: duplicate id in page errored: %v", err)
	}
	if stats.New != 1 {
		t.Fatalf("expected 1 new, got %d", stats.New)
	}
	var hist int
	_ = store.db.QueryRow("SELECT COUNT(*) FROM listing_history WHERE listing_id=7").Scan(&hist)
	if hist != 1 {
		t.Fatalf("expected 1 history row, got %d", hist)
	}
}

// Test_DiffJSON: field-level diff between two versions.
func Test_DiffJSON(t *testing.T) {
	oldJ := `{"id":1,"price":70000,"beds":3,"location":{"lat":1.0,"lng":2.0},"user":{"name":"x"}}`
	newJ := `{"id":1,"price":75000,"beds":3,"location":{"lat":1.5,"lng":2.0},"user":{"name":"x"},"pool":true}`
	diffs := DiffJSON(oldJ, newJ)
	found := map[string]bool{}
	for _, d := range diffs {
		found[d.Field] = true
	}
	if !found["price"] || !found["location.lat"] || !found["pool"] {
		t.Fatalf("unexpected diffs: %+v", diffs)
	}
	if found["beds"] || found["user.name"] {
		t.Fatalf("unchanged fields reported as diffs: %+v", diffs)
	}
}

// Test_LoggerSetQuietRestores: SetQuiet(false) must restore console logging
// (audit Bug 4). Verified indirectly: no panic, logger still writes to file.
func Test_LoggerSetQuietRestores(t *testing.T) {
	dir := t.TempDir()
	lg, err := NewLogger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	lg.SetQuiet(true)
	lg.Infof("quiet message")
	lg.SetQuiet(false)
	lg.Infof("post-unquiet message")
	data, err := os.ReadFile(filepath.Join(dir, "crawler.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "post-unquiet message") {
		t.Fatal("logger stopped writing after SetQuiet cycle")
	}
}

// Test_ApplyCategoriesFlag: -categories parsing.
func Test_ApplyCategoriesFlag(t *testing.T) {
	cfg := DefaultConfig()
	if err := applyCategoriesFlag(&cfg, "rent:1,5,4;sale:3,2"); err != nil {
		t.Fatal(err)
	}
	if len(cfg.CategoriesRent) != 3 || len(cfg.CategoriesSale) != 2 {
		t.Fatalf("parsed sets wrong: rent=%v sale=%v", cfg.CategoriesRent, cfg.CategoriesSale)
	}
	if err := applyCategoriesFlag(&cfg, "36"); err != nil {
		t.Fatal(err)
	}
	if len(cfg.CategoriesRent) != 1 || cfg.CategoriesRent[0] != 36 {
		t.Fatalf("plain list should apply to rent: %v", cfg.CategoriesRent)
	}
	if err := applyCategoriesFlag(&cfg, "rent:1;bad"); err == nil {
		t.Fatal("expected parse error for invalid id")
	}
}

// Test_CrawlWindow: window helpers - plain and wrapped windows.
func Test_CrawlWindow(t *testing.T) {
	loc := time.FixedZone("R", 3*3600) // Riyadh-like UTC+3
	mk := func(h, m int) time.Time {
		return time.Date(2026, 8, 9, h, m, 0, 0, loc)
	}
	// plain window 2-8
	for _, tc := range []struct {
		h    int
		want bool
	}{{1, false}, {2, true}, {7, true}, {8, false}, {12, false}} {
		if got := isInWindow(mk(tc.h, 0), 2, 8); got != tc.want {
			t.Errorf("isInWindow(%d:00, 2, 8) = %v want %v", tc.h, got, tc.want)
		}
	}
	// wrapped window 22-4
	for _, tc := range []struct {
		h    int
		want bool
	}{{21, false}, {22, true}, {23, true}, {0, true}, {3, true}, {4, false}} {
		if got := isInWindow(mk(tc.h, 0), 22, 4); got != tc.want {
			t.Errorf("isInWindow(%d:00, 22, 4) = %v want %v", tc.h, got, tc.want)
		}
	}
	// nextWindowStart
	if n := nextWindowStart(mk(12, 0), 2, 8); n.Hour() != 2 || n.Day() != 10 {
		t.Errorf("nextWindowStart(12:00, 2-8) = %v, want tomorrow 02:00", n)
	}
	if n := nextWindowStart(mk(1, 0), 2, 8); n.Hour() != 2 || n.Day() != 9 {
		t.Errorf("nextWindowStart(01:00, 2-8) = %v, want today 02:00", n)
	}
	if n := nextWindowStart(mk(5, 0), 2, 8); n.Hour() != 2 || n.Day() != 10 {
		t.Errorf("nextWindowStart(05:00 inside window, 2-8) = %v, want tomorrow 02:00", n)
	}
	if n := nextWindowStart(mk(3, 0), 22, 4); n.Hour() != 22 || n.Day() != 9 {
		t.Errorf("nextWindowStart(03:00 inside wrapped 22-4) = %v, want today 22:00", n)
	}
}

// Test_RandomBetweenRange: pacing range sanity.
func Test_RandomBetweenRange(t *testing.T) {
	for i := 0; i < 2000; i++ {
		d := randomBetween(10.5, 14.5)
		if d.Seconds() < 10.5 || d.Seconds() > 14.5 {
			t.Fatalf("out of range: %v", d)
		}
	}
	if randomBetween(5, 2).Seconds() != 5 {
		t.Fatal("degenerate range must return min")
	}
}

// Test_CompressDecompressLossless verifies that AQZ1 dictionary compression
// round-trips 100% of a realistic multi-kilobyte AQAR listing byte-for-byte.
func Test_CompressDecompressLossless(t *testing.T) {
	listing := map[string]any{
		"id": 6560996, "category": 1, "price": 45000, "area": 165,
		"beds": 4, "rooms": 5, "livings": 1, "wc": 3, "furnished": 0,
		"title":   "شقة للإيجار في حي النرجس بمدينة الرياض",
		"content": "شقة فاخرة جديدة، غرف واسعة، موقف خاص، قريبة من الخدمات والمدارس",
		"address": "شارع ريحانة بنت زيد، حي النرجس، الرياض، منطقة الرياض",
		"location": map[string]any{"lat": 24.85123, "lng": 46.63192},
		"user": map[string]any{
			"user_id": 123456, "name": "مكتب عقارات النرجس", "phone": "966500000000",
			"type": 2, "verified": true, "iam_verified": true, "fal_license_number": "1200012345",
		},
		"imgs":      []string{"000000001_1750000001234.jpg", "000000002_1750000005678.jpg"},
		"imgs_desc": []string{"", ""},
	}
	raw, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	comp := CompressJSON(raw)
	if !IsCompressedJSON(comp) {
		t.Fatalf("expected AQZ1 magic header on compressed payload (len=%d)", len(comp))
	}
	if len(comp) >= len(raw) {
		t.Fatalf("expected compressed size (%d) < raw size (%d)", len(comp), len(raw))
	}
	decomp, err := DecompressJSON(comp)
	if err != nil {
		t.Fatalf("DecompressJSON failed: %v", err)
	}
	if decomp != string(raw) {
		t.Fatalf("round-trip mismatch:\ngot:  %s\nwant: %s", decomp, string(raw))
	}
	// Plain JSON passed to DecompressJSON must also return unchanged.
	plainOut, err := DecompressJSON(raw)
	if err != nil || plainOut != string(raw) {
		t.Fatalf("plain fallback failed: err=%v", err)
	}
}

// Test_CanonicalContentHashIgnoresNoise verifies that API replica array jitter
// and volatile counters do not change the canonical hash, while real changes do.
func Test_CanonicalContentHashIgnoresNoise(t *testing.T) {
	v1 := []byte(`{"bookable_unit_interested_users_count":0,"bookable_unit_plans":null,"id":100,"imgs_desc":null,"price":50000,"refresh":1700000000,"sov_campaign_id":null,"title":"شقة","user":{"name":"أحمد","paid":1,"review":4.5},"whatsapp_requested_count":2,"whatsapp_responded_count":1}`)
	v2 := []byte(`{"bookable_unit_interested_users_count":5,"bookable_unit_plans":[""],"id":100,"imgs_desc":[""],"price":50000,"refresh":1700099999,"sov_campaign_id":"cmp_1","title":"شقة","user":{"name":"أحمد","paid":2,"review":4.8},"whatsapp_requested_count":9,"whatsapp_responded_count":7}`)
	v3 := []byte(`{"bookable_unit_interested_users_count":5,"bookable_unit_plans":[],"id":100,"imgs_desc":[],"price":50000,"refresh":1700199999,"sov_campaign_id":null,"title":"شقة","user":{"name":"أحمد","paid":0,"review":4.1},"whatsapp_requested_count":9,"whatsapp_responded_count":7}`)

	h1 := CanonicalContentHashBytes(v1)
	h2 := CanonicalContentHashBytes(v2)
	h3 := CanonicalContentHashBytes(v3)
	if h1 != h2 || h2 != h3 {
		t.Fatalf("noise fields changed canonical hash:\nh1=%s\nh2=%s\nh3=%s", h1, h2, h3)
	}

	// Real price change MUST produce a different hash.
	vPrice := []byte(`{"bookable_unit_interested_users_count":5,"bookable_unit_plans":[],"id":100,"imgs_desc":[],"price":55000,"refresh":1700199999,"sov_campaign_id":null,"title":"شقة","user":{"name":"أحمد","paid":0,"review":4.1},"whatsapp_requested_count":9,"whatsapp_responded_count":7}`)
	if CanonicalContentHashBytes(vPrice) == h1 {
		t.Fatal("real price change did not change canonical hash")
	}

	// Real non-empty image description MUST produce a different hash.
	vDesc := []byte(`{"bookable_unit_interested_users_count":5,"bookable_unit_plans":[],"id":100,"imgs_desc":["واجهة المبنى"],"price":50000,"refresh":1700199999,"sov_campaign_id":null,"title":"شقة","user":{"name":"أحمد","paid":0,"review":4.1},"whatsapp_requested_count":9,"whatsapp_responded_count":7}`)
	if CanonicalContentHashBytes(vDesc) == h1 {
		t.Fatal("real imgs_desc caption did not change canonical hash")
	}
}

// Test_TouchClearsZombieRemovedAndUpdatesBlob ensures that a listing re-seen
// after being marked removed has its removed_at cleared and does not spam
// listing_history when only volatile fields changed.
func Test_TouchClearsZombieRemovedAndUpdatesBlob(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	l1 := []map[string]any{{
		"id": float64(42), "category": float64(1), "price": float64(60000),
		"refresh": float64(1000), "imgs_desc": nil,
	}}
	if _, err := store.IngestPage(ctx, 1, 1, l1); err != nil {
		t.Fatal(err)
	}

	// Cycle 2 & 3: listing 42 is absent -> Marked removed with grace=2 at cycle 3.
	if rem, err := store.MarkRemovedWithGrace(ctx, 2, 2); err != nil || rem != 0 {
		t.Fatalf("grace=2 at cycle 2 should remove 0, got %d (err=%v)", rem, err)
	}
	if rem, err := store.MarkRemovedWithGrace(ctx, 3, 2); err != nil || rem != 1 {
		t.Fatalf("grace=2 at cycle 3 should remove 1, got %d (err=%v)", rem, err)
	}

	// Cycle 4: listing 42 reappears with a newer refresh timestamp and imgs_desc=[""] jitter.
	l2 := []map[string]any{{
		"id": float64(42), "category": float64(1), "price": float64(60000),
		"refresh": float64(2000), "imgs_desc": []any{""},
	}}
	st, err := store.IngestPage(ctx, 2, 4, l2)
	if err != nil {
		t.Fatal(err)
	}
	if st.Updated != 0 || st.New != 0 {
		t.Fatalf("expected unchanged (0 new, 0 updated), got %+v", st)
	}

	var removedAt *string
	var refresh int64
	if err := store.db.QueryRow("SELECT removed_at, refresh FROM listings WHERE id=42").Scan(&removedAt, &refresh); err != nil {
		t.Fatal(err)
	}
	if removedAt != nil {
		t.Fatalf("zombie removed_at was not cleared on touch: %v", *removedAt)
	}
	if refresh != 2000 {
		t.Fatalf("expected refresh=2000 to be updated on touch, got %d", refresh)
	}
	fullJSON, err := store.GetListingJSON(ctx, 42)
	if err != nil || !strings.Contains(fullJSON, `"refresh":2000`) {
		t.Fatalf("expected full decompressed JSON with refresh=2000, got %q (err=%v)", fullJSON, err)
	}
}

// Test_CompactDBLosslessAndPurgesNoise tests CompactDB end-to-end.
func Test_CompactDBLosslessAndPurgesNoise(t *testing.T) {
	dbPath := t.TempDir() + "/compact_test.db"
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	runID, _ := store.StartRun(ctx, 1)
	_ = store.FinishRun(ctx, runID, "completed", 10, 5, 2, 2, 0, 0, 0, "")

	// Seed 2 listings with uncompressed raw JSON and noisy history rows
	j1 := `{"id":1,"category":1,"price":50000,"imgs_desc":null,"refresh":100}`
	j1Noise := `{"id":1,"category":1,"price":50000,"imgs_desc":[""],"refresh":200}`
	j1Real := `{"id":1,"category":1,"price":48000,"imgs_desc":[""],"refresh":300}`
	j2 := `{"id":2,"category":1,"price":90000}`

	_, err = store.db.Exec(`
		INSERT INTO listings (id, data_json, data_hash, category, price, first_seen_at, last_seen_at, last_change_at, removed_at, seen_cycle)
		VALUES
		(1, ?, 'oldhash1', 1, 48000, '2026-08-01T00:00:00Z', '2026-08-03T00:00:00Z', '2026-08-03T00:00:00Z', '2026-08-02T00:00:00Z', 1),
		(2, ?, 'oldhash2', 1, 90000, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', '2026-08-02T00:00:00Z', 0)
	`, j1Real, j2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`
		INSERT INTO listing_history (listing_id, data_json, change_type, fetched_at) VALUES
		(1, ?, 'new',     '2026-08-01T00:00:00Z'),
		(1, ?, 'update',  '2026-08-01T06:00:00Z'),
		(1, ?, 'removed', '2026-08-02T00:00:00Z'),
		(1, ?, 'update',  '2026-08-03T00:00:00Z'),
		(2, ?, 'new',     '2026-08-01T00:00:00Z'),
		(2, ?, 'removed', '2026-08-02T00:00:00Z')
	`, j1, j1Noise, j1Noise, j1Real, j2, j2)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	lg, _ := NewLogger(t.TempDir())
	defer lg.Close()

	st, err := CompactDB(ctx, dbPath, lg)
	if err != nil {
		t.Fatalf("CompactDB: %v", err)
	}
	if st.ListingsTotal != 2 || st.ZombiesHealed != 1 || st.NoiseUpdatesPurged != 1 || st.FalseRemovalsPurged != 1 {
		t.Fatalf("unexpected CompactStats: %+v", st)
	}
	if st.HistoryNewRows != 4 { // id 1: new + real update; id 2: new + removed
		t.Fatalf("expected 4 kept history rows, got %d", st.HistoryNewRows)
	}

	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	gotJ1, err := store2.GetListingJSON(ctx, 1)
	if err != nil || gotJ1 != j1Real {
		t.Fatalf("listing 1 lossless check failed: got %q want %q (err=%v)", gotJ1, j1Real, err)
	}
	hist1, err := store2.ListingHistory(ctx, 1, 10)
	if err != nil || len(hist1) != 2 {
		t.Fatalf("expected 2 history rows for listing 1, got %d (err=%v)", len(hist1), err)
	}
}

// Test_ProxyFallbackDirect verifies that a dead configured proxy automatically
// falls back to direct connection when ProxyFallbackDirect is enabled.
func Test_ProxyFallbackDirect(t *testing.T) {
	fake := &fakeAPI{counts: map[int]int{1: 42}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.APIURL = srv.URL + "/graphql"
	cfg.ProxyURL = "socks5://127.0.0.1:1" // closed port -> immediate dial failure
	cfg.ProxyFallbackDirect = true

	lg, _ := NewLogger(t.TempDir())
	defer lg.Close()

	client := NewClient(cfg, lg)
	n, err := client.Count(context.Background(), 1)
	if err != nil {
		t.Fatalf("expected automatic fallback to direct connection, got error: %v", err)
	}
	if n != 42 {
		t.Fatalf("expected count=42, got %d", n)
	}
}

// Test_PriceRadarAndInspector verifies RecentChanges, InspectListing, and ArchiveAnalytics.
func Test_PriceRadarAndInspector(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/radar.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Cycle 1: ingest listing 777 at 100,000 SAR
	_, err = store.IngestPage(ctx, 1, 1, []map[string]any{{
		"id": float64(777), "category": float64(1), "price": float64(100000),
		"area": float64(180), "beds": float64(4), "title": "شقة فاخرة", "address": "الرياض",
	}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	// Cycle 2: price drops to 85,000 SAR (-15%)
	_, err = store.IngestPage(ctx, 2, 2, []map[string]any{{
		"id": float64(777), "category": float64(1), "price": float64(85000),
		"area": float64(180), "beds": float64(4), "title": "شقة فاخرة", "address": "الرياض",
	}})
	if err != nil {
		t.Fatal(err)
	}

	sum, err := store.InspectListing(ctx, 777)
	if err != nil {
		t.Fatalf("InspectListing: %v", err)
	}
	if sum.Price != 85000 || sum.CompressedBytes == 0 || sum.RawBytes <= sum.CompressedBytes {
		t.Fatalf("unexpected ListingSummary: %+v", sum)
	}

	drops, err := store.RecentChanges(ctx, 10, true)
	if err != nil || len(drops) != 1 {
		t.Fatalf("expected 1 price drop, got %d (err=%v)", len(drops), err)
	}
	if drops[0].OldPrice != 100000 || drops[0].NewPrice != 85000 || drops[0].PriceDelta != -15000 {
		t.Fatalf("unexpected PriceChangeEvent: %+v", drops[0])
	}

	an, err := store.ArchiveAnalytics(ctx)
	if err != nil || an.ActiveListings != 1 || an.UpdatedListings != 1 {
		t.Fatalf("unexpected ArchiveAnalytics: %+v (err=%v)", an, err)
	}
}

var _ = time.Now
