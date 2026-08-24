package tools

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
)

const (
	exifOrientationNormal = 1
	exifOrientationTag    = 0x0112
	pngSignature          = "\x89PNG\r\n\x1a\n"
)

// imageOrientation reads the one piece of image metadata that changes pixel
// meaning. Malformed or unsupported metadata is ignored: a valid raster image
// must not become unreadable because an optional metadata block is broken.
func imageOrientation(data []byte, format string) int {
	var exif []byte
	switch format {
	case "jpeg":
		exif = jpegEXIF(data)
	case "png":
		exif = pngEXIF(data)
	case "webp":
		exif = webpEXIF(data)
	}
	if len(exif) >= 6 && string(exif[:6]) == "Exif\x00\x00" {
		exif = exif[6:]
	}
	orientation, ok := tiffOrientation(exif)
	if !ok || orientation < 1 || orientation > 8 {
		return exifOrientationNormal
	}
	return orientation
}

func tiffOrientation(data []byte) (int, bool) {
	if len(data) < 8 {
		return 0, false
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, false
	}
	if order.Uint16(data[2:4]) != 42 {
		return 0, false
	}
	offset := uint64(order.Uint32(data[4:8]))
	if offset > uint64(len(data)-2) {
		return 0, false
	}
	count := uint64(order.Uint16(data[offset : offset+2]))
	entries := offset + 2
	if count > (uint64(len(data))-entries)/12 {
		return 0, false
	}
	for index := uint64(0); index < count; index++ {
		entry := data[entries+index*12 : entries+(index+1)*12]
		if order.Uint16(entry[:2]) != exifOrientationTag {
			continue
		}
		if order.Uint16(entry[2:4]) != 3 || order.Uint32(entry[4:8]) != 1 {
			return 0, false
		}
		return int(order.Uint16(entry[8:10])), true
	}
	return 0, false
}

func jpegEXIF(data []byte) []byte {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return nil
	}
	for offset := 2; offset < len(data); {
		_, marker, payload, next, ok := nextJPEGSegment(data, offset)
		if !ok || marker == 0xda || marker == 0xd9 {
			return nil
		}
		if marker == 0xe1 && len(payload) >= 6 && string(payload[:6]) == "Exif\x00\x00" {
			return payload
		}
		offset = next
	}
	return nil
}

func pngEXIF(data []byte) []byte {
	if len(data) < len(pngSignature) || string(data[:len(pngSignature)]) != pngSignature {
		return nil
	}
	for offset := len(pngSignature); offset+12 <= len(data); {
		length := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		end := uint64(offset) + 12 + length
		if end > uint64(len(data)) {
			return nil
		}
		if string(data[offset+4:offset+8]) == "eXIf" {
			return data[offset+8 : int(end)-4]
		}
		offset = int(end)
	}
	return nil
}

func webpEXIF(data []byte) []byte {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil
	}
	for offset := 12; offset+8 <= len(data); {
		length := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		end := uint64(offset) + 8 + length
		if end > uint64(len(data)) {
			return nil
		}
		if string(data[offset:offset+4]) == "EXIF" {
			return data[offset+8 : int(end)]
		}
		offset = int(end + length%2)
	}
	return nil
}

// stripImageMetadata removes information that is neither pixels nor required
// to render them. Color-description chunks and JPEG color-transform markers
// remain intact; orientation is applied to pixels before this function is used.
func stripImageMetadata(data []byte, format string) ([]byte, error) {
	switch format {
	case "jpeg":
		return stripJPEGMetadata(data)
	case "png":
		return stripPNGMetadata(data)
	default:
		return data, nil
	}
}

func stripJPEGMetadata(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return nil, fmt.Errorf("invalid JPEG marker stream")
	}
	output := make([]byte, 0, len(data))
	output = append(output, data[:2]...)
	removed := false
	for offset := 2; offset < len(data); {
		start, marker, payload, next, ok := nextJPEGSegment(data, offset)
		if !ok {
			return nil, fmt.Errorf("invalid JPEG marker stream")
		}
		if marker == 0xd9 {
			output = append(output, data[start:next]...)
			if next != len(data) {
				removed = true
			}
			if !removed {
				return data, nil
			}
			return output, nil
		}
		if marker == 0xda {
			output = append(output, data[start:next]...)
			scanEnd := nextJPEGScanMarker(data, next)
			if scanEnd < 0 {
				return nil, fmt.Errorf("JPEG scan has no terminating marker")
			}
			output = append(output, data[next:scanEnd]...)
			offset = scanEnd
			continue
		}
		if keepJPEGSegment(marker, payload) {
			output = append(output, data[start:next]...)
		} else {
			removed = true
		}
		offset = next
	}
	return nil, fmt.Errorf("JPEG marker stream has no image data")
}

