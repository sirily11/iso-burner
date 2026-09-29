package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/printer"
)

// fakePrinters is an in-memory printer.Service.
type fakePrinters struct {
	printers     []printer.Printer
	queues       map[string]printer.Queue
	shareErr     error
	advertiseErr map[string]error
	mu           sync.Mutex
	advertising  map[string]bool
	shared       []string
	unshared     []string
}

func (f *fakePrinters) List(context.Context) ([]printer.Printer, error) {
	return slices.Clone(f.printers), nil
}

func (f *fakePrinters) Share(_ context.Context, names []string) error {
	if f.shareErr != nil {
		return f.shareErr
	}
	f.shared = append(f.shared, names...)
	return nil
}

func (f *fakePrinters) Unshare(_ context.Context, names []string) error {
	f.unshared = append(f.unshared, names...)
	return nil
}

func (f *fakePrinters) Advertise(ctx context.Context, p printer.Printer) error {
	if err := f.advertiseErr[p.Name]; err != nil {
		return err
	}
	f.mu.Lock()
	if f.advertising == nil {
		f.advertising = map[string]bool{}
	}
	f.advertising[p.Name] = true
	f.mu.Unlock()
	<-ctx.Done()
	f.mu.Lock()
	delete(f.advertising, p.Name)
	f.mu.Unlock()
	return nil
}

