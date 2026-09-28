// Command iso-burner splits a folder into size-limited ISO files, or burns
// ISO files to disc with one or more drives, through an interactive TUI.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/settings"
	"github.com/sirily11/iso-burner/internal/store"
	"github.com/sirily11/iso-burner/internal/tui"
)

func main() {
	var opts tui.Options
	var outputDir, mode, dbPath string
	flag.StringVar(&mode, "mode", "", `"generate" or "burn"; asks when empty`)
	flag.StringVar(&opts.Folder, "folder", "", "source folder to pre-fill; in burn mode, where to browse for ISOs")
	flag.StringVar(&opts.Pattern, "regex", "", "file-selection regex to pre-fill")
	flag.StringVar(&opts.ISOName, "name", "", "ISO name to pre-fill")
	flag.StringVar(&outputDir, "output", ".", "directory for generated ISO files")
	flag.StringVar(&dbPath, "db", "", "SQLite database that records burn progress (default: in the user config directory)")
	flag.Parse()
	switch mode {
	case "":
	case "generate":
		opts.Mode = tui.ModeGenerate
	case "burn":
		opts.Mode = tui.ModeBurn
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q (want \"generate\" or \"burn\")\n", mode)
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

	if opts.Mode != tui.ModeGenerate {
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

	final, err := tea.NewProgram(tui.New(opts)).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if opts.Store != nil {
		opts.Store.Close()
	}

	m := final.(tui.Model)
	if m.Mode() == tui.ModeBurn {
		os.Exit(burnSummary(m, dbPath))
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

// burnSummary prints how burn mode ended and returns the exit code.
func burnSummary(m tui.Model, dbPath string) int {
	snap, stopped, ok := m.BurnResult()
	if !ok {
		fmt.Println("Cancelled.")
		return 130
	}
	switch {
	case snap.Err != nil:
		fmt.Fprintln(os.Stderr, "burning failed:", snap.Err)
		fmt.Printf("%d of %d disc(s) done. Run burn mode again to resume.\n", snap.Done, snap.Total)
		return 1
	case stopped || snap.Done < snap.Total:
		fmt.Printf("Stopped with %d of %d disc(s) done. Run burn mode again to resume (progress saved in %s).\n", snap.Done, snap.Total, dbPath)
		return 130
	}
	fmt.Printf("Burned %d disc(s).\n", snap.Total)
	return 0
}
