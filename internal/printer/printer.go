// Package printer shares local printers through AirPrint. Windows uses a
// native IPP server and virtual queues; Unix systems use CUPS.
package printer

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// PaperSize is a form reported by the printer driver. Dimensions are in
// hundredths of a millimetre, as used by IPP.
type PaperSize struct {
	Name          string
	Width, Height int
	WindowsID     int // driver form ID; preserve it when selecting label stock
}

// Printer is one local print queue.
type Printer struct {
	// Name is the CUPS queue name, e.g. "HP_LaserJet_MFP_M141w".
	Name         string
	Info         string
	Location     string
	Model        string
	DefaultPaper PaperSize
	PaperSizes   []PaperSize
	Color        bool
	// Shared is whether this service shares the queue on the network.
	Shared bool
	// State is "idle", "printing" or "disabled".
	State string
}

// Label is a human-readable name for the printer.
func (p Printer) Label() string { return cmp.Or(p.Info, p.Name) }

// Job is one print job waiting in, or printing from, a queue.
type Job struct {
	ID int
	// Rank is "active" for the job printing now, else its place in line
	// such as "1st".
	Rank  string
	Owner string
	Title string
	Size  int64
}

// Printing reports whether the job is the one being printed.
func (j Job) Printing() bool { return j.Rank == "active" }

// Queue is a printer's status and jobs.
type Queue struct {
	// Status is what CUPS says about the printer, e.g. "ready and printing".
	Status string
	Jobs   []Job
}

// Printing returns the job being printed, if any.
func (q Queue) Printing() (Job, bool) {
	for _, j := range q.Jobs {
		if j.Printing() {
			return j, true
		}
	}
	return Job{}, false
}

// Waiting counts the jobs queued behind the one printing.
func (q Queue) Waiting() int {
	n := 0
	for _, j := range q.Jobs {
		if !j.Printing() {
			n++
		}
	}
	return n
}

// Service lists printers, changes which are shared and reads their queues.
type Service interface {
	List(ctx context.Context) ([]Printer, error)
	// Share shares the named printers, turning on printer sharing for the
	// whole machine if needed.
	Share(ctx context.Context, names []string) error
	// Unshare stops sharing the named printers.
	Unshare(ctx context.Context, names []string) error
	Queue(ctx context.Context, name string) (Queue, error)
	// Advertise publishes a shared printer as an AirPrint printer until ctx
	// is cancelled or advertising fails.
	Advertise(ctx context.Context, p Printer) error
}

// Runner runs a CUPS command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// CUPS is a Service backed by the CUPS command-line tools.
type CUPS struct {
	Run Runner
	// Serve runs a command until it exits or ctx is cancelled; nil runs it
	// on this machine.
	Serve func(ctx context.Context, name string, args ...string) error
	// OS picks the Bonjour tool; empty uses runtime.GOOS.
	OS string
}

// System returns the Service for this machine.
func System() Service { return systemService() }

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", fmt.Errorf("printer sharing needs CUPS, which was not found on %s (%s)", runtime.GOOS, name)
	}
	// Stdin stays empty so a password prompt fails instead of hanging.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return string(out), fmt.Errorf("%s: %s", name, msg)
		}
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// List returns the local print queues. Printers CUPS found on the network but
// has no queue for are left out, since only local queues can be shared.
func (c CUPS) List(ctx context.Context) ([]Printer, error) {
	out, err := c.Run(ctx, "lpstat", "-p")
	if err != nil {
		if strings.Contains(out, "No destinations added") {
			return nil, nil
		}
		return nil, err
	}
	printers := parseLpstat(out)
	for i, p := range printers {
		opts, err := c.Run(ctx, "lpoptions", "-p", p.Name)
		if err != nil {
			return nil, err
		}
		o := parseOptions(opts)
		printers[i].Info = o["printer-info"]
		printers[i].Location = o["printer-location"]
		printers[i].Model = o["printer-make-and-model"]
		printers[i].Shared = o["printer-is-shared"] == "true"
	}
	return printers, nil
}

func (c CUPS) Share(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	if _, err := c.Run(ctx, "cupsctl", "--share-printers"); err != nil {
		return permissionHint(err)
	}
	return c.setShared(ctx, names, true)
}

func (c CUPS) Unshare(ctx context.Context, names []string) error {
	return c.setShared(ctx, names, false)
}

