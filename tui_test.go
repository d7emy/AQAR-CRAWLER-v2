package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestTUIView renders the dashboard against a seeded DB and checks the
// expected counters/progress appear. Headless: no terminal required.
func TestTUIView(t *testing.T) {
	db := t.TempDir() + "/test.db"
	store, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	// seed: one completed run + live stats with two categories
	ctx := context.Background()
	if _, err := store.StartRun(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, 1, "completed", 61, 60, 29305, 24799, 1, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	ls := LiveStats{
		Cycle: 2, Running: true, Paused: false,
		CycleStartedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		Requests: 12, Pages: 11, Seen: 5500, Fresh: 420, Updated: 7,
		TotalListings: 24799, TotalHistory: 24800,
		Categories: []CatStat{
			{Set: "rent", ID: 1, Name: "Apartment for rent", Total: 29562, From: 24500, Done: false},
			{Set: "rent", ID: 5, Name: "Villa for rent", Total: 0, From: 0, Done: false},
		},
	}
	if err := store.SaveLiveStats(ctx, ls); err != nil {
		t.Fatal(err)
	}

	m := NewTUIModel(nil, store, Config{}, true, false)
	m.width, m.height = 110, 30
	m.refresh()
	m.refresh() // second pass so totals/err settle

	v := m.View()
	for _, want := range []string{
		"AQAR CRAWLER (watch)",
		"cycle 2",
		"RUNNING",
		"24,799",
		"24,800",
		"5,500",
		"Apartment for rent",
		"29,562",
		"24,500 / 29,562",
		"HISTORY",
		"#1 completed",
		"24,799 new",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q\n--- view ---\n%s", want, v)
		}
	}
	t.Logf("rendered %d bytes OK:\n%s", len(v), v)
}

// TestStatsSnapshot exercises the stats setters for races and sanity.
func TestStatsSnapshot(t *testing.T) {
	s := NewStats()
	s.BeginCycle(3, buildCatStats([]categoryGroup{
		{label: "rent", ids: []int{1, 5}},
		{label: "sale", ids: []int{3}},
	}))
	s.SetCounters(10, 9, 1000, 100, 5, 0, 0, "")
	s.SetCatProgress(0, 500, 1000, false, false)
	s.SetCatProgress(2, 100, 100, true, false)
	s.SetActiveCat(1)
	s.AddTotal(100, 105)
	s.SetPaused(true)
	snap := s.Snapshot()
	if snap.Cycle != 3 || snap.Requests != 10 || snap.Fresh != 100 || snap.TotalListings != 100 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if !snap.Paused || !snap.Running || len(snap.Categories) != 3 {
		t.Fatalf("unexpected snapshot state: %+v", snap)
	}
	if snap.Categories[2].Done != true {
		t.Fatalf("cat done flag lost: %+v", snap.Categories[2])
	}
	if snap.Categories[0].Name != "Apartment for rent" {
		t.Fatalf("category name lookup failed: %+v", snap.Categories[0])
	}
}
