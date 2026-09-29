package printer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/google/uuid"
	"github.com/grandcat/zeroconf"
)

const nativeFormats = "application/pdf,image/urf,image/jpeg,image/png"
const maxDocumentBytes = 128 << 20

// Document is one received IPP document, stored only until it is handed to
// the Windows driver. Native keeps job state in memory rather than in CUPS.
type Document struct {
	Printer, Path, Format, Title, Owner, Media string
	ID, Copies, Orientation                    int
}

// Native hosts IPP and Bonjour directly in this process. Callbacks isolate
// the Windows print driver from the portable protocol and queue implementation.
type Native struct {
	ListPrinters func(context.Context) ([]Printer, error)
	Print        func(context.Context, Document) error
	Interfaces   func(context.Context) ([]net.Interface, error)
	ListenAddr   string // empty uses :8631; tests can request 127.0.0.1:0
	mu           sync.Mutex
	printers     map[string]Printer
	jobs         map[int]*nativeJob
	nextID       int
	server       *http.Server
	listener     net.Listener
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	closed       bool
	started      time.Time
}

type nativeJob struct {
	ID int
	Document
	State     int // IPP pending=3, held=4, processing=5, canceled=7, aborted=8, completed=9
	Size      int64
	Error     string
	Created   time.Time
	Cancel    context.CancelFunc
	Receiving bool
}

func (n *Native) List(ctx context.Context) ([]Printer, error) {
	rows, err := n.ListPrinters(ctx)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := range rows {
		_, rows[i].Shared = n.printers[rows[i].Name]
	}
	return rows, nil
}

func (n *Native) Share(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	rows, err := n.ListPrinters(ctx)
	if err != nil {
		return err
	}
	available := map[string]Printer{}
	for _, p := range rows {
		available[p.Name] = p
	}
	for _, name := range names {
		if _, ok := available[name]; !ok {
			return fmt.Errorf("printer %q is no longer installed", name)
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return errors.New("AirPrint server is closed")
	}
	if n.server == nil {
		addr := n.ListenAddr
		if addr == "" {
			addr = ":8631"
		}
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("start native AirPrint server: %w", err)
		}
		n.listener = listener
		n.started = time.Now()
		n.ctx, n.cancel = context.WithCancel(context.Background())
		n.printers = map[string]Printer{}
		n.jobs = map[int]*nativeJob{}
		n.server = &http.Server{Handler: n, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: 2 * time.Minute, IdleTimeout: 30 * time.Second}
		n.wg.Add(1)
		go func() { defer n.wg.Done(); _ = n.server.Serve(listener) }()
	}
	for _, name := range names {
		p := available[name]
		p.Shared = true
		n.printers[name] = p
	}
	return nil
}

func (n *Native) Unshare(_ context.Context, names []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, name := range names {
		delete(n.printers, name)
	}
	return nil
}

