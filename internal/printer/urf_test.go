package printer

import (
	"bytes"
	"encoding/binary"
	"image"
	"testing"
)

func urfFixture(width, height int, color bool, body []byte) []byte {
	h := make([]byte, 44)
	copy(h, "UNIRAST\x00")
	binary.BigEndian.PutUint32(h[8:12], 1)
	h[12] = 8
	if color {
		h[12], h[13] = 24, 1
	}
	binary.BigEndian.PutUint32(h[24:28], uint32(width))
	binary.BigEndian.PutUint32(h[28:32], uint32(height))
	binary.BigEndian.PutUint32(h[32:36], 300)
	return append(h, body...)
}

func TestAppleRasterPixels(t *testing.T) {
	// Two identical rows, two literal RGB pixels, then a white-fill command.
	data := urfFixture(3, 2, true, []byte{1, 255, 255, 0, 0, 0, 255, 0, 128})
	count := 0
	if err := decodeURF(bytes.NewReader(data), func(page int, img image.Image) error {
		count++
		r, g, b, _ := img.At(0, 1).RGBA()
		if r != 65535 || g != 0 || b != 0 {
			t.Fatal("RGB literal pixel was lost")
		}
		r, g, b, _ = img.At(2, 0).RGBA()
		if r != 65535 || g != 65535 || b != 65535 {
			t.Fatal("white fill was lost")
		}
		return nil
	}); err != nil || count != 1 {
		t.Fatalf("URF decode: %v / %d", err, count)
	}
	gray := urfFixture(3, 1, false, []byte{0, 2, 80})
	if err := decodeURF(bytes.NewReader(gray), func(page int, img image.Image) error {
		r, g, b, _ := img.At(2, 0).RGBA()
		if r != 80*257 || g != r || b != r {
			t.Fatal("grayscale repeat was lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAppleRasterRejectsInvalidRuns(t *testing.T) {
	for _, data := range [][]byte{urfFixture(2, 1, true, []byte{0, 2, 255, 0, 0}), urfFixture(2, 1, false, []byte{2, 128}), urfFixture(100000, 100000, false, nil), []byte("UNIRAST\x00")} {
		if err := decodeURF(bytes.NewReader(data), func(int, image.Image) error { return nil }); err == nil {
			t.Fatal("invalid raster should fail")
		}
	}
}
