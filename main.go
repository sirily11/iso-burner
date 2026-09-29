// Command iso-burner splits a folder into size-limited ISO files, or burns
// ISO files to disc with one or more drives, through an interactive TUI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/auth"
	"github.com/sirily11/iso-burner/internal/config"
	"github.com/sirily11/iso-burner/internal/logfile"
	"github.com/sirily11/iso-burner/internal/recent"
	"github.com/sirily11/iso-burner/internal/remote"
	"github.com/sirily11/iso-burner/internal/settings"
	"github.com/sirily11/iso-burner/internal/store"
	"github.com/sirily11/iso-burner/internal/tui"
)

func main() {
	var opts tui.Options
	var outputDir, mode, dbPath, serverURL string
	flag.StringVar(&mode, "mode", "", `"generate", "burn" or "printer"; asks when empty`)
	flag.StringVar(&opts.Folder, "folder", "", "source folder to pre-fill; in burn mode, where to browse for ISOs")
	flag.StringVar(&opts.Pattern, "regex", "", "file-selection regex to pre-fill")
	flag.StringVar(&opts.ISOName, "name", "", "ISO name to pre-fill")
	flag.StringVar(&opts.StartIndex, "start", "", "number of the first ISO to pre-fill (default 1)")
	flag.StringVar(&outputDir, "output", ".", "directory for generated ISO files")
	flag.StringVar(&dbPath, "db", "", "SQLite database that records burn progress (default: in the user config directory)")
	flag.StringVar(&serverURL, "server", config.RxStorageURL, `rxstorage server that progress is synced to while signed in; "" disables syncing`)
	flag.Parse()
	switch mode {
	case "":
	case "generate":
		opts.Mode = tui.ModeGenerate
	case "burn":
		opts.Mode = tui.ModeBurn
	case "printer":
		opts.Mode = tui.ModePrinter
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q (want \"generate\", \"burn\" or \"printer\")\n", mode)
		os.Exit(2)
	}
	if opts.Folder == "" && flag.NArg() > 0 {
		opts.Folder = flag.Arg(0)
	}
	absOutput, err := filepath.Abs(outputDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "output directory:", err)
		os.Exit(1)
	}
	opts.OutputDir = absOutput

	if path, err := logfile.DefaultPath(); err != nil {
		fmt.Fprintln(os.Stderr, "debug log:", err)
	} else if err := logfile.Open(path); err != nil {
		fmt.Fprintln(os.Stderr, "debug log:", err)
	} else {
		slog.Info("started", "args", os.Args[1:])
	}

	if path, err := recent.DefaultPath(); err != nil {
		fmt.Fprintln(os.Stderr, "recent selections:", err)
	} else if opts.Recent, err = recent.Load(path); err != nil {
		// A damaged file only loses the pre-filled values; it is rewritten
		// with the next selection.
		fmt.Fprintln(os.Stderr, "recent selections:", err)
		opts.RecentPath = path
	} else {
		opts.RecentPath = path
	}

	if opts.Mode != tui.ModeGenerate && opts.Mode != tui.ModePrinter {
		if dbPath == "" {
			if dbPath, err = store.DefaultPath(); err != nil {
				fmt.Fprintln(os.Stderr, "burn database:", err)
				os.Exit(1)
			}
		}
		st, err := store.Open(dbPath)
		if err != nil {
			// Generating still works; burn mode reports the missing database.
			fmt.Fprintln(os.Stderr, "burn database:", err)
		} else {
			opts.Store, opts.DBPath = st, dbPath
		}
	}

	if svc, err := auth.New(); err != nil {
		fmt.Fprintln(os.Stderr, "sign-in:", err)
	} else {
		opts.Auth = svc
		if serverURL != "" {
			opts.Sync = remote.NewClient(serverURL, svc)
		}
	}

	final, err := tea.NewProgram(tui.New(opts)).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if opts.Store != nil {
		opts.Store.Close()
	}

	m := final.(tui.Model)
	if err := m.CloseSync(5 * time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "rxstorage sync:", err)
	}
	m.StopAdvertising()
	if m.Mode() == tui.ModeBurn {
		os.Exit(burnSummary(m, dbPath))
	}
	if m.Mode() == tui.ModePrinter {
		if shared := m.SharedPrinters(); len(shared) > 0 {
			if m.TemporaryPrinterSharing() {
				fmt.Printf("Stopped AirPrint sharing: %s\nRun iso-burner again to share these printers.\n", strings.Join(shared, ", "))
			} else {
				fmt.Printf("Stopped advertising to iPhones and iPads: %s\nThe printers stay shared with other computers; run iso-burner again to use AirPrint.\n", strings.Join(shared, ", "))
			}
		}
		os.Exit(0)
	}
	if m.Mode() == tui.ModeUpload {
		os.Exit(uploadSummary(m))
	}
	cfg, chunks := m.Result()
	if cfg == nil {
		fmt.Println("Cancelled.")
		os.Exit(130)
	}
	if err := m.GenerateErr(); err != nil {
		fmt.Fprintln(os.Stderr, "generation failed:", err)
		os.Exit(1)
	}
	fmt.Printf("Created %d ISO file(s):\n", len(chunks))
	for _, c := range chunks {
		fmt.Printf("  %s  %d entries, %s of data\n", filepath.Join(absOutput, c.Name), len(c.Pieces), settings.FormatBytes(c.Size))
	}
}

// uploadSummary prints how upload mode ended and returns the exit code.
func uploadSummary(m tui.Model) int {
	done, failed, total, err, ok := m.UploadResult()
	if !ok {
		fmt.Println("Cancelled.")
		return 130
	}
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "upload failed:", err)
		fmt.Printf("%d of %d file(s) uploaded.\n", done, total)
		return 1
	case failed > 0:
		fmt.Fprintf(os.Stderr, "%d of %d file(s) failed to upload (see the log for details).\n", failed, total)
		return 1
	}
	fmt.Printf("Uploaded %d file(s).\n", done)
	return 0
}

// burnSummary prints how burn mode ended and returns the exit code.
func burnSummary(m tui.Model, dbPath string) int {
	if result, ok := m.VerificationResult(); ok {
		switch {
		case errors.Is(result.Err, context.Canceled):
			fmt.Println("Verification stopped; completeness has not been confirmed.")
			return 130
		case result.Err != nil:
			fmt.Fprintln(os.Stderr, "disc verification failed:", result.Err)
			return 1
		default:
			fmt.Println("Disc is complete and matches the ISO byte for byte.")
			return 0
		}
	}
	snap, stopped, ok := m.BurnResult()
	if !ok {
		fmt.Println("Cancelled.")
		return 130
	}
	counts := fmt.Sprintf("%d of %d disc(s) done", snap.Done, snap.Total)
	if snap.Skipped > 0 {
		counts += fmt.Sprintf("; %d skipped", snap.Skipped)
	}
	switch {
	case snap.Err != nil:
		fmt.Fprintln(os.Stderr, "burning failed:", snap.Err)
		fmt.Printf("%s. Run burn mode again to resume.\n", counts)
		return 1
	case stopped || snap.Done+snap.Skipped < snap.Total:
		fmt.Printf("Stopped with %s. Run burn mode again to resume (progress saved in %s).\n", counts, dbPath)
		return 130
	case snap.Skipped > 0:
		fmt.Printf("Finished: %d disc(s) burned, %d skipped.\n", snap.Done, snap.Skipped)
		return 0
	}
	fmt.Printf("Burned %d disc(s).\n", snap.Total)
	return 0
}
