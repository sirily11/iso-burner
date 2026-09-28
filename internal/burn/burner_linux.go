package burn

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/sirily11/iso-burner/internal/drive"
)

// System returns the burner for this operating system: growisofs on Linux.
func System() Burner { return linuxBurner{} }

type linuxBurner struct{}

func (linuxBurner) Burn(ctx context.Context, d drive.Drive, iso string, size int64, opts BurnOptions) error {
	args := []string{"-dvd-compat"}
	if opts.Speed > SpeedMax {
		args = append(args, "-speed="+strconv.Itoa(int(opts.Speed)))
	}
	cmd := exec.CommandContext(ctx, "growisofs", append(args, "-Z", d.ID+"="+iso)...)
	return runLines(cmd, func(line string) {
		if n, ok := parseGrowisofs(line); ok {
			opts.Progress(n)
		} else if speed, ok := parseGrowisofsSpeed(line); ok {
			opts.SpeedUsed(speed)
		} else if stage, ok := parseGrowisofsStage(line); ok {
			opts.Stage(stage)
		}
	})
}

func (linuxBurner) OpenDisc(ctx context.Context, d drive.Drive) (io.ReadCloser, error) {
	return os.Open(d.ID)
}

func (linuxBurner) Eject(ctx context.Context, d drive.Drive) error {
	return exec.CommandContext(ctx, "eject", d.ID).Run()
}
