package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// ---------------------------------------------------------------------------
// styles

var (
	styleHeader  = lipgloss.NewStyle().Background(lipgloss.Color("33")).Foreground(lipgloss.Color("15")).Bold(true).Padding(0, 1)
	styleTitle   = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	styleRunning = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	styleIdle    = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
	styleError   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	stylePaused  = lipgloss.NewStyle().Foreground(lipgloss.Color("215")).Bold(true)
	styleMuted   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleLabel   = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	styleValue   = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	styleSection = lipgloss.NewStyle().Foreground(lipgloss.Color("215")).Bold(true)
	styleCatDone = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleCatErr  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleCatCur  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("238"))
	styleKeyHint = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleBarFg   = "42"
	styleBarBg   = "240"
)

const barWidth = 18

// ---------------------------------------------------------------------------
// model

type tickMsg time.Time

type tuiModel struct {
	stats *Stats // non-nil when the crawler runs in this process
	store *Store
	watch bool   // standalone watch mode (crawler is a separate process)
	once  bool   // quit the TUI when the single cycle finishes
	cfg   Config

	live        LiveStats // last snapshot (from stats or from DB)
	cycleStart  time.Time
	nextCycleAt time.Time
	stale       bool
	runs        []RunSummary
	dbSize      float64
	err         string
	done        bool

	scroll int
	width  int
	height int
}

func NewTUIModel(stats *Stats, store *Store, cfg Config, watch, once bool) *tuiModel {
	m := &tuiModel{stats: stats, store: store, watch: watch, once: once, cfg: cfg}
	if stats != nil {
		m.refresh()
	}
	return m
}

// Init starts the 1s refresh ticker.
func (m *tuiModel) Init() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "p":
			if m.stats != nil {
				m.stats.SetPaused(!m.stats.IsPaused())
			}
		case "j", "down":
			m.scroll++
		case "k", "up":
			if m.scroll > 0 {
				m.scroll--
			}
		case "g", "home":
			m.scroll = 0
		case "G", "end":
			m.scroll = 1 << 30
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		m.refresh()
		if m.done {
			return m, tea.Quit
		}
		return m, tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
	}
	return m, nil
}

// refresh pulls the latest snapshot and DB facts.
func (m *tuiModel) refresh() {
	ctx := context.Background()
	if m.stats != nil {
		m.live = m.stats.Snapshot()
		m.stale = false
	} else {
		ls, updated, ok, err := m.store.LoadLiveStats(ctx)
		if err != nil {
			m.err = "load live stats: " + err.Error()
		} else if ok {
			m.live = ls
			m.stale = time.Since(updated) > 30*time.Second
		} else {
			m.live = LiveStats{}
			m.stale = true
		}
	}
	if m.live.CycleStartedAt != "" {
		if t, err := time.Parse(time.RFC3339, m.live.CycleStartedAt); err == nil {
			m.cycleStart = t
		}
	} else {
		m.cycleStart = time.Time{}
	}
	if m.live.NextCycleAt != "" {
		if t, err := time.Parse(time.RFC3339, m.live.NextCycleAt); err == nil {
			m.nextCycleAt = t
		}
	} else {
		m.nextCycleAt = time.Time{}
	}

	if runs, err := m.store.LastRuns(ctx, 8); err == nil {
		m.runs = runs
	} else {
		m.err = "load runs: " + err.Error()
	}
	if m.live.TotalListings == 0 {
		if n, err := m.store.DBListingsCount(ctx); err == nil {
			m.live.TotalListings = n
		}
	}
	if m.live.TotalHistory == 0 {
		if h, err := m.store.DBHistoryCount(ctx); err == nil {
			m.live.TotalHistory = h
		}
	}
	m.dbSize = m.store.DBSizeMB()

	if m.once && m.live.Cycle > 0 && !m.live.Running {
		m.done = true
	}
}

