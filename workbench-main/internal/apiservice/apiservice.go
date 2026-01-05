package apiservice

import (
	"net/http"
	"ondcworkbench/internal/ondc/cache"
)

type ModuleRole string

const (
	BAP ModuleRole = "BAP"
	BPP ModuleRole = "BPP"
)


type RequestOwner string

const (
	BuyerNP  RequestOwner = "buyer_np"
	SellerNP RequestOwner = "seller_np"
	BuyerMock RequestOwner = "buyer_mock"
	SellerMock RequestOwner = "seller_mock"
)

type ModuleType string

const (
	Receiver ModuleType = "receiver"
	Caller   ModuleType = "caller"
)

type PayloadEnvelope struct {
	Context struct {
		TransactionID string `json:"transaction_id"`
		BapID         string `json:"bap_id"`
		BapURI		string `json:"bap_uri"`
		BppID         string `json:"bpp_id,omitempty"`
		BppURI		string `json:"bpp_uri,omitempty"`
		Action 	  string `json:"action"`
	}
}

type PayloadRaw = map[string]interface{}

// WorkbenchRequestData represents the data structure for workbench requests
type WorkbenchRequestData struct {
	Request http.Request
	BodyRaw 	  PayloadRaw
	BodyEnvelope PayloadEnvelope
	RequestOwner RequestOwner
	ModuleType  ModuleType
	SubscriberURL string
	TransactionID string
	TransactionProperties TransactionProperties
	// Additional fields set after initialization
	Difficulty cache.SessionDifficulty
	FlowID string
	SessionID string
}
// TransactionProperties represents the `x-transaction-properties` configuration.
//
// It contains:
//   - SupportedActions: a transition map of API action -> allowed next actions
//     Example (YAML):
//       supportedActions:
//         search: ["on_search"]
//         on_search: ["search", "select", "init"]
//
//   - APIProperties: per-action metadata such as async predecessor requirements
//     and valid transaction partners.
//     Example (YAML):
//       apiProperties:
//         on_select:
//           async_predecessor: "select"
//           transaction_partner: ["select"]
type TransactionProperties struct {
    // SupportedActions maps an action (key) to the list of actions that are allowed
    // to follow it in the transaction flow.
    //
    // Notes:
    //   - Keys can be empty ("") or the literal string "null" if present in the input YAML.
    SupportedActions map[string][]string `json:"supportedActions" yaml:"supportedActions"`

    // APIProperties maps an action (key) to its additional configuration.
    APIProperties map[string]ActionProperties `json:"apiProperties" yaml:"apiProperties"`
}

// ActionProperties describes configuration for a single API action.
type ActionProperties struct {
    // AsyncPredecessor indicates the action that must have occurred earlier for this
    // action to be considered valid in an async flow.
    //
    // It is a pointer because the YAML/JSON value can be null (or omitted).
    // Example:
    //   async_predecessor: null
    //   async_predecessor: "select"
    AsyncPredecessor *string `json:"async_predecessor" yaml:"async_predecessor"`

    // TransactionPartner lists actions that are considered valid partners/anchors
    // for this action in the same transaction (as per your config rules).
    TransactionPartner []string `json:"transaction_partner" yaml:"transaction_partner"`
}

