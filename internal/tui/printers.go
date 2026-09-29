package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/printer"
	"github.com/sirily11/iso-burner/internal/settings"
)

// queueRefresh is how often the job queues of shared printers are re-read.
const queueRefresh = 2 * time.Second

// printerScreen is which screen of printer mode is showing.
type printerScreen int

const (
	screenPrinterPick   printerScreen = iota // tick the printers to share
	screenPrinterQueues                      // job queue summary of every shared printer
	screenPrinterDetail                      // every job of one shared printer
)

type printersLoadedMsg struct {
	printers []printer.Printer
	err      error
}

// printersSharedMsg reports that the sharing changes were applied.
type printersSharedMsg struct {
	shared   []printer.Printer
	unshared int
	err      error
}

// queuesLoadedMsg carries the job queues read by one refresh. gen ties it to
// the refresh loop that asked, so loops from earlier visits stop.
type queuesLoadedMsg struct {
	gen    int
	queues map[string]printer.Queue
	errs   map[string]error
}

type queueTickMsg struct{ gen int }

// advertiseEndedMsg reports that the AirPrint advert of a printer stopped.
type advertiseEndedMsg struct {
	gen  int
	name string
	err  error
}

// advertising is the set of running AirPrint adverts. It is shared by pointer
// so every copy of the model can stop the same adverts.
type advertising struct {
	gen    int
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// printerShare shares printers over AirPrint and then watches their queues.
type printerShare struct {
	svc    printer.Service
	screen printerScreen

	loading  bool
	applying bool
	printers []printer.Printer
	selected map[string]bool // by queue name
	cursor   int
	err      error
	notice   string

	shared    []printer.Printer
	queues    map[string]printer.Queue
	queueErrs map[string]error
	loaded    bool // the first refresh finished
	gen       int
	queueIdx  int
	detailOff int

	adv     *advertising // nil when nothing is advertised
	advGen  int
	advErrs map[string]error // by queue name

	back bool // the user left printer mode
}

// newPrinterShare builds printer mode. gen continues the refresh generation of
// an earlier visit so its refresh loop cannot be mistaken for the new one.
func newPrinterShare(svc printer.Service, gen int) printerShare {
	return printerShare{svc: svc, loading: true, selected: map[string]bool{}, gen: gen}
}

func (p printerShare) list() tea.Cmd {
	svc := p.svc
	return func() tea.Msg {
		printers, err := svc.List(context.Background())
		return printersLoadedMsg{printers: printers, err: err}
	}
}

// apply shares the ticked printers and stops sharing the unticked ones that
// were shared before.
func (p printerShare) apply() tea.Cmd {
	svc := p.svc
	var share, unshare []string
	var shared []printer.Printer
	for _, pr := range p.printers {
		switch {
		case p.selected[pr.Name]:
			if !pr.Shared {
				share = append(share, pr.Name)
			}
			pr.Shared = true
			shared = append(shared, pr)
		case pr.Shared:
			unshare = append(unshare, pr.Name)
		}
	}
	return func() tea.Msg {
		ctx := context.Background()
		if err := svc.Unshare(ctx, unshare); err != nil {
			return printersSharedMsg{err: err}
		}
		if err := svc.Share(ctx, share); err != nil {
			return printersSharedMsg{err: err}
		}
		return printersSharedMsg{shared: shared, unshared: len(unshare)}
	}
}

func (p printerShare) loadQueues() tea.Cmd {
	svc, gen := p.svc, p.gen
	names := make([]string, len(p.shared))
	for i, pr := range p.shared {
		names[i] = pr.Name
	}
	return func() tea.Msg {
		msg := queuesLoadedMsg{gen: gen, queues: map[string]printer.Queue{}, errs: map[string]error{}}
		for _, name := range names {
			q, err := svc.Queue(context.Background(), name)
			if err != nil {
				msg.errs[name] = err
				continue
			}
			msg.queues[name] = q
		}
		return msg
	}
}

func queueTick(gen int) tea.Cmd {
	return tea.Tick(queueRefresh, func(time.Time) tea.Msg { return queueTickMsg{gen: gen} })
}

// advertiseTimeout bounds how long stopping the adverts waits for the Bonjour
// tools to exit.
const advertiseTimeout = 3 * time.Second

// startAdvertising publishes shared as AirPrint printers, replacing any adverts
// that are running. macOS shares the queues without the Bonjour record iOS
// looks for, so iso-burner publishes it while it runs.
func (p printerShare) startAdvertising(shared []printer.Printer) (printerShare, tea.Cmd) {
	p.stopAdvertising()
	p.advGen++
	p.advErrs = map[string]error{}
	ctx, cancel := context.WithCancel(context.Background())
	adv := &advertising{gen: p.advGen, cancel: cancel}
	p.adv = adv
	svc := p.svc
	cmds := make([]tea.Cmd, len(shared))
	for i, pr := range shared {
		adv.wg.Add(1)
		cmds[i] = func() tea.Msg {
			defer adv.wg.Done()
			err := svc.Advertise(ctx, pr)
			return advertiseEndedMsg{gen: adv.gen, name: pr.Name, err: err}
		}
	}
	return p, tea.Batch(cmds...)
}

// stopAdvertising withdraws the AirPrint adverts and waits for them to end.
func (p *printerShare) stopAdvertising() {
	if p.adv == nil {
		return
	}
	adv := p.adv
	p.adv = nil
	adv.cancel()
	done := make(chan struct{})
	go func() { adv.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(advertiseTimeout):
	}
}

// openQueues switches to the queue summary and starts refreshing it.
func (p printerShare) openQueues(shared []printer.Printer) (printerShare, tea.Cmd) {
	p.gen++
	p.screen = screenPrinterQueues
	p.shared = shared
	p.queues, p.queueErrs, p.loaded = nil, nil, false
	p.queueIdx = 0
	return p, p.loadQueues()
}

func (p printerShare) update(msg tea.Msg) (printerShare, tea.Cmd) {
	switch msg := msg.(type) {
	case printersLoadedMsg:
		p.loading = false
		p.printers, p.err = msg.printers, msg.err
		clear(p.selected)
		for _, pr := range p.printers {
			if pr.Shared {
				p.selected[pr.Name] = true
			}
		}
		p.cursor = min(p.cursor, max(len(p.printers)-1, 0))
		return p, nil

	case printersSharedMsg:
		p.applying = false
		if msg.err != nil {
			p.err = msg.err
			return p, nil
		}
		if len(msg.shared) == 0 {
			p.notice = fmt.Sprintf("Stopped sharing %d printer(s)", msg.unshared)
			p.shared = nil
			p.stopAdvertising()
			p.loading = true
			return p, p.list()
		}
		// Reflect the change so going back shows the printers as shared.
		for i, pr := range p.printers {
			p.printers[i].Shared = p.selected[pr.Name]
		}
		p, advertise := p.startAdvertising(msg.shared)
		p, load := p.openQueues(msg.shared)
		return p, tea.Batch(advertise, load)

	case advertiseEndedMsg:
		if p.adv != nil && msg.gen == p.adv.gen && msg.err != nil {
			p.advErrs[msg.name] = msg.err
		}
		return p, nil

	case queuesLoadedMsg:
		if msg.gen != p.gen || p.screen == screenPrinterPick {
			return p, nil
		}
		p.queues, p.queueErrs, p.loaded = msg.queues, msg.errs, true
		return p, queueTick(p.gen)

	case queueTickMsg:
		if msg.gen != p.gen || p.screen == screenPrinterPick {
			return p, nil
		}
		return p, p.loadQueues()

	case tea.KeyMsg:
		switch p.screen {
		case screenPrinterPick:
			return p.updatePick(msg)
		case screenPrinterQueues:
			return p.updateQueues(msg)
		case screenPrinterDetail:
			return p.updateDetail(msg)
		}
	}
	return p, nil
}

func (p printerShare) updatePick(key tea.KeyMsg) (printerShare, tea.Cmd) {
	if p.applying {
		return p, nil
	}
	switch key.String() {
	case "esc", "q":
		p.back = true
		return p, nil
	case "r":
		if !p.loading {
			p.loading, p.err, p.notice = true, nil, ""
			return p, p.list()
		}
		return p, nil
	}
	if p.loading || len(p.printers) == 0 {
		return p, nil
	}
	switch key.String() {
	case "up", "k", "shift+tab":
		p.cursor = (p.cursor + len(p.printers) - 1) % len(p.printers)
	case "down", "j", "tab":
		p.cursor = (p.cursor + 1) % len(p.printers)
	case " ", "x":
		name := p.printers[p.cursor].Name
		if p.selected[name] {
			delete(p.selected, name)
		} else {
			p.selected[name] = true
		}
		p.err, p.notice = nil, ""
	case "a":
		all := len(p.selected) == len(p.printers)
		clear(p.selected)
		if !all {
			for _, pr := range p.printers {
				p.selected[pr.Name] = true
			}
		}
		p.err, p.notice = nil, ""
	case "enter":
		wasShared := false
		for _, pr := range p.printers {
			wasShared = wasShared || pr.Shared
		}
		if len(p.selected) == 0 && !wasShared {
			p.err = fmt.Errorf("select at least one printer")
			return p, nil
		}
		p.applying, p.err, p.notice = true, nil, ""
		return p, p.apply()
	}
	return p, nil
}

func (p printerShare) updateQueues(key tea.KeyMsg) (printerShare, tea.Cmd) {
	switch key.String() {
	case "esc", "q":
		// Leaving stops the refresh loop; the printers stay shared.
		p.screen = screenPrinterPick
		return p, nil
	case "up", "k", "shift+tab":
		p.queueIdx = (p.queueIdx + len(p.shared) - 1) % len(p.shared)
	case "down", "j", "tab":
		p.queueIdx = (p.queueIdx + 1) % len(p.shared)
	case "enter", "right", "l":
		p.screen, p.detailOff = screenPrinterDetail, 0
	}
	return p, nil
}

func (p printerShare) updateDetail(key tea.KeyMsg) (printerShare, tea.Cmd) {
	jobs := len(p.queues[p.shared[p.queueIdx].Name].Jobs)
	switch key.String() {
	case "esc", "q", "left", "h":
		p.screen = screenPrinterQueues
	case "up", "k":
		p.detailOff = max(p.detailOff-1, 0)
	case "down", "j":
		if p.detailOff+maxPreviewFiles < jobs {
			p.detailOff++
		}
	}
	return p, nil
}

func (p printerShare) title() string {
	return "ISO Burner · Share printer"
}

func (p printerShare) view() string {
	switch p.screen {
	case screenPrinterQueues:
		return p.queuesView()
	case screenPrinterDetail:
		return p.detailView()
	}
	return p.pickView()
}

func (p printerShare) pickView() string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("Printers") + dimStyle.Render("  (ticked printers are shared via AirPrint)") + "\n\n")
	switch {
	case p.loading:
		b.WriteString(dimStyle.Render("Looking for printers…") + "\n")
	case p.err != nil && len(p.printers) == 0:
		b.WriteString(errorStyle.Render("✗ "+p.err.Error()) + "\n")
	case len(p.printers) == 0:
		b.WriteString(dimStyle.Render("No printers found. Add one in the system settings and press r to rescan.") + "\n")
	default:
		for i, pr := range p.printers {
			box := "[ ]"
			if p.selected[pr.Name] {
				box = "[x]"
			}
			line := box + " " + pr.Label()
			if i == p.cursor {
				line = selectedStyle.Render("› " + line)
			} else {
				line = "  " + line
			}
			info := pr.State
			if pr.Shared {
				info += " · shared"
			}
			b.WriteString(line + "  " + dimStyle.Render(info) + "\n")
		}
		b.WriteString("\n" + okStyle.Render(fmt.Sprintf("%d of %d printer(s) selected", len(p.selected), len(p.printers))) + "\n")
		if p.applying {
			b.WriteString("\n" + dimStyle.Render("Updating printer sharing…") + "\n")
		}
		if p.err != nil {
			b.WriteString("\n" + errorStyle.Render("✗ "+p.err.Error()) + "\n")
		}
	}
	if p.notice != "" {
		b.WriteString("\n" + okStyle.Render("✓ "+p.notice) + "\n")
	}
	return b.String()
}

