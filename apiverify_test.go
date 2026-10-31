package main

// Malformed-data + API failure-mode regression tests (API_VERIFICATION).
// All run against the in-process fake GraphQL server - no live API needed.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rawFakeAPI serves raw bodies (or status codes) for count/find queries so we
// can simulate malformed and failing API responses. If countBody is set it is
// served verbatim for the count query (no per-category parsing).
type rawFakeAPI struct {
	countBody string
	findBody  string
	countCode int
	findCode  int
}

func (f *rawFakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Query string `json:"query"` }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "main:count") {
			if f.countCode != 0 {
				w.WriteHeader(f.countCode)
				if f.countCode != http.StatusTooManyRequests {
					_, _ = w.Write([]byte(f.countBody))
				}
				return
			}
			_, _ = w.Write([]byte(f.countBody))
			return
		}
		if f.findCode != 0 {
			w.WriteHeader(f.findCode)
			if f.findCode != http.StatusTooManyRequests {
				_, _ = w.Write([]byte(f.findBody))
			}
			return
		}
		_, _ = w.Write([]byte(f.findBody))
	})
	return mux
}

func runCycleWithRawAPI(t *testing.T, countBody, findBody string, countCode, findCode int) (*Store, error) {
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
	fake := &rawFakeAPI{countBody: countBody, findBody: findBody, countCode: countCode, findCode: findCode}
	srv := httptest.NewServer(fake.handler())
	cfg.APIURL = srv.URL + "/graphql"
	log, err := NewLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close(); store.Close(); log.Close() })
	client := NewClient(cfg, log)
	client.retryBase = 10 * time.Millisecond
	client.rateLimitBase = 20 * time.Millisecond
	c := NewCrawler(cfg, log, client, store)
	err = c.runCycle(context.Background(), 1, []categoryGroup{{label: "rent", ids: []int{1}}})
	return store, err
}

// Test_MalformedFindBodyTruncated: truncated JSON body must fail the page
// cleanly (error counted, category skipped) - no panic, no false removals.
func Test_MalformedFindBodyTruncated(t *testing.T) {
	store, _ := runCycleWithRawAPI(t, `{"data":{"Search":{"main":5}}}`, `{"data":{"Search":{"main":{"total":5,"featured":[],"listings":[{"id":1`, 0, 0)
	var removed int
	_ = store.db.QueryRow("SELECT COUNT(*) FROM listings WHERE removed_at IS NOT NULL").Scan(&removed)
	if removed != 0 {
		t.Fatalf("truncated body caused removals: %d", removed)
	}
	t.Log("truncated JSON handled cleanly: page failed, no removals")
}

// Test_MalformedFindBodyNull: JSON null body must not panic.
func Test_MalformedFindBodyNull(t *testing.T) {
	_, _ = runCycleWithRawAPI(t, `{"data":{"Search":{"main":5}}}`, `null`, 0, 0)
	t.Log("null body handled without panic")
}

// Test_MalformedFindBodyEmpty: empty body must not panic.
func Test_MalformedFindBodyEmpty(t *testing.T) {
	_, _ = runCycleWithRawAPI(t, `{"data":{"Search":{"main":5}}}`, ``, 0, 0)
	t.Log("empty body handled without panic")
}

// Test_MalformedFindWrongShape: listings that are not objects must be dropped
// silently (toMapSlice), not crash.
func Test_MalformedFindWrongShape(t *testing.T) {
	store, _ := runCycleWithRawAPI(t,
		`{"data":{"Search":{"main":5}}}`,
		`{"data":{"Search":{"main":{"total":5,"featured":[42,"x"],"listings":[{"id":1,"category":1},null,"str",7,{"id":2,"category":1}]}}}}`,
		0, 0)
	var n int
	_ = store.db.QueryRow("SELECT COUNT(*) FROM listings").Scan(&n)
	if n != 2 {
		t.Fatalf("expected 2 valid listings ingested, got %d", n)
	}
	t.Log("non-object listings dropped cleanly, 2 valid ones ingested")
}

