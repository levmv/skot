package imageinput

import (
	"context"
	"image"
	"image/color"
	"math"

	xdraw "golang.org/x/image/draw"
)

// resizeImage applies the separable Catmull-Rom filter using a sliding window of
// horizontally filtered rows. Retaining only the vertical filter's support avoids
// x/image/draw's output-width * source-height buffer of four float64s per pixel.
// The support widens when shrinking so that every source pixel contributes.
func resizeImage(ctx context.Context, source image.Image, width, height int) (*image.NRGBA, error) {
	bounds := source.Bounds()
	horizontal, _ := resampleSpans(bounds.Dx(), width)
	vertical, rowCount := resampleSpans(bounds.Dy(), height)
	rows := make([][4]float64, width*rowCount)
	window := make([][][4]float64, rowCount)
	input := make([]color.RGBA64, bounds.Dx())
	reader := rgba64Source(source)
	target := image.NewNRGBA(image.Rect(0, 0, width, height))

	nextRow := 0
	for y, span := range vertical {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Source rows advance monotonically. Once they leave the filter's support,
		// their slots can be reused without recomputing any horizontal filtering.
		nextRow = max(nextRow, span.first)
		for ; nextRow < span.first+len(span.weights); nextRow++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for x := range input {
				input[x] = reader.RGBA64At(bounds.Min.X+x, bounds.Min.Y+nextRow)
			}
			start := nextRow % rowCount * width
			resampleRow(rows[start:start+width], input, horizontal)
		}
		for i := range span.weights {
			start := (span.first + i) % rowCount * width
			window[i] = rows[start : start+width]
		}
		for x := range width {
			var r, g, b, a float64
			for i, weight := range span.weights {
				pixel := window[i][x]
				r += pixel[0] * weight
				g += pixel[1] * weight
				b += pixel[2] * weight
				a += pixel[3] * weight
			}
			// Filter premultiplied colors, retaining overshoot until both passes
			// finish. Clamping earlier would alter edges and translucent pixels.
			target.SetRGBA64(x, y, color.RGBA64{
				R: resampleUint16(min(r, a) * span.invWeight),
				G: resampleUint16(min(g, a) * span.invWeight),
				B: resampleUint16(min(b, a) * span.invWeight),
				A: resampleUint16(a * span.invWeight),
			})
		}
	}
	return target, nil
}

type resampleSpan struct {
	first     int
	weights   []float64
	invWeight float64
}

func resampleSpans(sourceSize, targetSize int) ([]resampleSpan, int) {
	scale := float64(sourceSize) / float64(targetSize)
	filterScale := max(1, scale)
	support := xdraw.CatmullRom.Support * filterScale
	spans := make([]resampleSpan, targetSize)
	weights := make([]float64, 0, targetSize*min(sourceSize, int(math.Ceil(2*support))+1))
	maxSpan := 0
	for x := range spans {
		center := (float64(x)+0.5)*scale - 0.5
		first := max(0, int(math.Floor(center-support)))
		last := min(sourceSize, int(math.Ceil(center+support)))
		start := len(weights)
		var total float64
		for i := first; i < last; i++ {
			distance := math.Abs((center - float64(i)) / filterScale)
			weight := 0.0
			if distance < xdraw.CatmullRom.Support {
				weight = xdraw.CatmullRom.At(distance)
			}
			weights = append(weights, weight)
			total += weight
		}
		spans[x] = resampleSpan{first: first, weights: weights[start:], invWeight: 1 / total}
		maxSpan = max(maxSpan, last-first)
	}
	return spans, maxSpan
}

func resampleRow(target [][4]float64, source []color.RGBA64, spans []resampleSpan) {
	for x, span := range spans {
		var r, g, b, a float64
		for i, weight := range span.weights {
			pixel := source[span.first+i]
			r += float64(pixel.R) * weight
			g += float64(pixel.G) * weight
			b += float64(pixel.B) * weight
			a += float64(pixel.A) * weight
		}
		factor := span.invWeight / 0xffff
		target[x] = [4]float64{r * factor, g * factor, b * factor, a * factor}
	}
}

func resampleUint16(value float64) uint16 {
	return uint16(max(0, min(0xffff, int(value*0xffff+0.5))))
}

// RGBA64At avoids allocating a color interface for every decoded pixel. The
// fallback also supports image implementations without that optional method.
func rgba64Source(source image.Image) image.RGBA64Image {
	if source, ok := source.(image.RGBA64Image); ok {
		return source
	}
	return rgba64Adapter{source}
}

type rgba64Adapter struct{ image.Image }

func (source rgba64Adapter) RGBA64At(x, y int) color.RGBA64 {
	r, g, b, a := source.At(x, y).RGBA()
	return color.RGBA64{R: uint16(r), G: uint16(g), B: uint16(b), A: uint16(a)}
}
