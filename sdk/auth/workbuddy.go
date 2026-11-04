package auth

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type WorkBuddyAuthenticator struct {
	provider string
}

func NewWorkBuddyAuthenticator(provider string) *WorkBuddyAuthenticator {
	return &WorkBuddyAuthenticator{provider: provider}
}

func (a *WorkBuddyAuthenticator) Provider() string { return a.provider }

func (a *WorkBuddyAuthenticator) RefreshLead() *time.Duration {
	lead := workbuddy.RefreshLead
	return &lead
}

func (a *WorkBuddyAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	client := util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second})
	service := workbuddy.NewService(a.provider, client)
	session, err := service.Start(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Open this URL to sign in to %s:\n%s\n", workbuddy.SiteForProvider(a.provider).Name, session.URL)
	if opts == nil || !opts.NoBrowser {
		if browser.IsAvailable() {
			if errOpen := browser.OpenURL(session.URL); errOpen != nil {
				log.WithError(errOpen).Warn("workbuddy: open authorization page")
			}
		}
	}
	creds, err := service.Wait(ctx, session)
	if err != nil {
		return nil, err
	}
	return workbuddy.NewAuthRecord(a.provider, creds), nil
}
