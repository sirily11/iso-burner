// Package winps locates the PowerShell executable used to reach Windows
// APIs such as WMI and IMAPI2.
package winps

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Path returns the PowerShell to run. It prefers Windows PowerShell from PATH,
// then its fixed install location under %SystemRoot% (for when PATH has been
// trimmed), then PowerShell 7 (pwsh). If none is found it returns
// "powershell" so the resulting error still names the missing program.
func Path() string {
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	p := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		return p
	}
	return "powershell"
}
