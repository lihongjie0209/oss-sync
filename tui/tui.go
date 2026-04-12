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
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"

	"github.com/oss-sync/db"
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
	database *db.DB
	stats    db.FullStats
	err      error
	interval time.Duration
	width    int
	height   int
}

// New creates a Model that polls the given database.
func New(database *db.DB, interval time.Duration) Model {
	return Model{database: database, interval: interval, width: 80}
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
		stats, err := m.database.GetFullStats()
		m.stats = stats
		m.err = err
		// Auto-quit when the session finishes and all pending work is done.
		if m.stats.Session != nil &&
			m.stats.Session.Status != "running" &&
			m.stats.Pending == 0 {
			return m, tea.Quit
		}
		return m, m.tick()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
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

	// ── Session section ─────────────────────────────────────────────────
	b.WriteString(headerStyle.Render("Session"))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", inner))
	b.WriteString("\n")

	if m.stats.Session == nil {
		b.WriteString(mutedStyle.Render("  No session found."))
		b.WriteString("\n")
	} else {
		sess := m.stats.Session
		b.WriteString(row("ID", fmt.Sprintf("%d", sess.ID)))
		b.WriteString(row("Mode", sess.Mode))
		b.WriteString(row("Started", sess.StartedAt.Local().Format("2006-01-02 15:04:05")))

		elapsed := time.Since(sess.StartedAt)
		if sess.FinishedAt != nil {
			elapsed = sess.FinishedAt.Sub(sess.StartedAt)
			b.WriteString(row("Finished", sess.FinishedAt.Local().Format("2006-01-02 15:04:05")))
		}
		b.WriteString(row("Elapsed", formatDuration(elapsed)))

		statusLabel := sess.Status
		switch sess.Status {
		case "running":
			statusLabel = okStyle.Render("● running")
		case "completed":
			statusLabel = okStyle.Render("✓ completed")
		case "failed":
			statusLabel = errStyle.Render("✗ failed: " + sess.ErrorMsg)
		}
		b.WriteString(labelStyle.Render("Status") + "  " + statusLabel + "\n")
	}

	// ── Objects section ──────────────────────────────────────────────────
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("Objects"))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", inner))
	b.WriteString("\n")

	s := m.stats
	b.WriteString(row("Total", fmt.Sprintf("%d", s.Total)))
	b.WriteString(labelStyle.Render("Synced") + "  " + okStyle.Render(fmt.Sprintf("%d", s.Synced)) + "\n")
	b.WriteString(labelStyle.Render("Pending") + "  " + warnStyle.Render(fmt.Sprintf("%d", s.Pending)) + "\n")
	b.WriteString(labelStyle.Render("Failed") + "  " + errStyle.Render(fmt.Sprintf("%d", s.Failed)) + "\n")

	// ── Progress bar ─────────────────────────────────────────────────────
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

	// ── Error ────────────────────────────────────────────────────────────
	if m.err != nil {
		b.WriteString("\n")
		b.WriteString(errStyle.Render("  Error: "+m.err.Error()))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("  q quit"))

	return borderStyle.Render(b.String())
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

// ─── Public API ───────────────────────────────────────────────────────────

// IsTTY returns true when stdout is an interactive terminal.
func IsTTY() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
}

// RunTUI starts the bubbletea loop. It blocks until the user presses q or
// the monitored sync session finishes.
func RunTUI(database *db.DB, interval time.Duration) error {
	m := New(database, interval)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// PrintHeadless writes a one-shot stats snapshot to w.
func PrintHeadless(w io.Writer, database *db.DB) error {
	stats, err := database.GetFullStats()
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

	if stats.Total > 0 {
		pct := float64(stats.Synced) / float64(stats.Total) * 100
		fmt.Fprintf(w, "  progress   %.1f%%\n", pct)
	}
	return nil
}