func (c CUPS) setShared(ctx context.Context, names []string, shared bool) error {
	for _, name := range names {
		if _, err := c.Run(ctx, "lpadmin", "-p", name, "-o", "printer-is-shared="+strconv.FormatBool(shared)); err != nil {
			return permissionHint(err)
		}
	}
	return nil
}

// permissionHint suggests sudo when CUPS refused a change for lack of rights.
func permissionHint(err error) error {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "forbidden") || strings.Contains(msg, "not authorized") || strings.Contains(msg, "password") {
		return fmt.Errorf("%w (run iso-burner as an administrator, e.g. with sudo)", err)
	}
	return err
}

func (c CUPS) Queue(ctx context.Context, name string) (Queue, error) {
	out, err := c.Run(ctx, "lpq", "-P", name)
	if err != nil {
		return Queue{}, err
	}
	return parseLpq(name, out)
}

// parseLpstat parses `lpstat -p`, whose lines look like:
//
//	printer HP_LaserJet is idle.  enabled since Thu Sep 10 14:16:10 2026
//	printer Brother now printing Brother-12.  enabled since …
//	printer Canon disabled since Thu Sep 10 14:16:10 2026 -
//		reason for the printer being disabled
func parseLpstat(out string) []Printer {
	var printers []Printer
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "printer" || line[0] != 'p' {
			continue
		}
		p := Printer{Name: f[1], State: "idle"}
		switch f[2] {
		case "disabled":
			p.State = "disabled"
		case "now":
			p.State = "printing"
		}
		printers = append(printers, p)
	}
	return printers
}

// parseOptions parses `lpoptions -p NAME`: space-separated key=value pairs
// where values with spaces are single-quoted. CUPS does not always escape
// quotes inside values, so a quote only closes a value when a space or the
// end of the line follows it.
func parseOptions(out string) map[string]string {
	opts := map[string]string{}
	s := strings.TrimSpace(out)
	for len(s) > 0 {
		end := strings.IndexByte(s, ' ')
		eq := strings.IndexByte(s, '=')
		if eq < 0 || (end >= 0 && end < eq) {
			// A key without a value, e.g. "printer-location".
			if end < 0 {
				opts[s] = ""
				break
			}
			opts[s[:end]] = ""
			s = strings.TrimLeft(s[end:], " ")
			continue
		}
		key, rest := s[:eq], s[eq+1:]
		var val strings.Builder
		i := 0
		if strings.HasPrefix(rest, "'") {
			for i = 1; i < len(rest); i++ {
				ch := rest[i]
				if ch == '\\' && i+1 < len(rest) {
					i++
					val.WriteByte(rest[i])
					continue
				}
				if ch == '\'' && (i+1 == len(rest) || rest[i+1] == ' ') {
					i++
					break
				}
				val.WriteByte(ch)
			}
		} else {
			for ; i < len(rest) && rest[i] != ' '; i++ {
				ch := rest[i]
				if ch == '\\' && i+1 < len(rest) {
					i++
					ch = rest[i]
				}
				val.WriteByte(ch)
			}
		}
		opts[key] = val.String()
		s = strings.TrimLeft(rest[i:], " ")
	}
	return opts
}

// parseLpq parses `lpq -P NAME`:
//
//	HP_LaserJet is ready and printing
//	Rank    Owner   Job     File(s)                         Total Size
//	active  alice   12      report.pdf                      12288 bytes
//	1st     bob     13      Photo 1.jpg                     1024 bytes
//
// or "no entries" instead of the table. Titles may contain spaces, so they
// are whatever lies between the job ID and the size.
func parseLpq(name, out string) (Queue, error) {
	var q Queue
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "", trimmed == "no entries", strings.HasPrefix(trimmed, "Rank "):
			continue
		case strings.HasPrefix(trimmed, name+" is "):
			q.Status = strings.TrimPrefix(trimmed, name+" is ")
			continue
		}
		f := strings.Fields(trimmed)
		if len(f) < 5 || f[len(f)-1] != "bytes" {
			continue
		}
		id, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		size, _ := strconv.ParseInt(f[len(f)-2], 10, 64)
		q.Jobs = append(q.Jobs, Job{ID: id, Rank: f[0], Owner: f[1],
			Title: strings.Join(f[3:len(f)-2], " "), Size: size})
	}
	if q.Status == "" && len(q.Jobs) == 0 {
		return q, errors.New("unexpected lpq output: " + strings.TrimSpace(out))
	}
	return q, nil
}
