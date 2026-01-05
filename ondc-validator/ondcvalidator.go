package ondcvalidator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"validationpkg"
	"validationpkg/storageutils"
	"validationpkg/validationutils"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type payloadEnvelope struct {
	Context struct {
		TransactionID string `json:"transaction_id"`
	}
}

type Config struct {
	StateFullValidations bool `json:"stateFullValidations" yaml:"stateFullValidations"`
	DebugMode bool `json:"debugMode" yaml:"debugMode"`
}

type ondcValidator struct {
	config *Config
	cache  definition.Cache
	storage validationutils.StorageInterface
}

func (v *ondcValidator) ValidatePayload(ctx context.Context, url *url.URL, payload []byte) error {
	log.Infof(ctx,"Starting L1 validation for URL: %s", url.String())
	payloadData, uniqueKey, err := convertToPayload(payload)
	if err != nil {
		return fmt.Errorf("invalid payload: %w", err)
	}
	endpoint := path.Base(url.String())
	result,err := validationpkg.PerformL1validations(endpoint, payloadData, &validationutils.ValidationConfig{
		StateFullValidations: v.config.StateFullValidations,
		Debug: 			  v.config.DebugMode,
		OnlyInvalid: true,
		HideParentErrors: true,
		UniqueKey: &uniqueKey,
		Store:    v.storage,
	}, validationutils.ExternalData{})

	log.Infof(ctx,"L1 validation completed with error count: %#+v", len(result))

	if err != nil {
		return fmt.Errorf("validation error: %w", err)
	}

	if len(result) > 0 {

		if(result[0].Valid){
			log.Infof(ctx,"L1 validation successful for URL: %s", url.String())
			return nil
		}

		var ondcErrors []model.Error
		for _, res := range result { 
			ondcErrors = append(ondcErrors, model.Error{
				Code:        strconv.Itoa(res.Code),
				Message:     res.Description,
				Paths: "",
			})
		}
		return &model.SchemaValidationErr{Errors: ondcErrors}
	}
	log.Infof(ctx,"L1 validation successful for URL: %s", url.String())
	return nil
}

func (v *ondcValidator) SaveValidationData(ctx context.Context, url *url.URL, payload []byte) error {
	payloadData,uniqueKey, err := convertToPayload(payload)
	if err != nil {
		return fmt.Errorf("invalid payload: %w", err)
	}
	endpoint := path.Base(url.String())
	storeConfig := storageutils.DefaultStorageConfig()
	return validationpkg.PerformL1validationsSave(endpoint, uniqueKey, payloadData, v.storage, &storeConfig)
}

func New(ctx context.Context, cache definition.Cache, config *Config) (definition.OndcValidator, func() error, error) {
	if config == nil {
		return nil, nil, fmt.Errorf("config cannot be nil")
	}
	v := &ondcValidator{
		config: config,
		cache:  cache,
		storage: &cacheWrapper{cache: cache},
	}
	return v, nil, nil
}

// convertToPayload returns:
// 1) the full JSON payload as an object usable by JSONPath (map/slice)
// 2) transaction_id (for uniqueKey)
// 3) error
func convertToPayload(data []byte) (interface{}, string, error) {
    // Full object for JSONPath validations
    var payloadObj interface{}
    if err := json.Unmarshal(data, &payloadObj); err != nil {
        return nil, "", fmt.Errorf("failed to parse JSON payload: %w", err)
    }

    // Minimal typed parse for transaction_id
    var env payloadEnvelope
    if err := json.Unmarshal(data, &env); err != nil {
        return nil, "", fmt.Errorf("failed to parse payload envelope: %w", err)
    }
    if env.Context.TransactionID == "" {
        return nil, "", fmt.Errorf("transaction_id is missing in context")
    }

    return payloadObj, env.Context.TransactionID, nil
}


type cacheWrapper struct {
	cache definition.Cache
}

func (c *cacheWrapper) SaveKey(uniquePrefix string, key string, value string) error {
	fullKey := fmt.Sprintf("%s:%s", uniquePrefix, key)
	return c.cache.Set(context.Background(), fullKey, value, 60 * 5)
}

func (c *cacheWrapper) GetKey(uniquePrefix string, key string) (string, error) {
	fullKey := fmt.Sprintf("%s:%s", uniquePrefix, key)
	value, err := c.cache.Get(context.Background(), fullKey)
	if err != nil {
		return "", err
	}
	return value, nil
}

func (c *cacheWrapper) DeleteKey(uniquePrefix string, key string) error {
	fullKey := fmt.Sprintf("%s:%s", uniquePrefix, key)
	return c.cache.Delete(context.Background(), fullKey)
}

func (c *cacheWrapper) ListKeys(uniquePrefix string) ([]string, error) {
	// Note: definition.Cache does not support listing keys, so this is a no-op.
	return nil, fmt.Errorf("ListKeys not supported in cacheWrapper")
}

func (c *cacheWrapper) ClearStorage() error {
	// Note: definition.Cache does not support clearing all keys, so this is a no-op.
	return c.cache.Clear(context.Background())
}

func (c *cacheWrapper) KeyExists(uniquePrefix string, key string) (bool, error) {
	fullKey := fmt.Sprintf("%s:%s", uniquePrefix, key)
	_, err := c.cache.Get(context.Background(), fullKey)
	if err != nil {
		return false, err
	}
	return true, nil
}