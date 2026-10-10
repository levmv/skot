package imageinput_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"

	"github.com/levmv/skot/imageinput"
	"github.com/levmv/skot/model"
)

func TestPrepareFitsOutputLimits(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 512, 256))
	random := rand.New(rand.NewPCG(1, 2))
	for i := range source.Pix {
		source.Pix[i] = byte(random.Uint32())
		if i%4 == 3 {
			source.Pix[i] = 255
		}
	}
	for _, format := range []string{"png", "jpeg"} {
		t.Run(format, func(t *testing.T) {
			var data bytes.Buffer
			var err error
			if format == "png" {
				err = png.Encode(&data, source)
			} else {
				err = jpeg.Encode(&data, source, &jpeg.Options{Quality: 95})
			}
			if err != nil {
				t.Fatal(err)
			}
			options := imageinput.Options{MaxSide: 128, MaxBytes: 4096}
			prepared, err := imageinput.Prepare(t.Context(), data.Bytes(), options)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.SourceWidth != 512 || prepared.SourceHeight != 256 || prepared.Image.Width > options.MaxSide || prepared.Image.Height > options.MaxSide || len(prepared.Image.Data) > options.MaxBytes {
				t.Fatalf("prepared source=%dx%d image=%dx%d bytes=%d", prepared.SourceWidth, prepared.SourceHeight, prepared.Image.Width, prepared.Image.Height, len(prepared.Image.Data))
			}
			decoded, gotFormat, err := image.Decode(bytes.NewReader(prepared.Image.Data))
			if err != nil || gotFormat != format {
				t.Fatalf("decode output: format=%q error=%v", gotFormat, err)
			}
			if decoded.Bounds().Dx() != prepared.Image.Width || decoded.Bounds().Dy() != prepared.Image.Height {
				t.Fatal("output dimensions disagree with pixels")
			}
			if err := model.ValidateContent(model.ImageToolContent("", prepared.Image)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrepareRejectsSourceLimitsAndUnattainableOutputSize(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewGray(image.Rect(0, 0, 20, 10))); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		options imageinput.Options
	}{
		{"source bytes", imageinput.Options{MaxSourceBytes: data.Len() - 1}},
		{"source pixels", imageinput.Options{MaxSourcePixels: 199}},
		{"output bytes", imageinput.Options{MaxBytes: 1}},
		{"negative limit", imageinput.Options{MaxSide: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := imageinput.Prepare(t.Context(), data.Bytes(), test.options); err == nil {
				t.Fatal("invalid image limits were accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := imageinput.Prepare(ctx, data.Bytes(), imageinput.Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preparation: %v", err)
	}
}

func TestPrepareGIFFirstFrameInsideLargerCanvas(t *testing.T) {
	palette := color.Palette{color.White, color.Black}
	frame := image.NewPaletted(image.Rect(2, 3, 10, 7), palette)
	var data bytes.Buffer
	if err := gif.EncodeAll(&data, &gif.GIF{
		Image: []*image.Paletted{frame}, Delay: []int{0},
		Config: image.Config{ColorModel: palette, Width: 20, Height: 10},
	}); err != nil {
		t.Fatal(err)
	}
	prepared, err := imageinput.Prepare(t.Context(), data.Bytes(), imageinput.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Image.MediaType != "image/png" || prepared.Image.Width != 8 || prepared.Image.Height != 4 {
		t.Fatalf("first frame = %s %dx%d", prepared.Image.MediaType, prepared.Image.Width, prepared.Image.Height)
	}
}
