package main

import (
	"context"
	"errors"

	fdtransport "github.com/ONDC-Official/automation-beckn-plugins/fd-transport"
	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// transportWrapperProvider implements beckn-onix's definition.TransportWrapperProvider.
type transportWrapperProvider struct{}

// New returns the fd-transport wrapper. Config keys (string values): routeTable (required);
// internalHosts and allowedHosts (comma-separated host[:port] lists); subscriberId,
// uniqueKeyId and signingPrivateKey (default env SUBSCRIBER_ID, UNIQUE_KEY_ID,
// SIGNING_PRIVATE); expiresIn (default PT5M); requireHTTPS (default true, env
// FD_REQUIRE_HTTPS overrides). See fdtransport.Config.
func (p transportWrapperProvider) New(ctx context.Context, config map[string]any) (definition.TransportWrapper, func(), error) {
	if ctx == nil {
		return nil, nil, errors.New("context cannot be nil")
	}
	c, err := fdtransport.ParseConfig(config)
	if err != nil {
		return nil, nil, err
	}
	w, err := fdtransport.New(c)
	if err != nil {
		return nil, nil, err
	}
	if !w.CanSign() {
		log.Warnf(ctx, "fd-transport: no signing key configured: every request for a participant will be refused")
	}
	return w, nil, nil
}

// Provider is the exported symbol the beckn-onix plugin manager looks for.
var Provider = transportWrapperProvider{}
