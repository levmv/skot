package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"strings"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	maxSourceImageBytes     = 20 << 20
	maxSourceImagePixels    = 40_000_000
	maxSourceImageDimension = 32_768
	maxNormalizedImageSide  = 2_000
	// Leave room for two maximum-sized read results in one canonical content
	// value while the agent enforces the aggregate emergency ceiling.
	maxNormalizedImageBytes = productlimits.MaxContentImageBytes / 2
)

var jpegImageQualities = [...]int{85, 80, 75}

// readImageFile reports recognized=false when path does not contain a known
// image signature. Recognized images own unsupported-format and decode errors
// so image files cannot fall through to the UTF-8 reader.
func readImageFile(ctx context.Context, path, display string) (output agent.ToolOutput, recognized bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return agent.ToolOutput{}, false, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return agent.ToolOutput{}, false, err
	}
	var sniff [512]byte
	count, readErr := io.ReadFull(file, sniff[:])
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return agent.ToolOutput{}, false, readErr
	}
	mediaType := strings.TrimSpace(strings.SplitN(http.DetectContentType(sniff[:count]), ";", 2)[0])
	if mediaType != "image/png" && mediaType != "image/jpeg" && mediaType != "image/gif" && mediaType != "image/webp" {
		if strings.HasPrefix(mediaType, "image/") {
			return agent.ToolOutput{}, true, fmt.Errorf("unsupported image format %q; read supports PNG, JPEG, GIF, and WebP", mediaType)
		}
		return agent.ToolOutput{}, false, nil
	}
	if info.Size() > maxSourceImageBytes {
		return agent.ToolOutput{}, true, fmt.Errorf("image exceeds %d-byte source limit", maxSourceImageBytes)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return agent.ToolOutput{}, true, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSourceImageBytes+1))
	if err != nil {
		return agent.ToolOutput{}, true, err
	}
	if len(data) > maxSourceImageBytes {
		return agent.ToolOutput{}, true, fmt.Errorf("image exceeds %d-byte source limit", maxSourceImageBytes)
	}
	if err := ctx.Err(); err != nil {
		return agent.ToolOutput{}, true, err
	}

	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return agent.ToolOutput{}, true, fmt.Errorf("decode image metadata: %w", err)
	}
	canonicalFormat := canonicalImageFormat(format)
	if canonicalFormat == "" {
		return agent.ToolOutput{}, true, fmt.Errorf("unsupported decoded image format %q", format)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxSourceImageDimension || config.Height > maxSourceImageDimension || config.Width > maxSourceImagePixels/config.Height {
		return agent.ToolOutput{}, true, fmt.Errorf("image dimensions %dx%d exceed safety limits", config.Width, config.Height)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return agent.ToolOutput{}, true, fmt.Errorf("decode image: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return agent.ToolOutput{}, true, err
	}

	orientation := imageOrientation(data, format)
	oriented := applyImageOrientation(decoded, orientation)
	sourceWidth, sourceHeight := oriented.Bounds().Dx(), oriented.Bounds().Dy()
	normalized, width, height := data, sourceWidth, sourceHeight
	if canonicalFormat != format || orientation != exifOrientationNormal || sourceWidth > maxNormalizedImageSide || sourceHeight > maxNormalizedImageSide || len(data) > maxNormalizedImageBytes {
		normalized, width, height, err = normalizeImage(ctx, oriented, canonicalFormat, sourceWidth, sourceHeight)
		if err != nil {
			return agent.ToolOutput{}, true, err
		}
	} else {
		normalized, err = stripImageMetadata(data, format)
		if err != nil {
			// Metadata parsing is deliberately stricter than raster decoding. If a
			// valid image uses an unfamiliar container layout, re-encoding still
			// gives the model safe canonical pixels instead of rejecting the file.
			normalized, width, height, err = normalizeImage(ctx, oriented, canonicalFormat, sourceWidth, sourceHeight)
			if err != nil {
				return agent.ToolOutput{}, true, err
			}
		}
	}
	normalizedType := "image/" + canonicalFormat
	digest := sha256.Sum256(data)
	metadata := fmt.Sprintf(
		"path: %s\nsha256: %x\nmedia_type: %s\nsource_size: %dx%d\nimage_size: %dx%d\n\nImage content follows.",
		display, digest, normalizedType, sourceWidth, sourceHeight, width, height,
	)
	return agent.ToolOutput{Content: agent.ImageToolContent(metadata, agent.ImageContent{
		MediaType: normalizedType, Data: normalized, Width: width, Height: height,
	})}, true, nil
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

func normalizeImage(ctx context.Context, source image.Image, format string, sourceWidth, sourceHeight int) ([]byte, int, int, error) {
	width, height := fittedImageDimensions(sourceWidth, sourceHeight, maxNormalizedImageSide)
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
		if format == "jpeg" {
			resized = imageOnWhite(resized)
		}
		encoded, err := encodeNormalizedImage(resized, format)
		if err != nil {
			return nil, 0, 0, err
		}
		if len(encoded) <= maxNormalizedImageBytes {
			return encoded, width, height, nil
		}
		factor := math.Sqrt(float64(maxNormalizedImageBytes)/float64(len(encoded))) * 0.9
		nextWidth := max(1, int(float64(width)*factor))
		nextHeight := max(1, int(float64(height)*factor))
		if nextWidth >= width && nextHeight >= height {
			return nil, 0, 0, fmt.Errorf("normalized image exceeds %d-byte limit", maxNormalizedImageBytes)
		}
		width, height = nextWidth, nextHeight
	}
}

func encodeNormalizedImage(source image.Image, format string) ([]byte, error) {
	if format == "png" {
		var encoded bytes.Buffer
		if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&encoded, source); err != nil {
			return nil, fmt.Errorf("encode normalized image: %w", err)
		}
		return encoded.Bytes(), nil
	}
	if format != "jpeg" {
		return nil, fmt.Errorf("unsupported image normalization format %q", format)
	}
	var smallest []byte
	for _, quality := range jpegImageQualities {
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, source, &jpeg.Options{Quality: quality}); err != nil {
			return nil, fmt.Errorf("encode normalized image: %w", err)
		}
		candidate := encoded.Bytes()
		if len(candidate) <= maxNormalizedImageBytes {
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
