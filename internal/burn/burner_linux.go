package burn

import (
	"context"
	"io"
	"os"
	"os/exec"

	"github.com/sirily11/iso-burner/internal/drive"
)

// System returns the burner for this operating system: growisofs on Linux.
func System() Burner { return linuxBurner{} }

type linuxBurner struct{}

func (linuxBurner) Burn(ctx context.Context, d drive.Drive, iso string, size int64, progress func(int64)) error {
	cmd := exec.CommandContext(ctx, "growisofs", "-dvd-compat", "-Z", d.ID+"="+iso)
	return runLines(cmd, func(line string) {
		if n, ok := parseGrowisofs(line); ok {
			progress(n)
		}
	})
}

func (linuxBurner) OpenDisc(ctx context.Context, d drive.Drive) (io.ReadCloser, error) {
	return os.Open(d.ID)
}

func (linuxBurner) Eject(ctx context.Context, d drive.Drive) error {
	return exec.CommandContext(ctx, "eject", d.ID).Run()
}
