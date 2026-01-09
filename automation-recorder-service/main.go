package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	grpcServiceName = "beckn.audit.v1.AuditService"
	grpcFullMethod  = "/" + grpcServiceName + "/LogEvent"
)

type config struct {
	ListenAddr string
	RedisAddr  string

	SkipCacheUpdate bool
	SkipNOPush      bool
	SkipDBSave      bool

	AsyncQueueSize   int
	AsyncWorkerCount int
	DropOnQueueFull  bool

	Env string

	APITTLSecondsDefault   int64
	CacheTTLSecondsDefault int64

	NOURL       string
	NOToken     string
	NOTimeout   time.Duration
	NOEnabledIn map[string]bool

	DBBaseURL     string
	DBAPIKey      string
	DBTimeout     time.Duration
	DBEnabledIn   map[string]bool
	DBSessionPath string
	DBPayloadPath string
}

func main() {
	ctx := context.Background()
	cfg, err := loadConfig()
	if err != nil {
		log.Errorf(ctx, err, "automation-recorder: invalid config")
		os.Exit(2)
	}

	rdb := newRedisClient(cfg.RedisAddr)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Errorf(ctx, err, "automation-recorder: failed to connect to redis")
		os.Exit(2)
	}

	lsn, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Errorf(ctx, err, "automation-recorder: listen failed")
		os.Exit(2)
	}

	dispatcher := newAsyncDispatcher(ctx, cfg.AsyncQueueSize, cfg.AsyncWorkerCount, cfg.DropOnQueueFull)

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(recoveryUnaryInterceptor),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    2 * time.Minute,
			Timeout: 20 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
	)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	registerAuditService(srv, &recorderServer{rdb: rdb, cfg: cfg, httpClient: httpClient, async: dispatcher})

	log.Infof(ctx, "automation-recorder: listening on %s", cfg.ListenAddr)
	if err := srv.Serve(lsn); err != nil {
		log.Errorf(ctx, err, "automation-recorder: grpc serve failed")
		os.Exit(1)
	}
}

func loadConfig() (config, error) {
	listenAddr := strings.TrimSpace(os.Getenv("RECORDER_LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = ":8089"
	}
	redisAddr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	if redisAddr == "" {
		redisAddr = strings.TrimSpace(os.Getenv("REDIS_HOST"))
	}
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}

	cfg := config{ListenAddr: listenAddr, RedisAddr: redisAddr}

	cfg.SkipCacheUpdate = envBool("RECORDER_SKIP_CACHE_UPDATE", false)
	cfg.SkipNOPush = envBool("RECORDER_SKIP_NO_PUSH", false)
	cfg.SkipDBSave = envBool("RECORDER_SKIP_DB_SAVE", false)

	cfg.AsyncQueueSize = envInt("RECORDER_ASYNC_QUEUE_SIZE", 1000)
	cfg.AsyncWorkerCount = envInt("RECORDER_ASYNC_WORKERS", 2)
	if cfg.AsyncWorkerCount < 1 {
		cfg.AsyncWorkerCount = 1
	}
	cfg.DropOnQueueFull = envBool("RECORDER_ASYNC_DROP_ON_FULL", true)

	cfg.Env = strings.ToLower(strings.TrimSpace(os.Getenv("RECORDER_ENV")))
	if cfg.Env == "" {
		cfg.Env = "dev"
	}

	cfg.APITTLSecondsDefault = int64(envInt("RECORDER_API_TTL_SECONDS_DEFAULT", 30))
	if cfg.APITTLSecondsDefault < 0 {
		cfg.APITTLSecondsDefault = 0
	}
	cfg.CacheTTLSecondsDefault = int64(envInt("RECORDER_CACHE_TTL_SECONDS_DEFAULT", 0))
	if cfg.CacheTTLSecondsDefault < 0 {
		cfg.CacheTTLSecondsDefault = 0
	}

	cfg.NOURL = strings.TrimSpace(os.Getenv("RECORDER_NO_URL"))
	cfg.NOToken = strings.TrimSpace(os.Getenv("RECORDER_NO_BEARER_TOKEN"))
	cfg.NOTimeout = time.Duration(envInt("RECORDER_NO_TIMEOUT_MS", 5000)) * time.Millisecond
	cfg.NOEnabledIn = parseEnvSet(os.Getenv("RECORDER_NO_ENABLED_ENVS"))

	cfg.DBBaseURL = strings.TrimSpace(os.Getenv("RECORDER_DB_BASE_URL"))
	cfg.DBAPIKey = strings.TrimSpace(os.Getenv("RECORDER_DB_API_KEY"))
	cfg.DBTimeout = time.Duration(envInt("RECORDER_DB_TIMEOUT_MS", 5000)) * time.Millisecond
	cfg.DBEnabledIn = parseEnvSet(os.Getenv("RECORDER_DB_ENABLED_ENVS"))
	cfg.DBSessionPath = "/api/sessions"
	// Matches TS: POST `${DATA_BASE_URL}/api/sessions/payload`
	cfg.DBPayloadPath = "/api/sessions/payload"

	return cfg, nil
}

