package main

import (
	"context"
	"errors"
	ondcvalidator "ondcValidator"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type ondcValidatorProvider struct{}

func (o *ondcValidatorProvider) New(ctx context.Context, cache definition.Cache, config map[string]string) (definition.OndcValidator, func() error, error) {
	if ctx == nil {
		return nil, nil, errors.New("context cannot be nil")
	}

	// Helper function to get bool with default
	getBool := func(key string, defaultValue bool) bool {
		if val, ok := config[key]; ok {
			return val == "true"
		}
		return defaultValue
	}

	StateFullValidations := getBool("stateFullValidations", false)
	DebugMode := getBool("debugMode", false)

	return ondcvalidator.New(ctx, cache, &ondcvalidator.Config{
		StateFullValidations: StateFullValidations,
		DebugMode:            DebugMode,
	})
}

var Provider = ondcValidatorProvider{}