package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/internal/network"
	"github.com/levmv/skot/model/transport"
)

type networkClients struct {
	model   *http.Client
	service *http.Client
}

func newNetworkClients(proxyURL string) (networkClients, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return networkClients{}, nil
	}
	proxy, err := network.Proxy(proxyURL)
	if err != nil {
		return networkClients{}, err
	}
	return networkClients{
		model:   transport.NewHTTPClient(transport.HTTPClientConfig{Proxy: proxy}),
		service: network.NewHTTPClient(proxy),
	}, nil
}

func (clients networkClients) contextWindow(ctx context.Context, modelID string) (int, error) {
	if clients.service == nil {
		return modelconfig.OpenRouterContextWindow(ctx, modelID)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return modelconfig.FetchOpenRouterContextWindow(ctx, clients.service, modelconfig.OpenRouterModelsURL, modelID)
}

func (clients networkClients) close() {
	for _, client := range []*http.Client{clients.model, clients.service} {
		if client != nil {
			client.CloseIdleConnections()
		}
	}
}
