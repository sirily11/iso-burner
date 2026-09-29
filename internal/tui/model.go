// Package tui implements the interactive wizard that collects ISO creation
// settings (source folder, file regex, target size preset and ISO name) and
// then shows generation progress.
package tui

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sirily11/iso-burner/internal/auth"
	"github.com/sirily11/iso-burner/internal/burn"
	"github.com/sirily11/iso-burner/internal/drive"
	"github.com/sirily11/iso-burner/internal/iso"
	"github.com/sirily11/iso-burner/internal/recent"
	"github.com/sirily11/iso-burner/internal/remote"
	"github.com/sirily11/iso-burner/internal/settings"
	"github.com/sirily11/iso-burner/internal/store"
)

type step int

const (
	stepFolder step = iota
	stepPattern
	stepSize
	stepName
	stepConfirm
	stepGenerate
)

var stepTitles = []string{"Folder", "File regex", "ISO size", "ISO name", "Preview", "Generate"}

// maxPreviewFiles caps how many files are listed on one preview page.
const maxPreviewFiles = 8

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	activeStep    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	inactiveStep  = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	labelStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	panelStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("63")).Padding(1, 2)
)

// scanDoneMsg carries the result of scanning the source folder.
type scanDoneMsg struct {
	folder string
	files  []settings.File
	err    error
}

// Options pre-fills the wizard, e.g. from command-line flags.
type Options struct {
	// Mode skips the mode selection screen when set.
	Mode Mode
	// ListDrives finds disc drives for burn mode; nil uses drive.List.
	ListDrives DriveLister
	// DiscRoot finds where the disc in a drive is mounted, for uploading its
	// files; nil uses drive.MountPoint.
	DiscRoot func(context.Context, drive.Drive) (string, error)
	// Store records burn sessions so they can be resumed; burning needs it.
	Store *store.Store
	// DBPath is shown so the user knows where progress is saved.
	DBPath string
	// Burner writes discs; nil uses burn.System().
	Burner burn.Burner
	// Auth signs in with RxAuth; nil disables the account screen.
	Auth auth.Service
	// Sync sends generation and burning progress to rxstorage while signed
	// in; nil disables syncing.
	Sync *remote.Client
	// SyncInterval throttles progress reports; zero uses remote.DefaultInterval.
	SyncInterval time.Duration
	// Recent pre-fills anything the other options leave empty with what the
	// last run selected.
	Recent recent.Recent
	// RecentPath is where selections are remembered; empty disables saving.
	RecentPath string
	Folder     string
	Pattern    string
	ISOName    string
	OutputDir  string
}

// Model is the Bubble Tea model for the settings wizard.
type Model struct {
	step step
	err  error

	mode       Mode
	modeIdx    int
	modeChosen bool // mode was picked on the selection screen, so esc returns there
	modeErr    error

	uploadAfterSignIn bool // upload was chosen while signed out; start it once signed in
	itemSearch        itemSearch
	uploadFiles       uploadFiles

	auth           auth.Service
	user           *auth.User // nil when signed out
	authChecking   bool
	authErr        error
	showAccount    bool // the account screen is showing over mode selection
	signingIn      bool
	signInURL      string
	signInCancel   context.CancelFunc
	confirmSignOut bool

	sync         *remote.Client
	syncInterval time.Duration
	hostName     string
	genSync      *remote.Reporter // nil unless generation progress is synced
	burnSync     *remote.Reporter // nil unless burning progress is synced
	syncDiscs    []store.Disc     // the burn session's discs, for per-ISO rows
	syncDiscsAt  time.Time

	recent     recent.Recent
	recentPath string

	isoHint   string // where the ISO picker starts browsing
	isoPicker isoPicker
	burnISOs  []string

	replicas        replicaEditor
	settingReplicas bool           // the copies screen is showing instead of the ISO picker
	replicaCounts   map[string]int // copies by ISO path, kept when going back to the picker
	burnJobs        []BurnJob

	listDrives DriveLister
	discRoot   func(context.Context, drive.Drive) (string, error)
	drives     DriveSelector
	burnDrives []drive.Drive

	store       *store.Store
	dbPath      string
	burner      burn.Burner
	resumeOffer *store.Session // unfinished session offered for resuming
	resumeDiscs []store.Disc
	resumeErr   error
	resuming    *store.Session // session chosen to resume
	burnErr     error          // why burning could not start

	engine      *burn.Engine
	burnSession int64
	burnSnap    burn.Snapshot
	burnDiscs   []store.Disc // loaded once burning ends
	burnStarted time.Time
	burnElapsed time.Duration
	burnStopped bool
	confirmStop bool
	dismissed   map[string]int64 // drive ID → disc whose insert dialog was put off

	folderInput  textinput.Model
	picker       folderPicker
	picking      bool
	patternInput textinput.Model
	nameInput    textinput.Model
	presetIdx    int

	scanning          bool
	allFiles          []settings.File
	matched           []settings.File
	chunks            []settings.Chunk
	previewISO        int
	previewFileOffset int

	result    *settings.Settings
	cancelled bool
	outputDir string
	width     int

	genProgress   *iso.Progress
	genStatuses   []iso.ChunkStatus
	genCancel     context.CancelFunc
	generating    bool
	genCancelling bool
	genErr        error
	genStarted    time.Time
	genElapsed    time.Duration
	genOffset     int
}