func (n *Native) Queue(_ context.Context, name string) (Queue, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	q := Queue{Status: "ready"}
	var jobs []*nativeJob
	for _, j := range n.jobs {
		if j.Printer == name {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	waiting := 0
	for _, j := range jobs {
		if j.State == 8 {
			q.Status = "print failed: " + j.Error
		}
		if j.State > 5 {
			continue
		}
		rank := "active"
		if j.State != 5 {
			waiting++
			rank = strconv.Itoa(waiting)
		} else {
			q.Status = "printing"
		}
		q.Jobs = append(q.Jobs, Job{ID: j.ID, Rank: rank, Owner: j.Owner, Title: j.Title, Size: j.Size})
	}
	return q, nil
}

func (n *Native) Advertise(ctx context.Context, p Printer) error {
	n.mu.Lock()
	if n.listener == nil {
		n.mu.Unlock()
		return errors.New("AirPrint server is not running")
	}
	port := n.listener.Addr().(*net.TCPAddr).Port
	n.mu.Unlock()
	txt := airprintTXT(p)
	for i, value := range txt {
		if strings.HasPrefix(value, "rp=") {
			txt[i] = "rp=printers/" + url.PathEscape(p.Name)
		}
		if strings.HasPrefix(value, "pdl=") {
			txt[i] = "pdl=" + nativeFormats
		}
		if strings.HasPrefix(value, "URF=") {
			txt[i] = "URF=W8,SRGB24,RS300"
		}
	}
	txt = append(txt, "Duplex=F", "Copies=T", "Color="+map[bool]string{true: "T", false: "F"}[p.Color], "UUID="+nativePrinterUUID(p.Name))
	var ifaces []net.Interface
	if n.Interfaces != nil {
		var err error
		ifaces, err = n.Interfaces(ctx)
		if err != nil {
			return err
		}
	}
	server, err := zeroconf.Register(AdvertisedName(p), "_ipp._tcp,_universal", "local.", port, txt, ifaces)
	if err != nil {
		return fmt.Errorf("native AirPrint discovery: %w", err)
	}
	defer server.Shutdown()
	<-ctx.Done()
	return nil
}

func (n *Native) Close() error {
	n.mu.Lock()
	n.closed = true
	if n.cancel != nil {
		n.cancel()
	}
	server := n.server
	for _, j := range n.jobs {
		if j.Cancel != nil {
			j.Cancel()
		}
	}
	n.mu.Unlock()
	if server != nil {
		_ = server.Close()
	}
	n.wg.Wait()
	return nil
}

func ippAttr(name string, tag goipp.Tag, values ...goipp.Value) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, v := range values {
		a.Values.Add(tag, v)
	}
	return a
}
func ippString(attrs goipp.Attributes, name, fallback string) string {
	for _, a := range attrs {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return fallback
}
func ippInt(attrs goipp.Attributes, name string, fallback int) int {
	s := ippString(attrs, name, "")
	v, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return v
}
func ippBool(attrs goipp.Attributes, name string, fallback bool) bool {
	s := ippString(attrs, name, "")
	v, err := strconv.ParseBool(s)
	if err != nil {
		return fallback
	}
	return v
}
func printerPath(name string) string { return "/printers/" + url.PathEscape(name) }

func nativePrinterUUID(name string) string {
	host, _ := os.Hostname()
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("iso-burner://"+host+printerPath(name))).String()
}

func ippJobID(attrs goipp.Attributes) int {
	if id := ippInt(attrs, "job-id", 0); id != 0 {
		return id
	}
	u, err := url.Parse(ippString(attrs, "job-uri", ""))
	if err != nil {
		return 0
	}
	id, _ := strconv.Atoi(filepath.Base(u.Path))
	return id
}

func (n *Native) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "IPP requires POST", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/ipp") {
		http.Error(w, "IPP content type required", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDocumentBytes+(64<<10))
	defer r.Body.Close()
	var req goipp.Message
	if err := req.Decode(io.LimitReader(r.Body, 64<<10)); err != nil {
		http.Error(w, "Invalid IPP request", http.StatusBadRequest)
		return
	}
	resp := goipp.Message{Version: req.Version, RequestID: req.RequestID, Code: 0}
	resp.Operation = goipp.Attributes{ippAttr("attributes-charset", goipp.TagCharset, goipp.String("utf-8")), ippAttr("attributes-natural-language", goipp.TagLanguage, goipp.String("en"))}
	name := strings.TrimPrefix(r.URL.Path, "/printers/")
	if printerName, _, jobURI := strings.Cut(name, "/jobs/"); jobURI {
		name = printerName
	}
	n.mu.Lock()
	p, ok := n.printers[name]
	if !ok || n.closed {
		resp.Code = 0x0406
	} else {
		n.handleIPP(r, &req, &resp, p)
	}
	n.mu.Unlock()
	w.Header().Set("Content-Type", "application/ipp")
	_ = resp.Encode(w)
}

