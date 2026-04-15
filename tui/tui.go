// Package tui provides a real-time dashboard for oss-sync.
// It supports two modes:
//   - TUI mode  : a bubbletea live-refresh terminal UI (requires an interactive TTY).
//   - Headless  : a single plain-text snapshot printed to stdout.
package tui

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"

	"github.com/oss-sync/db"
	"github.com/oss-sync/syncer"
)

// ─── styles ───────────────────────────────────────────────────────────────

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12")).
			Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("6"))

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			Width(14)

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15"))

	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	mutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("8")).
			Padding(0, 1)
)

// ─── bubbletea model ──────────────────────────────────────────────────────

type tickMsg time.Time

// Model is the bubbletea TUI model.
type Model struct {
	database       *db.DB
	scopes         []string
	done           <-chan struct{}
	stats          db.FullStats
	recent         []db.RecentSyncRecord
	failed         []db.SyncRecord
	err            error
	interval       time.Duration
	width          int
	height         int
	baselineTime   time.Time
	baselineSynced int64
	baselineBytes  int64
	baselineFound  int64
	lastSample     time.Time
	lastLiveBytes  int64
	filesPerSecond float64
	bytesPerSecond float64
	discoverPerSec float64
	discoveryDone  bool
	activeTab      int
	userQuit       bool
}

// New creates a Model that polls the given database.
func New(database *db.DB, scopes []string, interval time.Duration, done <-chan struct{}) Model {
	return Model{database: database, scopes: scopes, interval: interval, done: done, width: 80}
}

