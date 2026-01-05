package cache

// This package holds shared cache models mirroring the TS types.

type TransactionId = string
type FlowId = string
type PayloadId = string

type EnvType = string

const (
	EnvStaging       EnvType = "STAGING"
	EnvPreProduction EnvType = "PRE-PRODUCTION"
	EnvLoggedIn      EnvType = "LOGGED-IN"
)

type SessionDifficulty struct {
	SensitiveTTL      bool `json:"sensitiveTTL"`
	UseGateway        bool `json:"useGateway"`
	StopAfterFirstNack bool `json:"stopAfterFirstNack"`
	ProtocolValidations bool `json:"protocolValidations"`
	TimeValidations   bool `json:"timeValidations"`
	HeaderValidaton   bool `json:"headerValidaton"`
	UseGzip           bool `json:"useGzip"`
}

type Expectation struct {
	SessionId       string  `json:"sessionId"`
	FlowId          string  `json:"flowId"`
	ExpectedAction  *string `json:"expectedAction,omitempty"`
	ExpireAt        string  `json:"expireAt"`
}

type ApiData struct {
	EntryType     string `json:"entryType"`
	Action        string `json:"action"`
	PayloadId     string `json:"payloadId"`
	MessageId     string `json:"messageId"`
	Response      any    `json:"response"`
	Timestamp     string `json:"timestamp"`
	RealTimestamp string `json:"realTimestamp"`
	TTL           *int64 `json:"ttl,omitempty"`
}

type FormApiType struct {
	EntryType     string `json:"entryType"`
	FormType      string `json:"formType"`
	FormId        string `json:"formId"`
	SubmissionId  *string `json:"submissionId,omitempty"`
	Timestamp     string `json:"timestamp"`
	Error         any    `json:"error,omitempty"`
}

// HistoryType in TS is a union of ApiData | FormApiType.
// In Go we represent it as an arbitrary JSON object (map) when decoding.
// When writing new items we typically append strongly-typed ApiData/FormApiType.

type TransactionCache struct {
	SessionId       *string           `json:"sessionId,omitempty"`
	FlowId          *string           `json:"flowId,omitempty"`
	LatestAction    string            `json:"latestAction"`
	LatestTimestamp string            `json:"latestTimestamp"`
	Type            string            `json:"type"`
	SubscriberType  string            `json:"subscriberType"`
	MessageIds      []string          `json:"messageIds"`
	ApiList         []any             `json:"apiList"`
	ReferenceData   map[string]any    `json:"referenceData"`
}

type SubscriberCache struct {
	ActiveSessions []Expectation `json:"activeSessions"`
}

type SessionCache struct {
	TransactionIds     []string          `json:"transactionIds"`
	FlowMap            map[string]string `json:"flowMap"`
	NpType             string            `json:"npType"`
	Domain             string            `json:"domain"`
	Version            string            `json:"version"`
	SubscriberId       *string           `json:"subscriberId,omitempty"`
	SubscriberUrl      string            `json:"subscriberUrl"`
	Env                string            `json:"env"`
	SessionDifficulty  SessionDifficulty `json:"sessionDifficulty"`
	UsecaseId          string            `json:"usecaseId"`
}

type RequestProperties struct {
	SubscriberUrl     string            `json:"subscriberUrl"`
	SubscriberType    string            `json:"subscriberType"`
	Action            string            `json:"action"`
	TransactionId     string            `json:"transactionId"`
	Difficulty        SessionDifficulty `json:"difficulty"`
	SessionId         *string           `json:"sessionId,omitempty"`
	FlowId            *string           `json:"flowId,omitempty"`
	Env               string            `json:"env"`
	TransactionHistory *TransactionCache `json:"transactionHistory,omitempty"`
	SessionData       *SessionCache     `json:"sessionData,omitempty"`
	RequestSource     *string           `json:"requestSource,omitempty"`
}