func newRedisClient(addr string) *redis.Client {
	password := os.Getenv("REDIS_PASSWORD")
	return redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: 0})
}

func recoveryUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf(ctx, fmt.Errorf("panic: %v", r), "automation-recorder: panic")
			err = status.Error(codes.Internal, "internal")
		}
	}()
	return handler(ctx, req)
}

// ---- gRPC service (registered without codegen) ----

type auditServiceServer interface {
	LogEvent(context.Context, *wrapperspb.BytesValue) (*emptypb.Empty, error)
}

type recorderServer struct {
	rdb        *redis.Client
	cfg        config
	httpClient *http.Client
	async      *asyncDispatcher
}


type auditPayload struct {
	RequestBody    map[string]any `json:"requestBody"`
	ResponseBody   map[string]any `json:"responseBody"`
	AdditionalData map[string]any `json:"additionalData"`
}

type derivedFields struct {
	PayloadID     string
	TransactionID string
	MessageID     string
	SubscriberURL string
	Action        string
	Timestamp     string
	APIName       string
	StatusCode    int64
	TTLSecs       int64
	CacheTTLSecs  int64
	IsMock        bool
	SessionID     string
}

func (s *recorderServer) LogEvent(ctx context.Context, in *wrapperspb.BytesValue) (*emptypb.Empty, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	var payload auditPayload
	if err := json.Unmarshal(in.Value, &payload); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid JSON")
	}
	if payload.RequestBody == nil {
		return nil, status.Error(codes.InvalidArgument, "requestBody must be a JSON object")
	}
	if payload.ResponseBody == nil {
		return nil, status.Error(codes.InvalidArgument, "responseBody must be a JSON object")
	}
	if payload.AdditionalData == nil {
		payload.AdditionalData = map[string]any{}
	}

	derived, err := deriveFields(payload)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if derived.PayloadID == "" {
		derived.PayloadID, _ = uuidV4()
	}
	if derived.TTLSecs == 0 {
		derived.TTLSecs = s.cfg.APITTLSecondsDefault
	}
	if derived.CacheTTLSecs == 0 {
		derived.CacheTTLSecs = s.cfg.CacheTTLSecondsDefault
	}

	key := createTransactionKey(derived.TransactionID, derived.SubscriberURL)
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid key")
	}

	var cacheTTL time.Duration
	if derived.CacheTTLSecs < 0 {
		return nil, status.Error(codes.InvalidArgument, "cache_ttl_seconds must be >= 0")
	}
	if derived.CacheTTLSecs > 0 {
		cacheTTL = time.Duration(derived.CacheTTLSecs) * time.Second
	}

	if !s.cfg.SkipCacheUpdate {
		in := cacheAppendInput{
			TransactionID: derived.TransactionID,
			MessageID:     derived.MessageID,
			SubscriberURL: derived.SubscriberURL,
			Action:        derived.Action,
			Timestamp:     derived.Timestamp,
			APIName:       derived.APIName,
			TTLSecs:       derived.TTLSecs,
			Response:      payload.ResponseBody,
		}
		if err := updateTransactionAtomically(ctx, s.rdb, key, &in, cacheTTL); err != nil {
			if errors.Is(err, errNotFound) {
				return nil, status.Error(codes.NotFound, "transaction not found")
			}
			if errors.Is(err, errAborted) {
				return nil, status.Error(codes.Aborted, "conflict, retry")
			}
			return nil, status.Error(codes.Internal, "cache update failed")
		}

		// Mirror TS behavior: flow status is stored in a separate key and only updated if it already exists.
		if err := setFlowStatusIfExists(ctx, s.rdb, derived.TransactionID, derived.SubscriberURL, "AVAILABLE", 5*time.Hour); err != nil {
			log.Warnf(ctx, "automation-recorder: failed to set flow status: %v", err)
		}
	}

	// Fire-and-forget side effects.
	baseCtx := context.Background()
	if !s.cfg.SkipNOPush {
		s.async.enqueue(baseCtx, "no-push", func(ctx context.Context) error {
			return sendLogsToNO(ctx, s.cfg, s.httpClient, derived, payload.RequestBody, payload.ResponseBody)
		})
	}
	if !s.cfg.SkipDBSave {
		s.async.enqueue(baseCtx, "db-save", func(ctx context.Context) error {
			return savePayloadToDB(ctx, s.cfg, s.httpClient, s.rdb, derived, payload.RequestBody, payload.ResponseBody, payload.AdditionalData)
		})
	}

	return &emptypb.Empty{}, nil
}


