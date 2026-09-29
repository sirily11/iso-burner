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
prints the rendered images. AirPrint lists the driver's supported paper sizes,
including custom label forms, and uses the paper configured in Windows Printing
Preferences as the default. The selected form is passed to the Windows driver
for both `media` and `media-col` print requests. No A4 size is invented when the
driver does not report paper settings. Preferences are refreshed when clients
query capabilities (cached for five seconds). Jobs support
single-sided printing, portrait or landscape, and 1–99 copies. Job state is not
persisted across app restarts. A job completes when it is handed to the Windows
print system; subsequent physical-printer status is managed by Windows.

Limits: 128 MB per document, 100 PDF/raster pages, 20 million pixels per page,
and 64 unfinished jobs. Print-Job and Create-Job/Send-Document, job queries, and
virtual job cancellation are supported. Duplex and multiple documents per job
are not advertised.

After updating, close older copies of iso-burner before starting the new binary;
only one instance can own port 8631. Close and reopen the iPhone print sheet and
reselect the printer to refresh its Bonjour path and paper capabilities. If you
copied the executable elsewhere, pass that exact path to the firewall setup
script using `-Executable`.

Cached addresses from older builds with escaped printer names are also accepted.
If the log reports an obsolete queue name (for example `Label_Printer`) as
`client-error-not-found`, cancel that pending iOS job and select the currently
shared printer by its name before submitting again.

If printing fails, the app's log at `%APPDATA%\iso-burner\iso-burner.log` records
IPP operations/statuses, document receipt, and Windows rendering/driver errors.
Document contents are not logged.

Tests use a fake driver for IPP workflows and Microsoft Print to PDF for the
live Windows rendering/driver test, so they do not print physical pages:

```powershell
$env:ISO_BURNER_TEST_WINDOWS_PRINTER = '1'
go test ./internal/printer -v
```