// New builds a wizard model with the given pre-filled values.
func New(opts Options) Model {
	last := opts.Recent
	sourceFolder := opts.Folder
	if sourceFolder == "" && settings.ValidateFolder(last.Folder) == nil {
		sourceFolder = last.Folder
	}
	folder := textinput.New()
	folder.Placeholder = "/path/to/source"
	folder.SetValue(sourceFolder)
	folder.Focus()

	pattern := textinput.New()
	pattern.Placeholder = `.*  (regex like \.(mkv|mp4)$ or glob like **/*.mp4)`
	pattern.SetValue(cmp.Or(opts.Pattern, last.Pattern))

	name := textinput.New()
	name.Placeholder = "backup"
	name.SetValue(cmp.Or(opts.ISOName, last.ISOName))

	m := Model{mode: opts.Mode, folderInput: folder, patternInput: pattern, nameInput: name, outputDir: opts.OutputDir,
		listDrives: opts.ListDrives, discRoot: opts.DiscRoot, store: opts.Store, dbPath: opts.DBPath, burner: opts.Burner,
		auth: opts.Auth, authChecking: opts.Auth != nil,
		sync: opts.Sync, syncInterval: opts.SyncInterval,
		recent: last, recentPath: opts.RecentPath}
	for i, p := range settings.Presets {
		if p.Name == last.Preset {
			m.presetIdx = i
		}
	}
	if m.syncInterval <= 0 {
		m.syncInterval = remote.DefaultInterval
	}
	m.hostName, _ = os.Hostname()
	if m.burner == nil {
		m.burner = burn.System()
	}
	if m.discRoot == nil {
		m.discRoot = drive.MountPoint
	}
	// Burn mode browses for ISOs, starting where they were most likely written.
	m.isoHint = cmp.Or(opts.Folder, last.ISODir, opts.OutputDir)
	if m.mode == ModeBurn {
		m.isoPicker = newISOPicker(m.isoHint)
		m.isoPicker.preselect(m.recent.ISOs)
		m.offerResume()
	}
	if opts.Folder == "" {
		// Nothing typed yet, so start by browsing for a folder, with the
		// last used one highlighted.
		m.picker, m.picking = newFolderPickerAt(sourceFolder), true
	}
	return m
}

// saveRecent remembers the current selections for the next run. Failing to
// save only loses the convenience, so errors are ignored.
func (m Model) saveRecent() {
	if m.recentPath != "" {
		_ = recent.Save(m.recentPath, m.recent)
	}
}

