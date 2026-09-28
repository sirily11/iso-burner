package burn

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/sirily11/iso-burner/internal/drive"
	"github.com/sirily11/iso-burner/internal/winps"
)

// System returns the burner for this operating system: the IMAPI2 COM API,
// driven through PowerShell, on Windows.
func System() Burner { return windowsBurner{} }

type windowsBurner struct{}

// prologue makes PowerShell stop on the first error and print UTF-8, so
// messages in a non-English Windows locale reach Go intact rather than in the
// console code page (e.g. GBK).
const prologue = `
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
`

// findRecorder is PowerShell that sets $recorder to the IMAPI2 recorder
// mounted at the drive letter in $env:ISO_BURNER_DRIVE.
const findRecorder = `
$letter = $env:ISO_BURNER_DRIVE.TrimEnd('\').TrimEnd(':') + ':\'
$recorder = $null
foreach ($id in (New-Object -ComObject IMAPI2.MsftDiscMaster2)) {
	$r = New-Object -ComObject IMAPI2.MsftDiscRecorder2
	$r.InitializeDiscRecorder($id)
	if ($r.VolumePathNames -contains $letter) { $recorder = $r; break }
}
if (-not $recorder) { throw "no disc recorder at $letter" }
`

// streamType is PowerShell that defines IsoBurnerStream, the stream IMAPI2
// reads the image through; it prints "PROGRESS <bytes>" as it goes, since
// IMAPI2's own progress events cannot reach a blocked PowerShell pipeline.
// ComTypes is aliased because .NET Framework also has an obsolete
// System.Runtime.InteropServices.STATSTG, which makes a bare STATSTG
// ambiguous.
const streamType = `
Add-Type -TypeDefinition @'
using System;
using System.IO;
using System.Runtime.InteropServices;
using ComTypes = System.Runtime.InteropServices.ComTypes;

[ComVisible(true)]
public class IsoBurnerStream : ComTypes.IStream {
	private FileStream file;
	private long reported = -1;
	// Share everything: FileShare.Read would fail whenever anything else
	// (an SMB client of the share, antivirus, the indexer) holds the image
	// with write access. Verification catches a change during the burn.
	public IsoBurnerStream(string path) { file = new FileStream(path, FileMode.Open, FileAccess.Read, FileShare.ReadWrite | FileShare.Delete, 1 << 20); }
	public void Read(byte[] pv, int cb, IntPtr pcbRead) {
		int total = 0;
		while (total < cb) {
			int n = file.Read(pv, total, cb - total);
			if (n <= 0) break;
			total += n;
		}
		if (pcbRead != IntPtr.Zero) Marshal.WriteInt32(pcbRead, total);
		long pos = file.Position;
		if (pos - reported >= (8L << 20) || pos == file.Length) {
			reported = pos;
			Console.Out.WriteLine("PROGRESS " + pos);
			Console.Out.Flush();
		}
	}
	public void Seek(long dlibMove, int dwOrigin, IntPtr plibNewPosition) {
		long pos = file.Seek(dlibMove, (SeekOrigin)dwOrigin);
		if (plibNewPosition != IntPtr.Zero) Marshal.WriteInt64(plibNewPosition, pos);
	}
	public void Stat(out ComTypes.STATSTG pstatstg, int grfStatFlag) {
		pstatstg = new ComTypes.STATSTG();
		pstatstg.type = 2;
		pstatstg.cbSize = file.Length;
	}
	public void Write(byte[] pv, int cb, IntPtr pcbWritten) { throw new NotSupportedException(); }
	public void SetSize(long libNewSize) { throw new NotSupportedException(); }
	public void CopyTo(ComTypes.IStream pstm, long cb, IntPtr pcbRead, IntPtr pcbWritten) { throw new NotSupportedException(); }
	public void Commit(int grfCommitFlags) { }
	public void Revert() { throw new NotSupportedException(); }
	public void LockRegion(long libOffset, long cb, int dwLockType) { throw new NotSupportedException(); }
	public void UnlockRegion(long libOffset, long cb, int dwLockType) { throw new NotSupportedException(); }
	public void Clone(out ComTypes.IStream ppstm) { throw new NotSupportedException(); }
	public void Close() { file.Dispose(); }
}
'@
`

