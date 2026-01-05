package main

import (
	"context"
	"keymanager"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type keyManagerProvider struct{}

var newKeyManagerFunc = keymanager.New

// New creates a new instance of the KeyManager plugin
func (k *keyManagerProvider) New(ctx context.Context, _ definition.Cache,_ definition.RegistryLookup, config map[string]string) (definition.KeyManager, func() error, error) {
	return newKeyManagerFunc(ctx, nil, nil, &keymanager.Config{})
}

// Provider is the exported provider instance for the KeyManager plugin
var Provider = keyManagerProvider{}