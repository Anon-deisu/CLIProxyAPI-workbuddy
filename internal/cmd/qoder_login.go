package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoQoderLogin runs the regional device authorization flow and saves credentials.
func DoQoderLogin(cfg *config.Config, options *LoginOptions, provider string) {
	if options == nil {
		options = &LoginOptions{}
	}
	record, savedPath, err := newAuthManager().Login(context.Background(), provider, cfg, &sdkAuth.LoginOptions{NoBrowser: options.NoBrowser})
	if err != nil {
		log.WithError(err).Error("Qoder authentication failed")
		return
	}
	fmt.Printf("Authenticated as %s\nAuthentication saved to %s\n", record.Label, savedPath)
}