// Result returns the confirmed settings and the planned chunks, or nil if the
// user quit before confirming or cancelled generation.
func (m Model) Result() (*settings.Settings, []settings.Chunk) {
	if m.cancelled || m.result == nil {
		return nil, nil
	}
	return m.result, m.chunks
}

// Mode returns the mode the user chose, or ModeNone if they quit first.
func (m Model) Mode() Mode {
	if m.cancelled {
		return ModeNone
	}
	return m.mode
}

// GenerateErr reports why ISO generation failed, if it did.
func (m Model) GenerateErr() error { return m.genErr }

func (m Model) Init() tea.Cmd {
	if m.auth == nil {
		return textinput.Blink
	}
	return tea.Batch(textinput.Blink, checkAuthCmd(m.auth))
}

// User returns the signed-in RxAuth account, or nil.
func (m Model) User() *auth.User { return m.user }

func scanCmd(folder string) tea.Cmd {
	return func() tea.Msg {
		files, err := settings.ScanFolder(folder)
		return scanDoneMsg{folder: folder, files: files, err: err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		return m, nil
	}
	if next, ok := m.updateAuthMsg(msg); ok {
		if next.uploadAfterSignIn && next.user != nil {
			next.uploadAfterSignIn, next.showAccount = false, false
			return next.startUpload()
		}
		return next, nil
	}
	if m.mode == ModeNone {
		if key, ok := msg.(tea.KeyMsg); ok {
			if m.showAccount {
				return m.updateAccount(key)
			}
			return m.updateMode(key)
		}
		return m, nil
	}
	if m.mode == ModeBurn {
		return m.updateBurn(msg)
	}
	if m.mode == ModeUpload {
		return m.updateUpload(msg)
	}
	if m.step == stepGenerate {
		return m.updateGenerate(msg)
	}
	if key, ok := msg.(tea.KeyMsg); ok && m.step == stepFolder && m.picking && key.Type != tea.KeyCtrlC && !m.scanning {
		return m.updatePicker(key)
	}
	switch msg := msg.(type) {
	case scanDoneMsg:
		m.scanning = false
		if msg.err != nil {
			m.err = fmt.Errorf("scan failed: %w", msg.err)
			return m, nil
		}
		m.allFiles = msg.files
		m.refreshMatches()
		return m.goTo(stepPattern)

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.cancelled = true
			return m, tea.Quit
		case tea.KeyEsc:
			if m.step == stepFolder && m.modeChosen {
				m.mode, m.err = ModeNone, nil
				return m, nil
			}
			if m.step == stepFolder {
				m.cancelled = true
				return m, tea.Quit
			}
			return m.goTo(m.step - 1)
		case tea.KeyEnter:
			return m.submit()
		}
		if m.scanning {
			return m, nil
		}
		if m.step == stepFolder && (msg.Type == tea.KeyTab || msg.Type == tea.KeyCtrlO) {
			m.picker, m.picking = newFolderPicker(m.folderInput.Value()), true
			m.err = nil
			return m, nil
		}
		if m.step == stepSize {
			switch msg.String() {
			case "up", "k":
				m.presetIdx = (m.presetIdx + len(settings.Presets) - 1) % len(settings.Presets)
			case "down", "j", "tab":
				m.presetIdx = (m.presetIdx + 1) % len(settings.Presets)
			}
			return m, nil
		}
		if m.step == stepConfirm {
			switch msg.String() {
			case "left", "h":
				if m.previewISO > 0 {
					m.previewISO--
					m.previewFileOffset = 0
				}
			case "right", "l", "tab":
				if m.previewISO+1 < len(m.chunks) {
					m.previewISO++
					m.previewFileOffset = 0
				}
			case "up", "k":
				if m.previewFileOffset > 0 {
					m.previewFileOffset--
				}
			case "down", "j":
				if m.previewFileOffset+maxPreviewFiles < len(m.chunks[m.previewISO].Pieces) {
					m.previewFileOffset++
				}
			}
			return m, nil
		}
	}

	return m.updateInput(msg)
}

