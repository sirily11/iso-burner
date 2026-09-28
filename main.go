// Command iso-burner collects settings for splitting a folder into
// size-limited ISO files via an interactive TUI, then generates them while
// showing per-ISO progress.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/settings"
	"github.com/sirily11/iso-burner/internal/tui"
)

func main() {
	var opts tui.Options
	var outputDir string
	flag.StringVar(&opts.Folder, "folder", "", "source folder to pre-fill")
	flag.StringVar(&opts.Pattern, "regex", "", "file-selection regex to pre-fill")
	flag.StringVar(&opts.ISOName, "name", "", "ISO name to pre-fill")
	flag.StringVar(&outputDir, "output", ".", "directory for generated ISO files")
	flag.Parse()
	if opts.Folder == "" && flag.NArg() > 0 {
		opts.Folder = flag.Arg(0)
	}
	absOutput, err := filepath.Abs(outputDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "output directory:", err)
		os.Exit(1)
	}
	opts.OutputDir = absOutput

	final, err := tea.NewProgram(tui.New(opts)).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	m := final.(tui.Model)
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
