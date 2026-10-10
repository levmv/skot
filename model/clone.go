package model

func cloneItems(items []Item) []Item {
	cloned := make([]Item, len(items))
	for i, item := range items {
		cloned[i] = item.Clone()
	}
	return cloned
}

// Clone returns an independent copy, including opaque state and image bytes.
func (item Item) Clone() Item {
	item.Content = item.Content.Clone()
	if item.ProviderContext != nil {
		context := *item.ProviderContext
		item.ProviderContext = &context
	}
	item.ProviderData = cloneProviderData(item.ProviderData)
	if item.ToolCall != nil {
		call := item.ToolCall.Clone()
		item.ToolCall = &call
	}
	item.ToolResult = item.ToolResult.Clone()
	item.Details = cloneDetails(item.Details)
	return item
}

// Clone returns an independent copy of a call and its provider references.
func (call ToolCall) Clone() ToolCall {
	call.ProviderReferences = append([]ProviderReference(nil), call.ProviderReferences...)
	for i := range call.ProviderReferences {
		call.ProviderReferences[i].Data = call.ProviderReferences[i].Data.Clone()
	}
	return call
}

// Clone returns an independent copy, or nil for a nil result.
func (result *ToolResult) Clone() *ToolResult {
	if result == nil {
		return nil
	}
	cloned := *result
	cloned.Content = result.Content.Clone()
	cloned.Details = cloneDetails(result.Details)
	return &cloned
}

func cloneDetails(details []Detail) []Detail {
	if len(details) == 0 {
		return nil
	}
	cloned := make([]Detail, len(details))
	for index, detail := range details {
		cloned[index] = Detail{Kind: detail.Kind, Data: detail.Data.Clone()}
	}
	return cloned
}
