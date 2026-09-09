package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"time"

	"github.com/levmv/skot/internal/codexauth"
)

const (
	accountQuotaRefreshInterval = time.Minute
	accountQuotaMaxAge          = 5 * time.Minute
	accountQuotaTimeout         = 15 * time.Second
)

// Account metadata stays outside the agent journal. The credential fingerprint
// also detects another Skot process replacing the login; tokens are not cached.
type accountQuotaCache struct {
	value      AccountQuota
	credential [sha256.Size]byte
	attempted  time.Time
	generation uint64
}

// AccountQuota returns a fresh allowance for the selected provider, if known.
// It never performs I/O and is safe while a refresh or model request is running.
func (application *Application) AccountQuota() AccountQuota {
	provider, _, _ := parseModelURI(application.CurrentModel())
	if provider != codexauth.Provider {
		return AccountQuota{}
	}
	application.mu.RLock()
	quota := application.state.accountQuota.value
	application.mu.RUnlock()
	now := time.Now()
	if quota.Window <= 0 || quota.ObservedAt.IsZero() || now.Before(quota.ObservedAt) ||
		now.Sub(quota.ObservedAt) >= accountQuotaMaxAge || !now.Before(quota.ResetsAt) {
		return AccountQuota{}
	}
	return quota
}

// RefreshAccountQuota performs best-effort account I/O. Frontends call it in
// the background; attempts are rate-limited and concurrent refreshes coalesce.
// A failed refresh leaves the last observation usable until it expires.
func (application *Application) RefreshAccountQuota(ctx context.Context) error {
	return application.refreshAccountQuota(ctx, nil)
}

func (application *Application) refreshAccountQuota(ctx context.Context, client *http.Client) error {
	if !application.quotaRefreshMu.TryLock() {
		return nil
	}
	defer application.quotaRefreshMu.Unlock()
	model := application.CurrentModel()
	provider, _, _ := parseModelURI(model)
	if provider != codexauth.Provider {
		return nil
	}
	tokens, err := storedCodexTokens(application.config.settings)
	if err != nil || !tokens.Valid() {
		application.invalidateAccountQuota()
		return err
	}
	credential := sha256.Sum256([]byte(tokens.AccessToken))
	now := time.Now()
	application.mu.Lock()
	cache := &application.state.accountQuota
	if cache.credential != credential {
		*cache = accountQuotaCache{credential: credential, generation: cache.generation + 1}
	}
	if now.Sub(cache.attempted) < accountQuotaRefreshInterval {
		application.mu.Unlock()
		return nil
	}
	cache.attempted = now
	generation := cache.generation
	application.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, accountQuotaTimeout)
	defer cancel()
	authorizer := codexAuthorizer{
		store: application.config.settings, modelURI: model, client: client, masker: application.config.masker,
	}
	quota, usedCredential, err := readCodexAccountQuota(ctx, authorizer)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New(application.config.masker.Redact(err.Error()))
	}
	// Refresh may have rotated the token. A different login or logout while the
	// request was running makes its result obsolete, even if HTTP succeeded.
	current, err := storedCodexTokens(application.config.settings)
	if err != nil || !current.Valid() || sha256.Sum256([]byte(current.AccessToken)) != usedCredential {
		return err
	}
	application.mu.Lock()
	defer application.mu.Unlock()
	if application.state.session != nil && application.state.accountQuota.generation == generation {
		application.state.accountQuota.value = quota
		application.state.accountQuota.credential = usedCredential
	}
	return nil
}

func (application *Application) invalidateAccountQuota() {
	application.mu.Lock()
	defer application.mu.Unlock()
	application.state.accountQuota = accountQuotaCache{generation: application.state.accountQuota.generation + 1}
}
