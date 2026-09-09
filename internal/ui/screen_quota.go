package ui

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/app"
)

type accountQuotaUpdatedMsg struct{}
type accountQuotaTickMsg struct{}

func (m screenModel) refreshAccountQuota() tea.Cmd {
	client, ctx := m.agent, m.ctx
	return func() tea.Msg {
		// The application throttles network requests. Checking more often lets
		// login/model changes appear promptly and expired observations disappear.
		_ = client.RefreshAccountQuota(ctx)
		return accountQuotaUpdatedMsg{}
	}
}

func compactAccountQuota(quota app.AccountQuota) string {
	if quota.Window != 7*24*time.Hour {
		return ""
	}
	if quota.RemainingPercent > 0 && quota.RemainingPercent < 1 {
		return "week <1% left"
	}
	return fmt.Sprintf("week %d%% left", int(quota.RemainingPercent))
}