// updatePicker routes a key to the folder picker. Choosing a folder fills the
// folder input and scans it right away.
func (m Model) updatePicker(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	next, chosen, closed := m.picker.update(key)
	m.picker = next
	if !closed {
		return m, nil
	}
	m.picking = false
	if chosen == "" {
		return m, m.folderInput.Focus()
	}
	m.folderInput.SetValue(chosen)
	m.folderInput.CursorEnd()
	return m.submit()
}

// updateInput forwards msg to the text input of the current step.
func (m Model) updateInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.step {
	case stepFolder:
		m.folderInput, cmd = m.folderInput.Update(msg)
	case stepPattern:
		prev := m.patternInput.Value()
		m.patternInput, cmd = m.patternInput.Update(msg)
		if m.patternInput.Value() != prev {
			m.refreshMatches()
		}
	case stepName:
		m.nameInput, cmd = m.nameInput.Update(msg)
		m.err = nil
	}
	return m, cmd
}

// refreshMatches re-applies the current regex to the scanned files so the
// pattern step can show a live preview.
func (m *Model) refreshMatches() {
	re, err := settings.CompilePattern(m.patternInput.Value())
	if err != nil {
		m.err = err
		m.matched = nil
		return
	}
	m.err = nil
	m.matched = settings.Filter(m.allFiles, re)
}

// submit validates the current step and advances on success.
func (m Model) submit() (tea.Model, tea.Cmd) {
	if m.scanning {
		return m, nil
	}
	m.err = nil
	switch m.step {
	case stepFolder:
		folder := strings.TrimSpace(m.folderInput.Value())
		if err := settings.ValidateFolder(folder); err != nil {
			m.err = err
			return m, nil
		}
		m.scanning = true
		return m, scanCmd(folder)

	case stepPattern:
		if _, err := settings.CompilePattern(m.patternInput.Value()); err != nil {
			m.err = err
			return m, nil
		}
		if len(m.matched) == 0 {
			m.err = fmt.Errorf("regex matches no files")
			return m, nil
		}
		return m.goTo(stepSize)

	case stepSize:
		return m.goTo(stepName)

	case stepName:
		name := strings.TrimSpace(m.nameInput.Value())
		if err := settings.ValidateISOName(name); err != nil {
			m.err = err
			return m, nil
		}
		chunks, err := settings.PlanChunks(m.matched, settings.UsableCapacity(settings.Presets[m.presetIdx].Bytes), name)
		if err != nil {
			m.err = err
			return m, nil
		}
		m.chunks = chunks
		m.previewISO = 0
		m.previewFileOffset = 0
		return m.goTo(stepConfirm)

	case stepConfirm:
		re, _ := settings.CompilePattern(m.patternInput.Value())
		m.result = &settings.Settings{
			Folder:  strings.TrimSpace(m.folderInput.Value()),
			Pattern: re,
			Preset:  settings.Presets[m.presetIdx],
			ISOName: strings.TrimSpace(m.nameInput.Value()),
		}
		if abs, err := filepath.Abs(m.result.Folder); err == nil {
			m.recent.Folder = abs
		}
		m.recent.Pattern = m.patternInput.Value()
		m.recent.ISOName = m.result.ISOName
		m.recent.Preset = m.result.Preset.Name
		m.saveRecent()
		return m.startGenerate()
	}
	return m, nil
}

// goTo switches to step s and moves focus to its input.
func (m Model) goTo(s step) (tea.Model, tea.Cmd) {
	m.step = s
	if s != stepPattern {
		m.err = nil
	}
	m.folderInput.Blur()
	m.patternInput.Blur()
	m.nameInput.Blur()
	switch s {
	case stepFolder:
		return m, m.folderInput.Focus()
	case stepPattern:
		return m, m.patternInput.Focus()
	case stepName:
		return m, m.nameInput.Focus()
	}
	return m, nil
}

