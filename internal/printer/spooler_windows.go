package printer

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"

	"github.com/sirily11/iso-burner/internal/winps"
)

// Publish on active physical adapters rather than VPN, VMware, or Hyper-V
// adapters whose addresses phones cannot reach.
func windowsLANInterfaces(ctx context.Context) ([]net.Interface, error) {
	out, err := queryWindowsPrinter(ctx, `ConvertTo-Json -Compress -InputObject @(Get-NetAdapter -Physical | Where-Object Status -eq 'Up' | ForEach-Object { [int]$_.ifIndex })`)
	if err != nil {
		return nil, err
	}
	var indices []int
	if err := json.Unmarshal(out, &indices); err != nil {
		return nil, err
	}
	var ifaces []net.Interface
	for _, index := range indices {
		iface, err := net.InterfaceByIndex(index)
		if err == nil && iface.Flags&net.FlagMulticast != 0 {
			ifaces = append(ifaces, *iface)
		}
	}
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("no active Ethernet or Wi-Fi adapter is available for AirPrint")
	}
	return ifaces, nil
}

const windowsPrinterList = `
Add-Type -AssemblyName System.Drawing
function Read-Paper($paper) {
    [pscustomobject]@{Name=$paper.PaperName;WindowsID=$paper.RawKind;
        Width=[int][Math]::Round($paper.Width*25.4);Height=[int][Math]::Round($paper.Height*25.4)}
}
$rows = @(Get-Printer | Sort-Object Name | ForEach-Object {
    $settings = New-Object System.Drawing.Printing.PrinterSettings
    $settings.PrinterName = $_.Name
    [pscustomobject]@{Name=$_.Name;DriverName=$_.DriverName;Location=$_.Location;State=[string]$_.PrinterStatus;
        DefaultPaper=(Read-Paper $settings.DefaultPageSettings.PaperSize);
        PaperSizes=@($settings.PaperSizes | ForEach-Object { Read-Paper $_ });Color=$settings.SupportsColor}
})
ConvertTo-Json -Compress -Depth 5 -InputObject $rows
`

const windowsPrinterQueue = `
$printer = Get-Printer | Where-Object { $_.Name -eq $env:ISO_BURNER_PRINTER }
if (-not $printer) { throw 'The selected printer is no longer installed.' }
$jobs = @(Get-PrintJob -PrinterName $env:ISO_BURNER_PRINTER | Sort-Object Position,ID |
    Select-Object ID,DocumentName,UserName,Size,@{Name='State';Expression={[string]$_.JobStatus}})
ConvertTo-Json -Compress -Depth 4 -InputObject @{Status=[string]$printer.PrinterStatus;Jobs=$jobs}
`

// All scripts are fixed; printer names travel in the environment rather than
// becoming PowerShell code. These queries never change Windows sharing.
func queryWindowsPrinter(ctx context.Context, script string, env ...string) ([]byte, error) {
	wrapped := `
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
try {
` + script + `
} catch {
    [Console]::Error.WriteLine($_.Exception.Message)
    exit 1
}
`
	units := utf16.Encode([]rune(wrapped))
	raw := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(raw[2*i:], u)
	}
	cmd := exec.CommandContext(ctx, winps.Path(), "-NoProfile", "-NonInteractive", "-EncodedCommand",
		base64.StdEncoding.EncodeToString(raw))
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if exit, ok := err.(*exec.ExitError); ok {
			if msg := strings.TrimSpace(string(exit.Stderr)); msg != "" {
				return nil, fmt.Errorf("Windows print queue: %s", msg)
			}
		}
		return nil, fmt.Errorf("Windows print queue: %w", err)
	}
	return out, nil
}

func listWindowsPrinters(ctx context.Context) ([]Printer, error) {
	out, err := queryWindowsPrinter(ctx, windowsPrinterList)
	if err != nil {
		return nil, err
	}
	return parseWindowsPrinterList(out)
}

func parseWindowsPrinterList(out []byte) ([]Printer, error) {
	var rows []struct {
		Name, DriverName, Location, State string
		DefaultPaper                      PaperSize
		PaperSizes                        []PaperSize
		Color                             bool
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("read Windows printer list: %w", err)
	}
	printers := make([]Printer, len(rows))
	for i, row := range rows {
		printers[i] = Printer{Name: row.Name, Info: row.Name, Model: row.DriverName,
			Location: row.Location, State: windowsPrinterState(row.State), DefaultPaper: row.DefaultPaper, PaperSizes: row.PaperSizes, Color: row.Color}
	}
	return printers, nil
}

func readWindowsQueue(ctx context.Context, name string) (Queue, error) {
	out, err := queryWindowsPrinter(ctx, windowsPrinterQueue, "ISO_BURNER_PRINTER="+name)
	if err != nil {
		return Queue{}, err
	}
	return parseWindowsPrinterQueue(out)
}

func parseWindowsPrinterQueue(out []byte) (Queue, error) {
	var row struct {
		Status string
		Jobs   []struct {
			ID                            int
			DocumentName, UserName, State string
			Size                          int64
		}
	}
	if err := json.Unmarshal(out, &row); err != nil {
		return Queue{}, fmt.Errorf("read Windows print jobs: %w", err)
	}
	q := Queue{Status: windowsPrinterState(row.Status)}
	waiting := 0
	for _, job := range row.Jobs {
		state := strings.ToLower(job.State)
		if strings.Contains(state, "printed") || strings.Contains(state, "completed") || strings.Contains(state, "deleted") {
			continue
		}
		rank := "active"
		if !strings.Contains(state, "printing") {
			waiting++
			rank = fmt.Sprintf("%d", waiting)
		}
		q.Jobs = append(q.Jobs, Job{ID: job.ID, Rank: rank, Owner: job.UserName, Title: job.DocumentName, Size: job.Size})
	}
	if _, printing := q.Printing(); printing && q.Status == "idle" {
		q.Status = "printing"
	}
	return q, nil
}

func windowsPrinterState(state string) string {
	if state == "" || state == "Normal" {
		return "idle"
	}
	return strings.ToLower(state)
}