func registerAuditService(s *grpc.Server, impl auditServiceServer) {
	s.RegisterService(&grpc.ServiceDesc{
		ServiceName: grpcServiceName,
		HandlerType: (*auditServiceServer)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "LogEvent",
				Handler: func(srv interface{}, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
					in := new(wrapperspb.BytesValue)
					if err := dec(in); err != nil {
						return nil, err
					}
					if interceptor == nil {
						return srv.(auditServiceServer).LogEvent(ctx, in)
					}
					info := &grpc.UnaryServerInfo{Server: srv, FullMethod: grpcFullMethod}
					handler := func(ctx context.Context, req any) (any, error) {
						return srv.(auditServiceServer).LogEvent(ctx, req.(*wrapperspb.BytesValue))
					}
					return interceptor(ctx, in, info, handler)
				},
			},
		},
		Streams:  []grpc.StreamDesc{},
		Metadata: "proto/audit.proto",
	}, impl)
}

// ---- cache update logic ----

var (
	errNotFound = errors.New("transaction not found")
	errAborted  = errors.New("aborted")
)

type transactionCache struct {
	TransactionId    *string   `json:"transactionId"`
	SubscriberUrl    *string   `json:"subscriberUrl"`
	LatestAction     *string   `json:"latestAction"`
	LatestTimestamp  *string   `json:"latestTimestamp"`
	ApiList          []apiData `json:"apiList"`
}

type apiData struct {
	ApiName   *string `json:"apiName"`
	Response  any     `json:"response"`
	Ttl       *int64  `json:"ttl"`
	Timestamp *string `json:"timestamp"`
}

func createTransactionKey(transactionID, subscriberURL string) string {
	transactionID = strings.TrimSpace(transactionID)
	subscriberURL = strings.TrimSpace(subscriberURL)
	subscriberURL = strings.TrimRight(subscriberURL, "/")
	if transactionID == "" || subscriberURL == "" {
		return ""
	}
	return transactionID + "::" + subscriberURL
}


type cacheAppendInput struct {
	TransactionID string
	MessageID     string
	SubscriberURL string
	Action        string
	Timestamp     string
	APIName       string
	TTLSecs       int64
	Response      any
}

