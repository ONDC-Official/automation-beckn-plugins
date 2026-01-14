package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	grpcServiceName = "beckn.audit.v1.AuditService"
	grpcFullMethod  = "/" + grpcServiceName + "/LogEvent"
)

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
	log.Infof(ctx, "[GRPC] LogEvent called, payload size: %d bytes", len(in.GetValue()))
	
	if in == nil {
		log.Errorf(ctx, nil, "[GRPC] ERROR: Request is nil")
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	var payload auditPayload
	if err := json.Unmarshal(in.Value, &payload); err != nil {
		log.Errorf(ctx, err, "[GRPC] ERROR: Failed to unmarshal payload")
		return nil, status.Error(codes.InvalidArgument, "invalid JSON")
	}
	if payload.RequestBody == nil {
		log.Errorf(ctx, nil, "[GRPC] ERROR: requestBody is nil")
		return nil, status.Error(codes.InvalidArgument, "requestBody must be a JSON object")
	}
	if payload.ResponseBody == nil {
		log.Errorf(ctx, nil, "[GRPC] ERROR: responseBody is nil")
		return nil, status.Error(codes.InvalidArgument, "responseBody must be a JSON object")
	}
	if payload.AdditionalData == nil {
		payload.AdditionalData = map[string]any{}
	}

	log.Infof(ctx, "[GRPC] Deriving fields from payload...")
	derived, err := deriveFields(payload)
	if err != nil {
		log.Errorf(ctx, err, "[GRPC] ERROR: Failed to derive fields")
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	log.Infof(ctx, "[GRPC] Transaction: %s, Action: %s, Subscriber: %s", derived.TransactionID, derived.Action, derived.SubscriberURL)
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
		log.Infof(ctx, "[GRPC] Updating cache for key: %s (TTL: %v)", key, cacheTTL)
		in := cacheAppendInput{
			PayloadID:     derived.PayloadID,
			TransactionID: derived.TransactionID,
			MessageID:     derived.MessageID,
			SubscriberURL: derived.SubscriberURL,
			Action:        derived.Action,
			Timestamp:     derived.Timestamp,
			TTLSecs:       derived.TTLSecs,
			Response:      payload.ResponseBody,
		}
		if err := updateTransactionAtomically(ctx, s.rdb, key, &in, cacheTTL); err != nil {
			log.Errorf(ctx, err, "[GRPC] ERROR: Cache update failed")
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

		log.Infof(ctx, "[GRPC] Cache updated successfully")
	} else {
		log.Infof(ctx, "[GRPC] Cache update skipped (SkipCacheUpdate=true)")
	}

	// Fire-and-forget side effects.
	baseCtx := context.Background()
	if !s.cfg.SkipNOPush {
		log.Infof(ctx, "[GRPC] Enqueueing Network Observability push")
		s.async.enqueue(baseCtx, "no-push", func(ctx context.Context) error {
			return sendLogsToNO(ctx, s.cfg, s.httpClient, derived, payload.RequestBody, payload.ResponseBody)
		})
	} else {
		log.Infof(ctx, "[GRPC] NO push skipped (SkipNOPush=true)")
	}
	if !s.cfg.SkipDBSave {
		log.Infof(ctx, "[GRPC] Enqueueing database save")
		s.async.enqueue(baseCtx, "db-save", func(ctx context.Context) error {
			return savePayloadToDB(ctx, s.cfg, s.httpClient, s.rdb, derived, payload.RequestBody, payload.ResponseBody, payload.AdditionalData)
		})
	} else {
		log.Infof(ctx, "[GRPC] DB save skipped (SkipDBSave=true)")
	}

	log.Infof(ctx, "[GRPC] LogEvent completed successfully")
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
