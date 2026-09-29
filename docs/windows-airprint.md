# Native Windows AirPrint

The Windows build hosts AirPrint itself using Bonjour discovery and IPP on TCP
port 8631. It lists installed Windows printers, receives jobs in an in-memory
queue, and prints through the installed Windows driver. CUPS, WSL, Bonjour
executables, and third-party Air Printer software are not required.

After building, run the firewall setup once in an Administrator PowerShell:

```powershell
& .\scripts\setup-windows-airprint.ps1
```

The rules permit only this executable, its IPP/Bonjour ports, and devices on the
local subnet. Re-run setup if the executable is moved. To remove the rules:

```powershell
Remove-NetFirewallRule -Name IsoBurner-NativeAirPrint-IPP,IsoBurner-NativeAirPrint-mDNS
```

Start the app and select the printers to share:

```powershell
& .\bin\iso-bunner.exe --mode printer
```

On an iPhone or iPad on the same LAN, choose **Print**, then the printer named
**[printer name] (AirPrint)**. Keep iso-burner open while sharing. Exiting stops
the IPP server and Bonjour advertisements, cancels unfinished virtual jobs, and
removes their temporary documents. Windows printer settings and other AirPrint
software are not changed.

Supported documents: PDF, JPEG, PNG, and Apple Raster (8-bit grayscale or 24-bit
sRGB at 300 dpi). Windows' built-in PDF API renders PDF pages; the Windows driver
prints the rendered images. Jobs use the driver's configured paper size,
single-sided printing, portrait or landscape, and 1–99 copies. Job state is not
persisted across app restarts. A job completes when it is handed to the Windows
print system; subsequent physical-printer status is managed by Windows.

Limits: 128 MB per document, 100 PDF/raster pages, 20 million pixels per page,
and 64 unfinished jobs. Print-Job and Create-Job/Send-Document, job queries, and
virtual job cancellation are supported. Duplex and multiple documents per job
are not advertised.

Tests use a fake driver for IPP workflows and Microsoft Print to PDF for the
live Windows rendering/driver test, so they do not print physical pages:

```powershell
$env:ISO_BURNER_TEST_WINDOWS_PRINTER = '1'
go test ./internal/printer -v
```
