package imageinput

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
	"testing"
)

func BenchmarkPreparePhoto(b *testing.B) {
	const width, height = 4032, 3024
	source := image.NewNRGBA(image.Rect(0, 0, width, height))
	random := rand.New(rand.NewPCG(1, 2))
	for y := range height {
		for x := range width {
			noise := int(random.Uint32()%31) - 15
			source.SetNRGBA(x, y, color.NRGBA{
				R: uint8(45 + 160*x/width + noise),
				G: uint8(45 + 160*y/height + noise),
				B: uint8(45 + 80*(x*height+y*width)/(width*height) + noise),
				A: 255,
			})
		}
	}
	var data bytes.Buffer
	if err := jpeg.Encode(&data, source, &jpeg.Options{Quality: 90}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Prepare(b.Context(), data.Bytes(), Options{MaxBytes: 1 << 20}); err != nil {
			b.Fatal(err)
		}
	}
}
