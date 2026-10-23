package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	log "github.com/sirupsen/logrus"
)

func DoGeminiEnterpriseLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}
	_, saved, err := newAuthManager().Login(context.Background(), "gemini-enterprise", cfg, &sdkAuth.LoginOptions{NoBrowser: options.NoBrowser, CallbackPort: options.CallbackPort})
	if err != nil {
		log.Errorf("Gemini Enterprise login failed: %v", err)
		return
	}
	fmt.Printf("Gemini Enterprise authentication saved to %s\n", saved)
}
