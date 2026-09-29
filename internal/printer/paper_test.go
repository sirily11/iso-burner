package printer

import (
	"context"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
)

func deliTestPrinter() Printer {
	return Printer{Name: "Deli", DefaultPaper: PaperSize{Name: "USER", Width: 10236, Height: 17882, WindowsID: 256},
		PaperSizes: []PaperSize{
			{Name: "4 x 6", Width: 10414, Height: 15240, WindowsID: 285},
			{Name: "Duplicate", Width: 10414, Height: 15240, WindowsID: 300},
			{Name: "Invalid"},
		}}
}

func TestDriverPaperCapabilities(t *testing.T) {
	p := deliTestPrinter()
	attrs := paperAttributes(p)
	if got := ippString(attrs, "media-default", ""); got != "custom_paper_102.36x178.82mm" {
		t.Fatalf("driver default = %s", got)
	}
	for _, name := range []string{"media-supported", "media-size-supported", "media-col-database"} {
		found := false
		for _, a := range attrs {
			if a.Name == name {
				found = true
				if len(a.Values) != 2 {
					t.Fatalf("%s has %d values, want default + distinct supported size", name, len(a.Values))
				}
			}
		}
		if !found {
			t.Fatalf("missing %s", name)
		}
	}
	col, ok := ippCollection(attrs, "media-col-default")
	if !ok {
		t.Fatal("missing default media collection")
	}
	size, _ := ippCollection(goipp.Attributes(col), "media-size")
	if ippInt(goipp.Attributes(size), "x-dimension", 0) != 10236 || ippInt(goipp.Attributes(size), "y-dimension", 0) != 17882 {
		t.Fatal("default dimensions do not match driver settings")
	}
	if attrs := paperAttributes(Printer{}); len(attrs) != 0 {
		t.Fatalf("invented paper sizes for an unknown driver: %v", attrs)
	}
	if got := (PaperSize{Width: 21006, Height: 29693}).keyword(); got != "iso_a4_210x297mm" {
		t.Fatalf("Windows rounded A4 should have its standard name, got %s", got)
	}
}

func TestRequestedPaper(t *testing.T) {
	p := deliTestPrinter()
	label := p.PaperSizes[0]
	for _, tc := range []struct {
		name  string
		attrs goipp.Attributes
		want  PaperSize
	}{
		{"default", nil, p.DefaultPaper},
		{"keyword", goipp.Attributes{ippAttr("media", goipp.TagKeyword, goipp.String(label.keyword()))}, label},
		{"iOS collection", goipp.Attributes{ippAttr("media-col", goipp.TagBeginCollection,
			goipp.Collection{ippAttr("media-size", goipp.TagBeginCollection, paperDimensions(label))})}, label},
		{"rounded dimensions", goipp.Attributes{ippAttr("media-col", goipp.TagBeginCollection,
			goipp.Collection{ippAttr("media-size", goipp.TagBeginCollection, paperDimensions(PaperSize{Width: 10240, Height: 17880}))})}, p.DefaultPaper},
		{"collection precedence", goipp.Attributes{
			ippAttr("media", goipp.TagKeyword, goipp.String("iso_a4_210x297mm")),
			ippAttr("media-col", goipp.TagBeginCollection, paperCollection(label))}, label},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requestedPaper(tc.attrs, p)
			if err != nil || got != tc.want {
				t.Fatalf("selected %+v (%v), want %+v", got, err, tc.want)
			}
		})
	}
	for _, attrs := range []goipp.Attributes{
		{ippAttr("media", goipp.TagKeyword, goipp.String("iso_a4_210x297mm"))},
		{ippAttr("media-col", goipp.TagKeyword, goipp.String("invalid"))},
		{ippAttr("media-col", goipp.TagBeginCollection, goipp.Collection{ippAttr("media-size", goipp.TagBeginCollection, paperDimensions(PaperSize{}))})},
		{ippAttr("media-col", goipp.TagBeginCollection, paperCollection(PaperSize{Width: 5000, Height: 7000}))},
	} {
		if _, err := requestedPaper(attrs, p); err == nil {
			t.Fatalf("accepted unsupported/invalid paper: %v", attrs)
		}
	}
}

func TestNativeMediaCollectionWorkflow(t *testing.T) {
	received := make(chan Document, 2)
	n, endpoint := nativeTestServer(t, func(_ context.Context, doc Document) error { received <- doc; return nil })
	n.mu.Lock()
	var p Printer
	for _, p = range n.printers {
	}
	p.PaperSizes = []PaperSize{{Name: "Small label", Width: 5000, Height: 7500, WindowsID: 283}}
	n.printers[p.Name] = p
	n.mu.Unlock()
	selected := p.PaperSizes[0]
	options := goipp.Attributes{ippAttr("media-col", goipp.TagBeginCollection,
		goipp.Collection{ippAttr("media-size", goipp.TagBeginCollection, paperDimensions(selected))})}
	if res := sendIPP(t, endpoint, 4, nil, options, nil); res.Code != 0 {
		t.Fatalf("Validate-Job rejected supported media: %v", res.Code)
	}
	for _, create := range []bool{false, true} {
		if create {
			res := sendIPP(t, endpoint, 5, nil, options, nil)
			if res.Code != 0 {
				t.Fatalf("Create-Job failed: %v", res.Code)
			}
			attrs := goipp.Attributes{ippAttr("job-id", goipp.TagInteger, goipp.Integer(ippInt(res.Job, "job-id", 0)))}
			if res := sendIPP(t, endpoint, 6, attrs, nil, []byte("document")); res.Code != 0 {
				t.Fatalf("Send-Document failed: %v", res.Code)
			}
		} else if res := sendIPP(t, endpoint, 2, nil, options, []byte("document")); res.Code != 0 {
			t.Fatalf("Print-Job failed: %v", res.Code)
		}
		select {
		case doc := <-received:
			if doc.Paper != selected {
				t.Fatalf("driver received paper %+v, want %+v", doc.Paper, selected)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("job did not reach the driver")
		}
	}
	bad := goipp.Attributes{ippAttr("media", goipp.TagKeyword, goipp.String("iso_a4_210x297mm"))}
	if res := sendIPP(t, endpoint, 4, nil, bad, nil); res.Code != 0x040b || len(res.Unsupported) != 1 {
		t.Fatalf("unsupported A4 did not return an IPP media error: %+v", res)
	}
}

func TestNativeRefreshesPaperSettings(t *testing.T) {
	n, endpoint := nativeTestServer(t, func(context.Context, Document) error { return nil })
	var p Printer
	for _, p = range n.printers {
	}
	p.DefaultPaper = deliTestPrinter().DefaultPaper
	n.ListPrinters = func(context.Context) ([]Printer, error) { return []Printer{p}, nil }
	n.lastRefresh = time.Time{}
	res := sendIPP(t, endpoint, 11, nil, nil, nil)
	if got := ippString(res.Printer, "media-default", ""); got != p.DefaultPaper.keyword() {
		t.Fatalf("stale paper after Windows preferences changed: %s", got)
	}
}