func (m Model) tick() tea.Cmd {
	return tea.Tick(m.interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Init starts the first tick.
func (m Model) Init() tea.Cmd {
	return m.tick()
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		stats, err := m.database.GetFullStatsForScopes(m.scopes)
		m.stats = stats
		if err == nil {
			m.recent, err = m.database.RecentSyncedForScopes(m.scopes, m.recentLimit())
		}
		if err == nil {
			m.failed, err = m.database.FailedRecordsForScopes(m.scopes, m.failedLimit())
		}
		m.err = err
		if err == nil {
			now := time.Now()
			live := syncer.GetLiveStatsForScopes(m.scopes)
			if m.baselineTime.IsZero() {
				m.baselineTime = now
				m.baselineSynced = m.stats.Synced
				m.baselineBytes = m.stats.SyncedBytes
				m.baselineFound = live.FilesDiscovered
				if live.BytesTransferred > 0 {
					m.baselineBytes = 0
				}
			}
			if session := m.stats.Session; session != nil {
				elapsed := now.Sub(m.baselineTime)
				if elapsed > 0 {
					runSynced := m.stats.Synced - m.baselineSynced
					m.filesPerSecond = math.Max(0, float64(runSynced)/elapsed.Seconds())
					runFound := live.FilesDiscovered - m.baselineFound
					m.discoverPerSec = math.Max(0, float64(runFound)/elapsed.Seconds())
				}
			}
			if !m.lastSample.IsZero() {
				elapsed := now.Sub(m.lastSample).Seconds()
				if elapsed > 0 {
					bytesBase := live.BytesTransferred
					if bytesBase == 0 {
						bytesBase = m.stats.SyncedBytes - m.baselineBytes
					}
					bytesDelta := float64(bytesBase-m.lastLiveBytes) / elapsed
					m.bytesPerSecond = math.Max(0, bytesDelta)
				}
			}
			m.lastSample = now
			m.lastLiveBytes = live.BytesTransferred
			if live.DiscoveryDone {
				m.discoveryDone = true
			}
			if live.BytesTransferred == 0 {
				m.lastLiveBytes = m.stats.SyncedBytes - m.baselineBytes
			}
		}
		if m.done != nil {
			select {
			case <-m.done:
				if m.stats.Pending == 0 {
					return m, tea.Quit
				}
			default:
			}
		} else if m.stats.Session != nil &&
			m.stats.Session.Status != "running" &&
			m.stats.Pending == 0 {
			// Auto-quit in stats mode when the latest observed session has finished.
			return m, tea.Quit
		}
		return m, m.tick()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.userQuit = true
			return m, tea.Quit
		case "tab", "right", "l":
			m.activeTab = 1 - m.activeTab
		case "shift+tab", "left", "h":
			m.activeTab = 1 - m.activeTab
		case "1":
			m.activeTab = 0
		case "2":
			m.activeTab = 1
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}
	return m, nil
}

// View renders the dashboard.
func (m Model) View() string {
	var b strings.Builder

	inner := m.width - 6 // account for border + padding
	if inner < 40 {
		inner = 40
	}

	b.WriteString(titleStyle.Render("  oss-sync  "))
	b.WriteString("\n\n")

	b.WriteString(renderSectionBlock("Session", inner, m.renderSessionBlock(inner)))
	b.WriteString("\n\n")
	b.WriteString(renderSectionBlock("Transfer metrics", inner, m.renderMetricsBlock(inner)))
	b.WriteString("\n")

	// ── Progress bar ─────────────────────────────────────────────────────
	s := m.stats
	if s.Total > 0 {
		b.WriteString("\n")
		b.WriteString(headerStyle.Render("Progress"))
		b.WriteString("\n")
		b.WriteString(strings.Repeat("─", inner))
		b.WriteString("\n")
		pct := float64(s.Synced) / float64(s.Total) * 100
		b.WriteString("  ")
		b.WriteString(progressBar(inner-10, int(pct)))
		b.WriteString(fmt.Sprintf("  %.1f%%\n", pct))
	}

	// ── Detail tabs ──────────────────────────────────────────────────────
	b.WriteString("\n")
	b.WriteString(m.renderTabBar(inner))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", inner))
	b.WriteString("\n")
	if m.activeTab == 0 {
		if len(m.recent) == 0 {
			b.WriteString(mutedStyle.Render("  No synced files yet."))
			b.WriteString("\n")
		} else {
			for _, record := range m.recent {
				b.WriteString(renderRecentRecord(record, inner))
				b.WriteString("\n")
			}
		}
	} else {
		if len(m.failed) == 0 {
			b.WriteString(mutedStyle.Render("  No failed files."))
			b.WriteString("\n")
		} else {
			for _, record := range m.failed {
				b.WriteString(renderFailedRecord(record, inner))
				b.WriteString("\n")
			}
		}
	}

	// ── Error ────────────────────────────────────────────────────────────
	if m.err != nil {
		b.WriteString("\n")
		b.WriteString(errStyle.Render("  Error: " + m.err.Error()))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("  tab switch  1 recent  2 failed  q quit"))

	return borderStyle.Render(b.String())
}

func (m Model) renderSessionBlock(width int) string {
	if m.stats.Session == nil {
		return mutedStyle.Render("  No session found.")
	}

	sess := m.stats.Session
	elapsed := time.Since(sess.StartedAt)
	if sess.FinishedAt != nil {
		elapsed = sess.FinishedAt.Sub(sess.StartedAt)
	}

	statusLabel := sess.Status
	switch sess.Status {
	case "running":
		statusLabel = okStyle.Render("● running")
	case "completed":
		statusLabel = okStyle.Render("✓ completed")
	case "failed":
		statusLabel = errStyle.Render("✗ failed: " + sess.ErrorMsg)
	}

	items := []metricItem{
		{Label: "ID", Value: fmt.Sprintf("%d", sess.ID), StyledValue: valueStyle.Render(fmt.Sprintf("%d", sess.ID))},
		{Label: "Mode", Value: sess.Mode, StyledValue: valueStyle.Render(sess.Mode)},
		{Label: "Started", Value: sess.StartedAt.Local().Format("2006-01-02 15:04:05"), StyledValue: valueStyle.Render(sess.StartedAt.Local().Format("2006-01-02 15:04:05"))},
		{Label: "Elapsed", Value: formatDuration(elapsed), StyledValue: valueStyle.Render(formatDuration(elapsed))},
		{Label: "Status", Value: sess.Status, StyledValue: statusLabel},
	}
	if sess.FinishedAt != nil {
		finished := sess.FinishedAt.Local().Format("2006-01-02 15:04:05")
		items = append(items, metricItem{Label: "Finished", Value: finished, StyledValue: valueStyle.Render(finished)})
	}
	return renderMetricGrid(width, items)
}

func (m Model) renderMetricsBlock(width int) string {
	s := m.stats
	items := []metricItem{
		{Label: "Total files", Value: formatCount(s.Total), StyledValue: valueStyle.Render(formatCount(s.Total))},
		{Label: "Synced files", Value: formatCount(s.Synced), StyledValue: okStyle.Render(formatCount(s.Synced))},
		{Label: "Pending", Value: formatCount(s.Pending), StyledValue: warnStyle.Render(formatCount(s.Pending))},
		{Label: "Failed", Value: formatCount(s.Failed), StyledValue: errStyle.Render(formatCount(s.Failed))},
		{Label: "Files/sec", Value: fmt.Sprintf("%.1f", m.filesPerSecond), StyledValue: valueStyle.Render(fmt.Sprintf("%.1f", m.filesPerSecond))},
		{Label: "Discover/sec", Value: fmt.Sprintf("%.1f", m.discoverPerSec), StyledValue: valueStyle.Render(fmt.Sprintf("%.1f", m.discoverPerSec))},
		{Label: "Discovery", Value: "status", StyledValue: m.discoveryText()},
		{Label: "Bytes/sec", Value: formatBytes(int64(m.bytesPerSecond)), StyledValue: valueStyle.Render(formatBytes(int64(m.bytesPerSecond)))},
		{Label: "ETA", Value: m.etaText(), StyledValue: valueStyle.Render(m.etaText())},
		{Label: "Synced bytes", Value: formatBytes(s.SyncedBytes), StyledValue: valueStyle.Render(formatBytes(s.SyncedBytes))},
		{Label: "Avg size", Value: formatBytes(s.AvgSyncedSize), StyledValue: valueStyle.Render(formatBytes(s.AvgSyncedSize))},
		{Label: "Max size", Value: formatBytes(s.MaxSyncedSize), StyledValue: valueStyle.Render(formatBytes(s.MaxSyncedSize))},
	}
	return renderMetricGrid(width, items)
}

func (m Model) discoveryText() string {
	if m.discoveryDone {
		return okStyle.Render("✓ done")
	}
	return warnStyle.Render("… running")
}

func (m Model) etaText() string {
	if m.stats.Pending <= 0 {
		return "00:00"
	}
	if m.filesPerSecond <= 0 {
		return "estimating..."
	}
	remainingSeconds := time.Duration(float64(time.Second) * (float64(m.stats.Pending) / m.filesPerSecond))
	if remainingSeconds <= 0 {
		return "estimating..."
	}
	return formatDuration(remainingSeconds)
}

func renderSectionBlock(title string, width int, body string) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render(title))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", width))
	b.WriteString("\n")
	b.WriteString(body)
	return lipgloss.NewStyle().Width(width).Render(b.String())
}

