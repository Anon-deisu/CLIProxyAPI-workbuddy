package auth

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type QoderAuthenticator struct {
	provider string
}

func NewQoderAuthenticator(provider string) *QoderAuthenticator {
	return &QoderAuthenticator{provider: qoder.SiteForProvider(provider).Provider}
}

func (a *QoderAuthenticator) Provider() string { return a.provider }

func (a *QoderAuthenticator) RefreshLead() *time.Duration {
	lead := qoder.RefreshLead
	return &lead
}

func (a *QoderAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("qoder: login requires configuration")
	}
	client := util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second})
	service := qoder.NewService(a.provider, client)
	session, err := service.Start(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Open this URL to sign in to %s:\n%s\n", qoder.SiteForProvider(a.provider).Name, session.URL)
	if opts == nil || !opts.NoBrowser {
		if browser.IsAvailable() {
			if errOpen := browser.OpenURL(session.URL); errOpen != nil {
				log.Warn("qoder: open authorization page failed")
			}
		}
	}
	creds, errWait := service.Wait(ctx, session)
	if errWait != nil {
		return nil, errWait
	}
	return qoder.NewAuthRecord(a.provider, creds), nil
}