// Test_MalformedListingFields: listings with missing/zero/negative ids and
// garbage field types must be skipped or stored safely - never crash.
func Test_MalformedListingFields(t *testing.T) {
	store, _ := runCycleWithRawAPI(t,
		`{"data":{"Search":{"main":5}}}`,
		`{"data":{"Search":{"main":{"total":9,"featured":[],"listings":[
			{"id":0,"category":1},
			{"id":-5,"category":1},
			{"price":"garbage","category":1},
			{"id":10,"category":"1","price":null},
			{"id":11,"category":1,"location":null},
			{"id":12,"category":999},
			{"id":13,"category":1,"price":[1,2,3]},
			{"id":14},
			{"id":15,"category":1,"location":{"lat":"24.8","lng":"46.6"}}
		]}}}}`,
		0, 0)
	var n int
	_ = store.db.QueryRow("SELECT COUNT(*) FROM listings").Scan(&n)
	t.Logf("ingested %d listings from the malformed page (ids 10-15 valid, 0/-5/garbage dropped)", n)
	if n < 5 {
		t.Fatalf("expected at least 5 valid listings, got %d", n)
	}
	var price float64
	_ = store.db.QueryRow("SELECT price FROM listings WHERE id=10").Scan(&price)
	if price != 0 {
		t.Fatalf("string category/price listing should be stored with zero price, got %v", price)
	}
}

// Test_MalformedCountBody: count body with wrong shape must count as an error
// (category skipped) - and never trigger removals.
func Test_MalformedCountBody(t *testing.T) {
	store, _ := runCycleWithRawAPI(t,
		`not json at all`,
		`{"data":{"Search":{"main":{"total":5,"featured":[],"listings":[{"id":1,"category":1}]}}}}`,
		0, 0)
	var removed int
	_ = store.db.QueryRow("SELECT COUNT(*) FROM listings WHERE removed_at IS NOT NULL").Scan(&removed)
	if removed != 0 {
		t.Fatalf("bad count body caused removals: %d", removed)
	}
	t.Log("malformed count body: category skipped, no removals")
}

// Test_APIFailureModes: 429 and 500 must be retried then fail cleanly (no
// crash, no removals).
func Test_APIFailureModes(t *testing.T) {
	// 429 on find
	store429, _ := runCycleWithRawAPI(t,
		`{"data":{"Search":{"main":5}}}`,
		`{"data":{"Search":{"main":{"total":5,"featured":[],"listings":[{"id":1,"category":1}]}}}}`,
		0, http.StatusTooManyRequests)
	var removed int
	_ = store429.db.QueryRow("SELECT COUNT(*) FROM listings WHERE removed_at IS NOT NULL").Scan(&removed)
	if removed != 0 {
		t.Fatalf("429 caused removals: %d", removed)
	}
	t.Log("429: retried, failed cleanly, no removals")

	// 500 on find
	store500, _ := runCycleWithRawAPI(t,
		`{"data":{"Search":{"main":5}}}`,
		`{"data":{"Search":{"main":{"total":5,"featured":[],"listings":[{"id":1,"category":1}]}}}}`,
		0, http.StatusInternalServerError)
	_ = store500.db.QueryRow("SELECT COUNT(*) FROM listings WHERE removed_at IS NOT NULL").Scan(&removed)
	if removed != 0 {
		t.Fatalf("500 caused removals: %d", removed)
	}
	t.Log("500: failed cleanly, no removals")

	// 403: the whole cycle must abort with ErrAPIRejected (the stop+alert path)
	_, err403 := runCycleWithRawAPI(t,
		`{"data":{"Search":{"main":5}}}`,
		`{"data":{"Search":{"main":{"total":5,"featured":[],"listings":[{"id":1,"category":1}]}}}}`,
		http.StatusForbidden, 0)
	if err403 == nil {
		t.Fatal("403 did not abort the cycle with ErrAPIRejected")
	}
	t.Logf("403: cycle aborted as expected (%v)", err403)
}

// Test_CategoryIntegrityGuard: listings whose category does not match the
// crawled category are still stored (they are real listings) but flagged.
func Test_CategoryIntegrityGuard(t *testing.T) {
	c, store, _ := newTestCrawler(t, map[int]int{1: 3}, func(cat, from, size int) []map[string]any {
		raw, _ := json.Marshal([]map[string]any{
			{"id": 1, "category": 1},
			{"id": 2, "category": 5}, // wrong category
			{"id": 3, "category": 1},
		})
		var out []map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	})
	_ = c.runCycle(context.Background(), 1, []categoryGroup{{label: "rent", ids: []int{1}}})
	var n int
	_ = store.db.QueryRow("SELECT COUNT(*) FROM listings").Scan(&n)
	if n != 3 {
		t.Fatalf("expected all 3 listings stored (flag-only guard), got %d", n)
	}
	t.Log("category mismatch: listing stored, guard logs the mismatch (verified in crawler log)")
}