func (n *Native) handleIPP(r *http.Request, req, resp *goipp.Message, p Printer) {
	uri := "ipp://" + r.Host + printerPath(p.Name)
	switch req.Code {
	case 0x000b: // Get-Printer-Attributes
		resp.Printer = n.printerAttributes(p, uri)
	case 0x0004: // Validate-Job
		resp.Code = n.validateJob(req)
	case 0x0002, 0x0005: // Print-Job / Create-Job
		if resp.Code = n.validateJob(req); resp.Code != 0 {
			return
		}
		pending := 0
		for id, j := range n.jobs {
			if j.State <= 5 {
				pending++
			}
			if j.State > 5 && time.Since(j.Created) > 10*time.Minute {
				delete(n.jobs, id)
			}
		}
		if pending >= 64 || len(n.jobs) >= 256 {
			resp.Code = 0x0507
			return
		}
		n.nextID++
		j := &nativeJob{ID: n.nextID, State: 4, Created: time.Now(), Document: Document{ID: n.nextID, Printer: p.Name,
			Title: ippString(req.Operation, "job-name", "AirPrint document"), Owner: ippString(req.Operation, "requesting-user-name", "AirPrint"),
			Format: ippString(req.Operation, "document-format", "application/octet-stream"), Copies: ippInt(req.Job, "copies", 1),
			Orientation: ippInt(req.Job, "orientation-requested", 3), Media: ippString(req.Job, "media", "")}}
		n.jobs[j.ID] = j
		if req.Code == 0x0002 {
			resp.Code = n.receiveDocument(r, req, j)
		}
		resp.Job = n.jobAttributes(j, uri)
	case 0x0006: // Send-Document
		j := n.jobs[ippJobID(req.Operation)]
		if j == nil || j.Printer != p.Name {
			resp.Code = 0x0406
			return
		}
		if j.State != 4 || j.Receiving || !ippBool(req.Operation, "last-document", true) {
			resp.Code = 0x0501
			return
		}
		if resp.Code = n.validateJob(req); resp.Code != 0 {
			return
		}
		resp.Code = n.receiveDocument(r, req, j)
		resp.Job = n.jobAttributes(j, uri)
	case 0x0008, 0x0009: // Cancel-Job / Get-Job-Attributes
		j := n.jobs[ippJobID(req.Operation)]
		if j == nil || j.Printer != p.Name {
			resp.Code = 0x0406
			return
		}
		if req.Code == 0x0008 {
			if j.State > 5 {
				resp.Code = 0x0404
				return
			}
			j.State = 7
			if j.Cancel != nil {
				j.Cancel()
			}
		}
		resp.Job = n.jobAttributes(j, uri)
	case 0x000a: // Get-Jobs (a separate Job group per job)
		resp.Groups = goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: resp.Operation}}
		var ids []int
		which := ippString(req.Operation, "which-jobs", "not-completed")
		for id, j := range n.jobs {
			if j.Printer == p.Name && (which == "all" || (which == "completed" && j.State > 5) || (which == "not-completed" && j.State <= 5)) {
				ids = append(ids, id)
			}
		}
		sort.Ints(ids)
		for _, id := range ids {
			resp.Groups.Add(goipp.Group{Tag: goipp.TagJobGroup, Attrs: n.jobAttributes(n.jobs[id], uri)})
		}
	default:
		resp.Code = 0x0501
	}
}

func (n *Native) validateJob(req *goipp.Message) goipp.Code {
	format := ippString(req.Operation, "document-format", "application/octet-stream")
	if format != "application/octet-stream" && !strings.Contains(","+nativeFormats+",", ","+format+",") {
		return 0x040a
	}
	if compression := ippString(req.Operation, "compression", "none"); compression != "none" {
		return 0x040f
	}
	if copies := ippInt(req.Job, "copies", 1); copies < 1 || copies > 99 {
		return 0x040b
	}
	if sides := ippString(req.Job, "sides", "one-sided"); sides != "one-sided" {
		return 0x040b
	}
	return 0
}

// Called with mu held. Release it during upload so other clients can query
// state; an unfinished upload stays held and is never sent to the driver.
func (n *Native) receiveDocument(r *http.Request, req *goipp.Message, j *nativeJob) goipp.Code {
	j.Receiving = true
	if f := ippString(req.Operation, "document-format", ""); f != "" {
		j.Format = f
	}
	file, err := os.CreateTemp("", "iso-burner-airprint-*")
	if err != nil {
		j.Receiving = false
		j.State = 8
		j.Error = err.Error()
		return 0x0500
	}
	path := file.Name()
	n.mu.Unlock()
	size, err := io.Copy(file, io.LimitReader(r.Body, maxDocumentBytes+1))
	closeErr := file.Close()
	n.mu.Lock()
	j.Receiving = false
	if err == nil {
		err = closeErr
	}
	if size > maxDocumentBytes {
		err = errors.New("document exceeds 128 MB")
	}
	if size == 0 && err == nil {
		err = errors.New("empty document")
	}
	if err != nil || j.State == 7 || n.closed {
		_ = os.Remove(path)
		if j.State != 7 {
			j.State = 8
			if err != nil {
				j.Error = err.Error()
			}
		}
		return 0x0400
	}
	j.Path = path
	j.Size = size
	j.State = 3
	ctx, cancel := context.WithCancel(n.ctx)
	j.Cancel = cancel
	n.wg.Add(1)
	go n.runJob(ctx, j.ID)
	return 0
}