func updateTransactionAtomically(ctx context.Context, rdb *redis.Client, key string, in *cacheAppendInput, cacheTTL time.Duration) error {
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := rdb.Watch(ctx, func(tx *redis.Tx) error {
			val, err := tx.Get(ctx, key).Result()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					return errNotFound
				}
				return err
			}

			var txn map[string]any
			if err := json.Unmarshal([]byte(val), &txn); err != nil {
				return err
			}
			if txn == nil {
				txn = map[string]any{}
			}

			txn["transactionId"] = strings.TrimSpace(in.TransactionID)
			txn["messageId"] = strings.TrimSpace(in.MessageID)
			txn["subscriberUrl"] = strings.TrimRight(strings.TrimSpace(in.SubscriberURL), "/")
			txn["latestAction"] = strings.TrimSpace(in.Action)
			txn["latestTimestamp"] = strings.TrimSpace(in.Timestamp)

			apiList, ok := txn["apiList"].([]any)
			if !ok || apiList == nil {
				apiList = []any{}
			}
			apiList = append(apiList, map[string]any{
				"apiName":    strings.TrimSpace(in.APIName),
				"response":   in.Response,
				"ttl":        in.TTLSecs,
				"timestamp":  strings.TrimSpace(in.Timestamp),
			})
			txn["apiList"] = apiList

			updated, err := json.Marshal(txn)
			if err != nil {
				return err
			}

			pipe := tx.TxPipeline()
			if cacheTTL > 0 {
				pipe.Set(ctx, key, string(updated), cacheTTL)
			} else {
				pipe.Set(ctx, key, string(updated), 0)
			}
			_, err = pipe.Exec(ctx)
			return err
		}, key)

		if err == nil {
			return nil
		}
		if errors.Is(err, errNotFound) {
			return err
		}
		// Conflict retry.
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		// If we returned a gRPC status error (e.g. invalid JSON), preserve it.
		st, ok := status.FromError(err)
		if ok {
			return st.Err()
		}
		return err
	}
	return errAborted
}

func createFlowStatusCacheKey(transactionID, subscriberURL string) string {
	transactionID = strings.TrimSpace(transactionID)
	subscriberURL = strings.TrimSpace(subscriberURL)
	if transactionID == "" || subscriberURL == "" {
		return ""
	}
	// Matches TS: `FLOW_STATUS_${transactionId}::${subscriberUrl}` (no trailing-slash trimming)
	return "FLOW_STATUS_" + transactionID + "::" + subscriberURL
}

func setFlowStatusIfExists(ctx context.Context, rdb *redis.Client, transactionID, subscriberURL, statusValue string, ttl time.Duration) error {
	if rdb == nil {
		return nil
	}
	key := createFlowStatusCacheKey(transactionID, subscriberURL)
	if key == "" {
		return nil
	}
	exists, err := rdb.Exists(ctx, key).Result()
	if err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	b, err := json.Marshal(map[string]any{"status": statusValue})
	if err != nil {
		return err
	}
	return rdb.Set(ctx, key, string(b), ttl).Err()
}

type asyncJob struct {
	name string
	fn   func(context.Context) error
}

type asyncDispatcher struct {
	ch             chan asyncJob
	workerCount    int
	dropOnQueueFull bool
	baseCtx        context.Context
	startOnce      sync.Once
}

