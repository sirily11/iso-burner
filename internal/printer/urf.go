package printer

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"image"
	"io"
)

// decodeURF supports the W8 and SRGB24 Apple Raster formats advertised in
// Bonjour. URF uses big-endian page headers and pixel-based PackBits rows.
func decodeURF(input io.Reader, page func(int, image.Image) error) error {
	r := bufio.NewReader(input)
	var magic [12]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return err
	}
	if string(magic[:8]) != "UNIRAST\x00" {
		return fmt.Errorf("invalid Apple Raster header")
	}
	count := int(binary.BigEndian.Uint32(magic[8:]))
	if count > 100 {
		return fmt.Errorf("Apple Raster exceeds 100 pages")
	}
	for index := 0; count == 0 || index < count; index++ {
		if index >= 100 {
			return fmt.Errorf("Apple Raster exceeds 100 pages")
		}
		var h [32]byte
		_, err := io.ReadFull(r, h[:])
		if err == io.EOF && count == 0 && index > 0 {
			return nil
		}
		if err != nil {
			return err
		}
		bpp := int(h[0]) / 8
		if !((h[0] == 8 && h[1] == 0) || (h[0] == 24 && h[1] == 1)) {
			return fmt.Errorf("unsupported Apple Raster color format %d/%d", h[0], h[1])
		}
		w, hh := int(binary.BigEndian.Uint32(h[12:16])), int(binary.BigEndian.Uint32(h[16:20]))
		if w < 1 || hh < 1 || int64(w)*int64(hh) > 20000000 {
			return fmt.Errorf("Apple Raster page is too large")
		}
		img := image.NewRGBA(image.Rect(0, 0, w, hh))
		row := make([]byte, w*bpp)
		for y := 0; y < hh; {
			repeat, err := r.ReadByte()
			if err != nil {
				return err
			}
			if y+int(repeat)+1 > hh {
				return fmt.Errorf("Apple Raster row repeat exceeds page")
			}
			for pos := 0; pos < len(row); {
				code, err := r.ReadByte()
				if err != nil {
					return err
				}
				if code == 128 {
					for i := pos; i < len(row); i++ {
						row[i] = 255
					}
					break
				}
				pixels := int(code) + 1
				if code > 128 {
					pixels = 257 - int(code)
				}
				bytes := pixels * bpp
				if pos+bytes > len(row) {
					return fmt.Errorf("Apple Raster run exceeds row")
				}
				if code > 128 {
					if _, err := io.ReadFull(r, row[pos:pos+bytes]); err != nil {
						return err
					}
				} else {
					if _, err := io.ReadFull(r, row[pos:pos+bpp]); err != nil {
						return err
					}
					for off := bpp; off < bytes; off += bpp {
						copy(row[pos+off:pos+off+bpp], row[pos:pos+bpp])
					}
				}
				pos += bytes
			}
			for dy := 0; dy <= int(repeat); dy++ {
				out := img.Pix[(y+dy)*img.Stride:]
				for x := 0; x < w; x++ {
					p := row[x*bpp : (x+1)*bpp]
					out[x*4], out[x*4+1], out[x*4+2], out[x*4+3] = p[0], p[0], p[0], 255
					if bpp == 3 {
						out[x*4+1], out[x*4+2] = p[1], p[2]
					}
				}
			}
			y += int(repeat) + 1
		}
		if err := page(index, img); err != nil {
			return err
		}
	}
	if _, err := r.Peek(1); err != io.EOF {
		return fmt.Errorf("unexpected data after Apple Raster pages")
	}
	return nil
}
