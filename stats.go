package main

import (
	"sync"
	"time"
)

// Category names as served by the Info query of the official app (v3.69).
// Used for display only.
var categoryNames = map[int]string{
	1: "Apartment for rent", 2: "Land for sale", 3: "Villa for sale",
	4: "Big flat for rent", 5: "Villa for rent", 6: "Apartment for sale",
	7: "Building for sale", 8: "Store for rent", 9: "Small house for sale",
	10: "Lounge for sale", 11: "Small house for rent", 12: "Farm for sale",
	13: "Lounge for rent", 14: "Office for rent", 15: "Land for rent",
	16: "Building for rent", 17: "Warehouse for rent", 18: "Tent for rent",
	19: "Room for rent", 20: "Store for sale", 22: "Floor for sale",
	23: "Chalet for rent", 24: "Other", 25: "Warehouse for sale",
	26: "Kiosk for sale", 27: "Kiosk for rent", 28: "Cinema for sale",
	29: "Cinema for rent", 30: "Parking for sale", 31: "Parking for rent",
	32: "Bank / ATM for sale", 33: "Bank / ATM for rent", 34: "Factory for sale",
	35: "Factory for rent", 36: "Hospital / health center for sale", 37: "Hospital / health center for rent",
	38: "Power station for sale", 39: "Power station for rent",
	40: "Communication tower for sale", 41: "Communication tower for rent", 42: "Room for sale",
	43: "Complex for sale", 44: "Complex for rent", 45: "Tower for sale",
	46: "Tower for rent", 47: "Office for sale", 48: "Hotel for sale",
	49: "Hotel for rent", 50: "Workshop for sale", 51: "Workshop for rent",
	52: "School for sale", 53: "School for rent", 54: "Station for sale",
	55: "Station for rent", 56: "Farm for rent", 57: "Studio for sale",
	58: "Studio for rent",
}

// CatStat is the live progress of one category in the current cycle.
type CatStat struct {
	Set     string `json:"set"`
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Total   int    `json:"total"`
	From    int    `json:"from"`
	Done    bool   `json:"done"`
	Errored bool   `json:"errored"`
}

// LiveStats is the compact snapshot persisted to the live_stats table so a
// separate watch process can render the same dashboard.
type LiveStats struct {
	Cycle          int       `json:"cycle"`
	CycleStartedAt string    `json:"cycle_started_at"`
	NextCycleAt    string    `json:"next_cycle_at"`
	Running        bool      `json:"running"`
	Waiting        bool      `json:"waiting"`
	Paused         bool      `json:"paused"`
	Requests       int       `json:"requests"`
	Pages          int       `json:"pages"`
	Seen           int       `json:"seen"`
	Fresh          int       `json:"fresh"`
	Updated        int       `json:"updated"`
	Removed        int       `json:"removed"`
	Errors         int       `json:"errors"`
	LastError      string    `json:"last_error"`
	TotalListings  int64     `json:"total_listings"`
	TotalHistory   int64     `json:"total_history"`
	ActiveCat      int       `json:"active_cat"`
	Categories     []CatStat `json:"categories"`
}

// Stats holds the live counters shared between the crawler and the TUI.
type Stats struct {
	mu sync.RWMutex

	cycle          int
	cycleStartedAt time.Time
	nextCycleAt    time.Time
	running        bool
	waiting        bool // sleeping until the next crawl window
	paused         bool

	requests, pages, seen, fresh, updated, removed, errors int
	lastError                                                string

	totalListings int64 // cumulative (never decreases; removals stay in DB)
	totalHistory  int64

	activeCat int
	cats      []CatStat
}

func NewStats() *Stats { return &Stats{} }

func (s *Stats) BeginCycle(cycle int, cats []CatStat) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cycle = cycle
	s.cycleStartedAt = time.Now()
	s.nextCycleAt = time.Time{}
	s.running = true
	s.requests, s.pages, s.seen = 0, 0, 0
	s.fresh, s.updated, s.removed, s.errors = 0, 0, 0, 0
	s.lastError = ""
	s.activeCat = 0
	s.cats = cats
}

func (s *Stats) SetCounters(requests, pages, seen, fresh, updated, removed, errors int, lastError string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests, s.pages, s.seen = requests, pages, seen
	s.fresh, s.updated, s.removed = fresh, updated, removed
	s.errors = errors
	s.lastError = lastError
}

func (s *Stats) SetActiveCat(i int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= 0 && i < len(s.cats) {
		s.activeCat = i
	}
}

func (s *Stats) SetCatProgress(i, from, total int, done, errored bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.cats) {
		return
	}
	s.cats[i].From = from
	s.cats[i].Total = total
	s.cats[i].Done = done
	s.cats[i].Errored = errored
}

func (s *Stats) FinishCycle(nextCycleAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.nextCycleAt = nextCycleAt
}

func (s *Stats) SetWaiting(w bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waiting = w
}

func (s *Stats) SetPaused(p bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = p
}

func (s *Stats) IsPaused() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.paused
}

func (s *Stats) AddTotal(listings, history int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalListings += listings
	s.totalHistory += history
}

func (s *Stats) SetTotal(listings, history int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalListings = listings
	s.totalHistory = history
}

// Snapshot returns a copy safe for concurrent readers.
func (s *Stats) Snapshot() LiveStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := LiveStats{
		Cycle: s.cycle, CycleStartedAt: fmtTime(s.cycleStartedAt),
		NextCycleAt: fmtTime(s.nextCycleAt), Running: s.running, Waiting: s.waiting, Paused: s.paused,
		Requests: s.requests, Pages: s.pages, Seen: s.seen, Fresh: s.fresh,
		Updated: s.updated, Removed: s.removed, Errors: s.errors,
		LastError: s.lastError, TotalListings: s.totalListings,
		TotalHistory: s.totalHistory, ActiveCat: s.activeCat,
		Categories: append([]CatStat(nil), s.cats...),
	}
	return out
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// buildCatStats builds the per-category progress list for a cycle.
func buildCatStats(groups []categoryGroup) []CatStat {
	var cats []CatStat
	for _, g := range groups {
		for _, id := range g.ids {
			cats = append(cats, CatStat{Set: g.label, ID: id, Name: categoryNames[id]})
		}
	}
	return cats
}
