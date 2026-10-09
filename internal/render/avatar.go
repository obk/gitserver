package render

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	_ "image/gif" // decoders for Avatar
	_ "image/jpeg"
	"image/png"
)

// Profile pictures are never served as uploaded. Avatar decodes the upload
// and draws a fresh PNG from its pixels, so nothing else in the file
// survives: no EXIF data (camera, GPS position), no second file hidden in
// it, no SVG or HTML that a browser might run.
const (
	AvatarSize      = 256     // pixels, square
	MaxAvatarUpload = 2 << 20 // bytes
	maxAvatarPixels = 16 << 20
)

var (
	ErrAvatarFormat = errors.New("the picture must be a PNG, JPEG or GIF file")
	ErrAvatarLarge  = errors.New("the picture is too large; it may have at most 16 million pixels")
	ErrAvatarBusy   = errors.New("the server is busy; try again in a moment")
)

// avatarSem bounds the memory spent decoding pictures: a 16-megapixel PNG
// takes 64 MiB while it is being converted.
var avatarSem = make(chan struct{}, 2)

// Avatar turns an uploaded PNG, JPEG or GIF (first frame) into a square
// PNG of at most AvatarSize pixels: the middle of the picture, scaled down
// and turned upright as its EXIF orientation says.
func Avatar(data []byte) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ErrAvatarFormat
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 1<<15 || cfg.Height > 1<<15 || cfg.Width*cfg.Height > maxAvatarPixels {
		return nil, ErrAvatarLarge
	}
	select {
	case avatarSem <- struct{}{}:
		defer func() { <-avatarSem }()
	default:
		return nil, ErrAvatarBusy
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrAvatarFormat
	}
	orientation := 1
	if format == "jpeg" {
		orientation = jpegOrientation(data)
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, squareThumb(img, orientation, AvatarSize)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// squareThumb crops the middle square of img, as seen after applying the
// EXIF orientation, and shrinks it to at most size pixels by averaging.
func squareThumb(img image.Image, orientation, size int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h // dimensions as displayed
	if orientation >= 5 && orientation <= 8 {
		dw, dh = h, w
	}
	// src maps a displayed pixel to the stored one.
	src := func(x, y int) (int, int) {
		switch orientation {
		case 2:
			return w - 1 - x, y
		case 3:
			return w - 1 - x, h - 1 - y
		case 4:
			return x, h - 1 - y
		case 5:
			return y, x
		case 6:
			return y, h - 1 - x
		case 7:
			return w - 1 - y, h - 1 - x
		case 8:
			return w - 1 - y, x
		}
		return x, y
	}
	side := min(dw, dh)
	ox, oy := (dw-side)/2, (dh-side)/2
	n := min(size, side)
	out := image.NewRGBA(image.Rect(0, 0, n, n))
	for j := range n {
		y0, y1 := oy+j*side/n, oy+(j+1)*side/n
		for i := range n {
			x0, x1 := ox+i*side/n, ox+(i+1)*side/n
			var r, g, bl, a, count uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					sx, sy := src(x, y)
					cr, cg, cb, ca := img.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					r, g, bl, a = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca)
					count++
				}
			}
			// #nosec G115 -- averages of 16-bit color values, shifted down to 0-255
			out.SetRGBA(i, j, color.RGBA{uint8(r / count >> 8), uint8(g / count >> 8), uint8(bl / count >> 8), uint8(a / count >> 8)})
		}
	}
	return out
}

// jpegOrientation reads the EXIF orientation (1-8) of a JPEG file, or
// returns 1 (upright) if it has none. Phones store photos sideways and set
// this tag instead of turning the pixels.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for p := 2; p+4 <= len(data); {
		if data[p] != 0xFF {
			return 1
		}
		marker := data[p+1]
		if marker == 0xDA || marker == 0xD9 { // image data starts: no EXIF before it
			return 1
		}
		n := int(binary.BigEndian.Uint16(data[p+2:]))
		if n < 2 || p+2+n > len(data) {
			return 1
		}
		seg := data[p+4 : p+2+n]
		if marker == 0xE1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
			return exifOrientation(seg[6:])
		}
		p += 2 + n
	}
	return 1
}

// exifOrientation finds tag 0x0112 in the first IFD of a TIFF structure.
func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	count := int(bo.Uint16(t[off:]))
	for i := range count {
		e := off + 2 + 12*i
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 && bo.Uint16(t[e+2:]) == 3 { // SHORT
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}
