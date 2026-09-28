package recent

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadMissingFile(t *testing.T) {
	r, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, Recent{}) {
		t.Errorf("got %+v, want empty", r)
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "recent.json")
	want := Recent{Folder: "/src", Pattern: `\.mkv$`, ISOName: "movies", Preset: "BD-50",
		ISODir: "/isos", ISOs: []string{"/isos/a.iso", "/isos/b.iso"}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
