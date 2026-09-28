package burn

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sirily11/iso-burner/internal/drive"
)

// System returns the burner for this operating system: hdiutil and drutil on
// macOS.
func System() Burner { return darwinBurner{} }

type darwinBurner struct{}

// hdiutilDevice maps a drutil drive index to the device path hdiutil burn
// expects. Both tools list drives in the same order.
func hdiutilDevice(ctx context.Context, id string) (string, error) {
	out, err := exec.CommandContext(ctx, "hdiutil", "burn", "-list").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("hdiutil burn -list: %w: %s", err, strings.TrimSpace(string(out)))
	}
	devices := parseHdiutilDevices(string(out))
	i, err := strconv.Atoi(id)
	if err != nil || i < 1 || i > len(devices) {
		return "", fmt.Errorf("drive %s is not listed by hdiutil burn -list", id)
	}
	return devices[i-1], nil
}

func (darwinBurner) Burn(ctx context.Context, d drive.Drive, iso string, size int64, opts BurnOptions) error {
	device, err := hdiutilDevice(ctx, d.ID)
	if err != nil {
		return err
	}
	// Verification is done by reading the disc back afterwards, which needs
	// the disc to stay in the drive.
	args := []string{"burn", iso, "-device", device, "-puppetstrings", "-noverifyburn", "-noeject"}
	if opts.Speed > SpeedMax {
		args = append(args, "-speed", strconv.Itoa(int(opts.Speed)))
	}
	cmd := exec.CommandContext(ctx, "hdiutil", args...)
	return runLines(cmd, func(line string) {
		if pct, ok := parsePuppetPercent(line); ok {
			opts.Progress(int64(pct / 100 * float64(size)))
		}
	})
}

func (darwinBurner) OpenDisc(ctx context.Context, d drive.Drive) (io.ReadCloser, error) {
	out, err := exec.CommandContext(ctx, "drutil", "-drive", d.ID, "status").Output()
	if err != nil {
		return nil, fmt.Errorf("drutil status: %w", err)
	}
	node, ok := parseDrutilDevNode(string(out))
	if !ok {
		return nil, fmt.Errorf("no disc found in drive %s", d.ID)
	}
	// The raw node (/dev/rdiskN) skips the buffer cache.
	return os.Open(strings.Replace(node, "/dev/disk", "/dev/rdisk", 1))
}

func (darwinBurner) Eject(ctx context.Context, d drive.Drive) error {
	return exec.CommandContext(ctx, "drutil", "-drive", d.ID, "eject").Run()
}
