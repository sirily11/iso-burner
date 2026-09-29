package printer

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestWindowsPrinterSnapshots(t *testing.T) {
	printers, err := parseWindowsPrinterList([]byte(`[{"Name":"Office's 彩色 printer","DriverName":"Driver","Location":"Desk","State":"Normal"}]`))
	if err != nil || len(printers) != 1 || printers[0].Name != "Office's 彩色 printer" || printers[0].State != "idle" || printers[0].Shared {
		t.Fatalf("Windows printers = %+v, %v", printers, err)
	}
	q, err := parseWindowsPrinterQueue([]byte(`{"Status":"Normal","Jobs":[
{"ID":7,"DocumentName":"照片.pdf","UserName":"alice","Size":4096,"State":"Printing, Spooling"},
{"ID":8,"DocumentName":"Next.pdf","UserName":"bob","Size":2048,"State":"Paused"},
{"ID":6,"DocumentName":"Old.pdf","UserName":"bob","Size":1024,"State":"Printed, Retained"}]}`))
	if err != nil || len(q.Jobs) != 2 || q.Status != "printing" || q.Waiting() != 1 {
		t.Fatalf("Windows queue = %+v, %v", q, err)
	}
	if j, ok := q.Printing(); !ok || j.ID != 7 || j.Title != "照片.pdf" || j.Size != 4096 {
		t.Fatalf("active job = %+v, %v", j, ok)
	}
	if _, err := parseWindowsPrinterQueue([]byte(`not JSON`)); err == nil {
		t.Fatal("invalid Windows response should report an error")
	}
}

func TestWindowsPaperSnapshot(t *testing.T) {
	printers, err := parseWindowsPrinterList([]byte(`[{"Name":"Deli DL-750W",
		"DefaultPaper":{"Name":"USER","Width":10236,"Height":17882,"WindowsID":256},
		"PaperSizes":[{"Name":"4 x 6","Width":10414,"Height":15240,"WindowsID":285}]}]`))
	if err != nil || len(printers) != 1 {
		t.Fatalf("printer snapshot: %v, %v", printers, err)
	}
	p := printers[0]
	if p.DefaultPaper.WindowsID != 256 || p.DefaultPaper.Width != 10236 || len(p.PaperSizes) != 1 || p.PaperSizes[0].WindowsID != 285 {
		t.Fatalf("driver paper details were lost: %+v", p)
	}
}

func TestWindowsSpoolerIntegration(t *testing.T) {
	if os.Getenv("ISO_BURNER_TEST_WINDOWS_PRINTER") == "" {
		t.Skip("set ISO_BURNER_TEST_WINDOWS_PRINTER for a read-only Windows spooler check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	printers, err := listWindowsPrinters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range printers {
		if !pr.DefaultPaper.valid() || len(pr.PaperSizes) == 0 {
			t.Fatalf("%s has no driver paper settings: %+v", pr.Name, pr)
		}
		t.Logf("%s: default %s (%gx%g mm), %d supported sizes", pr.Name, pr.DefaultPaper.Name,
			float64(pr.DefaultPaper.Width)/100, float64(pr.DefaultPaper.Height)/100, len(pr.PaperSizes))
		if _, err := readWindowsQueue(ctx, pr.Name); err != nil {
			t.Fatalf("%s: %v", pr.Name, err)
		}
	}
	// A printer name is data even when it looks like executable PowerShell.
	if _, err := readWindowsQueue(ctx, "'; Write-Output injected; '"); err == nil {
		t.Fatal("nonexistent printer should report an error")
	}
}
