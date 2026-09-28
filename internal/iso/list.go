package iso

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"

	"github.com/sirily11/iso-burner/internal/settings"
)

// Image is an ISO image opened for reading the files stored in it.
type Image struct {
	path string
	disk *disk.Disk
	fs   *iso9660.FileSystem
}

// OpenImage opens the ISO image at imagePath read-only.
func OpenImage(imagePath string) (*Image, error) {
	d, err := diskfs.Open(imagePath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", imagePath, err)
	}
	fsys, err := d.GetFilesystem(0)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("read %s: %w", imagePath, err)
	}
	isoFS, ok := fsys.(*iso9660.FileSystem)
	if !ok {
		fsys.Close()
		d.Close()
		return nil, fmt.Errorf("%s is not an ISO 9660 image", imagePath)
	}
	return &Image{path: imagePath, disk: d, fs: isoFS}, nil
}

// Close releases the image.
func (im *Image) Close() error {
	im.fs.Close()
	return im.disk.Close()
}

// Files returns every regular file stored in the image, with slash-separated
// paths relative to the image root.
func (im *Image) Files() ([]settings.File, error) {
	var files []settings.File
	if err := walkISO(im.fs, ".", &files); err != nil {
		return nil, fmt.Errorf("read %s: %w", im.path, err)
	}
	return files, nil
}

// Extract copies the file at relPath in the image to dst.
func (im *Image) Extract(relPath, dst string) (err error) {
	src, err := im.fs.OpenFile(path.Join("/", relPath), os.O_RDONLY)
	if err != nil {
		return fmt.Errorf("open %s in %s: %w", relPath, path.Base(im.path), err)
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, src); err != nil {
		return fmt.Errorf("extract %s: %w", relPath, err)
	}
	return nil
}

// ListFiles returns every regular file stored in the ISO image at imagePath,
// with slash-separated paths relative to the image root.
func ListFiles(imagePath string) ([]settings.File, error) {
	im, err := OpenImage(imagePath)
	if err != nil {
		return nil, err
	}
	defer im.Close()
	return im.Files()
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
