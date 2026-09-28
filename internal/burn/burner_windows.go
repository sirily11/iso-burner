package burn

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode/utf16"

	"github.com/sirily11/iso-burner/internal/drive"
	"github.com/sirily11/iso-burner/internal/winps"
)

// System returns the burner for this operating system: the IMAPI2 COM API,
// driven through PowerShell, on Windows.
func System() Burner { return windowsBurner{} }

type windowsBurner struct{}

// findRecorder is PowerShell that sets $recorder to the IMAPI2 recorder
// mounted at the drive letter in $env:ISO_BURNER_DRIVE.
const findRecorder = `
$ErrorActionPreference = 'Stop'
$letter = $env:ISO_BURNER_DRIVE.TrimEnd('\').TrimEnd(':') + ':\'
$recorder = $null
foreach ($id in (New-Object -ComObject IMAPI2.MsftDiscMaster2)) {
	$r = New-Object -ComObject IMAPI2.MsftDiscRecorder2
	$r.InitializeDiscRecorder($id)
	if ($r.VolumePathNames -contains $letter) { $recorder = $r; break }
}
if (-not $recorder) { throw "no disc recorder at $letter" }
`

// burnScript writes $env:ISO_BURNER_ISO to the disc through a stream that
// prints "PROGRESS <bytes>" as IMAPI2 reads the image, since IMAPI2's own
// progress events cannot reach a blocked PowerShell pipeline.
const burnScript = `
# Progress records (such as Add-Type's) would be written to stderr as CLIXML
# and bury the real error message.
$ProgressPreference = 'SilentlyContinue'
Add-Type -TypeDefinition @'
using System;
using System.IO;
using System.Runtime.InteropServices;
using System.Runtime.InteropServices.ComTypes;

[ComVisible(true)]
public class IsoBurnerStream : IStream {
	private FileStream file;
	private long reported = -1;
	public IsoBurnerStream(string path) { file = new FileStream(path, FileMode.Open, FileAccess.Read, FileShare.Read, 1 << 20); }
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
	public void Stat(out STATSTG pstatstg, int grfStatFlag) {
		pstatstg = new STATSTG();
		pstatstg.type = 2;
		pstatstg.cbSize = file.Length;
	}
	public void Write(byte[] pv, int cb, IntPtr pcbWritten) { throw new NotSupportedException(); }
	public void SetSize(long libNewSize) { throw new NotSupportedException(); }
	public void CopyTo(IStream pstm, long cb, IntPtr pcbRead, IntPtr pcbWritten) { throw new NotSupportedException(); }
	public void Commit(int grfCommitFlags) { }
	public void Revert() { throw new NotSupportedException(); }
	public void LockRegion(long libOffset, long cb, int dwLockType) { throw new NotSupportedException(); }
	public void UnlockRegion(long libOffset, long cb, int dwLockType) { throw new NotSupportedException(); }
	public void Clone(out IStream ppstm) { throw new NotSupportedException(); }
	public void Close() { file.Dispose(); }
}
'@
try {
` + findRecorder + `
	$format = New-Object -ComObject IMAPI2.MsftDiscFormat2Data
	if (-not $format.IsRecorderSupported($recorder)) { throw "drive $letter cannot burn data discs" }
	$format.Recorder = $recorder
	# A freshly inserted disc takes a while to spin up, and until then IMAPI2
	# reports it as unsupported, so give the drive time to become ready.
	$deadline = (Get-Date).AddSeconds(60)
	while (-not $format.IsCurrentMediaSupported($recorder)) {
		if ((Get-Date) -gt $deadline) {
			$types = @('unknown media', 'CD-ROM', 'CD-R', 'CD-RW', 'DVD-ROM', 'DVD-RAM', 'DVD+R', 'DVD+RW',
				'DVD+R DL', 'DVD-R', 'DVD-RW', 'DVD-R DL', 'disk', 'DVD+RW DL', 'HD DVD-ROM', 'HD DVD-R',
				'HD DVD-RAM', 'BD-ROM', 'BD-R', 'BD-RE')
			try { $type = $types[[int]$format.CurrentPhysicalMediaType] } catch { $type = $null }
			if (-not $type) { throw "no disc in $letter, or the drive is not ready" }
			throw "the $type disc in $letter cannot be written (it may be finalized, read-only or unsupported)"
		}
		Start-Sleep -Seconds 2
	}
	if (-not $format.MediaHeuristicallyBlank) { throw "the disc in $letter is not blank" }
	$format.ClientName = 'iso-burner'
	$format.ForceMediaToBeClosed = $true
	$stream = New-Object IsoBurnerStream $env:ISO_BURNER_ISO
	try { $format.Write($stream) } finally { $stream.Close() }
} catch {
	[Console]::Error.WriteLine($_.Exception.Message)
	exit 1
}
`

const ejectScript = `
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

func (windowsBurner) Burn(ctx context.Context, d drive.Drive, iso string, size int64, progress func(int64)) error {
	cmd := powershell(ctx, burnScript, "ISO_BURNER_DRIVE="+d.ID, "ISO_BURNER_ISO="+iso)
	return runLines(cmd, func(line string) {
		if n, ok := parseWindowsProgress(line); ok {
			progress(n)
		}
	})
}

func (windowsBurner) OpenDisc(ctx context.Context, d drive.Drive) (io.ReadCloser, error) {
	return os.Open(`\\.\` + strings.TrimSuffix(strings.TrimSuffix(d.ID, `\`), ":") + ":")
}

func (windowsBurner) Eject(ctx context.Context, d drive.Drive) error {
	return runLines(powershell(ctx, ejectScript, "ISO_BURNER_DRIVE="+d.ID), func(string) {})
}
