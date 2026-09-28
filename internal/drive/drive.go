// Package drive discovers the optical disc drives attached to this machine so
// ISO images can be burned to them.
package drive

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Drive is one optical disc drive.
type Drive struct {
	// ID addresses the drive when burning: the drutil drive index on macOS
	// ("1"), the drive letter on Windows ("E:") and the device path on Linux
	// ("/dev/sr0").
	ID     string
	Vendor string
	Model  string
	// Detail is extra platform information such as the bus or loaded media.
	Detail string
}

// Name is a human-readable label for the drive.
func (d Drive) Name() string {
	name := strings.TrimSpace(d.Vendor + " " + d.Model)
	if name == "" {
		return "Optical drive"
	}
	return name
}

// List returns the optical drives attached to this machine. An empty list with
// a nil error means the lookup worked but no drive is connected.
func List(ctx context.Context) ([]Drive, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.CommandContext(ctx, "drutil", "list").Output()
		if err != nil {
			return nil, fmt.Errorf("drutil list: %w", err)
		}
		return parseDrutil(string(out))
	case "windows":
		out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
			"ConvertTo-Json -Compress -InputObject @(Get-CimInstance Win32_CDROMDrive | "+
				"Select-Object Drive,Caption,Manufacturer,MediaLoaded)").Output()
		if err != nil {
			return nil, fmt.Errorf("query Win32_CDROMDrive: %w", err)
		}
		return parseWindows(out)
	case "linux":
		return listSysBlock("/sys/block")
	}
	return nil, fmt.Errorf("listing disc drives is not supported on %s", runtime.GOOS)
}

// parseDrutil parses `drutil list`, whose rows line up under a header like:
//
//	   Vendor   Product           Rev   Bus       SupportLevel
//	1  HL-DT-ST DVDRW  GS21N      SA15  SATA      Apple Shipping
//
// Vendor and product may contain spaces, so columns are cut at the header's
// offsets instead of split on whitespace.
func parseDrutil(out string) ([]Drive, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	var cols []int // start offsets of Vendor, Product, Rev, Bus, SupportLevel
	var drives []Drive
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if cols == nil {
			for _, h := range []string{"Vendor", "Product", "Rev", "Bus", "SupportLevel"} {
				i := strings.Index(line, h)
				if i < 0 {
					return nil, fmt.Errorf("unexpected drutil output: %q", line)
				}
				cols = append(cols, i)
			}
			continue
		}
		index, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if _, err := strconv.Atoi(index); err != nil {
			continue
		}
		field := func(i int) string {
			start := min(cols[i], len(line))
			end := len(line)
			if i+1 < len(cols) {
				end = min(cols[i+1], len(line))
			}
			return strings.TrimSpace(line[start:end])
		}
		detail := field(3)
		if level := field(4); level != "" {
			detail = strings.TrimSpace(detail + " · " + level)
		}
		drives = append(drives, Drive{ID: index, Vendor: field(0), Model: field(1), Detail: detail})
	}
	return drives, sc.Err()
}

// parseWindows parses the JSON produced by the Win32_CDROMDrive query.
// ConvertTo-Json emits a bare object for one drive and null for none.
func parseWindows(out []byte) ([]Drive, error) {
	type cdrom struct {
		Drive        string
		Caption      string
		Manufacturer string
		MediaLoaded  bool
	}
	out = []byte(strings.TrimSpace(string(out)))
	if len(out) == 0 || string(out) == "null" {
		return nil, nil
	}
	var rows []cdrom
	if out[0] == '{' {
		var one cdrom
		if err := json.Unmarshal(out, &one); err != nil {
			return nil, fmt.Errorf("parse drive list: %w", err)
		}
		rows = []cdrom{one}
	} else if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("parse drive list: %w", err)
	}
	drives := make([]Drive, 0, len(rows))
	for _, r := range rows {
		if r.Drive == "" {
			continue
		}
		detail := "no disc"
		if r.MediaLoaded {
			detail = "disc loaded"
		}
		vendor := r.Manufacturer
		if strings.HasPrefix(vendor, "(") { // "(Standard CD-ROM drives)"
			vendor = ""
		}
		drives = append(drives, Drive{ID: r.Drive, Vendor: vendor, Model: r.Caption, Detail: detail})
	}
	return drives, nil
}

// listSysBlock finds optical drives (sr*) under a Linux /sys/block directory.
func listSysBlock(root string) ([]Drive, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var drives []Drive
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "sr") {
			continue
		}
		read := func(name string) string {
			b, _ := os.ReadFile(filepath.Join(root, e.Name(), "device", name))
			return strings.TrimSpace(string(b))
		}
		drives = append(drives, Drive{ID: "/dev/" + e.Name(), Vendor: read("vendor"), Model: read("model")})
	}
	return drives, nil
}
