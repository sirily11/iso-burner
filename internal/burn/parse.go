package burn

import (
	"regexp"
	"strconv"
	"strings"
)

// parsePuppetPercent reads a `hdiutil -puppetstrings` progress line such as
// "PERCENT:12.500000". Negative percentages mean "indeterminate".
func parsePuppetPercent(line string) (float64, bool) {
	v, ok := strings.CutPrefix(line, "PERCENT:")
	if !ok {
		return 0, false
	}
	pct, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || pct < 0 {
		return 0, false
	}
	return min(pct, 100), true
}

var growisofsProgress = regexp.MustCompile(`^\s*(\d+)/(\d+)\s*\(\s*[\d.]+%\)`)

// parseGrowisofs reads a growisofs progress line such as
// "  52068352/4700372992 ( 1.1%) @2.2x, remaining 5:27 RBU 100.0% UBU  99.8%"
// and returns the bytes written.
func parseGrowisofs(line string) (int64, bool) {
	m := growisofsProgress.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	return n, err == nil
}

// parseWindowsProgress reads a "PROGRESS <bytes>" line printed by the
// PowerShell burn script.
func parseWindowsProgress(line string) (int64, bool) {
	v, ok := strings.CutPrefix(line, "PROGRESS ")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n, err == nil
}

var drutilDevNode = regexp.MustCompile(`Name:\s*(/dev/disk\d+)`)

// parseDrutilDevNode finds the device node of the inserted disc in
// `drutil status` output, e.g. "/dev/disk4".
func parseDrutilDevNode(out string) (string, bool) {
	m := drutilDevNode.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// parseHdiutilDevices lists the devices printed by `hdiutil burn -list`, one
// IOService path per drive.
func parseHdiutilDevices(out string) []string {
	var devices []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "IO") {
			devices = append(devices, line)
		}
	}
	return devices
}
