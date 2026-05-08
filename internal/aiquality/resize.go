package aiquality

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"

	"golang.org/x/image/draw"
)

// Resize decodes a JPEG, scales it to width × (height·width/origW)
// preserving aspect, and re-encodes at JPEG quality 85. Used to put
// every candidate frame for an AI-quality batch on the same baseline so
// resolution differences don't bias the scores.
//
// If width >= the source width the original bytes are returned
// unchanged — no point upscaling.
func Resize(src []byte, width int) ([]byte, error) {
	if width <= 0 {
		return src, nil
	}
	srcImg, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	sb := srcImg.Bounds()
	if sb.Dx() == 0 || sb.Dy() == 0 {
		return nil, fmt.Errorf("empty image")
	}
	if width >= sb.Dx() {
		return src, nil
	}
	w := width
	h := sb.Dy() * w / sb.Dx()
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), srcImg, sb, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return out.Bytes(), nil
}