func (m Model) renderTabBar(inner int) string {
	tabs := []string{
		renderTab("Recent synced", m.activeTab == 0, len(m.recent)),
		renderTab("Failed files", m.activeTab == 1, int(m.stats.Failed)),
	}
	return lipgloss.NewStyle().Width(inner).Render(strings.Join(tabs, "  "))
}

func renderTab(label string, active bool, count int) string {
	text := fmt.Sprintf("%s (%d)", label, count)
	style := mutedStyle
	if active {
		style = headerStyle
	}
	return style.Render(text)
}

type metricItem struct {
	Label       string
	Value       string
	StyledValue string
}

func renderMetricGrid(width int, items []metricItem) string {
	if len(items) == 0 {
		return ""
	}

	columns := width / 28
	if columns < 1 {
		columns = 1
	}
	colWidth := width / columns
	if colWidth < 20 {
		colWidth = 20
	}

	rows := make([]string, 0, (len(items)+columns-1)/columns)
	for i := 0; i < len(items); i += columns {
		end := i + columns
		if end > len(items) {
			end = len(items)
		}
		parts := make([]string, 0, columns)
		for _, item := range items[i:end] {
			parts = append(parts, renderMetricCell(item, colWidth))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, parts...))
	}
	return strings.Join(rows, "\n")
}

func renderMetricCell(item metricItem, width int) string {
	raw := item.Label + " " + item.Value
	if len([]rune(raw)) > width-2 {
		raw = truncateMiddle(raw, width-2)
	}

	styled := labelStyle.UnsetWidth().Render(item.Label) + " " + item.StyledValue
	return lipgloss.NewStyle().Width(width).Render(styled)
}

func (m Model) recentLimit() int {
	if m.height <= 0 {
		return 10
	}
	limit := m.height - 24
	if limit < 5 {
		return 5
	}
	if limit > 20 {
		return 20
	}
	return limit
}

func (m Model) failedLimit() int {
	return m.recentLimit()
}

func row(label, value string) string {
	return labelStyle.Render(label) + "  " + valueStyle.Render(value) + "\n"
}

