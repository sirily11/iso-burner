param([string[]]$DriveLetters = @('E', 'H'))

# Calls the same Windows API used by ISO Burner.
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Text;

public static class DiscLabelProbe {
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern bool GetVolumeInformation(
        string root, StringBuilder label, int labelSize,
        IntPtr serial, IntPtr maxComponentLength, IntPtr flags,
        IntPtr fileSystemName, int fileSystemNameSize);
}
'@

foreach ($letter in $DriveLetters) {
    $drive = $letter.Trim().TrimEnd([char]':').ToUpperInvariant()
    if ($drive -notmatch '^[A-Z]$') { throw "Invalid drive letter: $letter" }
    $root = "${drive}:\"
    $label = [Text.StringBuilder]::new(256)
    $ok = [DiscLabelProbe]::GetVolumeInformation(
        $root, $label, $label.Capacity,
        [IntPtr]::Zero, [IntPtr]::Zero, [IntPtr]::Zero,
        [IntPtr]::Zero, 0)
    if ($ok) {
        $name = if ($label.Length) { $label.ToString() } else { '<empty>' }
        Write-Output "${drive}: label = $name"
    } else {
        $code = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
        $message = [ComponentModel.Win32Exception]::new($code).Message
        Write-Output "${drive}: error $code ($message)"
    }
}
