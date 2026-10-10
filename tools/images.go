package tools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/imageinput"
	"github.com/levmv/skot/model"
)

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
	if info.Size() > imageinput.DefaultMaxSourceBytes {
		return agent.ToolOutput{}, true, fmt.Errorf("image exceeds %d-byte source limit", imageinput.DefaultMaxSourceBytes)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return agent.ToolOutput{}, true, err
	}
	data, err := io.ReadAll(io.LimitReader(file, imageinput.DefaultMaxSourceBytes+1))
	if err != nil {
		return agent.ToolOutput{}, true, err
	}
	if len(data) > imageinput.DefaultMaxSourceBytes {
		return agent.ToolOutput{}, true, fmt.Errorf("image exceeds %d-byte source limit", imageinput.DefaultMaxSourceBytes)
	}
	prepared, err := imageinput.Prepare(ctx, data, imageinput.Options{})
	if err != nil {
		return agent.ToolOutput{}, true, err
	}
	digest := sha256.Sum256(data)
	metadata := fmt.Sprintf(
		"path: %s\nsha256: %x\nmedia_type: %s\nsource_size: %dx%d\nimage_size: %dx%d\n\nImage content follows.",
		display, digest, prepared.Image.MediaType, prepared.SourceWidth, prepared.SourceHeight, prepared.Image.Width, prepared.Image.Height,
	)
	return agent.ToolOutput{Content: model.ImageToolContent(metadata, prepared.Image)}, true, nil
}
