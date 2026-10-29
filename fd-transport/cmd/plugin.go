package main

import (
	"context"
	"errors"

	fdtransport "github.com/ONDC-Official/automation-beckn-plugins/fd-transport"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// transportWrapperProvider implements beckn-onix's definition.TransportWrapperProvider.
type transportWrapperProvider struct{}

// New returns the fd-transport wrapper. Config key: internalHosts (comma-separated
// host[:port] list of workbench-internal destinations, e.g. the mock service).
func (p transportWrapperProvider) New(ctx context.Context, config map[string]any) (definition.TransportWrapper, func(), error) {
	if ctx == nil {
		return nil, nil, errors.New("context cannot be nil")
	}
	c, err := fdtransport.ParseConfig(config)
	if err != nil {
		return nil, nil, err
	}
	return fdtransport.New(c), nil, nil
}

// Provider is the exported symbol the beckn-onix plugin manager looks for.
var Provider = transportWrapperProvider{}
