package printer

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// Windows.Data.Pdf renders PDF pages without installing a third-party viewer.
const windowsRenderPDF = `
Add-Type -AssemblyName System.Runtime.WindowsRuntime
[Windows.Storage.StorageFile,Windows.Storage,ContentType=WindowsRuntime] > $null
[Windows.Data.Pdf.PdfDocument,Windows.Data.Pdf,ContentType=WindowsRuntime] > $null
[Windows.Storage.Streams.InMemoryRandomAccessStream,Windows.Storage.Streams,ContentType=WindowsRuntime] > $null
[Windows.Data.Pdf.PdfPageRenderOptions,Windows.Data.Pdf,ContentType=WindowsRuntime] > $null
$asTask = [System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
    $_.Name -eq 'AsTask' -and $_.IsGenericMethod -and $_.GetParameters().Count -eq 1 -and
    $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation` + "`" + `1'
} | Select-Object -First 1
$asAction = [System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
    $_.Name -eq 'AsTask' -and -not $_.IsGenericMethod -and $_.GetParameters().Count -eq 1 -and
    $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncAction'
} | Select-Object -First 1
function Await($operation, $resultType) {
    $task = $asTask.MakeGenericMethod($resultType).Invoke($null,@($operation))
    $task.Wait()
    return $task.Result
}
$file = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($env:ISO_BURNER_DOCUMENT)) ([Windows.Storage.StorageFile])
$pdf = Await ([Windows.Data.Pdf.PdfDocument]::LoadFromFileAsync($file)) ([Windows.Data.Pdf.PdfDocument])
if ($pdf.PageCount -gt 100) { throw 'PDF exceeds 100 pages.' }
$total = 0
for ($i=0; $i -lt $pdf.PageCount; $i++) {
    $page = $pdf.GetPage($i)
    $stream = New-Object Windows.Storage.Streams.InMemoryRandomAccessStream
    try {
        $options = New-Object Windows.Data.Pdf.PdfPageRenderOptions
        $options.DestinationWidth = [uint32][Math]::Ceiling($page.Size.Width * 300 / 96)
        $options.DestinationHeight = [uint32][Math]::Ceiling($page.Size.Height * 300 / 96)
        if ([double]$options.DestinationWidth * $options.DestinationHeight -gt 20000000) { throw 'PDF page is too large.' }
        $action = $asAction.Invoke($null,@($page.RenderToStreamAsync($stream,$options)))
        $action.Wait()
        $input = [System.IO.WindowsRuntimeStreamExtensions]::AsStreamForRead($stream)
        $dest = [IO.File]::Create((Join-Path $env:ISO_BURNER_PAGES ('{0:D5}.png' -f $i)))
        try { $input.CopyTo($dest); $total += $dest.Length } finally { $dest.Dispose(); $input.Dispose() }
        if ($total -gt 536870912) { throw 'Rendered document exceeds 512 MB.' }
    } finally { $stream.Dispose(); $page.Dispose() }
}
`

