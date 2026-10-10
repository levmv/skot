// Package imageinput prepares PNG, JPEG, GIF, and WebP bytes for model input.
// PNG and GIF produce PNG; JPEG and WebP produce JPEG. GIF uses its first frame.
package imageinput

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"

	"github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/model"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// DefaultMaxSourceBytes is the input byte limit used when Options.MaxSourceBytes
// is zero. Callers reading files or uploads can enforce it before buffering them.
const DefaultMaxSourceBytes = 20 << 20

// Options controls source limits and output size. Zero fields use the documented
// defaults. Negative limits are invalid. Smaller MaxBytes may reduce resolution.
type Options struct {
	MaxSourceBytes  int // Default: 20 MiB.
	MaxSourcePixels int // Default: 40 million; maximum: 100 million.
	MaxSide         int // Longest output side. Default: 2000; maximum: 32768.
	MaxBytes        int // Output bytes. Default: 8 MiB; maximum: 16 MiB.
}

// Result contains a model-ready image and its oriented source dimensions before
// resizing (the first frame for GIF).
type Result struct {
	Image        model.ImageContent
	SourceWidth  int
	SourceHeight int
}

// Prepare decodes an image, applies EXIF orientation, removes nonvisual metadata,
// and fits it within the output limits. Suitable PNG/JPEG raster data is preserved
// without recompression. JPEG transparency is composited on white.
//
// Source dimensions are checked before full decoding. Cancellation is checked
// between processing steps; a running codec or resize operation is not interrupted.
// The result never aliases data.
func Prepare(ctx context.Context, data []byte, options Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	options, err := options.withDefaults()
	if err != nil {
		return Result{}, err
	}
	if len(data) == 0 || len(data) > options.MaxSourceBytes {
		return Result{}, fmt.Errorf("image source bytes are outside the 1..%d limit", options.MaxSourceBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("decode image metadata: %w", err)
	}
	canonicalFormat := canonicalImageFormat(format)
	if canonicalFormat == "" {
		return Result{}, fmt.Errorf("unsupported image format %q; supported formats are PNG, JPEG, GIF, and WebP", format)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > limits.MaxImageDimension || config.Height > limits.MaxImageDimension || config.Width > options.MaxSourcePixels/config.Height {
		return Result{}, fmt.Errorf("image dimensions %dx%d exceed source limits", config.Width, config.Height)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("decode image: %w", err)
	}
	bounds := decoded.Bounds()
	// GIF's first frame may occupy only part of its logical canvas.
	if decodedFormat != format || (format != "gif" && (bounds.Dx() != config.Width || bounds.Dy() != config.Height)) {
		return Result{}, fmt.Errorf("decoded image does not match its header")
	}
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 || bounds.Dx() > limits.MaxImageDimension || bounds.Dy() > limits.MaxImageDimension || bounds.Dx() > options.MaxSourcePixels/bounds.Dy() {
		return Result{}, fmt.Errorf("decoded image dimensions exceed source limits")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	orientation := imageOrientation(data, format)
	oriented := applyImageOrientation(decoded, orientation)
	width, height := oriented.Bounds().Dx(), oriented.Bounds().Dy()
	result := Result{SourceWidth: width, SourceHeight: height}
	var normalized []byte
	if canonicalFormat == format && orientation == exifOrientationNormal && width <= options.MaxSide && height <= options.MaxSide {
		// Strip first: metadata alone may have pushed a suitable image over the
		// byte budget. Unfamiliar container layouts fall back to re-encoding.
		cleaned, stripErr := stripImageMetadata(data, format)
		if stripErr == nil && len(cleaned) <= options.MaxBytes {
			normalized = bytes.Clone(cleaned)
		}
	}
	if normalized == nil {
		normalized, width, height, err = normalizeImage(ctx, oriented, canonicalFormat, width, height, options)
		if err != nil {
			return Result{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	result.Image = model.ImageContent{MediaType: "image/" + canonicalFormat, Data: normalized, Width: width, Height: height}
	return result, nil
}

func (options Options) withDefaults() (Options, error) {
	if options.MaxSourceBytes < 0 || options.MaxSourcePixels < 0 || options.MaxSide < 0 || options.MaxBytes < 0 {
		return Options{}, fmt.Errorf("image limits cannot be negative")
	}
	if options.MaxSourceBytes == 0 {
		options.MaxSourceBytes = DefaultMaxSourceBytes
	}
	if options.MaxSourcePixels == 0 {
		options.MaxSourcePixels = 40_000_000
	}
	if options.MaxSide == 0 {
		options.MaxSide = 2_000
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = limits.MaxContentImageBytes / 2
	}
	if options.MaxSourcePixels > limits.MaxImagePixels || options.MaxSide > limits.MaxImageDimension || options.MaxBytes > limits.MaxContentImageBytes {
		return Options{}, fmt.Errorf("image limits exceed supported pixels, dimensions, or bytes")
	}
	return options, nil
}

func canonicalImageFormat(sourceFormat string) string {
	switch sourceFormat {
	case "png", "gif":
		return "png"
	case "jpeg", "webp":
		return "jpeg"
	default:
		return ""
	}
}

func normalizeImage(ctx context.Context, source image.Image, format string, sourceWidth, sourceHeight int, options Options) ([]byte, int, int, error) {
	width, height := fittedImageDimensions(sourceWidth, sourceHeight, options.MaxSide)
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		resized := source
		if width != sourceWidth || height != sourceHeight || source.Bounds().Min.X != 0 || source.Bounds().Min.Y != 0 {
			target := image.NewNRGBA(image.Rect(0, 0, width, height))
			xdraw.CatmullRom.Scale(target, target.Bounds(), source, source.Bounds(), xdraw.Src, nil)
			resized = target
		}
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		if format == "jpeg" {
			resized = imageOnWhite(resized)
		}
		encoded, err := encodeNormalizedImage(ctx, resized, format, options.MaxBytes)
		if err != nil {
			return nil, 0, 0, err
		}
		if len(encoded) <= options.MaxBytes {
			return encoded, width, height, nil
		}
		factor := math.Sqrt(float64(options.MaxBytes)/float64(len(encoded))) * 0.9
		nextWidth := max(1, int(float64(width)*factor))
		nextHeight := max(1, int(float64(height)*factor))
		if nextWidth >= width && nextHeight >= height {
			return nil, 0, 0, fmt.Errorf("normalized image exceeds %d-byte limit", options.MaxBytes)
		}
		width, height = nextWidth, nextHeight
	}
}

func encodeNormalizedImage(ctx context.Context, source image.Image, format string, maxBytes int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if format == "png" {
		var encoded bytes.Buffer
		if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&encoded, source); err != nil {
			return nil, fmt.Errorf("encode normalized image: %w", err)
		}
		return encoded.Bytes(), nil
	}
	var smallest []byte
	for _, quality := range [...]int{85, 80, 75} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, source, &jpeg.Options{Quality: quality}); err != nil {
			return nil, fmt.Errorf("encode normalized image: %w", err)
		}
		candidate := encoded.Bytes()
		if len(candidate) <= maxBytes {
			return candidate, nil
		}
		if len(smallest) == 0 || len(candidate) < len(smallest) {
			smallest = candidate
		}
	}
	return smallest, nil
}

func imageOnWhite(source image.Image) image.Image {
	bounds := source.Bounds()
	target := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	stddraw.Draw(target, target.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)
	stddraw.Draw(target, target.Bounds(), source, bounds.Min, stddraw.Over)
	return target
}

func fittedImageDimensions(width, height, maxSide int) (int, int) {
	if width <= maxSide && height <= maxSide {
		return width, height
	}
	if width >= height {
		return maxSide, max(1, height*maxSide/width)
	}
	return max(1, width*maxSide/height), maxSide
}
