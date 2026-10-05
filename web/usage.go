package web

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// Usage contains provider-reported accounting data. Empty fields mean unknown,
// not zero. Amounts are decimal strings to preserve the reported precision.
// Inspect Usage even when Search or Fetch returns an error.
type Usage struct {
	// Credits is the service's own billing unit, reported by Tavily and Firecrawl.
	Credits string `json:"credits,omitempty"`
	// EstimatedCostUSD is Exa's costDollars.total. Exa documents it as an
	// estimate, not an account charge; billing uses separate usage counters.
	EstimatedCostUSD string `json:"estimated_cost_usd,omitempty"`
	RequestID        string `json:"request_id,omitempty"`
}

func (client *Client) readUsage(raw []byte) Usage {
	switch client.provider {
	case "tavily":
		var payload struct {
			RequestID string `json:"request_id"`
			Usage     struct {
				Credits jsontext.Value `json:"credits"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			return Usage{Credits: reportedNumber(payload.Usage.Credits), RequestID: payload.RequestID}
		}
	case "exa":
		var payload struct {
			RequestID   string `json:"requestId"`
			CostDollars struct {
				Total jsontext.Value `json:"total"`
			} `json:"costDollars"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			return Usage{EstimatedCostUSD: reportedNumber(payload.CostDollars.Total), RequestID: payload.RequestID}
		}
	case "firecrawl":
		// https://github.com/firecrawl/firecrawl/blob/main/apps/js-sdk/firecrawl/src/v2/types.ts
		var payload struct {
			Data struct {
				Metadata struct {
					ScrapeID    string         `json:"scrapeId"`
					CreditsUsed jsontext.Value `json:"creditsUsed"`
				} `json:"metadata"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			return Usage{Credits: reportedNumber(payload.Data.Metadata.CreditsUsed), RequestID: payload.Data.Metadata.ScrapeID}
		}
	}
	return Usage{}
}

func reportedNumber(raw jsontext.Value) string {
	if raw.Kind() == '0' {
		return strings.TrimSpace(string(raw))
	}
	return ""
}
