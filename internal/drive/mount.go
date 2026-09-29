package drive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// ErrNotMounted means the drive has no disc, or the disc in it has not been
// mounted by the system yet.
var ErrNotMounted = errors.New("no mounted disc in the drive: insert a disc and wait for it to appear")

// MountPoint returns the folder where the system mounted the disc in d, so
// its files can be read like any other folder.
func MountPoint(ctx context.Context, d Drive) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.CommandContext(ctx, "drutil", "-drive", d.ID, "status").Output()
		if err != nil {
			return "", fmt.Errorf("drutil status: %w", err)
		}
		node, ok := parseDrutilDevNode(string(out))
		if !ok {
			return "", ErrNotMounted
		}
		mounts, err := exec.CommandContext(ctx, "mount").Output()
		if err != nil {
			return "", fmt.Errorf("mount: %w", err)
		}
		return mountedAt(parseMount(string(mounts)), node)
	case "windows":
		root := strings.TrimSuffix(strings.TrimSuffix(d.ID, `\`), ":") + `:\`
		if _, err := os.ReadDir(root); err != nil {
			return "", ErrNotMounted
		}
		return root, nil
	case "linux":
		b, err := os.ReadFile("/proc/mounts")
		if err != nil {
			return "", err
		}
		node := d.ID
		if real, err := filepath.EvalSymlinks(node); err == nil {
			node = real
		}
		return mountedAt(parseProcMounts(string(b)), node)
	}
	return "", fmt.Errorf("reading discs is not supported on %s", runtime.GOOS)
}

// mount is one mounted filesystem.
type mount struct{ device, dir string }

// mountedAt finds where the device node, or on macOS one of its slices
// (/dev/disk4s1 for /dev/disk4), is mounted.
func mountedAt(mounts []mount, node string) (string, error) {
	for _, exact := range []bool{true, false} {
		for _, m := range mounts {
			if m.device == node || !exact && isSlice(m.device, node) {
				return m.dir, nil
			}
		}
	}
	return "", ErrNotMounted
}

func isSlice(device, node string) bool {
	rest, ok := strings.CutPrefix(device, node+"s")
	if !ok || rest == "" {
		return false
	}
	_, err := strconv.Atoi(rest)
	return err == nil
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

// parseMount parses macOS `mount` output, whose lines look like:
//
//	/dev/disk4 on /Volumes/My Disc (cd9660, local, nodev, nosuid, read-only)
//
// Mount points may contain spaces, so the options are cut at the last " (".
func parseMount(out string) []mount {
	var mounts []mount
	for line := range strings.SplitSeq(out, "\n") {
		device, rest, ok := strings.Cut(line, " on ")
		if !ok {
			continue
		}
		if i := strings.LastIndex(rest, " ("); i >= 0 {
			rest = rest[:i]
		}
		mounts = append(mounts, mount{device: device, dir: rest})
	}
	return mounts
}

// parseProcMounts parses Linux /proc/mounts, where spaces and other special
// characters in paths are written as octal escapes like \040.
func parseProcMounts(out string) []mount {
	var mounts []mount
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		mounts = append(mounts, mount{device: unescapeMount(fields[0]), dir: unescapeMount(fields[1])})
	}
	return mounts
}

func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
