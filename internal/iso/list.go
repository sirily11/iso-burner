package iso

import (
	"fmt"
	"io/fs"
	"path"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"

	"github.com/sirily11/iso-burner/internal/settings"
)

// ListFiles returns every regular file stored in the ISO image at imagePath,
// with slash-separated paths relative to the image root.
func ListFiles(imagePath string) ([]settings.File, error) {
	d, err := diskfs.Open(imagePath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", imagePath, err)
	}
	defer d.Close()
	fsys, err := d.GetFilesystem(0)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", imagePath, err)
	}
	defer fsys.Close()
	isoFS, ok := fsys.(*iso9660.FileSystem)
	if !ok {
		return nil, fmt.Errorf("%s is not an ISO 9660 image", imagePath)
	}
	var files []settings.File
	if err := walkISO(isoFS, ".", &files); err != nil {
		return nil, fmt.Errorf("read %s: %w", imagePath, err)
	}
	return files, nil
}

// walkISO appends the regular files under dir. Directory entries in an ISO do
// not report fs.ModeDir, so IsDir is checked instead of the file type.
func walkISO(isoFS *iso9660.FileSystem, dir string, files *[]settings.File) error {
	entries, err := isoFS.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := path.Join(dir, e.Name())
		if e.IsDir() {
			if err := walkISO(isoFS, p, files); err != nil {
				return err
			}
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		*files = append(*files, settings.File{RelPath: p, Size: info.Size()})
	}
	return nil
}