func (f *fakePrinters) advertised() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name := range f.advertising {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func (f *fakePrinters) Queue(_ context.Context, name string) (printer.Queue, error) {
	q, ok := f.queues[name]
	if !ok {
		return printer.Queue{}, errors.New("no such printer")
	}
	return q, nil
}

func newFakePrinters() *fakePrinters {
	return &fakePrinters{
		printers: []printer.Printer{
			{Name: "HP", Info: "Office HP", State: "printing"},
			{Name: "Canon", Info: "Canon Photo", State: "idle", Shared: true},
			{Name: "Brother", State: "idle"},
		},
		queues: map[string]printer.Queue{
			"HP": {Status: "ready and printing", Jobs: []printer.Job{
				{ID: 12, Rank: "active", Owner: "alice", Title: "report.pdf", Size: 2048},
				{ID: 13, Rank: "1st", Owner: "bob", Title: "Photo 1.jpg", Size: 1024},
			}},
			"Canon": {Status: "ready"},
		},
	}
}

// sendPrinter delivers msg and runs the returned commands until they stop
// producing printer messages. Refresh ticks are skipped so tests do not wait,
// and commands still running after a moment, like AirPrint adverts, are left
// running until the adverts are stopped.
func sendPrinter(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, cmd := m.Update(msg)
	m = next.(Model)
	pending := []tea.Cmd{cmd}
	for len(pending) > 0 {
		cmd, pending = pending[0], pending[1:]
		if cmd == nil {
			continue
		}
		out := make(chan tea.Msg, 1)
		go func() { out <- cmd() }()
		var res tea.Msg
		select {
		case res = <-out:
		case <-time.After(100 * time.Millisecond):
			continue
		}
		switch res := res.(type) {
		case tea.BatchMsg:
			pending = append(pending, res...)
		case printersLoadedMsg, printersSharedMsg, queuesLoadedMsg, advertiseEndedMsg:
			next, cmd = m.Update(res)
			m = next.(Model)
			pending = append(pending, cmd)
		}
	}
	return m
}

func openPrinterMode(t *testing.T, svc *fakePrinters) Model {
	t.Helper()
	m := New(Options{Folder: t.TempDir(), Printers: svc})
	m = sendPrinter(t, m, key("4"))
	if m.Mode() != ModePrinter {
		t.Fatalf("pressing 4 should open printer mode, got %d", m.Mode())
	}
	return m
}

func TestPrinterShareFlow(t *testing.T) {
	svc := newFakePrinters()
	m := openPrinterMode(t, svc)
	view := m.View()
	for _, want := range []string{"[ ] Office HP", "[x] Canon Photo", "idle · shared", "1 of 3 printer(s) selected"} {
		if !strings.Contains(view, want) {
			t.Errorf("picker missing %q:\n%s", want, view)
		}
	}

	// Tick HP (the cursor starts on it) and untick Canon.
	m = sendPrinter(t, m, key(" "))
	m = sendPrinter(t, m, key("down"))
	m = sendPrinter(t, m, key(" "))
	m = sendPrinter(t, m, key("down"))
	m = sendPrinter(t, m, key(" "))
	m = sendPrinter(t, m, key("enter"))
	if !slices.Equal(svc.shared, []string{"HP", "Brother"}) || !slices.Equal(svc.unshared, []string{"Canon"}) {
		t.Fatalf("shared %v, unshared %v", svc.shared, svc.unshared)
	}
	if m.printers.screen != screenPrinterQueues {
		t.Fatalf("sharing should open the queues, screen=%d", m.printers.screen)
	}
	view = m.View()
	for _, want := range []string{"Shared printers", "› Office HP", "printing report.pdf", "1 queued",
		"Brother", "no such printer", "1 printing · 1 job(s) queued"} {
		if !strings.Contains(view, want) {
			t.Errorf("queues missing %q:\n%s", want, view)
		}
	}

	m = sendPrinter(t, m, key("enter"))
	view = m.View()
	for _, want := range []string{"Jobs (2)", "#12", "report.pdf", "printing · alice", "#13", "queued 1st · bob"} {
		if !strings.Contains(view, want) {
			t.Errorf("detail missing %q:\n%s", want, view)
		}
	}

	m = sendPrinter(t, m, key("esc"))
	if m.printers.screen != screenPrinterQueues {
		t.Fatal("esc in details should go back to the queues")
	}
	m = sendPrinter(t, m, key("esc"))
	if m.printers.screen != screenPrinterPick || !strings.Contains(m.View(), "[x] Office HP") {
		t.Fatalf("esc in the queues should go back to the picker:\n%s", m.View())
	}
	m = sendPrinter(t, m, key("esc"))
	if m.Mode() != ModeNone || m.cancelled {
		t.Fatal("esc in the picker should return to mode selection")
	}
	if got := m.SharedPrinters(); !slices.Equal(got, []string{"Office HP", "Brother"}) {
		t.Fatalf("SharedPrinters = %v", got)
	}
}

func TestPrinterRefreshIgnoresStaleLoops(t *testing.T) {
	svc := newFakePrinters()
	m := openPrinterMode(t, svc)
	m = sendPrinter(t, m, key("enter")) // share Canon as it is
	gen := m.printers.gen
	next, cmd := m.Update(queueTickMsg{gen: gen - 1})
	if cmd != nil {
		t.Fatal("a tick from an old refresh loop should be dropped")
	}
	if _, cmd = next.(Model).Update(queueTickMsg{gen: gen}); cmd == nil {
		t.Fatal("a tick from the current loop should refresh the queues")
	}
}

func TestPrinterShareErrors(t *testing.T) {
	svc := newFakePrinters()
	svc.printers[1].Shared = false
	m := openPrinterMode(t, svc)
	m = sendPrinter(t, m, key("enter"))
	if m.printers.err == nil || m.printers.screen != screenPrinterPick {
		t.Fatal("enter with nothing selected and nothing shared should show an error")
	}

	svc.shareErr = errors.New("cupsctl: Forbidden")
	m = sendPrinter(t, m, key(" "))
	m = sendPrinter(t, m, key("enter"))
	if !strings.Contains(m.View(), "Forbidden") || m.printers.screen != screenPrinterPick {
		t.Fatalf("a failed share should stay on the picker with the error:\n%s", m.View())
	}
}

func TestPrinterUnshareAll(t *testing.T) {
	svc := newFakePrinters()
	m := openPrinterMode(t, svc)
	m = sendPrinter(t, m, key("down"))
	m = sendPrinter(t, m, key(" ")) // untick Canon, the only shared printer
	m = sendPrinter(t, m, key("enter"))
	if !slices.Equal(svc.unshared, []string{"Canon"}) || m.printers.screen != screenPrinterPick {
		t.Fatalf("unticking everything should stop sharing and stay on the picker: %v", svc.unshared)
	}
	if !strings.Contains(m.View(), "Stopped sharing 1 printer(s)") {
		t.Fatalf("missing notice:\n%s", m.View())
	}
}

func TestPrinterAirPrintAdverts(t *testing.T) {
	svc := newFakePrinters()
	svc.advertiseErr = map[string]error{"Brother": errors.New("dns-sd not found")}
	m := openPrinterMode(t, svc)
	m = sendPrinter(t, m, key(" ")) // tick HP; Canon is already shared
	m = sendPrinter(t, m, key("down"))
	m = sendPrinter(t, m, key("down"))
	m = sendPrinter(t, m, key(" ")) // tick Brother
	m = sendPrinter(t, m, key("enter"))
	if got := svc.advertised(); !slices.Equal(got, []string{"Canon", "HP"}) {
		t.Fatalf("advertising %v, want Canon and HP", got)
	}
	if !strings.Contains(m.View(), "dns-sd not found") {
		t.Fatalf("a failed advert should show on its printer:\n%s", m.View())
	}
	m = sendPrinter(t, m, key("enter"))
	if !strings.Contains(m.View(), "Office HP (AirPrint)") {
		t.Fatalf("details should show the AirPrint name:\n%s", m.View())
	}

	// Leaving printer mode keeps advertising; coming back and unsharing HP
	// replaces the adverts.
	m = sendPrinter(t, m, key("esc"))
	m = sendPrinter(t, m, key("esc"))
	m = sendPrinter(t, m, key("esc"))
	m = sendPrinter(t, m, key("4"))
	if got := svc.advertised(); len(got) != 2 {
		t.Fatalf("adverts should outlive leaving printer mode, got %v", got)
	}
	m.printers.printers[0].Shared = true // the fake does not persist sharing
	m.printers.printers[2].Shared = true
	m.printers.selected = map[string]bool{"Canon": true}
	m = sendPrinter(t, m, key("enter"))
	if got := svc.advertised(); !slices.Equal(got, []string{"Canon"}) {
		t.Fatalf("after unsharing HP, advertising %v", got)
	}

	m.StopAdvertising()
	if got := svc.advertised(); len(got) != 0 {
		t.Fatalf("StopAdvertising should end every advert, still %v", got)
	}
}
