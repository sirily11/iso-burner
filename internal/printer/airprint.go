package printer

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// cupsPort is where CUPS accepts print jobs for shared queues.
const cupsPort = "631"

// airprintFormats are the document formats advertised to iOS. PDF comes first
// so iOS sends PDF, which CUPS converts for any printer.
const airprintFormats = "application/pdf,image/urf,image/jpeg,image/png,image/pwg-raster"

// AdvertisedName is the Bonjour name iPhones and iPads show for p. It differs
// from the name CUPS advertises the shared queue under, which iOS ignores, so
// the two do not collide.
func AdvertisedName(p Printer) string { return p.Label() + " (AirPrint)" }

// airprintTXT is the Bonjour TXT record iOS needs to list p as an AirPrint
// printer: without URF and the _universal subtype iOS skips the printer, and
// macOS leaves both out when it shares a queue.
func airprintTXT(p Printer) []string {
	model := cmp.Or(p.Model, p.Label())
	return []string{
		"txtvers=1",
		"qtotal=1",
		"rp=printers/" + p.Name,
		"ty=" + model,
		"product=(" + model + ")",
		"note=" + p.Location,
		"priority=0",
		"pdl=" + airprintFormats,
		"URF=W8,SRGB24,CP1,RS300-600",
		"Transparent=T",
		"Binary=T",
		"kind=document",
		"air=none",
	}
}

// advertiseCommand is the Bonjour tool invocation that publishes p on goos.
func advertiseCommand(goos string, p Printer) (string, []string, error) {
	name := AdvertisedName(p)
	switch goos {
	case "darwin":
		return "dns-sd", append([]string{"-R", name, "_ipp._tcp,_universal", "local.", cupsPort}, airprintTXT(p)...), nil
	case "linux":
		return "avahi-publish", append([]string{"-s", "--sub", "_universal._sub._ipp._tcp", name, "_ipp._tcp", cupsPort},
			airprintTXT(p)...), nil
	}
	return "", nil, fmt.Errorf("advertising AirPrint printers is not supported on %s", goos)
}

func (c CUPS) Advertise(ctx context.Context, p Printer) error {
	name, args, err := advertiseCommand(cmp.Or(c.OS, runtime.GOOS), p)
	if err != nil {
		return err
	}
	serve := c.Serve
	if serve == nil {
		serve = serveCommand
	}
	err = serve(ctx, name, args...)
	if ctx.Err() != nil {
		return nil
	}
	if err == nil {
		err = errors.New("stopped unexpectedly")
	}
	return fmt.Errorf("AirPrint advertising: %w", err)
}

// serveCommand runs a long-lived command until it exits or ctx is cancelled,
// which kills it.
func serveCommand(ctx context.Context, name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s not found", name)
	}
	var out strings.Builder
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(out.String()); msg != "" {
			return fmt.Errorf("%s: %s", name, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
