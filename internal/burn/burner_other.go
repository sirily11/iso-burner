//go:build !darwin && !linux && !windows

package burn

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/sirily11/iso-burner/internal/drive"
)

// System returns a burner that reports burning is unsupported here.
func System() Burner { return unsupported{} }

type unsupported struct{}

var errUnsupported = fmt.Errorf("burning discs is not supported on %s", runtime.GOOS)

func (unsupported) Burn(context.Context, drive.Drive, string, int64, BurnOptions) error {
	return errUnsupported
}

func (unsupported) OpenDisc(context.Context, drive.Drive) (io.ReadCloser, error) {
	return nil, errUnsupported
}

func (unsupported) Eject(context.Context, drive.Drive) error { return errUnsupported }
