// How to run:
//
// 1) Create example payload files under ./examples (relative to this package):
//      validationpkg/examples/search.json
//      validationpkg/examples/on_search.json
//    Each file must contain a JSON array of payload objects.
//
// 2) From the validationpkg module directory, run:
//      go test -run TestPerformL1validations_Examples -v
//    Or from the repo root:
//      go test ./ondcValidator/validationpkg -run TestPerformL1validations_Examples -v
//
// Outputs are written under ./examples_output/<action>/case-XXX/output.json.

package validationpkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"validationpkg/validationutils"
)

type noopStorage struct{}

func (n *noopStorage) SaveKey(uniquePrefix string, key string, value string) error { return nil }
func (n *noopStorage) GetKey(uniquePrefix string, key string) (string, error) {
	return "", fmt.Errorf("noop storage: key not found")
}
func (n *noopStorage) DeleteKey(uniquePrefix string, key string) error { return nil }
func (n *noopStorage) ListKeys(uniquePrefix string) ([]string, error) {
	return nil, fmt.Errorf("noop storage: list not supported")
}
func (n *noopStorage) ClearStorage() error { return nil }
func (n *noopStorage) KeyExists(uniquePrefix string, key string) (bool, error) { return false, nil }

type validationRunOutput struct {
	Action    string                            `json:"action"`
	Case      int                               `json:"case"`
	UniqueKey string                            `json:"uniqueKey"`
	Error     string                            `json:"error,omitempty"`
	Results   []validationutils.ValidationOutput `json:"results,omitempty"`
}

func TestPerformL1validations_Examples(t *testing.T) {
	examplesDir := "examples"
	if _, err := os.Stat(examplesDir); err != nil {
		if os.IsNotExist(err) {
			t.Skipf("%s directory not found; skipping example-based validations", examplesDir)
		}
		t.Fatalf("failed to stat %s: %v", examplesDir, err)
	}

	files, err := filepath.Glob(filepath.Join(examplesDir, "*.json"))
	if err != nil {
		t.Fatalf("failed to glob examples: %v", err)
	}
	if len(files) == 0 {
		t.Skipf("no example files found in %s", examplesDir)
	}

	outputRoot := "examples_output"
	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		t.Fatalf("failed to create output dir %s: %v", outputRoot, err)
	}

	store := &noopStorage{}

	for _, filePath := range files {
		action := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))

		t.Run(action, func(t *testing.T) {
			data, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatalf("failed to read %s: %v", filePath, err)
			}

			var payloads []json.RawMessage
			if err := json.Unmarshal(data, &payloads); err != nil {
				t.Fatalf("%s must contain a JSON array of payloads: %v", filePath, err)
			}
			if len(payloads) == 0 {
				t.Skipf("no payloads in %s", filePath)
			}

			for i, raw := range payloads {
				caseNum := i + 1

				var payload interface{}
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Errorf("case %d: payload is not valid JSON: %v", caseNum, err)
					continue
				}

				uniqueKey := extractTransactionID(payload)
				if uniqueKey == "" {
					uniqueKey = fmt.Sprintf("%s-case-%03d", action, caseNum)
				}

				cfg := &validationutils.ValidationConfig{
					StateFullValidations: false,
					Debug:                false,
					OnlyInvalid:           true,
					HideParentErrors:      true,
					UniqueKey:             &uniqueKey,
					Store:                 store,
				}

				results, runErr := PerformL1validations(action, payload, cfg, validationutils.ExternalData{})
				out := validationRunOutput{
					Action:    action,
					Case:      caseNum,
					UniqueKey: uniqueKey,
					Results:   results,
				}
				if runErr != nil {
					out.Error = runErr.Error()
				}

				caseDir := filepath.Join(outputRoot, action, fmt.Sprintf("case-%03d", caseNum))
				if err := os.MkdirAll(caseDir, 0o755); err != nil {
					t.Errorf("case %d: failed to create output dir %s: %v", caseNum, caseDir, err)
					continue
				}

				encoded, err := json.MarshalIndent(out, "", "  ")
				if err != nil {
					t.Errorf("case %d: failed to marshal output: %v", caseNum, err)
					continue
				}

				if err := os.WriteFile(filepath.Join(caseDir, "output.json"), encoded, 0o644); err != nil {
					t.Errorf("case %d: failed to write output.json: %v", caseNum, err)
				}

				if runErr != nil {
					t.Errorf("case %d: PerformL1validations returned error: %v", caseNum, runErr)
				}
			}
		})
	}
}

func extractTransactionID(payload interface{}) string {
	root, ok := payload.(map[string]interface{})
	if !ok {
		return ""
	}
	ctx, ok := root["context"].(map[string]interface{})
	if !ok {
		return ""
	}
	if v, ok := ctx["transaction_id"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}