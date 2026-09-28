package burn

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStreamTypeCompiles runs the Add-Type block of the burn script, which
// Go cannot check, so a C# error fails CI instead of every burn.
func TestStreamTypeCompiles(t *testing.T) {
	script := prologue + `
try {
` + streamType + `
} catch {
	[Console]::Error.WriteLine($_.Exception.Message)
	exit 1
}
`
	if err := runLines(powershell(context.Background(), script), func(string) {}); err != nil {
		t.Fatalf("Add-Type failed: %v", err)
	}
}

// TestScriptsParse runs every script through the PowerShell parser, so a
// syntax error fails CI rather than a burn or eject.
func TestScriptsParse(t *testing.T) {
	const parse = prologue + `
$src = [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($env:ISO_BURNER_SCRIPT))
$tokens = $null; $errs = $null
[void][System.Management.Automation.Language.Parser]::ParseInput($src, [ref]$tokens, [ref]$errs)
if ($errs) {
	foreach ($e in $errs) { [Console]::Error.WriteLine("line $($e.Extent.StartLineNumber): $($e.Message)") }
	exit 1
}
`
	for name, script := range map[string]string{"burn": burnScript, "eject": ejectScript} {
		cmd := powershell(context.Background(), parse, "ISO_BURNER_SCRIPT="+base64.StdEncoding.EncodeToString([]byte(script)))
		if err := runLines(cmd, func(string) {}); err != nil {
			t.Errorf("%s script does not parse: %v", name, err)
		}
	}
}

// TestStreamReportsProgressAndStage reads an image through IsoBurnerStream
// the way IMAPI2 does and checks the lines the burner parses.
func TestStreamReportsProgressAndStage(t *testing.T) {
	const size = 20<<20 + 4096
	iso := filepath.Join(t.TempDir(), "image.iso")
	if err := os.WriteFile(iso, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	script := prologue + `
try {
` + streamType + `
	$stream = New-Object IsoBurnerStream $env:ISO_BURNER_ISO
	$buf = New-Object byte[] (1 -shl 20)
	# IMAPI2 may read past the end; the stream must not report twice.
	for ($i = 0; $i -lt 25; $i++) { $stream.Read($buf, $buf.Length, [IntPtr]::Zero) }
	$stream.Close()
} catch {
	[Console]::Error.WriteLine($_.Exception.Message)
	exit 1
}
`
	var progress []int64
	var stages []string
	err := runLines(powershell(context.Background(), script, "ISO_BURNER_ISO="+iso), func(line string) {
		if n, ok := parseWindowsProgress(line); ok {
			progress = append(progress, n)
		} else if s, ok := parseWindowsStage(line); ok {
			stages = append(stages, s)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) == 0 || progress[len(progress)-1] != size {
		t.Fatalf("progress = %v, want it to end at %d", progress, size)
	}
	if strings.Join(stages, "|") != "writing buffered data and closing the disc" {
		t.Fatalf("stages = %q, want one closing stage", stages)
	}
}
