package printer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/grandcat/zeroconf"
)

func nativeTestServer(t *testing.T, print func(context.Context, Document) error) (*Native, string) {
	t.Helper()
	n := &Native{ListenAddr: "127.0.0.1:0", ListPrinters: func(context.Context) ([]Printer, error) {
		return []Printer{{Name: "Office's 彩色 printer", Model: "Native driver", MediaWidth: 10000, MediaHeight: 15000}}, nil
	}, Print: print}
	if err := n.Share(context.Background(), []string{"Office's 彩色 printer"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	return n, "http://" + n.listener.Addr().String() + printerPath("Office's 彩色 printer")
}

func TestNativeBonjourIntegration(t *testing.T) {
	if os.Getenv("ISO_BURNER_TEST_WINDOWS_PRINTER") == "" {
		t.Skip("opt in to LAN discovery integration")
	}
	n, _ := nativeTestServer(t, func(context.Context, Document) error { return nil })
	if actual, ok := systemService().(*Native); ok {
		n.Interfaces = actual.Interfaces
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.Advertise(ctx, Printer{Name: "Office's 彩色 printer"}) }()
	var ifaces []net.Interface
	if n.Interfaces != nil {
		var err error
		ifaces, err = n.Interfaces(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	resolver, err := zeroconf.NewResolver(zeroconf.SelectIPTraffic(zeroconf.IPv4), zeroconf.SelectIfaces(ifaces))
	if err != nil {
		t.Fatal(err)
	}
	entries := make(chan *zeroconf.ServiceEntry, 8)
	if err := resolver.Browse(ctx, "_ipp._tcp,_universal", "local.", entries); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			t.Fatal("advertising stopped before discovery")
		case entry, ok := <-entries:
			if !ok {
				t.Fatal("AirPrint subtype was not discovered")
			}
			// DNS decoders may return escaped spaces/UTF-8 octets in instance
			// labels. The unique test port identifies our advertised service.
			if entry.Port == n.listener.Addr().(*net.TCPAddr).Port {
				if entry.Port != n.listener.Addr().(*net.TCPAddr).Port || len(entry.AddrIPv4) == 0 {
					t.Fatalf("invalid Bonjour endpoint: %+v", entry)
				}
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("Bonjour shutdown hung")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("native AirPrint service was not discovered on the LAN")
		}
	}
}

func sendIPP(t *testing.T, endpoint string, op goipp.Code, attrs, job goipp.Attributes, doc []byte) goipp.Message {
	t.Helper()
	req := goipp.Message{Version: goipp.DefaultVersion, Code: op, RequestID: 42, Operation: attrs, Job: job}
	var body bytes.Buffer
	if err := req.Encode(&body); err != nil {
		t.Fatal(err)
	}
	body.Write(doc)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(endpoint, "application/ipp", &body)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		text, _ := io.ReadAll(response.Body)
		t.Fatalf("HTTP %d: %s", response.StatusCode, text)
	}
	var result goipp.Message
	if err := result.Decode(response.Body); err != nil {
		t.Fatal(err)
	}
	if result.RequestID != 42 {
		t.Fatal("IPP request ID was not preserved")
	}
	return result
}

func TestNativeIPPQueueAndCancellation(t *testing.T) {
	received := make(chan Document, 2)
	n, endpoint := nativeTestServer(t, func(ctx context.Context, doc Document) error { received <- doc; <-ctx.Done(); return ctx.Err() })
	capabilities := sendIPP(t, endpoint, 11, nil, nil, nil)
	if capabilities.Code != 0 || ippString(capabilities.Printer, "printer-make-and-model", "") != "Native driver" || !strings.Contains(ippString(capabilities.Printer, "media-default", ""), "100x150mm") {
		t.Fatalf("bad capabilities: %+v", capabilities)
	}
	if ippString(capabilities.Printer, "printer-uuid", "") != "urn:uuid:"+nativePrinterUUID("Office's 彩色 printer") || ippInt(capabilities.Printer, "printer-up-time", 0) < 1 {
		t.Fatal("missing stable printer identity or uptime")
	}
	attrs := goipp.Attributes{ippAttr("document-format", goipp.TagMimeType, goipp.String("application/pdf")), ippAttr("job-name", goipp.TagName, goipp.String("My document"))}
	options := goipp.Attributes{ippAttr("copies", goipp.TagInteger, goipp.Integer(2))}
	res := sendIPP(t, endpoint, 2, attrs, options, []byte("%PDF-native test"))
	if res.Code != 0 {
		t.Fatalf("Print-Job status %#x", res.Code)
	}
	id := ippInt(res.Job, "job-id", 0)
	var doc Document
	select {
	case doc = <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("virtual queue did not reach the driver")
	}
	data, err := os.ReadFile(doc.Path)
	if err != nil || string(data) != "%PDF-native test" || doc.Copies != 2 || doc.Printer != "Office's 彩色 printer" || doc.ID != id {
		t.Fatalf("incorrect driver document: %+v / %v", doc, err)
	}
	jobAttr := goipp.Attributes{ippAttr("job-id", goipp.TagInteger, goipp.Integer(id))}
	state := sendIPP(t, endpoint+"/jobs/"+fmt.Sprint(id), 9, jobAttr, nil, nil)
	if ippInt(state.Job, "job-state", 0) != 5 {
		t.Fatal("processing job state was not reported")
	}
	all := sendIPP(t, endpoint, 10, nil, nil, nil)
	if len(all.Groups) != 2 {
		t.Fatalf("Get-Jobs groups = %d", len(all.Groups))
	}
	if sendIPP(t, endpoint, 8, jobAttr, nil, nil).Code != 0 {
		t.Fatal("Cancel-Job failed")
	}
	for i := 0; i < 30; i++ {
		if _, err := os.Stat(doc.Path); os.IsNotExist(err) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(doc.Path); !os.IsNotExist(err) {
		t.Fatal("canceled document was not removed")
	}
	state = sendIPP(t, endpoint, 9, jobAttr, nil, nil)
	if ippInt(state.Job, "job-state", 0) != 7 {
		t.Fatal("job did not stay canceled")
	}
	q, err := n.Queue(context.Background(), doc.Printer)
	if err != nil || len(q.Jobs) != 0 {
		t.Fatalf("canceled job remains queued: %+v,%v", q, err)
	}
}

func TestNativeCreateJobSendDocumentAndUnshare(t *testing.T) {
	received := make(chan Document, 1)
	n, endpoint := nativeTestServer(t, func(ctx context.Context, doc Document) error { received <- doc; return nil })
	create := sendIPP(t, endpoint, 5, nil, nil, nil)
	if create.Code != 0 || ippInt(create.Job, "job-state", 0) != 4 {
		t.Fatal("Create-Job did not hold an empty job")
	}
	send := goipp.Attributes{ippAttr("job-uri", goipp.TagURI, goipp.String(ippString(create.Job, "job-uri", ""))), ippAttr("last-document", goipp.TagBoolean, goipp.Boolean(true)), ippAttr("document-format", goipp.TagMimeType, goipp.String("image/urf"))}
	if sendIPP(t, endpoint, 6, send, nil, []byte("UNIRAST\x00document")).Code != 0 {
		t.Fatal("Send-Document failed")
	}
	select {
	case doc := <-received:
		if doc.Format != "image/urf" {
			t.Fatal("Send-Document format not forwarded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Send-Document never reached the driver")
	}
	bad := goipp.Attributes{ippAttr("document-format", goipp.TagMimeType, goipp.String("application/postscript"))}
	if sendIPP(t, endpoint, 4, bad, nil, nil).Code != 0x040a {
		t.Fatal("unsupported format was accepted")
	}
	if sendIPP(t, endpoint, 6, send, nil, []byte("duplicate")).Code == 0 {
		t.Fatal("duplicate document was accepted")
	}
	if err := n.Unshare(context.Background(), []string{"Office's 彩色 printer"}); err != nil {
		t.Fatal(err)
	}
	if sendIPP(t, endpoint, 11, nil, nil, nil).Code != 0x0406 {
		t.Fatal("unshared printer remains accessible")
	}
}

func TestNativeMalformedAndFailedDocuments(t *testing.T) {
	n, endpoint := nativeTestServer(t, func(context.Context, Document) error { return fmt.Errorf("driver rejected document") })
	res := sendIPP(t, endpoint, 2, nil, nil, nil)
	if res.Code == 0 {
		t.Fatal("empty document should not be accepted")
	}
	res = sendIPP(t, endpoint, 2, nil, nil, []byte("image"))
	id := ippInt(res.Job, "job-id", 0)
	attrs := goipp.Attributes{ippAttr("job-id", goipp.TagInteger, goipp.Integer(id))}
	for i := 0; i < 50; i++ {
		state := sendIPP(t, endpoint, 9, attrs, nil, nil)
		if ippInt(state.Job, "job-state", 0) == 8 {
			q, _ := n.Queue(context.Background(), "Office's 彩色 printer")
			if !strings.Contains(q.Status, "driver rejected") {
				t.Fatalf("missing driver error: %+v", q)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("driver failure did not abort the job")
}