func (m Model) View() string {
	if m.cancelled || (m.step != stepGenerate && m.result != nil) {
		return ""
	}
	if m.mode == ModeBurn {
		return m.burnView()
	}
	if m.mode == ModeUpload {
		return m.uploadView()
	}

	var b strings.Builder
	if m.mode == ModeNone {
		if m.showAccount {
			return m.accountView()
		}
		b.WriteString(titleStyle.Render("ISO Burner") + "\n")
		b.WriteString(m.accountSummary() + "\n\n")
		b.WriteString(m.modeView())
		b.WriteString("\n" + dimStyle.Render("↑/↓: choose · 1-3 or enter: select · a: account · esc: quit"))
		return panelStyle.Render(b.String()) + "\n"
	}
	title := "ISO Burner · Settings"
	if m.step == stepGenerate {
		title = "ISO Burner · Generate"
	}
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\n")
	b.WriteString(m.breadcrumb())
	b.WriteString("\n\n")

	switch m.step {
	case stepFolder:
		if m.picking {
			b.WriteString(m.picker.view())
			break
		}
		b.WriteString(labelStyle.Render("Source folder"))
		b.WriteString("\n" + m.folderInput.View() + "\n")
		if m.scanning {
			b.WriteString("\n" + dimStyle.Render("Scanning folder…") + "\n")
		}
	case stepPattern:
		b.WriteString(m.patternView())
	case stepSize:
		b.WriteString(m.sizeView())
	case stepName:
		b.WriteString(m.nameView())
	case stepConfirm:
		b.WriteString(m.confirmView())
	case stepGenerate:
		b.WriteString(m.generateView())
	}

	if m.err != nil {
		b.WriteString("\n" + errorStyle.Render("✗ "+m.err.Error()) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render(m.help()))
	return panelStyle.Render(b.String()) + "\n"
}

func (m Model) breadcrumb() string {
	parts := make([]string, len(stepTitles))
	for i, t := range stepTitles {
		label := fmt.Sprintf("%d. %s", i+1, t)
		if step(i) == m.step {
			parts[i] = activeStep.Render(label)
		} else {
			parts[i] = inactiveStep.Render(label)
		}
	}
	return strings.Join(parts, inactiveStep.Render("  ›  "))
}

func (m Model) patternView() string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("File regex") + dimStyle.Render("  (regex or glob, matched against paths relative to the folder)"))
	b.WriteString("\n" + m.patternInput.View() + "\n\n")
	if m.err == nil {
		b.WriteString(okStyle.Render(fmt.Sprintf("%d of %d files match · %s",
			len(m.matched), len(m.allFiles), settings.FormatBytes(settings.TotalSize(m.matched)))))
		b.WriteString("\n")
		for i, f := range m.matched {
			if i == maxPreviewFiles {
				b.WriteString(dimStyle.Render(fmt.Sprintf("  … and %d more", len(m.matched)-maxPreviewFiles)) + "\n")
				break
			}
			b.WriteString(dimStyle.Render(fmt.Sprintf("  %s  (%s)", f.RelPath, settings.FormatBytes(f.Size))) + "\n")
		}
	}
	return b.String()
}

