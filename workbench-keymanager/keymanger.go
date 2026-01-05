package keymanager

import (
	"context"
	"fmt"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/spf13/viper"
)

type Env struct {
	SubscriberID   string `mapstructure:"SUBSCRIBER_ID"` // SubscriberID is the identifier for the subscriber.
	UniqueKeyID    string `mapstructure:"UNIQUE_KEY_ID"` // UniqueKeyID is the identifier for the key pair.
	SigningPrivate string `mapstructure:"SIGNING_PRIVATE"` // SigningPrivate is the private key used for signing operations.
	SigningPublic  string `mapstructure:"SIGNING_PUBLIC"` // SigningPublic is the public key corresponding to the signing private key.
	EncrPrivate    string `mapstructure:"ENCR_PRIVATE"` // EncrPrivate is the private key used for encryption operations.
	EncrPublic     string `mapstructure:"ENCR_PUBLIC"` // EncrPublic is the public key corresponding to the encryption private key.
}

type KeyMgr struct {
	env *Env
}

type Config struct {

}

func New(ctx context.Context, cache definition.Cache, registryLookup definition.RegistryLookup, cfg *Config) (*KeyMgr, func() error, error) {
	mgr := &KeyMgr{
		env: NewEnv(),
	}
	return mgr, func() error { return nil }, nil
}

func (k *KeyMgr) GenerateKeyset() (*model.Keyset,error){
	return &model.Keyset{
		SubscriberID:   k.env.SubscriberID,
		UniqueKeyID:    k.env.UniqueKeyID,
		SigningPrivate: k.env.SigningPrivate,
		SigningPublic:  k.env.SigningPublic,
		EncrPrivate:    k.env.EncrPrivate,
		EncrPublic:     k.env.EncrPublic,
	},nil
}

func (k *KeyMgr) InsertKeyset(ctx context.Context, keyID string, keyset *model.Keyset) error {
	return nil
}

func (k *KeyMgr) Keyset(ctx context.Context, keyID string) (*model.Keyset, error) {
	return &model.Keyset{
		SubscriberID:   k.env.SubscriberID,
		UniqueKeyID:    k.env.UniqueKeyID,
		SigningPrivate: k.env.SigningPrivate,
		SigningPublic:  k.env.SigningPublic,
		EncrPrivate:    k.env.EncrPrivate,
		EncrPublic:     k.env.EncrPublic,
	}, nil
}

func (k *KeyMgr) DeleteKeyset(ctx context.Context, keyID string) error {
	return nil
}

func (k *KeyMgr) LookupNPKeys(ctx context.Context, subscriberID string,uniqueKeyID string) (signingPublicKey string, encrPublicKey string, err error){
	return "", "", fmt.Errorf("not implemented")
}

func NewEnv() *Env {
	env := Env{}
	viper.SetConfigFile(".env")
	if err := viper.ReadInConfig(); err != nil {
		panic(err)
	}
	if err := viper.Unmarshal(&env); err != nil {
		panic(err)
	}
	return &env
}