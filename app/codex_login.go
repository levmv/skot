package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/levmv/skot/internal/codexauth"
	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/internal/state"
)

// BrowserLogin stages a browser authorization without changing stored
// credentials. Call Wait once, then Complete after success to save the login.
// SubmitRedirect is safe while Wait is running. Close cancels outstanding work
// and must also be called when a completed login is abandoned.
type BrowserLogin interface {
	AuthorizationURL() string
	SubmitRedirect(string) error
	Wait() error
	Complete(context.Context) error
	Close()
}

type codexBrowserLogin struct {
	flow        *codexauth.Login
	ctx         context.Context
	cancel      context.CancelFunc
	application *Application
	tokens      codexauth.Tokens
}

func (application *Application) BeginBrowserLogin(ctx context.Context, provider string) (BrowserLogin, error) {
	if _, err := application.requireRuntime(); err != nil {
		return nil, err
	}
	if strings.ToLower(strings.TrimSpace(provider)) != codexauth.Provider {
		return nil, fmt.Errorf("%s does not support browser login", provider)
	}
	if application.config.settings == nil {
		return nil, errors.New("auth store is unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	flow, err := codexauth.Start(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	return &codexBrowserLogin{flow: flow, ctx: ctx, cancel: cancel, application: application}, nil
}

func (login *codexBrowserLogin) AuthorizationURL() string { return login.flow.URL }
func (login *codexBrowserLogin) SubmitRedirect(value string) error {
	return login.flow.SubmitRedirect(value)
}
func (login *codexBrowserLogin) Close() {
	login.cancel()
	login.flow.Close()
}

func (login *codexBrowserLogin) Wait() error {
	tokens, err := login.flow.Wait()
	if err != nil {
		return err
	}
	login.tokens = tokens
	modelconfig.MaskCodexTokens(login.application.config.masker, tokens)
	return nil
}

func (login *codexBrowserLogin) Complete(ctx context.Context) error {
	if err := login.ctx.Err(); err != nil {
		return err
	}
	if !login.tokens.Valid() {
		return errors.New("browser login has not completed")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(login.ctx, cancel)
	defer stop()
	profile, err := modelconfig.CodexProfile(login.tokens)
	if err != nil {
		return err
	}
	return login.application.updateCredential(ctx, codexauth.Provider, func(store *state.Store, provider string) error {
		return store.UpdateCredential(ctx, provider, func(state.CredentialProfile) (state.CredentialProfile, error) {
			if err := ctx.Err(); err != nil {
				return state.CredentialProfile{}, err
			}
			return profile, nil
		})
	})
}
