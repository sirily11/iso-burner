// Package burn writes ISO images to blank discs, reads them back to check the
// burn, and runs a burn session across several drives at once.
package burn

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/sirily11/iso-burner/internal/drive"
)

// Speed is a requested write speed as a multiple of the disc type's 1x rate,
// e.g. 4 for 4x. SpeedMax asks for the fastest speed the drive supports.
type Speed int

const SpeedMax Speed = 0

// Speeds are the write speeds the user can choose from, slowest last.
var Speeds = []Speed{SpeedMax, 2, 4, 6, 8, 12, 16}

func (s Speed) String() string {
	if s <= SpeedMax {
		return "Max"
	}
	return fmt.Sprintf("%dx", int(s))
}

// BurnOptions are how to burn a disc and where to report on it.
type BurnOptions struct {
	// Speed is the requested write speed. Drives round it to a speed they
	// support.
	Speed Speed
	// Progress is called with the bytes written so far.
	Progress func(written int64)
	// SpeedUsed is called with the write speed the drive actually uses,
	// e.g. "4x", when the burner can tell.
	SpeedUsed func(speed string)
}

// Burner talks to the operating system's disc burning tools.
type Burner interface {
	// Burn writes the ISO image at iso (size bytes) to the blank disc in d.
	// The disc stays in the drive afterwards so it can be verified.
	Burn(ctx context.Context, d drive.Drive, iso string, size int64, opts BurnOptions) error
	// OpenDisc opens the raw contents of the disc in d for reading back.
	OpenDisc(ctx context.Context, d drive.Drive) (io.ReadCloser, error)
	// Eject ejects the disc in d.
	Eject(ctx context.Context, d drive.Drive) error
}

// MismatchError reports that a disc does not hold the same bytes as its ISO.
type MismatchError struct{ Offset int64 }

func (e *MismatchError) Error() string {
	return fmt.Sprintf("disc differs from the ISO at byte %d", e.Offset)
}

// verifyChunk is how much of the ISO and disc are compared at a time. It is a
// multiple of the 2048-byte optical sector so raw device reads stay aligned.
const verifyChunk = 1 << 20

// Verify reads disc back and checks that it starts with exactly the bytes of
// the ISO file at isoPath, so every file on the disc matches its source.
// progress is called with the bytes compared so far.
func Verify(ctx context.Context, isoPath string, disc io.Reader, progress func(checked int64)) error {
	f, err := os.Open(isoPath)
	if err != nil {
		return err
	}
	defer f.Close()
	want := make([]byte, verifyChunk)
	got := make([]byte, verifyChunk)
	var off int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := io.ReadFull(f, want)
		if n == 0 {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		if _, err := io.ReadFull(disc, got[:n]); err != nil {
			return fmt.Errorf("read disc at byte %d: %w", off, err)
		}
		if !bytes.Equal(want[:n], got[:n]) {
			for i := range n {
				if want[i] != got[i] {
					return &MismatchError{Offset: off + int64(i)}
				}
			}
		}
		off += int64(n)
		if progress != nil {
			progress(off)
		}
	}
}

// runLines runs cmd and calls line for every line it prints on stdout or
// stderr, splitting on carriage returns too since burn tools redraw progress
// in place. A failure includes the last lines of output, and more of them go
// to the debug log.
func runLines(cmd *exec.Cmd, line func(string)) error {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	var tail []string
	sc := bufio.NewScanner(out)
	sc.Split(scanLines)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" {
			continue
		}
		tail = append(tail, l)
		if len(tail) > 50 {
			tail = tail[1:]
		}
		line(l)
	}
	if err := cmd.Wait(); err != nil {
		slog.Error("command failed", "cmd", cmd.Path, "err", err, "output", strings.Join(tail, "\n"))
		if len(tail) > 0 {
			return fmt.Errorf("%w: %s", err, strings.Join(tail[max(0, len(tail)-3):], " · "))
		}
		return err
	}
	return nil
}

// scanLines is bufio.ScanLines that also breaks on a lone '\r'.
func scanLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