func progressBar(width, pct int) string {
	if width < 4 {
		width = 4
	}
	filled := int(math.Round(float64(width) * float64(pct) / 100))
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return okStyle.Render("[") + bar + okStyle.Render("]")
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func formatCount(v int64) string {
	return strconv.FormatInt(v, 10)
}

func formatBytes(v int64) string {
	if v <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(v)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", v, units[unit])
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

func renderRecentRecord(record db.RecentSyncRecord, inner int) string {
	timePart := record.SyncedAt.Local().Format("15:04:05")
	sizePart := formatBytes(record.Size)
	pathPart := record.Key
	if record.SourceKey != "" && record.SourceKey != record.Key {
		pathPart = record.SourceKey + " -> " + record.Key
	}

	prefix := mutedStyle.Render("  "+timePart) + "  " + valueStyle.Render(padRight(sizePart, 10)) + "  "
	maxPathWidth := inner - 20
	if maxPathWidth < 20 {
		maxPathWidth = 20
	}
	return prefix + truncateMiddle(pathPart, maxPathWidth)
}

func renderFailedRecord(record db.SyncRecord, inner int) string {
	sizePart := formatBytes(record.Size)
	pathPart := record.Key
	if record.SourceKey != "" && record.SourceKey != record.Key {
		pathPart = record.SourceKey + " -> " + record.Key
	}
	prefix := errStyle.Render("  FAIL") + "  " + valueStyle.Render(padRight(sizePart, 10)) + "  "
	maxPathWidth := inner - 20
	if maxPathWidth < 20 {
		maxPathWidth = 20
	}
	line := prefix + truncateMiddle(pathPart, maxPathWidth)
	reason := mutedStyle.Render("       " + truncateMiddle(record.ErrorMsg, inner-7))
	return line + "\n" + reason
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func truncateMiddle(s string, width int) string {
	runes := []rune(s)
	if width <= 3 || len(runes) <= width {
		return s
	}
	left := (width - 3) / 2
	right := width - 3 - left
	return string(runes[:left]) + "..." + string(runes[len(runes)-right:])
}

// ─── Public API ───────────────────────────────────────────────────────────

// IsTTY returns true when stdout is an interactive terminal.
func IsTTY() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
}

// RunTUI starts the bubbletea loop. It blocks until the user presses q or
// the monitored sync session finishes.
func RunTUI(database *db.DB, scopes []string, interval time.Duration) (bool, error) {
	m := New(database, scopes, interval, nil)
	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return false, err
	}
	return finalModel.(Model).userQuit, nil
}

func RunSyncTUI(database *db.DB, scopes []string, interval time.Duration, done <-chan struct{}) (bool, error) {
	m := New(database, scopes, interval, done)
	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return false, err
	}
	return finalModel.(Model).userQuit, nil
}

// PrintHeadless writes a one-shot stats snapshot to w.
func PrintHeadless(w io.Writer, database *db.DB, scopes []string) error {
	stats, err := database.GetFullStatsForScopes(scopes)
	if err != nil {
		return err
	}

	fmt.Fprintln(w, "── oss-sync stats ──────────────────────────────")
	if s := stats.Session; s != nil {
		fmt.Fprintf(w, "  Session ID : %d\n", s.ID)
		fmt.Fprintf(w, "  Mode       : %s\n", s.Mode)
		fmt.Fprintf(w, "  Started    : %s\n", s.StartedAt.Local().Format("2006-01-02 15:04:05"))
		if s.FinishedAt != nil {
			fmt.Fprintf(w, "  Finished   : %s\n", s.FinishedAt.Local().Format("2006-01-02 15:04:05"))
			fmt.Fprintf(w, "  Elapsed    : %s\n", formatDuration(s.FinishedAt.Sub(s.StartedAt)))
		} else {
			fmt.Fprintf(w, "  Elapsed    : %s\n", formatDuration(time.Since(s.StartedAt)))
		}
		fmt.Fprintf(w, "  Status     : %s\n", s.Status)
		if s.ErrorMsg != "" {
			fmt.Fprintf(w, "  Error      : %s\n", s.ErrorMsg)
		}
	} else {
		fmt.Fprintln(w, "  (no sessions recorded)")
	}

	fmt.Fprintln(w, "────────────────────────────────────────────────")
	fmt.Fprintf(w, "  %-10s %d\n", "total", stats.Total)
	fmt.Fprintf(w, "  %-10s %d\n", "synced", stats.Synced)
	fmt.Fprintf(w, "  %-10s %d\n", "pending", stats.Pending)
	fmt.Fprintf(w, "  %-10s %d\n", "failed", stats.Failed)
	fmt.Fprintf(w, "  %-10s %s\n", "bytes", formatBytes(stats.SyncedBytes))
	fmt.Fprintf(w, "  %-10s %s\n", "avg-size", formatBytes(stats.AvgSyncedSize))
	fmt.Fprintf(w, "  %-10s %s\n", "max-size", formatBytes(stats.MaxSyncedSize))

	if stats.Total > 0 {
		pct := float64(stats.Synced) / float64(stats.Total) * 100
		fmt.Fprintf(w, "  progress   %.1f%%\n", pct)
	}
	return nil
}
