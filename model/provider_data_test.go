package model

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
)

func TestValidateProviderDataRejectsInvalidDuplicateAndOversizedData(t *testing.T) {
	for _, entries := range [][]ProviderData{
		{{Kind: "", Data: jsontext.Value(`{}`)}},
		{{Kind: "broken", Data: jsontext.Value(`{`)}},
		{{Kind: "same", Data: jsontext.Value(`1`)}, {Kind: "same", Data: jsontext.Value(`2`)}},
		{{Kind: "large", Data: jsontext.Value(`"` + strings.Repeat("x", maxProviderDataBytes) + `"`)}},
	} {
		if err := ValidateProviderData(entries); err == nil {
			t.Fatalf("invalid provider data was accepted: %#v", entries)
		}
	}
}

func TestAcceptedReasoningOwnsNormalizedProviderData(t *testing.T) {
	data := jsontext.Value(`{"encrypted_content":"ciphertext"}`)
	context := ReplayContext{Backend: "responses.openai", Epoch: "epoch_1"}
	accepted, err := context.AcceptResponse(Response{Items: []Item{{
		Kind: ItemReasoning, Text: "summary",
		ProviderData: []ProviderData{{Kind: " responses.reasoning_item ", Data: data}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	data[2] = 'X'
	item := accepted.Items[0]
	if item.ProviderContext == nil || item.ProviderContext.Backend != "responses.openai" || item.ProviderContext.Epoch != "epoch_1" ||
		len(item.ProviderData) != 1 || item.ProviderData[0].Kind != "responses.reasoning_item" || string(item.ProviderData[0].Data) != `{"encrypted_content":"ciphertext"}` {
		t.Fatalf("accepted reasoning = %#v", item)
	}
}