func newAsyncDispatcher(baseCtx context.Context, queueSize, workerCount int, dropOnQueueFull bool) *asyncDispatcher {
	if queueSize <= 0 {
		queueSize = 1000
	}
	if workerCount <= 0 {
		workerCount = 1
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return &asyncDispatcher{ch: make(chan asyncJob, queueSize), workerCount: workerCount, dropOnQueueFull: dropOnQueueFull, baseCtx: baseCtx}
}

func (d *asyncDispatcher) start() {
	d.startOnce.Do(func() {
		for i := 0; i < d.workerCount; i++ {
			go func() {
				for job := range d.ch {
					ctx, cancel := context.WithTimeout(d.baseCtx, 15*time.Second)
					err := job.fn(ctx)
					cancel()
					if err != nil {
						log.Warnf(d.baseCtx, "automation-recorder: async job %s failed: %v", job.name, err)
					}
				}
			}()
		}
	})
}

func (d *asyncDispatcher) enqueue(ctx context.Context, name string, fn func(context.Context) error) {
	if d == nil {
		return
	}
	d.start()
	job := asyncJob{name: name, fn: fn}
	select {
	case d.ch <- job:
		return
	default:
		if d.dropOnQueueFull {
			log.Warnf(ctx, "automation-recorder: async queue full; dropping job %s", name)
			return
		}
		d.ch <- job
	}
}

func deriveFields(p auditPayload) (derivedFields, error) {
	ad := p.AdditionalData
	out := derivedFields{}

	out.PayloadID = getString(ad, "payload_id")
	out.TransactionID = getString(ad, "transaction_id")
	out.MessageID = getString(ad, "message_id")
	out.SubscriberURL = getString(ad, "subscriber_url")
	out.Action = getString(ad, "action")
	out.Timestamp = getString(ad, "timestamp")
	out.APIName = getString(ad, "api_name")
	out.StatusCode = getInt64(ad, "status_code")
	out.TTLSecs = getInt64(ad, "ttl_seconds")
	out.CacheTTLSecs = getInt64(ad, "cache_ttl_seconds")
	out.IsMock = getBool(ad, "is_mock")
	out.SessionID = getString(ad, "session_id")

	// Backfill from requestBody.context if not provided in additionalData.
	ctxObj, _ := p.RequestBody["context"].(map[string]any)
	if strings.TrimSpace(out.MessageID) == "" && ctxObj != nil {
		out.MessageID = getString(ctxObj, "message_id")
	}

	if strings.TrimSpace(out.TransactionID) == "" {
		return derivedFields{}, fmt.Errorf("transaction_id is required in additionalData")
	}
	if strings.TrimSpace(out.SubscriberURL) == "" {
		return derivedFields{}, fmt.Errorf("subscriber_url is required in additionalData")
	}
	if strings.TrimSpace(out.Action) == "" {
		out.Action = "unknown_action"
	}
	if strings.TrimSpace(out.Timestamp) == "" {
		out.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if strings.TrimSpace(out.APIName) == "" {
		out.APIName = "unknown_api"
	}

	return out, nil
}

func sendLogsToNO(ctx context.Context, cfg config, client *http.Client, d derivedFields, requestBody, responseBody map[string]any) error {
	if strings.TrimSpace(cfg.NOURL) == "" {
		return nil
	}
	if len(cfg.NOEnabledIn) > 0 && !cfg.NOEnabledIn[cfg.Env] {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	client.Timeout = cfg.NOTimeout

	endpoint, err := url.JoinPath(cfg.NOURL, "/v1/api/push-txn-logs")
	if err != nil {
		return err
	}

	common := map[string]any{
		"payloadId":     d.PayloadID,
		"transactionId": d.TransactionID,
		"subscriberUrl": strings.TrimRight(d.SubscriberURL, "/"),
		"action":        d.Action,
		"timestamp":     d.Timestamp,
		"apiName":       d.APIName,
	}

	// Send request log.
	if err := postJSON(ctx, client, endpoint, cfg.NOToken, mergeMaps(common, map[string]any{"type": "request", "request": requestBody})); err != nil {
		return err
	}
	// Send response log.
	if err := postJSON(ctx, client, endpoint, cfg.NOToken, mergeMaps(common, map[string]any{"type": "response", "response": responseBody, "statusCode": d.StatusCode})); err != nil {
		return err
	}
	return nil
}

func savePayloadToDB(ctx context.Context, cfg config, client *http.Client, rdb *redis.Client, d derivedFields, requestBody, responseBody map[string]any, additionalData map[string]any) error {
	if strings.TrimSpace(cfg.DBBaseURL) == "" {
		return nil
	}
	if len(cfg.DBEnabledIn) > 0 && !cfg.DBEnabledIn[cfg.Env] {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	client.Timeout = cfg.DBTimeout

	// Load transaction from Redis; if it doesn't exist, match TS behavior and skip DB save.
	txn, err := loadTransactionMap(ctx, rdb, createTransactionKey(d.TransactionID, d.SubscriberURL))
	if err != nil {
		return err
	}
	if txn == nil {
		return nil
	}

	sessionId := strings.TrimSpace(getString(txn, "sessionId"))
	flowId := strings.TrimSpace(getString(txn, "flowId"))
	npType := strings.TrimSpace(getString(txn, "subscriberType"))

	if sessionId == "" {
		// Matches TS: key = sha256(transactionKey)
		sessionId = sha256Hex(createTransactionKey(d.TransactionID, d.SubscriberURL))
	}

	// Check/Create session in DB
	checkURL, err := url.JoinPath(cfg.DBBaseURL, cfg.DBSessionPath, "check", sessionId)
	if err != nil {
		return err
	}
	exists, err := getBoolJSON(ctx, client, checkURL, cfg.DBAPIKey)
	if err != nil {
		return err
	}
	if !exists {
		createURL, err := url.JoinPath(cfg.DBBaseURL, cfg.DBSessionPath)
		if err != nil {
			return err
		}
		domain := getContextString(requestBody, "domain")
		version := getContextString(requestBody, "version")
		if strings.TrimSpace(version) == "" {
			version = getContextString(requestBody, "core_version")
		}
		sessionPayload := map[string]any{
			"sessionId":      sessionId,
			"npType":         npType,
			"npId":           strings.TrimSpace(d.SubscriberURL),
			"domain":         domain,
			"version":        version,
			"sessionType":    "AUTOMATION",
			"sessionActive":  true,
		}
		if err := postJSONWithAPIKey(ctx, client, createURL, cfg.DBAPIKey, sessionPayload); err != nil {
			return err
		}
	}

	// Save payload
	payloadURL, err := url.JoinPath(cfg.DBBaseURL, cfg.DBPayloadPath)
	if err != nil {
		return err
	}

	action := strings.ToUpper(strings.TrimSpace(d.Action))
	messageID := strings.TrimSpace(d.MessageID)
	if messageID == "" {
		messageID = getContextString(requestBody, "message_id")
	}

	// Allow passing request headers from additionalData (plugin remap can populate this).
	reqHeader := any(map[string]any{})
	if additionalData != nil {
		if v, ok := additionalData["reqHeader"]; ok {
			reqHeader = v
		} else if v, ok := additionalData["req_header"]; ok {
			reqHeader = v
		} else if v, ok := additionalData["request_headers"]; ok {
			reqHeader = v
		}
	}

	requestPayload := map[string]any{
		"messageId":     messageID,
		"transactionId": strings.TrimSpace(d.TransactionID),
		"payloadId":     strings.TrimSpace(d.PayloadID),
		"action":        action,
		"bppId":         getContextString(requestBody, "bpp_id"),
		"bapId":         getContextString(requestBody, "bap_id"),
		"reqHeader":     reqHeader,
		"jsonRequest":   requestBody,
		"jsonResponse":  map[string]any{"response": responseBody},
		"httpStatus":    d.StatusCode,
		"flowId":        flowId,
		"sessionDetails": map[string]any{
			"sessionId": sessionId,
		},
	}

	return postJSONWithAPIKey(ctx, client, payloadURL, cfg.DBAPIKey, requestPayload)
}

func loadTransactionMap(ctx context.Context, rdb *redis.Client, key string) (map[string]any, error) {
	if rdb == nil || strings.TrimSpace(key) == "" {
		return nil, nil
	}
	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(val), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func getContextString(requestBody map[string]any, key string) string {
	ctxObj, _ := requestBody["context"].(map[string]any)
	if ctxObj == nil {
		return ""
	}
	return getString(ctxObj, key)
}

func getBoolJSON(ctx context.Context, client *http.Client, endpoint string, apiKey string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("http %s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var v any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return false, err
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	case map[string]any:
		if inner, ok := t["data"].(bool); ok {
			return inner, nil
		}
	}
	return false, nil
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, bearerToken string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(bearerToken) != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %s returned %d", endpoint, resp.StatusCode)
	}
	return nil
}

func postJSONWithAPIKey(ctx context.Context, client *http.Client, endpoint string, apiKey string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %s returned %d", endpoint, resp.StatusCode)
	}
	return nil
}

func getStatus(ctx context.Context, client *http.Client, endpoint string, apiKey string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func mergeMaps(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func getString(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func getInt64(m map[string]any, k string) int64 {
	v, ok := m[k]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err == nil {
			return parsed
		}
	}
	return 0
}

func getBool(m map[string]any, k string) bool {
	v, ok := m[k]
	if !ok || v == nil {
		return false
	}
	b, _ := v.(bool)
	return b
}

func parseEnvSet(v string) map[string]bool {
	res := map[string]bool{}
	for _, p := range strings.Split(v, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		res[p] = true
	}
	return res
}

func envBool(name string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}

func uuidV4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	buf := make([]byte, 36)
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf), nil
}

// Optional helper for parsing ints from env (kept for future expansion).
func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