// queueSummary describes a printer's queue in one line.
func (p printerShare) queueSummary(name string) string {
	if err := p.advErrs[name]; err != nil {
		return errorStyle.Render("✗ " + err.Error())
	}
	if err := p.queueErrs[name]; err != nil {
		return errorStyle.Render("✗ " + err.Error())
	}
	q, ok := p.queues[name]
	if !ok {
		return dimStyle.Render("loading…")
	}
	var parts []string
	if j, ok := q.Printing(); ok {
		parts = append(parts, okStyle.Render("printing "+jobTitle(j)))
	} else {
		parts = append(parts, dimStyle.Render(q.Status))
	}
	parts = append(parts, fmt.Sprintf("%d queued", q.Waiting()))
	return strings.Join(parts, dimStyle.Render(" · "))
}

func (p printerShare) queuesView() string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("Shared printers") +
		dimStyle.Render("  (iPhones and iPads on this network can print to these via AirPrint while iso-burner is open)") + "\n\n")
	printing, waiting := 0, 0
	for i, pr := range p.shared {
		line := pr.Label()
		if i == p.queueIdx {
			line = selectedStyle.Render("› " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
		b.WriteString("     " + p.queueSummary(pr.Name) + "\n")
		q := p.queues[pr.Name]
		if _, ok := q.Printing(); ok {
			printing++
		}
		waiting += q.Waiting()
	}
	if p.loaded {
		b.WriteString("\n" + okStyle.Render(fmt.Sprintf("%d printing · %d job(s) queued", printing, waiting)) + "\n")
	}
	return b.String()
}

func (p printerShare) detailView() string {
	pr := p.shared[p.queueIdx]
	var b strings.Builder
	row := func(k, v string) {
		if v != "" {
			b.WriteString(labelStyle.Render(fmt.Sprintf("%-10s", k)) + " " + v + "\n")
		}
	}
	b.WriteString(labelStyle.Render(pr.Label()) + "\n\n")
	row("Queue", pr.Name)
	row("Model", pr.Model)
	row("Location", pr.Location)
	if err := p.advErrs[pr.Name]; err != nil {
		row("AirPrint", errorStyle.Render("✗ "+err.Error()))
	} else {
		row("AirPrint", printer.AdvertisedName(pr))
	}
	if err := p.queueErrs[pr.Name]; err != nil {
		b.WriteString("\n" + errorStyle.Render("✗ "+err.Error()) + "\n")
		return b.String()
	}
	q, ok := p.queues[pr.Name]
	if !ok {
		b.WriteString("\n" + dimStyle.Render("Reading the job queue…") + "\n")
		return b.String()
	}
	row("Status", q.Status)
	b.WriteString("\n" + labelStyle.Render(fmt.Sprintf("Jobs (%d)", len(q.Jobs))) + "\n")
	if len(q.Jobs) == 0 {
		b.WriteString(dimStyle.Render("  No jobs. Print from an iPhone or iPad to see them here.") + "\n")
		return b.String()
	}
	start := min(p.detailOff, len(q.Jobs)-1)
	end := min(start+maxPreviewFiles, len(q.Jobs))
	for _, j := range q.Jobs[start:end] {
		state := "queued " + j.Rank
		if j.Printing() {
			state = okStyle.Render("printing")
		}
		b.WriteString(fmt.Sprintf("  #%-5d %s  %s\n", j.ID, jobTitle(j),
			dimStyle.Render(fmt.Sprintf("%s · %s · %s", state, j.Owner, settings.FormatBytes(j.Size)))))
	}
	if len(q.Jobs) > maxPreviewFiles {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Showing jobs %d–%d of %d", start+1, end, len(q.Jobs))) + "\n")
	}
	return b.String()
}

func jobTitle(j printer.Job) string {
	if j.Title == "" {
		return fmt.Sprintf("job %d", j.ID)
	}
	return j.Title
}

func (p printerShare) help() string {
	switch p.screen {
	case screenPrinterQueues:
		return "↑/↓: choose · enter: details · esc: back (printers stay shared)"
	case screenPrinterDetail:
		return "↑/↓: scroll jobs · esc: back"
	}
	if p.loading || len(p.printers) == 0 {
		return "r: rescan · esc: back"
	}
	return "↑/↓: move · space: toggle · a: all/none · r: rescan · enter: share · esc: back"
}

// startPrinters opens printer mode.
func (m Model) startPrinters() (tea.Model, tea.Cmd) {
	m.mode = ModePrinter
	m.modeChosen = true
	prev := m.printers
	m.printers = newPrinterShare(m.printerSvc, prev.gen)
	// Adverts outlive leaving printer mode, so keep them to stop later.
	m.printers.adv, m.printers.advGen, m.printers.advErrs, m.printers.shared = prev.adv, prev.advGen, prev.advErrs, prev.shared
	return m, m.printers.list()
}

func (m Model) updatePrinters(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	next, cmd := m.printers.update(msg)
	m.printers = next
	if next.back {
		m.printers.back = false
		if !m.modeChosen {
			m.cancelled = true
			return m, tea.Quit
		}
		m.mode = ModeNone
		return m, nil
	}
	return m, cmd
}

func (m Model) printersView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.printers.title()) + "\n\n")
	b.WriteString(m.printers.view())
	b.WriteString("\n" + dimStyle.Render(m.printers.help()))
	return panelStyle.Render(b.String()) + "\n"
}

// StopAdvertising withdraws the AirPrint adverts. Call it before exiting, or
// the Bonjour tools keep advertising printers nobody is serving.
func (m Model) StopAdvertising() {
	m.printers.stopAdvertising()
	if closer, ok := m.printerSvc.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

// TemporaryPrinterSharing reports whether exiting also stops the print server.
func (m Model) TemporaryPrinterSharing() bool {
	_, ok := m.printerSvc.(*printer.Native)
	return ok
}

// SharedPrinters returns the labels of the printers shared in printer mode.
func (m Model) SharedPrinters() []string {
	var out []string
	for _, pr := range m.printers.shared {
		out = append(out, pr.Label())
	}
	return out
}