func (m *tuiModel) View() string {
	if m.width == 0 {
		m.width = 100
	}
	if m.height == 0 {
		m.height = 30
	}
	w := m.width - 2
	var b strings.Builder

	// --- header ---
	status := styleIdle.Render("IDLE")
	if m.live.Running {
		status = styleRunning.Render("RUNNING")
	}
	if m.live.Waiting {
		status = styleIdle.Render("WAITING")
	}
	if m.live.Paused {
		status = stylePaused.Render("PAUSED")
	}
	head := "AQAR CRAWLER"
	if m.watch {
		head = "AQAR CRAWLER (watch)"
	}
	right := fmt.Sprintf("cycle %d | %s | %s", m.live.Cycle, status, fmtDB(m.dbSize))
	pad := max(0, w-lipgloss.Width(head)-lipgloss.Width(right))
	b.WriteString(styleHeader.Render(trunc(styleTitle.Render(head)+strings.Repeat(" ", pad)+styleTitle.Render(right), w)) + "\n")

	// --- main counters ---
	line := func(pairs ...[2]string) string {
		var sb strings.Builder
		for _, p := range pairs {
			sb.WriteString(styleLabel.Render(p[0] + " "))
			sb.WriteString(styleValue.Render(p[1]))
			sb.WriteString("  ")
		}
		return trunc(sb.String(), w)
	}
	b.WriteString(line(
		[2]string{"listings", comma(m.live.TotalListings)},
		[2]string{"new", comma(int64(m.live.Fresh))},
		[2]string{"updated", comma(int64(m.live.Updated))},
		[2]string{"removed", comma(int64(m.live.Removed))},
	) + "\n")
	b.WriteString(line(
		[2]string{"requests", comma(int64(m.live.Requests))},
		[2]string{"pages", comma(int64(m.live.Pages))},
		[2]string{"seen", comma(int64(m.live.Seen))},
		[2]string{"history", comma(m.live.TotalHistory)},
		[2]string{"errors", styleErr(m.live.Errors)},
	) + "\n")

	// --- cycle info ---
	var info strings.Builder
	if !m.cycleStart.IsZero() {
		info.WriteString("started " + m.cycleStart.Local().Format("15:04:05"))
		info.WriteString(" | uptime " + fmtDur(time.Since(m.cycleStart)))
	}
	if !m.nextCycleAt.IsZero() {
		info.WriteString(" | next cycle in " + fmtDur(time.Until(m.nextCycleAt)))
	}
	if info.Len() == 0 {
		info.WriteString("no cycle running yet")
	}
	if m.watch && m.stale {
		b.WriteString(styleError.Render("NO CRAWLER RUNNING - live stats are stale (>30s)") + "\n")
	} else {
		b.WriteString(trunc(info.String(), w) + "\n")
	}
	if m.live.LastError != "" {
		b.WriteString(styleError.Render("last error: "+trunc(m.live.LastError, w-12)) + "\n")
	}

	// --- categories (scrollable) ---
	b.WriteString(styleSection.Render("CATEGORY PROGRESS"))
	if len(m.live.Categories) > 0 {
		b.WriteString("  " + styleMuted.Render("(j/k scroll)"))
	}
	b.WriteString("\n")
	rows := m.categoryRows()
	// reserve: header(1) + counters(2) + cycle info(1..2) + history(<=7) + footer(1)
	visible := m.height - 12 - min(7, max(0, m.height-22))
	visible = max(3, visible)
	if len(rows) > visible {
		maxScroll := len(rows) - visible
		if m.scroll > maxScroll {
			m.scroll = maxScroll
		}
	}
	for i := m.scroll; i < len(rows) && i < m.scroll+visible; i++ {
		b.WriteString(rows[i] + "\n")
	}

	// --- history ---
	histH := min(7, max(0, m.height-22))
	if histH > 0 && len(m.runs) > 0 {
		b.WriteString(styleSection.Render("HISTORY") + "\n")
		for i := 0; i < len(m.runs) && i < histH; i++ {
			r := m.runs[i]
			st := r.Status
			switch st {
			case "completed":
				st = styleCatDone.Render(st)
			case "aborted":
				st = styleCatErr.Render(st)
			default:
				st = styleIdle.Render(st)
			}
			b.WriteString(trunc(fmt.Sprintf(" #%d %s | %s | %d req | %s seen | +%s new | %s upd | %s rem | %s err",
				r.Cycle, st,
				timeFmt(r.StartedAt),
				r.Requests, comma(int64(r.Seen)), comma(int64(r.Fresh)),
				comma(int64(r.Updated)), comma(int64(r.Removed)), comma(int64(r.Errors))), w) + "\n")
		}
	}

	// --- footer ---
	keys := "[p] pause   [j/k] scroll   [g/G] top/bottom   [q] quit"
	if m.watch {
		keys = "[j/k] scroll   [g/G] top/bottom   [q] quit"
	}
	b.WriteString(styleKeyHint.Render(keys))
	return b.String()
}

func (m *tuiModel) categoryRows() []string {
	rows := make([]string, 0, len(m.live.Categories))
	for i, c := range m.live.Categories {
		label := fmt.Sprintf("%s/%02d", c.Set, c.ID)
		name := c.Name
		if name == "" {
			name = fmt.Sprintf("cat %d", c.ID)
		}
		state := ""
		switch {
		case c.Errored:
			state = styleCatErr.Render("ERR")
		case c.Done:
			state = styleCatDone.Render("done")
		}
		pct := 0.0
		if c.Done {
			pct = 100
		} else if c.Total > 0 {
			pct = float64(c.From) / float64(c.Total) * 100
		}
		counts := fmt.Sprintf("%s / %s", comma(int64(c.From)), comma(int64(c.Total)))
		if c.Done && c.Total == 0 {
			counts = "—"
		}
		text := fmt.Sprintf(" %s %-26s %s %5.1f%%  %s  %s",
			trunc(label, 7), trunc(name, 26), progressBar(pct), pct, counts, state)
		if m.live.ActiveCat == i && !m.watch {
			rows = append(rows, trunc(styleCatCur.Render(text), m.width-2))
		} else {
			rows = append(rows, trunc(text, m.width-2))
		}
	}
	return rows
}

// ---------------------------------------------------------------------------
// helpers

func progressBar(pct float64) string {
	fill := int(pct / 100 * barWidth)
	if fill > barWidth {
		fill = barWidth
	}
	f := lipgloss.NewStyle().Foreground(lipgloss.Color(styleBarFg)).Render(strings.Repeat("█", fill))
	e := lipgloss.NewStyle().Foreground(lipgloss.Color(styleBarBg)).Render(strings.Repeat("░", barWidth-fill))
	return f + e
}

func styleErr(n int) string {
	s := fmt.Sprintf("%d", n)
	if n > 0 {
		return styleError.Render(s)
	}
	return s
}

func trunc(s string, n int) string {
	if n <= 0 || lipgloss.Width(s) <= n {
		return s
	}
	return runewidth.Truncate(s, n, "…")
}

func comma(v int64) string {
	s := fmt.Sprintf("%d", v)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	var out strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteByte(byte(c))
	}
	if neg {
		return "-" + out.String()
	}
	return out.String()
}

func fmtDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02dh %02dm %02ds", h, m, s)
}

func timeFmt(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("01-02 15:04")
}

func fmtDB(mb float64) string {
	if mb < 1024 {
		return fmt.Sprintf("%.1f MB", mb)
	}
	return fmt.Sprintf("%.2f GB", mb/1024)
}