func nextJPEGScanMarker(data []byte, offset int) int {
	for offset < len(data)-1 {
		if data[offset] != 0xff {
			offset++
			continue
		}
		start := offset
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset >= len(data) {
			return -1
		}
		marker := data[offset]
		if marker == 0x00 || marker >= 0xd0 && marker <= 0xd7 {
			offset++
			continue
		}
		return start
	}
	return -1
}

func nextJPEGSegment(data []byte, offset int) (start int, marker byte, payload []byte, next int, ok bool) {
	if offset >= len(data) || data[offset] != 0xff {
		return 0, 0, nil, 0, false
	}
	start = offset
	for offset < len(data) && data[offset] == 0xff {
		offset++
	}
	if offset >= len(data) || data[offset] == 0x00 {
		return 0, 0, nil, 0, false
	}
	marker = data[offset]
	offset++
	if marker == 0xd8 || marker == 0xd9 || marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
		return start, marker, nil, offset, true
	}
	if offset+2 > len(data) {
		return 0, 0, nil, 0, false
	}
	length := int(binary.BigEndian.Uint16(data[offset : offset+2]))
	if length < 2 || offset > len(data)-length {
		return 0, 0, nil, 0, false
	}
	next = offset + length
	return start, marker, data[offset+2 : next], next, true
}

func keepJPEGSegment(marker byte, payload []byte) bool {
	if marker == 0xfe {
		return false
	}
	if marker < 0xe0 || marker > 0xef {
		return true
	}
	switch marker {
	case 0xe0:
		// Keep only the ordinary thumbnail-free JFIF header. JFXX and JFIF
		// thumbnails are nonessential embedded images rather than raster data.
		return len(payload) == 14 && string(payload[:5]) == "JFIF\x00" && payload[12] == 0 && payload[13] == 0
	case 0xe2: // Preserve ICC, but not unrelated APP2 payloads such as FlashPix.
		return len(payload) >= 12 && string(payload[:12]) == "ICC_PROFILE\x00"
	case 0xee: // Adobe color-transform marker is needed for CMYK/YCCK interpretation.
		return true
	default:
		return false
	}
}

func stripPNGMetadata(data []byte) ([]byte, error) {
	if len(data) < len(pngSignature) || string(data[:len(pngSignature)]) != pngSignature {
		return nil, fmt.Errorf("invalid PNG signature")
	}
	output := make([]byte, 0, len(data))
	output = append(output, data[:len(pngSignature)]...)
	removed := false
	for offset := len(pngSignature); offset+12 <= len(data); {
		length := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		end := uint64(offset) + 12 + length
		if end > uint64(len(data)) {
			return nil, fmt.Errorf("invalid PNG chunk stream")
		}
		chunkType := string(data[offset+4 : offset+8])
		if keepPNGChunk(chunkType) {
			output = append(output, data[offset:int(end)]...)
		} else {
			removed = true
		}
		offset = int(end)
		if chunkType == "IEND" {
			if offset != len(data) {
				removed = true
			}
			if !removed {
				return data, nil
			}
			return output, nil
		}
	}
	return nil, fmt.Errorf("PNG chunk stream has no IEND")
}

func keepPNGChunk(chunkType string) bool {
	if len(chunkType) != 4 {
		return false
	}
	// Critical chunks carry the actual raster. The small whitelist of ancillary
	// chunks below changes color or transparency interpretation; everything
	// else is presentation advice, animation, text, time, EXIF, or private data.
	if chunkType[0] >= 'A' && chunkType[0] <= 'Z' {
		return true
	}
	switch chunkType {
	case "cHRM", "gAMA", "iCCP", "sBIT", "sRGB", "tRNS":
		return true
	default:
		return false
	}
}

type orientedImage struct {
	source      image.Image
	orientation int
	bounds      image.Rectangle
}

func applyImageOrientation(source image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return source
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if orientation >= 5 {
		width, height = height, width
	}
	return orientedImage{source: source, orientation: orientation, bounds: image.Rect(0, 0, width, height)}
}

func (oriented orientedImage) ColorModel() color.Model { return oriented.source.ColorModel() }

func (oriented orientedImage) Bounds() image.Rectangle { return oriented.bounds }

func (oriented orientedImage) At(x, y int) color.Color {
	if !image.Pt(x, y).In(oriented.bounds) {
		return color.NRGBA{}
	}
	sourceBounds := oriented.source.Bounds()
	width, height := sourceBounds.Dx(), sourceBounds.Dy()
	var sourceX, sourceY int
	switch oriented.orientation {
	case 2:
		sourceX, sourceY = width-1-x, y
	case 3:
		sourceX, sourceY = width-1-x, height-1-y
	case 4:
		sourceX, sourceY = x, height-1-y
	case 5:
		sourceX, sourceY = y, x
	case 6:
		sourceX, sourceY = y, height-1-x
	case 7:
		sourceX, sourceY = width-1-y, height-1-x
	case 8:
		sourceX, sourceY = width-1-y, x
	default:
		sourceX, sourceY = x, y
	}
	return oriented.source.At(sourceBounds.Min.X+sourceX, sourceBounds.Min.Y+sourceY)
}
