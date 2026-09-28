package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/iso"
	"github.com/sirily11/iso-burner/internal/remote"
	"github.com/sirily11/iso-burner/internal/settings"
)

// genRows is how many ISOs the progress list shows at once.
const genRows = 5

const genTickInterval = 150 * time.Millisecond

type genTickMsg struct{}

type genDoneMsg struct{ err error }

func genTick() tea.Cmd {
	return tea.Tick(genTickInterval, func(time.Time) tea.Msg { return genTickMsg{} })
}

// startGenerate begins writing m.chunks in the background.
func (m Model) startGenerate() (Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	m.step = stepGenerate
	m.genProgress = iso.NewProgress(len(m.chunks))
	m.genStatuses = m.genProgress.Snapshot()
	m.genCancel = cancel
	m.generating = true
	m.genStarted = time.Now()
	m.genOffset = 0
	m.genSync = m.newReporter(remote.NewJobID())
	m.reportGenerate()
	folder, out, target, chunks, prog := m.result.Folder, m.outputDir, m.result.Preset.Bytes, m.chunks, m.genProgress
	run := func() tea.Msg {
		return genDoneMsg{err: iso.Generate(ctx, folder, out, target, chunks, prog)}
	}
	return m, tea.Batch(run, genTick())
}

func (m Model) updateGenerate(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case genTickMsg:
		if !m.generating {
			return m, nil
		}
		m.genStatuses = m.genProgress.Snapshot()
		m.genElapsed = time.Since(m.genStarted)
		m.reportGenerate()
		return m, genTick()
	case genDoneMsg:
		m.generating = false
		m.genErr = msg.err
		m.genStatuses = m.genProgress.Snapshot()
		m.genElapsed = time.Since(m.genStarted)
		m.reportGenerate()
		if m.genCancelling {
			m.cancelled = true
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			if !m.generating || m.genCancelling {
				// Finished, or a second ctrl+c while waiting on cancellation.
				m.cancelled = m.generating
				m.reportGenerate()
				return m, tea.Quit
			}
			m.genCancelling = true
			m.genCancel()
			m.reportGenerate()
			return m, nil
		case "enter", "q", "esc":
			if !m.generating {
				return m, tea.Quit
			}
		case "up", "k":
			m.genOffset--
		case "down", "j":
			m.genOffset++
		case "pgup":
			m.genOffset -= genRows
		case "pgdown":
			m.genOffset += genRows
		case "home", "g":
			m.genOffset = 0
		case "end", "G":
			m.genOffset = len(m.chunks)
		}
		m.genOffset = max(0, min(m.genOffset, len(m.chunks)-genRows))
	}
	return m, nil
}

// genOrder lists chunk indexes with in-progress ISOs first, then failed,
// queued and finished ones, each group in plan order.
func genOrder(statuses []iso.ChunkStatus) []int {
	rank := func(s iso.Stage) int {
		switch s {
		case iso.StageCopying, iso.StageFinalizing:
			return 0
		case iso.StageFailed:
			return 1
		case iso.StageQueued:
			return 2
		}
		return 3
	}
	order := make([]int, len(statuses))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return rank(statuses[order[a]].Stage) < rank(statuses[order[b]].Stage)
	})
	return order
}

func fraction(done, total int64) float64 {
	if total <= 0 {
		return 1
	}
	return min(1, float64(done)/float64(total))
}

func (m Model) barWidth() int {
	if m.width == 0 {
		return 30
	}
	return max(10, min(40, m.width-80))
}

func (m Model) generateView() string {
	var b strings.Builder
	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage(), progress.WithWidth(m.barWidth()))

	var copied, total int64
	counts := map[iso.Stage]int{}
	for i, s := range m.genStatuses {
		copied += min(s.Copied, m.chunks[i].Size)
		total += m.chunks[i].Size
		counts[s.Stage]++
	}
	switch {
	case m.genCancelling && m.generating:
		b.WriteString(errorStyle.Render("Cancelling… waiting for workers to stop") + "\n")
	case m.generating:
		b.WriteString(labelStyle.Render(fmt.Sprintf("Generating %d ISO file(s)", len(m.chunks))) +
			dimStyle.Render(" in "+m.outputDir) + "\n")
	case m.genErr != nil:
		b.WriteString(errorStyle.Render(fmt.Sprintf("✗ Generation finished with errors (%d failed)", counts[iso.StageFailed])) + "\n")
	default:
		b.WriteString(okStyle.Render(fmt.Sprintf("✓ Created %d ISO file(s) in %s", len(m.chunks), m.outputDir)) + "\n")
	}
	overall := fraction(copied, total)
	if !m.generating && m.genErr == nil {
		overall = 1
	}
	b.WriteString(fmt.Sprintf("%s %3.0f%%  %s / %s\n", bar.ViewAs(overall), overall*100,
		settings.FormatBytes(copied), settings.FormatBytes(total)))
	b.WriteString(dimStyle.Render(fmt.Sprintf("%d done · %d active · %d queued · %d failed · %s elapsed",
		counts[iso.StageDone], counts[iso.StageCopying]+counts[iso.StageFinalizing],
		counts[iso.StageQueued], counts[iso.StageFailed], m.genElapsed.Round(time.Second))) + "\n")
	if line := m.syncStatusLine(m.genSync); line != "" {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")

	nameWidth := 0
	for _, c := range m.chunks {
		nameWidth = max(nameWidth, len(c.Name))
	}
	nameWidth = min(nameWidth, 32)

	order := genOrder(m.genStatuses)
	end := min(m.genOffset+genRows, len(order))
	for _, i := range order[m.genOffset:end] {
		c, s := m.chunks[i], m.genStatuses[i]
		pct := fraction(s.Copied, c.Size)
		var state string
		switch s.Stage {
		case iso.StageQueued:
			state = dimStyle.Render("queued")
		case iso.StageCopying:
			state = fmt.Sprintf("%s / %s", settings.FormatBytes(min(s.Copied, c.Size)), settings.FormatBytes(c.Size))
		case iso.StageFinalizing:
			state = "finalizing…"
		case iso.StageDone:
			pct, state = 1, okStyle.Render("✓ done")
		case iso.StageFailed:
			state = errorStyle.Render("✗ " + truncate(s.Err.Error(), 40))
		}
		name := truncate(c.Name, nameWidth)
		b.WriteString(fmt.Sprintf("  %-*s %s %3.0f%%  %s\n", nameWidth, name, bar.ViewAs(pct), pct*100, state))
	}
	if len(order) > genRows {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Showing ISOs %d–%d of %d", m.genOffset+1, end, len(order))) + "\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:max(0, n-1)]) + "…"
}

func (m Model) generateHelp() string {
	switch {
	case m.generating && m.genCancelling:
		return "ctrl+c: force quit"
	case m.generating:
		return "↑/↓: scroll · ctrl+c: cancel"
	default:
		return "↑/↓: scroll · enter: exit"
	}
}