// burnScript writes $env:ISO_BURNER_ISO to the disc. Add-Type runs inside
// the try so a compile error is printed as one plain line, not as CLIXML.
const burnScript = prologue + `
try {
` + streamType + findRecorder + `
	$format = New-Object -ComObject IMAPI2.MsftDiscFormat2Data
	if (-not $format.IsRecorderSupported($recorder)) { throw "drive $letter cannot burn data discs" }
	$format.Recorder = $recorder
	$types = @('unknown media', 'CD-ROM', 'CD-R', 'CD-RW', 'DVD-ROM', 'DVD-RAM', 'DVD+R', 'DVD+RW',
		'DVD+R DL', 'DVD-R', 'DVD-RW', 'DVD-R DL', 'disk', 'DVD+RW DL', 'HD DVD-ROM', 'HD DVD-R',
		'HD DVD-RAM', 'BD-ROM', 'BD-R', 'BD-RE')
	# A freshly inserted disc takes a while to spin up, and until then IMAPI2
	# reports it as unsupported, so give the drive time to become ready.
	$deadline = (Get-Date).AddSeconds(60)
	while (-not $format.IsCurrentMediaSupported($recorder)) {
		if ((Get-Date) -gt $deadline) {
			try { $type = $types[[int]$format.CurrentPhysicalMediaType] } catch { $type = $null }
			if (-not $type) { throw "no disc in $letter, or the drive is not ready" }
			throw "the $type disc in $letter cannot be written (it may be finalized, read-only or unsupported)"
		}
		Start-Sleep -Seconds 2
	}
	$type = $types[[int]$format.CurrentPhysicalMediaType]
	if (-not $format.MediaHeuristicallyBlank) { throw "the $type disc in $letter is not blank" }
	# IMAPI2's own "stream too large" error gives no sizes, so check first.
	$size = (Get-Item -LiteralPath $env:ISO_BURNER_ISO).Length
	$free = [int64]$format.FreeSectorsOnMedia * 2048
	if ($size -gt $free) {
		throw ("the ISO is {0:N2} GB ({1} bytes) but the {2} disc in {3} only holds {4:N2} GB ({5} bytes)" -f ($size / 1e9), $size, $type, $letter, ($free / 1e9), $free)
	}
	$format.ClientName = 'iso-burner'
	$format.ForceMediaToBeClosed = $true
	# IMAPI2 speeds are in sectors per second; 1x depends on the disc type.
	$perX = 675
	if ($type -like 'CD*') { $perX = 75 } elseif ($type -like 'BD*') { $perX = 2195 } elseif ($type -like 'HD DVD*') { $perX = 4568 }
	$speed = [int]$env:ISO_BURNER_SPEED
	# -1 (0xFFFFFFFF) asks for the fastest speed; the drive rounds any
	# other request to a speed it supports.
	$sectors = -1
	if ($speed -gt 0) { $sectors = $speed * $perX }
	try { $format.SetWriteSpeed($sectors, $false) } catch { Write-Output "SPEED could not set the write speed: $($_.Exception.Message)" }
	try {
		$supported = @($format.SupportedWriteSpeeds | ForEach-Object { '{0:0.#}x' -f ($_ / $perX) }) -join ', '
		Write-Output ('SPEED {0:0.#}x (drive supports {1})' -f ($format.CurrentWriteSpeed / $perX), $supported)
	} catch { }
	$stream = New-Object IsoBurnerStream $env:ISO_BURNER_ISO
	try { $format.Write($stream) } finally { $stream.Close() }
} catch {
	[Console]::Error.WriteLine($_.Exception.Message)
	exit 1
}
`

const ejectScript = prologue + `
try {
` + findRecorder + `
	$recorder.EjectMedia()
} catch {
	[Console]::Error.WriteLine($_.Exception.Message)
	exit 1
}
`

// powershell runs script, passed as UTF-16LE base64 so it needs no quoting.
func powershell(ctx context.Context, script string, env ...string) *exec.Cmd {
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(raw[2*i:], u)
	}
	cmd := exec.CommandContext(ctx, winps.Path(), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

func (windowsBurner) Burn(ctx context.Context, d drive.Drive, iso string, size int64, opts BurnOptions) error {
	cmd := powershell(ctx, burnScript, "ISO_BURNER_DRIVE="+d.ID, "ISO_BURNER_ISO="+iso,
		"ISO_BURNER_SPEED="+strconv.Itoa(int(opts.Speed)))
	return runLines(cmd, func(line string) {
		if n, ok := parseWindowsProgress(line); ok {
			opts.Progress(n)
		} else if speed, ok := parseWindowsSpeed(line); ok {
			opts.SpeedUsed(speed)
		}
	})
}

func (windowsBurner) OpenDisc(ctx context.Context, d drive.Drive) (io.ReadCloser, error) {
	return os.Open(`\\.\` + strings.TrimSuffix(strings.TrimSuffix(d.ID, `\`), ":") + ":")
}

func (windowsBurner) Eject(ctx context.Context, d drive.Drive) error {
	return runLines(powershell(ctx, ejectScript, "ISO_BURNER_DRIVE="+d.ID), func(string) {})
}
