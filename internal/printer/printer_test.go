package printer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseLpstat(t *testing.T) {
	out := `printer HP_LaserJet is idle.  enabled since Thu Sep 10 14:16:10 2026
printer Brother now printing Brother-12.  enabled since Thu Sep 10 14:16:10 2026
printer Canon disabled since Thu Sep 10 14:16:10 2026 -
	printer is out of paper
`
	got := parseLpstat(out)
	want := []Printer{
		{Name: "HP_LaserJet", State: "idle"},
		{Name: "Brother", State: "printing"},
		{Name: "Canon", State: "disabled"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseLpstat = %+v, want %+v", got, want)
	}
}

func TestParseOptions(t *testing.T) {
	out := `copies=1 printer-info='HP LaserJet MFP M141w (F23992) 2' printer-is-shared=false printer-location printer-make-and-model='Flyingbee Deli DL-750W' note='Alice\'s desk' place='Air Printer on DESKTOP's PC' last=x\ y`
	got := parseOptions(out)
	want := map[string]string{
		"copies":                 "1",
		"printer-info":           "HP LaserJet MFP M141w (F23992) 2",
		"printer-is-shared":      "false",
		"printer-location":       "",
		"printer-make-and-model": "Flyingbee Deli DL-750W",
		"note":                   "Alice's desk",
		"place":                  "Air Printer on DESKTOP's PC",
		"last":                   "x y",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseOptions =\n%v\nwant\n%v", got, want)
	}
}

func TestParseLpq(t *testing.T) {
	out := `HP_LaserJet is ready and printing
Rank    Owner   Job     File(s)                         Total Size
active  alice   12      report.pdf                      12288 bytes
1st     bob     13      Photo 1.jpg                     1024 bytes
`
	q, err := parseLpq("HP_LaserJet", out)
	if err != nil {
		t.Fatal(err)
	}
	if q.Status != "ready and printing" || len(q.Jobs) != 2 {
		t.Fatalf("queue = %+v", q)
	}
	if j := q.Jobs[1]; j.ID != 13 || j.Rank != "1st" || j.Owner != "bob" || j.Title != "Photo 1.jpg" || j.Size != 1024 {
		t.Fatalf("second job = %+v", j)
	}
	if j, ok := q.Printing(); !ok || j.ID != 12 {
		t.Fatalf("printing = %+v, %v", j, ok)
	}
	if q.Waiting() != 1 {
		t.Fatalf("waiting = %d, want 1", q.Waiting())
	}

	q, err = parseLpq("HP_LaserJet", "HP_LaserJet is ready\nno entries\n")
	if err != nil || q.Status != "ready" || len(q.Jobs) != 0 {
		t.Fatalf("empty queue = %+v, %v", q, err)
	}
	if _, err := parseLpq("HP_LaserJet", "garbage"); err == nil {
		t.Fatal("unexpected output should fail")
	}
}

// fakeRunner records commands and answers from a table keyed by the command
// line.
type fakeRunner struct {
	calls   []string
	answers map[string]string
	fail    map[string]error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	return f.answers[line], f.fail[line]
}

func TestCUPSList(t *testing.T) {
	f := &fakeRunner{answers: map[string]string{
		"lpstat -p":       "printer HP is idle.  enabled since today\n",
		"lpoptions -p HP": "printer-info='Office HP' printer-is-shared=true printer-location=Lobby",
	}}
	got, err := CUPS{Run: f.run}.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Printer{{Name: "HP", Info: "Office HP", Location: "Lobby", Shared: true, State: "idle"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %+v, want %+v", got, want)
	}

	f = &fakeRunner{
		answers: map[string]string{"lpstat -p": "lpstat: No destinations added.\n"},
		fail:    map[string]error{"lpstat -p": errors.New("exit 1")},
	}
	if got, err := (CUPS{Run: f.run}).List(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("no printers: %+v, %v", got, err)
	}
}

func TestCUPSShare(t *testing.T) {
	f := &fakeRunner{}
	c := CUPS{Run: f.run}
	if err := c.Share(context.Background(), []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Unshare(context.Background(), []string{"C"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cupsctl --share-printers",
		"lpadmin -p A -o printer-is-shared=true",
		"lpadmin -p B -o printer-is-shared=true",
		"lpadmin -p C -o printer-is-shared=false",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}

	f = &fakeRunner{fail: map[string]error{"cupsctl --share-printers": errors.New("cupsctl: Forbidden")}}
	err := CUPS{Run: f.run}.Share(context.Background(), []string{"A"})
	if err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("forbidden share should suggest sudo, got %v", err)
	}
}