// PrintDocument sends rendered pages through the installed Windows driver.
// A StandardPrintController prevents print dialogs from blocking the queue.
const windowsPrintPages = `
Add-Type -ReferencedAssemblies System.Drawing -TypeDefinition @'
using System;
using System.IO;
using System.Drawing;
using System.Drawing.Printing;
public static class IsoBurnerPrint {
    public static void Run(string printer, string title, string folder, int copies, bool landscape, string output) {
        string[] pages = Directory.GetFiles(folder);
        Array.Sort(pages, StringComparer.Ordinal);
        if (pages.Length == 0) throw new Exception("The document has no printable pages.");
        using (PrintDocument doc = new PrintDocument()) {
            doc.PrinterSettings.PrinterName = printer;
            if (!doc.PrinterSettings.IsValid) throw new Exception("The selected Windows printer is unavailable.");
            doc.DocumentName = title;
            doc.PrintController = new StandardPrintController();
            doc.PrinterSettings.Copies = (short)copies;
            doc.PrinterSettings.Duplex = Duplex.Simplex;
            doc.DefaultPageSettings.Color = doc.PrinterSettings.SupportsColor;
            doc.DefaultPageSettings.Landscape = landscape;
            doc.DefaultPageSettings.Margins = new Margins(0,0,0,0);
            if (!String.IsNullOrEmpty(output)) {
                doc.PrinterSettings.PrintToFile = true;
                doc.PrinterSettings.PrintFileName = output;
            }
            int index = 0;
            doc.PrintPage += delegate(object sender, PrintPageEventArgs e) {
                using (Image page = Image.FromFile(pages[index])) {
                    RectangleF area = e.PageSettings.PrintableArea;
                    float scale = Math.Min(area.Width / page.Width, area.Height / page.Height);
                    float width = page.Width * scale, height = page.Height * scale;
                    e.Graphics.DrawImage(page, (area.Width-width)/2, (area.Height-height)/2, width, height);
                }
                e.HasMorePages = ++index < pages.Length;
            };
            doc.Print();
        }
    }
}
'@
[IsoBurnerPrint]::Run($env:ISO_BURNER_PRINTER,$env:ISO_BURNER_TITLE,$env:ISO_BURNER_PAGES,
    [int]$env:ISO_BURNER_COPIES,($env:ISO_BURNER_LANDSCAPE -eq 'true'),$env:ISO_BURNER_PRINT_OUTPUT)
`

func printWindowsDocument(ctx context.Context, doc Document) error {
	dir, err := os.MkdirTemp("", "iso-burner-pages-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := renderWindowsDocument(ctx, doc, dir); err != nil {
		return err
	}
	_, err = queryWindowsPrinter(ctx, windowsPrintPages, "ISO_BURNER_PRINTER="+doc.Printer, "ISO_BURNER_TITLE="+doc.Title,
		"ISO_BURNER_PAGES="+dir, "ISO_BURNER_COPIES="+strconv.Itoa(doc.Copies), "ISO_BURNER_LANDSCAPE="+strconv.FormatBool(doc.Orientation == 4), "ISO_BURNER_PRINT_OUTPUT=")
	return err
}

func renderWindowsDocument(ctx context.Context, doc Document, dir string) error {
	file, err := os.Open(doc.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	header := make([]byte, 8)
	n, _ := io.ReadFull(file, header)
	header = header[:n]
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	format := doc.Format
	if format == "application/octet-stream" {
		switch {
		case bytes.HasPrefix(header, []byte("%PDF-")):
			format = "application/pdf"
		case bytes.Equal(header, []byte("UNIRAST\x00")):
			format = "image/urf"
		case bytes.HasPrefix(header, []byte{0xff, 0xd8}):
			format = "image/jpeg"
		case bytes.Equal(header, []byte("\x89PNG\r\n\x1a\n")):
			format = "image/png"
		default:
			return fmt.Errorf("unsupported document format")
		}
	}
	switch format {
	case "application/pdf":
		_, err = queryWindowsPrinter(ctx, windowsRenderPDF, "ISO_BURNER_DOCUMENT="+doc.Path, "ISO_BURNER_PAGES="+dir)
		return err
	case "image/urf":
		return decodeURF(file, func(page int, img image.Image) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			out, err := os.Create(filepath.Join(dir, fmt.Sprintf("%05d.png", page)))
			if err != nil {
				return err
			}
			err = png.Encode(out, img)
			closeErr := out.Close()
			if err != nil {
				return err
			}
			return closeErr
		})
	case "image/jpeg", "image/png":
		cfg, _, err := image.DecodeConfig(file)
		if err != nil {
			return err
		}
		if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 20000000 {
			return fmt.Errorf("image is too large")
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		img, _, err := image.Decode(file)
		if err != nil {
			return err
		}
		out, err := os.Create(filepath.Join(dir, "00000.png"))
		if err != nil {
			return err
		}
		err = png.Encode(out, img)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	return fmt.Errorf("unsupported document format %q", format)
}
