package drive

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseDrutil(t *testing.T) {
	out := "   Vendor   Product           Rev   Bus       SupportLevel\n" +
		"1  HL-DT-ST DVDRW  GS21N      SA15  SATA      Apple Shipping\n" +
		"2  PIONEER  BD-RW   BDR-XD07  1.00  USB       Unsupported\n"
	got, err := parseDrutil(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []Drive{
		{ID: "1", Vendor: "HL-DT-ST", Model: "DVDRW  GS21N", Detail: "SATA · Apple Shipping"},
		{ID: "2", Vendor: "PIONEER", Model: "BD-RW   BDR-XD07", Detail: "USB · Unsupported"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseDrutilNoDrives(t *testing.T) {
	got, err := parseDrutil("   Vendor   Product           Rev   Bus       SupportLevel\n\n")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v; want no drives", got, err)
	}
	if _, err := parseDrutil("something else\n"); err == nil {
		t.Fatal("unexpected header should fail")
	}
}

func TestParseWindows(t *testing.T) {
	many := `[{"Drive":"E:","Caption":"HL-DT-ST BD-RE WH16NS60","Manufacturer":"(Standard CD-ROM drives)","MediaLoaded":true},` +
		`{"Drive":"F:","Caption":"PIONEER BD-RW BDR-XD07","Manufacturer":"Pioneer","MediaLoaded":false}]`
	got, err := parseWindows([]byte(many))
	if err != nil {
		t.Fatal(err)
	}
	want := []Drive{
		{ID: "E:", Model: "HL-DT-ST BD-RE WH16NS60", Detail: "disc loaded"},
		{ID: "F:", Vendor: "Pioneer", Model: "PIONEER BD-RW BDR-XD07", Detail: "no disc"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}

	one, err := parseWindows([]byte(`{"Drive":"D:","Caption":"DVD","MediaLoaded":false}` + "\r\n"))
	if err != nil || len(one) != 1 || one[0].ID != "D:" {
		t.Fatalf("single object: got %+v, %v", one, err)
	}
	for _, empty := range []string{"", "null", "[]"} {
		if got, err := parseWindows([]byte(empty)); err != nil || len(got) != 0 {
			t.Fatalf("%q: got %+v, %v", empty, got, err)
		}
	}
}

func TestListSysBlock(t *testing.T) {
	root := t.TempDir()
	for _, dev := range []string{"sda", "sr0"} {
		if err := os.MkdirAll(filepath.Join(root, dev, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "sr0", "device", "vendor"), []byte("ASUS    \n"), 0o644)
	os.WriteFile(filepath.Join(root, "sr0", "device", "model"), []byte("BW-16D1HT       \n"), 0o644)

	got, err := listSysBlock(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Drive{{ID: "/dev/sr0", Vendor: "ASUS", Model: "BW-16D1HT"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got, err := listSysBlock(filepath.Join(root, "missing")); err != nil || got != nil {
		t.Fatalf("missing root: got %+v, %v", got, err)
	}
}

func TestName(t *testing.T) {
	if got := (Drive{Vendor: "PIONEER", Model: "BD-RW"}).Name(); got != "PIONEER BD-RW" {
		t.Fatalf("got %q", got)
	}
	if got := (Drive{}).Name(); got != "Optical drive" {
		t.Fatalf("got %q", got)
	}
}
