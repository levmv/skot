// Package modelinput checks semantic input shared by the protocol adapters.
package modelinput

import (
	"fmt"

	"github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/model"
)

// ValidateContent borrows request without modifying or retaining it. It runs
// before wire encoding so rejected input does not allocate base64 payloads.
func ValidateContent(request model.Request, imageInputUnsupported bool) error {
	images, imageBytes := 0, 0
	for index, item := range request.Items {
		if item.Content != nil && (item.Kind != model.ItemUserText || item.Text != "") {
			return fmt.Errorf("item %d: Content requires a user item with empty Text", index)
		}
		var content model.Content
		switch item.Kind {
		case model.ItemUserText:
			if item.ToolResult != nil || item.ToolCall != nil {
				return fmt.Errorf("user item %d has unrelated tool payload", index)
			}
			content = item.UserContent()
		case model.ItemToolResult:
			if item.ToolResult == nil {
				return fmt.Errorf("tool result item %d is invalid", index)
			}
			content = item.ToolResult.Content
		}
		if err := model.ValidateContent(content); err != nil {
			return fmt.Errorf("item %d: %w", index, err)
		}
		for _, part := range content {
			if part.Kind != model.ContentPartImage {
				continue
			}
			if imageInputUnsupported {
				return model.ErrImageInputUnsupported
			}
			images++
			if images > limits.MaxRequestImages {
				return fmt.Errorf("%w: request has more than %d images", model.ErrModelRequestTooLarge, limits.MaxRequestImages)
			}
			if imageBytes > limits.MaxContentImageBytes-len(part.Image.Data) {
				return fmt.Errorf("%w: request image bytes exceed %d", model.ErrModelRequestTooLarge, limits.MaxContentImageBytes)
			}
			imageBytes += len(part.Image.Data)
		}
	}
	return nil
}
