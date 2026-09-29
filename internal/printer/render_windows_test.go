package printer

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// The real Windows driver is exercised using Print to PDF, never a physical
// printer. Then Windows.Data.Pdf renders its output back to a PNG page.
func TestWindowsPrintAndPDFRender(t *testing.T) {
	if os.Getenv("ISO_BURNER_TEST_WINDOWS_PRINTER") == "" {
		t.Skip("opt in to native Windows print integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	printers, err := listWindowsPrinters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var selected PaperSize
	for _, p := range printers {
		if p.Name == "Microsoft Print to PDF" {
			for _, paper := range p.PaperSizes {
				if paper.keyword() == "iso_a5_148x210mm" {
					selected = paper
				}
			}
		}
	}
	if !selected.valid() {
		t.Fatal("Microsoft Print to PDF does not report A5")
	}
	dir := t.TempDir()
	pages := filepath.Join(dir, "pages")
	if err := os.Mkdir(pages, 0700); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	f, err := os.Create(filepath.Join(pages, "00000.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	pdf := filepath.Join(dir, "native-print.pdf")
	if _, err = queryWindowsPrinter(ctx, windowsPrintPages, "ISO_BURNER_PRINTER=Microsoft Print to PDF", "ISO_BURNER_TITLE=Native AirPrint test", "ISO_BURNER_PAGES="+pages, "ISO_BURNER_COPIES=1", "ISO_BURNER_LANDSCAPE=false", "ISO_BURNER_PRINT_OUTPUT="+pdf,
		"ISO_BURNER_PAPER_ID="+strconv.Itoa(selected.WindowsID), "ISO_BURNER_PAPER_WIDTH="+strconv.Itoa(selected.Width), "ISO_BURNER_PAPER_HEIGHT="+strconv.Itoa(selected.Height)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pdf)
	if err != nil || len(data) < 8 || string(data[:5]) != "%PDF-" {
		t.Fatalf("Windows PDF output: %v", err)
	}
	box := regexp.MustCompile(`/MediaBox\s*\[\s*0(?:\.0)?\s+0(?:\.0)?\s+([\d.]+)\s+([\d.]+)`).FindSubmatch(data)
	if len(box) != 3 {
		t.Fatal("Microsoft Print to PDF output has no page dimensions")
	}
	width, _ := strconv.ParseFloat(string(box[1]), 64)
	height, _ := strconv.ParseFloat(string(box[2]), 64)
	// PrinterSettings rounds to 1/100 inch; the PDF driver retains its native
	// metric form dimensions. Half that rounding is about 0.36 PDF points.
	if math.Abs(width-float64(selected.Width)*72/2540) > 0.4 || math.Abs(height-float64(selected.Height)*72/2540) > 0.4 {
		t.Fatalf("PDF uses %gx%g points instead of selected A5", width, height)
	}
	rendered := filepath.Join(dir, "rendered")
	_ = os.Mkdir(rendered, 0700)
	// Received AirPrint documents have temporary paths without file extensions.
	input := filepath.Join(dir, "airprint-document")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := renderWindowsDocument(ctx, Document{Path: input, Format: "application/pdf"}, rendered); err != nil {
		t.Fatal(err)
	}
	out, err := os.Open(filepath.Join(rendered, "00000.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	decoded, err := png.Decode(out)
	if err != nil || decoded.Bounds().Dx() < 100 {
		t.Fatalf("Windows PDF rendering: %v", err)
	}
	// WinRT render dimensions are DIPs and vary with display scaling. The PDF
	// MediaBox above verifies physical stock; the bitmap must keep its aspect.
	if math.Abs(float64(decoded.Bounds().Dx())/float64(decoded.Bounds().Dy())-width/height) > 0.002 {
		t.Fatalf("rendered page changed the selected paper's aspect ratio: %v", decoded.Bounds())
	}
	// Rendering must preserve content, not produce a blank page.
	r, g, b, _ := decoded.At(decoded.Bounds().Dx()/2, decoded.Bounds().Dy()/2).RGBA()
	if r <= g || r <= b {
		t.Fatalf("rendered center is not red: %d,%d,%d", r, g, b)
	}
}

func TestWindowsSelectDriverForms(t *testing.T) {
	if os.Getenv("ISO_BURNER_TEST_WINDOWS_PRINTER") == "" {
		t.Skip("opt in to read-only driver form checks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// This loads settings and selects forms in memory; it never calls Print.
	_, err := queryWindowsPrinter(ctx, windowsPrintHelper+`
foreach ($printer in (Get-Printer)) {
    $settings = New-Object System.Drawing.Printing.PrinterSettings
    $settings.PrinterName = $printer.Name
    $forms = @($settings.DefaultPageSettings.PaperSize) + @($settings.PaperSizes | Select-Object -First 1 -Last 1)
    foreach ($paper in $forms) {
        if ($paper.Width -le 0 -or $paper.Height -le 0) { continue }
        $selected = [IsoBurnerPrint]::SelectPaper($settings,$paper.RawKind,
            [int][Math]::Round($paper.Width*25.4),[int][Math]::Round($paper.Height*25.4))
        if ($selected.RawKind -ne $paper.RawKind -or $selected.Width -ne $paper.Width -or $selected.Height -ne $paper.Height) {
            throw ('Driver form lost for ' + $printer.Name + ': ' + $paper.PaperName)
        }
    }
}
`)
	if err != nil {
		t.Fatal(err)
	}
}
