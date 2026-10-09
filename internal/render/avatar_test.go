package render

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

// halves is a w×h picture, red on the left half and blue on the right.
func halves(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{255, 0, 0, 255}
			if x >= w/2 {
				c = color.RGBA{0, 0, 255, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func decodePNG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("output is not a PNG: %v", err)
	}
	return img
}

func isRed(c color.Color) bool { r, g, b, _ := c.RGBA(); return r > 0xc000 && g < 0x4000 && b < 0x4000 }
func isBlue(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return b > 0xc000 && g < 0x4000 && r < 0x4000
}

func TestAvatar(t *testing.T) {
	var big, small, anim bytes.Buffer
	png.Encode(&big, halves(800, 400))
	jpeg.Encode(&small, halves(60, 30), &jpeg.Options{Quality: 95})
	gif.EncodeAll(&anim, &gif.GIF{Image: []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 10, 10), color.Palette{color.Black})}, Delay: []int{0}})
	for name, tc := range map[string]struct {
		in   []byte
		size int
	}{
		"large png":  {big.Bytes(), AvatarSize},
		"small jpeg": {small.Bytes(), 30},
		"gif":        {anim.Bytes(), 10},
	} {
		out, err := Avatar(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		img := decodePNG(t, out)
		if b := img.Bounds(); b.Dx() != tc.size || b.Dy() != tc.size {
			t.Errorf("%s: %v, want %d×%d", name, b, tc.size, tc.size)
		}
	}
	// The middle square is kept: red on its left, blue on its right.
	out, _ := Avatar(big.Bytes())
	img := decodePNG(t, out)
	if !isRed(img.At(10, 128)) || !isBlue(img.At(245, 128)) {
		t.Errorf("crop: %v %v", img.At(10, 128), img.At(245, 128))
	}

	for _, in := range [][]byte{nil, []byte("hello"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), big.Bytes()[:100]} {
		if _, err := Avatar(in); err != ErrAvatarFormat {
			t.Errorf("Avatar(%.20q) = %v, want ErrAvatarFormat", in, err)
		}
	}
	// A picture claiming huge dimensions is refused before it is decoded.
	if _, err := Avatar(pngHeader(20000, 20000)); err != ErrAvatarLarge {
		t.Errorf("huge picture: %v", err)
	}
}

// pngHeader is the start of a PNG file of w×h pixels, without its pixels.
func pngHeader(w, h uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	b = append(b, ihdr...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...)))
}

// withOrientation inserts an EXIF block with this orientation into a JPEG.
func withOrientation(j []byte, orientation uint16, order binary.AppendByteOrder) []byte {
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08")
	if order == binary.LittleEndian {
		tiff = []byte("II\x2a\x00\x08\x00\x00\x00")
	}
	tiff = order.AppendUint16(tiff, 1) // one IFD entry
	tiff = order.AppendUint16(tiff, 0x0112)
	tiff = order.AppendUint16(tiff, 3)
	tiff = order.AppendUint32(tiff, 1)
	tiff = order.AppendUint16(tiff, orientation)
	tiff = append(tiff, 0, 0, 0, 0, 0, 0) // padding, next IFD = 0
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := binary.BigEndian.AppendUint16([]byte{0xFF, 0xE1}, uint16(len(seg)+2))
	return append(append(append([]byte{}, j[:2]...), append(app1, seg...)...), j[2:]...)
}

func TestAvatarOrientation(t *testing.T) {
	var j bytes.Buffer
	jpeg.Encode(&j, halves(40, 20), &jpeg.Options{Quality: 95})
	for _, tc := range []struct {
		orientation uint16
		order       binary.AppendByteOrder
		top, left   func(color.Color) bool // where red ends up
	}{
		{1, binary.BigEndian, nil, isRed},
		{6, binary.BigEndian, isRed, nil},    // turned clockwise: the left half is now on top
		{6, binary.LittleEndian, isRed, nil}, // same, little-endian EXIF
		{8, binary.BigEndian, isBlue, nil},   // counter-clockwise: the right half is on top
		{3, binary.BigEndian, nil, isBlue},   // upside down
	} {
		in := withOrientation(j.Bytes(), tc.orientation, tc.order)
		if got := jpegOrientation(in); got != int(tc.orientation) {
			t.Fatalf("jpegOrientation = %d, want %d", got, tc.orientation)
		}
		out, err := Avatar(in)
		if err != nil {
			t.Fatal(err)
		}
		img := decodePNG(t, out)
		if tc.left != nil && !(tc.left(img.At(2, 10)) && !tc.left(img.At(17, 10))) {
			t.Errorf("orientation %d: left %v, right %v", tc.orientation, img.At(2, 10), img.At(17, 10))
		}
		if tc.top != nil && !(tc.top(img.At(10, 2)) && !tc.top(img.At(10, 17))) {
			t.Errorf("orientation %d: top %v, bottom %v", tc.orientation, img.At(10, 2), img.At(10, 17))
		}
		if bytes.Contains(out, []byte("Exif")) {
			t.Errorf("orientation %d: EXIF data kept", tc.orientation)
		}
	}
}
