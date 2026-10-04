package agent

import modelapi "github.com/levmv/skot/model"

// cloneContentForProjection copies part and image headers while sharing image
// bytes owned by the runtime. Projection only changes structure and text, so
// context estimates and requests can reuse the image payloads.
func cloneContentForProjection(content modelapi.Content) modelapi.Content {
	if content == nil {
		return nil
	}
	cloned := make(modelapi.Content, len(content))
	for index, part := range content {
		cloned[index] = part
		if part.Image != nil {
			image := *part.Image
			cloned[index].Image = &image
		}
	}
	return cloned
}
