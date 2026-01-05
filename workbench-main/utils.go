package ondcworkbench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"ondcworkbench/internal/apiservice"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
)


func validateConfig(config *Config) error {
	if config.ProtocolVersion == "" {
		return fmt.Errorf("protocol version cannot be empty")
	}
	if config.ProtocolDomain == "" {
		return fmt.Errorf("protocol domain cannot be empty")
	}
	if config.ModuleRole != "BAP" && config.ModuleRole != "BPP" {
		return fmt.Errorf("module role must be either 'BAP' or 'BPP'")
	}
	if(config.AuditURL == ""){
		return fmt.Errorf("audit URL cannot be empty")
	}
	return nil
}

func setRequestCookies(requestData *apiservice.WorkbenchRequestData) {
	httpReq := &requestData.Request
	httpReq.AddCookie(&http.Cookie{
		Name: "flow_id",
		Value: requestData.FlowID,
	})
	httpReq.AddCookie(&http.Cookie{
		Name: "session_id",
		Value: requestData.SessionID,
	})
	httpReq.AddCookie(&http.Cookie{
		Name: "transaction_id",
		Value: requestData.TransactionID,
	})
	httpReq.AddCookie(&http.Cookie{
		Name: "subscriber_url",
		Value: requestData.SubscriberURL,
	})
	httpReq.AddCookie(&http.Cookie{
		Name: "header_validation",
		Value: getBooleanString(requestData.Difficulty.HeaderValidaton),
	})
	httpReq.AddCookie(&http.Cookie{
		Name: "protocol_validation",
		Value: getBooleanString(requestData.Difficulty.ProtocolValidations),
	})
	httpReq.AddCookie(&http.Cookie{
		Name: "use_gzip",
		Value: getBooleanString(requestData.Difficulty.UseGzip),
	})
	httpReq.AddCookie(&http.Cookie{
		Name: "request_owner",
		Value: string(requestData.RequestOwner),
	})
}

func getBooleanString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func getTransactionPropertiesFromConfigService(ctx context.Context, configServiceURL, protocolDomain, protocolVersion string) (apiservice.TransactionProperties, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(configServiceURL) == "" {
		return apiservice.TransactionProperties{}, fmt.Errorf("configServiceURL cannot be empty")
	}
	if strings.TrimSpace(protocolDomain) == "" {
		return apiservice.TransactionProperties{}, fmt.Errorf("protocolDomain cannot be empty")
	}
	if strings.TrimSpace(protocolVersion) == "" {
		return apiservice.TransactionProperties{}, fmt.Errorf("protocolVersion cannot be empty")
	}

	baseURL, err := url.Parse(configServiceURL)
	if err != nil {
		return apiservice.TransactionProperties{}, fmt.Errorf("invalid configServiceURL: %w", err)
	}

	// Match TS path: /api-service/supportedActions
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/api-service/supportedActions"
	query := baseURL.Query()
	query.Set("domain", protocolDomain)
	query.Set("version", protocolVersion)
	baseURL.RawQuery = query.Encode()

	log.Infof(ctx, "Loading config from API: %s", baseURL.String())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL.String(), nil)
	if err != nil {
		return apiservice.TransactionProperties{}, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return apiservice.TransactionProperties{}, fmt.Errorf("config service request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return apiservice.TransactionProperties{}, fmt.Errorf("failed to read config service response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		trimmed := string(body)
		if len(trimmed) > 2000 {
			trimmed = trimmed[:2000] + "..."
		}
		return apiservice.TransactionProperties{}, fmt.Errorf("config service returned %d: %s", resp.StatusCode, trimmed)
	}

	// Axios code uses response.data.data, so the response body is expected to be:
	// { "data": { "supportedActions": {...}, "apiProperties": {...} }, ... }
	var envelope struct {
		Data apiservice.TransactionProperties `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return apiservice.TransactionProperties{}, fmt.Errorf("failed to parse config service response JSON: %w", err)
	}

	if envelope.Data.SupportedActions == nil {
		envelope.Data.SupportedActions = map[string][]string{}
	}
	if envelope.Data.APIProperties == nil {
		envelope.Data.APIProperties = map[string]apiservice.ActionProperties{}
	}
	if len(envelope.Data.APIProperties) == 0 {
		return apiservice.TransactionProperties{}, fmt.Errorf("config service returned empty apiProperties")
	}

	return envelope.Data, nil
}