func (n *Native) runJob(ctx context.Context, id int) {
	defer n.wg.Done()
	n.mu.Lock()
	cancel := n.jobs[id].Cancel
	n.mu.Unlock()
	defer cancel()
	// Serialize each printer's virtual queue without blocking other printers.
	for {
		n.mu.Lock()
		j := n.jobs[id]
		blocked := false
		for otherID, other := range n.jobs {
			if other.Printer == j.Printer && otherID < id && (other.State == 3 || other.State == 5) {
				blocked = true
				break
			}
		}
		if j.State == 7 || ctx.Err() != nil {
			path := j.Path
			n.mu.Unlock()
			_ = os.Remove(path)
			return
		}
		if !blocked {
			j.State = 5
			doc := j.Document
			n.mu.Unlock()
			err := n.Print(ctx, doc)
			_ = os.Remove(doc.Path)
			n.mu.Lock()
			if j.State != 7 {
				j.State = 9
				if err != nil {
					j.State = 8
					j.Error = err.Error()
				}
			}
			n.mu.Unlock()
			return
		}
		n.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (n *Native) jobAttributes(j *nativeJob, uri string) goipp.Attributes {
	reason := "none"
	if j.State == 4 {
		reason = "job-data-insufficient"
	}
	if j.State == 8 {
		reason = "document-unprintable-error"
	}
	return goipp.Attributes{
		ippAttr("job-id", goipp.TagInteger, goipp.Integer(j.ID)), ippAttr("job-uri", goipp.TagURI, goipp.String(uri+"/jobs/"+strconv.Itoa(j.ID))),
		ippAttr("job-printer-uri", goipp.TagURI, goipp.String(uri)), ippAttr("job-state", goipp.TagEnum, goipp.Integer(j.State)),
		ippAttr("job-state-reasons", goipp.TagKeyword, goipp.String(reason)), ippAttr("job-state-message", goipp.TagText, goipp.String(j.Error)),
		ippAttr("job-name", goipp.TagName, goipp.String(j.Title)), ippAttr("job-originating-user-name", goipp.TagName, goipp.String(j.Owner)),
		ippAttr("job-k-octets", goipp.TagInteger, goipp.Integer((j.Size+1023)/1024)),
		ippAttr("time-at-creation", goipp.TagInteger, goipp.Integer(max(1, int(j.Created.Sub(n.started).Seconds())))),
		ippAttr("job-printer-up-time", goipp.TagInteger, goipp.Integer(max(1, int(time.Since(n.started).Seconds())))),
	}
}

func (n *Native) printerAttributes(p Printer, uri string) goipp.Attributes {
	state, queued := 3, 0
	for _, j := range n.jobs {
		if j.Printer == p.Name && j.State <= 5 {
			queued++
			if j.State == 5 {
				state = 4
			}
		}
	}
	w, h := p.MediaWidth, p.MediaHeight
	if w == 0 || h == 0 {
		w, h = 21000, 29700
	}
	media := fmt.Sprintf("custom_paper_%gx%gmm", float64(w)/100, float64(h)/100)
	size := goipp.Collection{ippAttr("x-dimension", goipp.TagInteger, goipp.Integer(w)), ippAttr("y-dimension", goipp.TagInteger, goipp.Integer(h))}
	col := goipp.Collection{ippAttr("media-size", goipp.TagBeginCollection, size), ippAttr("media-size-name", goipp.TagKeyword, goipp.String(media)), ippAttr("media-type", goipp.TagKeyword, goipp.String("stationery")), ippAttr("media-source", goipp.TagKeyword, goipp.String("auto"))}
	for _, edge := range []string{"bottom", "left", "right", "top"} {
		col = append(col, ippAttr("media-"+edge+"-margin", goipp.TagInteger, goipp.Integer(0)))
	}
	attrs := goipp.Attributes{
		ippAttr("printer-uri-supported", goipp.TagURI, goipp.String(uri)), ippAttr("uri-authentication-supported", goipp.TagKeyword, goipp.String("none")), ippAttr("uri-security-supported", goipp.TagKeyword, goipp.String("none")),
		ippAttr("printer-name", goipp.TagName, goipp.String(p.Name)), ippAttr("printer-info", goipp.TagText, goipp.String(p.Label())), ippAttr("printer-location", goipp.TagText, goipp.String(p.Location)), ippAttr("printer-make-and-model", goipp.TagText, goipp.String(p.Model)),
		ippAttr("printer-uuid", goipp.TagURI, goipp.String("urn:uuid:"+nativePrinterUUID(p.Name))), ippAttr("printer-up-time", goipp.TagInteger, goipp.Integer(max(1, int(time.Since(n.started).Seconds())))),
		ippAttr("printer-state", goipp.TagEnum, goipp.Integer(state)), ippAttr("printer-state-reasons", goipp.TagKeyword, goipp.String("none")), ippAttr("printer-is-accepting-jobs", goipp.TagBoolean, goipp.Boolean(true)), ippAttr("queued-job-count", goipp.TagInteger, goipp.Integer(queued)),
		ippAttr("ipp-versions-supported", goipp.TagKeyword, goipp.String("1.1"), goipp.String("2.0")), ippAttr("operations-supported", goipp.TagEnum, goipp.Integer(2), goipp.Integer(4), goipp.Integer(5), goipp.Integer(6), goipp.Integer(8), goipp.Integer(9), goipp.Integer(10), goipp.Integer(11)),
		ippAttr("charset-configured", goipp.TagCharset, goipp.String("utf-8")), ippAttr("charset-supported", goipp.TagCharset, goipp.String("utf-8")), ippAttr("natural-language-configured", goipp.TagLanguage, goipp.String("en")), ippAttr("generated-natural-language-supported", goipp.TagLanguage, goipp.String("en")),
		ippAttr("document-format-default", goipp.TagMimeType, goipp.String("application/octet-stream")), ippAttr("document-format-supported", goipp.TagMimeType, goipp.String("application/pdf"), goipp.String("image/urf"), goipp.String("image/jpeg"), goipp.String("image/png"), goipp.String("application/octet-stream")),
		ippAttr("compression-supported", goipp.TagKeyword, goipp.String("none")), ippAttr("copies-default", goipp.TagInteger, goipp.Integer(1)), ippAttr("copies-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: 99}), ippAttr("multiple-document-jobs-supported", goipp.TagBoolean, goipp.Boolean(false)),
		ippAttr("sides-default", goipp.TagKeyword, goipp.String("one-sided")), ippAttr("sides-supported", goipp.TagKeyword, goipp.String("one-sided")), ippAttr("color-supported", goipp.TagBoolean, goipp.Boolean(p.Color)),
		ippAttr("printer-resolution-default", goipp.TagResolution, goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi}), ippAttr("printer-resolution-supported", goipp.TagResolution, goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi}), ippAttr("urf-supported", goipp.TagKeyword, goipp.String("W8"), goipp.String("SRGB24"), goipp.String("RS300")),
		ippAttr("media-default", goipp.TagKeyword, goipp.String(media)), ippAttr("media-supported", goipp.TagKeyword, goipp.String(media)), ippAttr("media-col-default", goipp.TagBeginCollection, col), ippAttr("media-col-ready", goipp.TagBeginCollection, col), ippAttr("media-col-database", goipp.TagBeginCollection, col),
		ippAttr("media-col-supported", goipp.TagKeyword, goipp.String("media-size"), goipp.String("media-size-name"), goipp.String("media-type"), goipp.String("media-source"), goipp.String("media-bottom-margin"), goipp.String("media-left-margin"), goipp.String("media-right-margin"), goipp.String("media-top-margin")),
		ippAttr("orientation-requested-default", goipp.TagEnum, goipp.Integer(3)), ippAttr("orientation-requested-supported", goipp.TagEnum, goipp.Integer(3), goipp.Integer(4)), ippAttr("print-quality-default", goipp.TagEnum, goipp.Integer(4)), ippAttr("print-quality-supported", goipp.TagEnum, goipp.Integer(4)),
		ippAttr("job-creation-attributes-supported", goipp.TagKeyword, goipp.String("copies"), goipp.String("media"), goipp.String("orientation-requested"), goipp.String("sides")),
		ippAttr("media-ready", goipp.TagKeyword, goipp.String(media)), ippAttr("media-type-supported", goipp.TagKeyword, goipp.String("stationery")), ippAttr("media-source-supported", goipp.TagKeyword, goipp.String("auto")),
		ippAttr("pdf-versions-supported", goipp.TagKeyword, goipp.String("adobe-1.3"), goipp.String("adobe-1.4"), goipp.String("adobe-1.5"), goipp.String("adobe-1.6"), goipp.String("adobe-1.7")),
		ippAttr("job-hold-until-default", goipp.TagKeyword, goipp.String("no-hold")), ippAttr("job-hold-until-supported", goipp.TagKeyword, goipp.String("no-hold")), ippAttr("job-priority-default", goipp.TagInteger, goipp.Integer(1)), ippAttr("job-priority-supported", goipp.TagInteger, goipp.Integer(1)),
		ippAttr("job-sheets-default", goipp.TagKeyword, goipp.String("none")), ippAttr("job-sheets-supported", goipp.TagKeyword, goipp.String("none")), ippAttr("multiple-document-handling-default", goipp.TagKeyword, goipp.String("single-document")), ippAttr("multiple-document-handling-supported", goipp.TagKeyword, goipp.String("single-document")),
	}
	mode := "monochrome"
	if p.Color {
		mode = "color"
	}
	attrs = append(attrs, ippAttr("print-color-mode-default", goipp.TagKeyword, goipp.String(mode)), ippAttr("print-color-mode-supported", goipp.TagKeyword, goipp.String(mode)))
	return attrs
}
