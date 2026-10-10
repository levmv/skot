package imageinput

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestApplyImageOrientation(t *testing.T) {
	source := image.NewGray(image.Rect(0, 0, 3, 2))
	copy(source.Pix, []uint8{1, 2, 3, 4, 5, 6})
	for _, test := range []struct {
		orientation int
		width       int
		values      []uint8
	}{
		{orientation: 1, width: 3, values: []uint8{1, 2, 3, 4, 5, 6}},
		{orientation: 2, width: 3, values: []uint8{3, 2, 1, 6, 5, 4}},
		{orientation: 3, width: 3, values: []uint8{6, 5, 4, 3, 2, 1}},
		{orientation: 4, width: 3, values: []uint8{4, 5, 6, 1, 2, 3}},
		{orientation: 5, width: 2, values: []uint8{1, 4, 2, 5, 3, 6}},
		{orientation: 6, width: 2, values: []uint8{4, 1, 5, 2, 6, 3}},
		{orientation: 7, width: 2, values: []uint8{6, 3, 5, 2, 4, 1}},
		{orientation: 8, width: 2, values: []uint8{3, 6, 2, 5, 1, 4}},
	} {
		oriented := applyImageOrientation(source, test.orientation)
		if oriented.Bounds().Dx() != test.width || oriented.Bounds().Dy() != len(test.values)/test.width {
			t.Fatalf("orientation %d bounds = %v", test.orientation, oriented.Bounds())
		}
		for index, want := range test.values {
			x, y := index%test.width, index/test.width
			got := color.GrayModel.Convert(oriented.At(x, y)).(color.Gray).Y
			if got != want {
				t.Fatalf("orientation %d pixel %d,%d = %d, want %d", test.orientation, x, y, got, want)
			}
		}
	}
}

func TestPrepareAppliesEXIFOrientation(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 80, 40))
	for y := 0; y < source.Bounds().Dy(); y++ {
		for x := 0; x < source.Bounds().Dx(); x++ {
			pixel := color.NRGBA{R: 255, A: 255}
			if x >= source.Bounds().Dx()/2 {
				pixel = color.NRGBA{B: 255, A: 255}
			}
			source.SetNRGBA(x, y, pixel)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, source, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	withOrientation := insertJPEGSegment(encoded.Bytes(), 0xe1, append([]byte("Exif\x00\x00"), littleEndianOrientation(6)...))
	output, err := Prepare(t.Context(), withOrientation, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if output.SourceWidth != 40 || output.SourceHeight != 80 {
		t.Fatalf("oriented source = %dx%d", output.SourceWidth, output.SourceHeight)
	}
	part := output.Image
	if part.Width != 40 || part.Height != 80 || bytes.Contains(part.Data, []byte("Exif\x00\x00")) {
		t.Fatalf("oriented image = %#v", part)
	}
	decoded, _, err := image.Decode(bytes.NewReader(part.Data))
	if err != nil {
		t.Fatal(err)
	}
	topR, _, topB, _ := decoded.At(20, 20).RGBA()
	bottomR, _, bottomB, _ := decoded.At(20, 60).RGBA()
	if topR <= topB || bottomB <= bottomR {
		t.Fatalf("orientation colors = top(%x,%x) bottom(%x,%x)", topR, topB, bottomR, bottomB)
	}
}

func TestPrepareStripsMetadataBeforeApplyingByteLimit(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 32, 16))
	var cleanJPEG, cleanPNG bytes.Buffer
	if err := jpeg.Encode(&cleanJPEG, source, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&cleanPNG, source); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name             string
		clean, decorated []byte
	}{
		{"jpeg", cleanJPEG.Bytes(), insertJPEGSegment(cleanJPEG.Bytes(), 0xfe, []byte("private comment"))},
		{"png", cleanPNG.Bytes(), insertPNGChunkAfterIHDR(cleanPNG.Bytes(), "tEXt", []byte("Comment\x00private comment"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, data := range [][]byte{test.clean, test.decorated} {
				before := bytes.Clone(data)
				output, err := Prepare(t.Context(), data, Options{MaxBytes: len(test.clean)})
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(output.Image.Data, test.clean) {
					t.Fatal("suitable raster was recompressed")
				}
				output.Image.Data[0] ^= 0xff
				if !bytes.Equal(data, before) {
					t.Fatal("prepared image aliases input bytes")
				}
			}
		})
	}
}

func littleEndianOrientation(orientation uint16) []byte {
	data := make([]byte, 26)
	copy(data[:4], []byte{'I', 'I', 42, 0})
	binary.LittleEndian.PutUint32(data[4:8], 8)
	binary.LittleEndian.PutUint16(data[8:10], 1)
	binary.LittleEndian.PutUint16(data[10:12], exifOrientationTag)
	binary.LittleEndian.PutUint16(data[12:14], 3)
	binary.LittleEndian.PutUint32(data[14:18], 1)
	binary.LittleEndian.PutUint16(data[18:20], orientation)
	return data
}

func insertJPEGSegment(data []byte, marker byte, payload []byte) []byte {
	segment := make([]byte, 4+len(payload))
	segment[0], segment[1] = 0xff, marker
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(payload)+2))
	copy(segment[4:], payload)
	output := make([]byte, 0, len(data)+len(segment))
	output = append(output, data[:2]...)
	output = append(output, segment...)
	return append(output, data[2:]...)
}

func insertPNGChunkAfterIHDR(data []byte, chunkType string, payload []byte) []byte {
	firstChunkEnd := len(pngSignature) + 12 + int(binary.BigEndian.Uint32(data[len(pngSignature):len(pngSignature)+4]))
	chunk := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], chunkType)
	copy(chunk[8:8+len(payload)], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	output := make([]byte, 0, len(data)+len(chunk))
	output = append(output, data[:firstChunkEnd]...)
	output = append(output, chunk...)
	return append(output, data[firstChunkEnd:]...)
}

func TestJPEGNormalizationCompositesTransparencyOnWhite(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 64, 16))
	for y := range 16 {
		for x := 32; x < 64; x++ {
			source.Set(x, y, color.Black)
		}
	}
	data, _, _, err := normalizeImage(t.Context(), source, "jpeg", 64, 16, Options{MaxSide: 2000, MaxBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	whiteR, whiteG, whiteB, _ := decoded.At(4, 8).RGBA()
	blackR, blackG, blackB, _ := decoded.At(60, 8).RGBA()
	if whiteR < 0xe000 || whiteG < 0xe000 || whiteB < 0xe000 || blackR > 0x2000 || blackG > 0x2000 || blackB > 0x2000 {
		t.Fatalf("JPEG matte colors = white(%x,%x,%x) black(%x,%x,%x)", whiteR, whiteG, whiteB, blackR, blackG, blackB)
	}
}
