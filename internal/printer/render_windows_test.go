package printer

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
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
	if _, err = queryWindowsPrinter(ctx, windowsPrintPages, "ISO_BURNER_PRINTER=Microsoft Print to PDF", "ISO_BURNER_TITLE=Native AirPrint test", "ISO_BURNER_PAGES="+pages, "ISO_BURNER_COPIES=1", "ISO_BURNER_LANDSCAPE=false", "ISO_BURNER_PRINT_OUTPUT="+pdf); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pdf)
	if err != nil || len(data) < 8 || string(data[:5]) != "%PDF-" {
		t.Fatalf("Windows PDF output: %v", err)
	}
	rendered := filepath.Join(dir, "rendered")
	_ = os.Mkdir(rendered, 0700)
	if err := renderWindowsDocument(ctx, Document{Path: pdf, Format: "application/pdf"}, rendered); err != nil {
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
	// Rendering must preserve content, not produce a blank page.
	r, g, b, _ := decoded.At(decoded.Bounds().Dx()/2, decoded.Bounds().Dy()/2).RGBA()
	if r <= g || r <= b {
		t.Fatalf("rendered center is not red: %d,%d,%d", r, g, b)
	}
}
