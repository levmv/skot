package model

import (
	"encoding/json/v2"
	"testing"
)

func TestUserContentOwnershipAndModelSwitch(t *testing.T) {
	original := Item{Kind: ItemUserText, Content: ImageToolContent("question", ImageContent{
		MediaType: "image/png", Data: []byte{1, 2, 3}, Width: 2, Height: 1,
	})}
	cloned := original.Clone()
	cloned.Content[0].Text = "changed"
	cloned.Content[1].Image.Width = 9
	cloned.Content[1].Image.Data[0] = 9
	if original.Content[0].Text != "question" || original.Content[1].Image.Width != 2 || original.Content[1].Image.Data[0] != 1 {
		t.Fatal("cloning changed caller-owned user content")
	}
	current, err := (ReplayContext{}).ForModel(Info{BackendID: "first", Provider: "test", Model: "first"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := current.ForModel(Info{BackendID: "second", Provider: "test", Model: "second"})
	if err != nil || next.Epoch == current.Epoch {
		t.Fatalf("model switch: %+v, %v", next, err)
	}
	request := next.PrepareRequest(nil, Request{Items: []Item{original}})
	if len(request.Items) != 1 || !request.Items[0].Content.HasImage() || request.Items[0].Content.Text() != "question" {
		t.Fatal("model switch lost user content")
	}
	request.Items[0].Content[1].Image.Data[0] = 9
	if original.Content[1].Image.Data[0] != 1 {
		t.Fatal("request projection aliases saved user image")
	}
}

func TestLegacyUserTextJSON(t *testing.T) {
	const legacy = `{"kind":"user_text","text":"hello"}`
	var item Item
	if err := json.Unmarshal([]byte(legacy), &item); err != nil {
		t.Fatal(err)
	}
	if item.Content != nil || item.UserContent().Text() != "hello" {
		t.Fatalf("legacy user = %+v", item)
	}
	encoded, err := json.Marshal(item)
	if err != nil || string(encoded) != legacy {
		t.Fatalf("legacy encoding = %s, %v", encoded, err)
	}
}
