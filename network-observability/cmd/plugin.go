package main

import (
	"context"
	"net/http"

	networkobservability "github.com/ONDC-Official/automation-beckn-plugins/network-observability"
)

type networkObservabilityProvider struct{}

func (p networkObservabilityProvider) New(ctx context.Context, c map[string]string) (func(http.Handler) http.Handler, error) {
	configPath := c["configPath"]
	if configPath == "" {
		configPath = c["config_path"]
	}
	if configPath == "" {
		configPath = c["config"]
	}
	return networkobservability.NewNetworkObservabilityMiddleware(ctx, configPath)
}

// Provider is the exported symbol the host uses to load this plugin.
var Provider = networkObservabilityProvider{}