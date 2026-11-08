package cmd

import (
	"context"
	"fmt"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	log "github.com/sirupsen/logrus"
)

func DoAntigravityEnterpriseLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}
	_, path, err := newAuthManager().Login(context.Background(), ae.Provider, cfg, &sdkAuth.LoginOptions{NoBrowser: options.NoBrowser, CallbackPort: options.CallbackPort, Prompt: options.Prompt})
	if err != nil {
		log.Errorf("Antigravity Enterprise authentication failed: %v", err)
		return
	}
	fmt.Printf("Antigravity Enterprise authorization saved to %s\n", path)
}