func (m Model) sizeView() string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("Target ISO size") + "\n")
	total := settings.TotalSize(m.matched)
	for i, p := range settings.Presets {
		usable := settings.UsableCapacity(p.Bytes)
		discs := (total + usable - 1) / usable
		size := fmt.Sprintf("%s = %s", settings.FormatBytesDecimal(p.Bytes), settings.FormatBytes(p.Bytes))
		line := fmt.Sprintf("%s  %s  · ≥%d disc(s)", p.Name, dimStyle.Render(size), discs)
		if i == m.presetIdx {
			b.WriteString(selectedStyle.Render("› "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

func (m Model) nameView() string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("ISO name") + "\n")
	b.WriteString(m.nameInput.View() + "\n\n")
	name := strings.TrimSpace(m.nameInput.Value())
	if name == "" {
		name = "{iso_name}"
	}
	b.WriteString(dimStyle.Render("Output files: " + settings.OutputName(name, 1) + ", " +
		settings.OutputName(name, 2) + ", …"))
	b.WriteString("\n")
	return b.String()
}

func (m Model) confirmView() string {
	var b strings.Builder
	row := func(k, v string) {
		b.WriteString(labelStyle.Render(fmt.Sprintf("%-10s", k)) + " " + v + "\n")
	}
	pattern := m.patternInput.Value()
	if pattern == "" {
		pattern = ".*"
	}
	preset := settings.Presets[m.presetIdx]
	row("Folder", strings.TrimSpace(m.folderInput.Value()))
	row("Regex", pattern)
	row("Files", fmt.Sprintf("%d (%s)", len(m.matched), settings.FormatBytes(settings.TotalSize(m.matched))))
	row("ISO size", fmt.Sprintf("%s · %s", preset.Name, settings.FormatBytes(preset.Bytes)))
	row("ISO name", strings.TrimSpace(m.nameInput.Value()))
	if m.outputDir != "" {
		row("Destination", m.outputDir)
	}

	c := m.chunks[m.previewISO]
	b.WriteString("\n" + labelStyle.Render(fmt.Sprintf("Planned ISO %d of %d", m.previewISO+1, len(m.chunks))) + "\n")
	row("Output", c.Name)
	row("Files", fmt.Sprintf("%d", len(c.Pieces)))
	row("Data size", settings.FormatBytes(c.Size))
	row("Est. size", settings.FormatBytes(c.Used+settings.ReservedPerISO))
	if parts := splitParts(c); parts > 0 {
		row("Split parts", fmt.Sprintf("%d", parts))
	}
	b.WriteString("\n" + labelStyle.Render("Contents") + "\n")
	start := m.previewFileOffset
	end := min(start+maxPreviewFiles, len(c.Pieces))
	for _, p := range c.Pieces[start:end] {
		b.WriteString(fmt.Sprintf("  %s  %s\n", p.Name(), dimStyle.Render(settings.FormatBytes(p.Length))))
	}
	if len(c.Pieces) > maxPreviewFiles {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Showing files %d–%d of %d", start+1, end, len(c.Pieces))) + "\n")
	}
	if split := splitFiles(m.chunks); len(split) > 0 {
		b.WriteString("\n" + labelStyle.Render(fmt.Sprintf("Split files (%d)", len(split))) +
			dimStyle.Render("  (rejoin numbered parts in order)") + "\n")
		for i, s := range split {
			if i == maxPreviewFiles {
				b.WriteString(dimStyle.Render(fmt.Sprintf("  … and %d more", len(split)-maxPreviewFiles)) + "\n")
				break
			}
			b.WriteString(dimStyle.Render(fmt.Sprintf("  %s  (%s) → %d parts", s.File.RelPath,
				settings.FormatBytes(s.File.Size), s.Parts)) + "\n")
		}
	}
	return b.String()
}

// splitParts counts the pieces in c that are parts of a split file.
func splitParts(c settings.Chunk) int {
	n := 0
	for _, p := range c.Pieces {
		if p.IsSplit() {
			n++
		}
	}
	return n
}

// splitFiles returns the first part of every split file, in plan order.
func splitFiles(chunks []settings.Chunk) []settings.Piece {
	var out []settings.Piece
	for _, c := range chunks {
		for _, p := range c.Pieces {
			if p.IsSplit() && p.Part == 1 {
				out = append(out, p)
			}
		}
	}
	return out
}

func (m Model) help() string {
	switch m.step {
	case stepFolder:
		if m.picking {
			return m.picker.help()
		}
		if m.modeChosen {
			return "enter: scan folder · tab: browse folders · esc: back"
		}
		return "enter: scan folder · tab: browse folders · esc: quit"
	case stepSize:
		return "↑/↓: choose · enter: next · esc: back"
	case stepGenerate:
		return m.generateHelp()
	case stepConfirm:
		return "←/→: ISO · ↑/↓: scroll files · enter: generate · esc: back · ctrl+c: quit"
	default:
		return "enter: next · esc: back · ctrl+c: quit"
	}
}
