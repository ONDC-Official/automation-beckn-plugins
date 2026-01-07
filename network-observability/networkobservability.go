package networkobservability

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AsaiYusuke/jsonpath"
	"github.com/beckn-one/beckn-onix/pkg/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Transport       string
	AuditURL        string
	AuditMethod     string
	Async           bool
	Timeout         time.Duration
	GRPCTarget      string
	GRPCInsecure    bool
	GRPCTimeout     time.Duration
	GRPCMethod      string
	QueueSize       int
	WorkerCount     int
	MaxBodyBytes    int64
	AuditHeaders    map[string]string
	BearerToken     string
	GRPCHeaders     map[string]string
	GRPCBearerToken string
	DropOnQueueFull bool
	RemapTemplate   any
}

// FileConfig is the YAML configuration schema.
// This keeps configuration structured (no JSON-in-string for remap).
type FileConfig struct {
	Transport       string            `yaml:"transport"`
	AuditURL        string            `yaml:"audit_url"`
	AuditMethod     string            `yaml:"audit_method"`
	Async           *bool             `yaml:"async"`
	TimeoutMs       *int              `yaml:"timeout_ms"`
	GRPCTarget      string            `yaml:"grpc_target"`
	GRPCInsecure    *bool             `yaml:"grpc_insecure"`
	GRPCTimeoutMs   *int              `yaml:"grpc_timeout_ms"`
	GRPCMethod      string            `yaml:"grpc_method"`
	QueueSize       *int              `yaml:"queue_size"`
	WorkerCount     *int              `yaml:"worker_count"`
	MaxBodyBytes    *int64            `yaml:"max_body_bytes"`
	Remap           any               `yaml:"remap"`
	DropOnQueueFull *bool             `yaml:"drop_on_queue_full"`
	AuditHeaders    map[string]string `yaml:"audit_headers"`
	BearerToken     string            `yaml:"audit_bearer_token"`
	GRPCHeaders     map[string]string `yaml:"grpc_headers"`
	GRPCBearerToken string            `yaml:"grpc_bearer_token"`
}

// NewNetworkObservabilityMiddleware loads plugin configuration from a YAML file.
func NewNetworkObservabilityMiddleware(ctx context.Context, configPath string) (func(http.Handler) http.Handler, error) {
	parsed, err := parseConfigFile(configPath)
	if err != nil {
		return nil, err
	}
	return newMiddlewareFromConfig(ctx, parsed)
}

func newMiddlewareFromConfig(ctx context.Context, parsed Config) (func(http.Handler) http.Handler, error) {
	transport := strings.ToLower(strings.TrimSpace(parsed.Transport))
	if transport == "" {
		transport = "http"
	}

	var sender auditSender
	switch transport {
	case "http":
		if strings.TrimSpace(parsed.AuditURL) == "" {
			log.Warnf(ctx, "network-observability: audit_url is empty; middleware is a no-op")
			return func(next http.Handler) http.Handler { return next }, nil
		}
		auditURL, err := url.Parse(parsed.AuditURL)
		if err != nil {
			return nil, err
		}
		headers := cloneStringMap(parsed.AuditHeaders)
		if parsed.BearerToken != "" {
			setAuthorizationIfMissing(headers, "Bearer "+parsed.BearerToken)
		}
		client := &http.Client{Timeout: parsed.Timeout}
		sender = &httpAuditSender{client: client, url: auditURL.String(), method: parsed.AuditMethod, headers: headers}
	case "grpc":
		if strings.TrimSpace(parsed.GRPCTarget) == "" {
			log.Warnf(ctx, "network-observability: grpc_target is empty; middleware is a no-op")
			return func(next http.Handler) http.Handler { return next }, nil
		}
		dialCtx := ctx
		if dialCtx == nil {
			dialCtx = context.Background()
		}
		if parsed.GRPCTimeout > 0 {
			var cancel context.CancelFunc
			dialCtx, cancel = context.WithTimeout(dialCtx, parsed.GRPCTimeout)
			defer cancel()
		}
		opts := []grpc.DialOption{grpc.WithBlock()}
		if parsed.GRPCInsecure {
			opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		} else {
			opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
		}
		conn, err := grpc.DialContext(dialCtx, parsed.GRPCTarget, opts...)
		if err != nil {
			return nil, fmt.Errorf("network-observability: failed to dial grpc_target %s: %w", parsed.GRPCTarget, err)
		}
		headers := cloneStringMap(parsed.GRPCHeaders)
		if parsed.GRPCBearerToken != "" {
			setAuthorizationIfMissing(headers, "Bearer "+parsed.GRPCBearerToken)
		}
		sender = &grpcAuditSender{conn: conn, timeout: parsed.GRPCTimeout, headers: headers, method: parsed.GRPCMethod}
	default:
		return nil, fmt.Errorf("network-observability: unsupported transport %q (allowed: http, grpc)", parsed.Transport)
	}

	dispatcher := newAuditDispatcher(ctx, sender, parsed.QueueSize, parsed.WorkerCount, parsed.DropOnQueueFull)

	remapTemplate := parsed.RemapTemplate
	if remapTemplate == nil {
		remapTemplate = map[string]any{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestUUID, uuidErr := uuidV4()
			if uuidErr != nil {
				log.Errorf(r.Context(), uuidErr, "network-observability: failed generating uuid")
				requestUUID = ""
			}

			_, reqBodyBytes, _ := captureRequestBody(r, parsed.MaxBodyBytes)

			crw := newCaptureResponseWriter(w, parsed.MaxBodyBytes)
			start := time.Now()
			next.ServeHTTP(crw, r)
			durationMs := time.Since(start).Milliseconds()

			resBodyBytes, _ := crw.bodyBytes()

			requestBody := parseJSONObjectOrEmpty(reqBodyBytes)
			responseBody := parseJSONObjectOrEmpty(resBodyBytes)
			sid := ""
			for _, c := range r.Cookies() {
				if c != nil && c.Name == "sid" {
					sid = c.Value
					break
				}
			}

			remapInput := map[string]any{
				"requestBody":  requestBody,
				"responseBody": responseBody,
				"ctx": map[string]any{
					"ts":          time.Now().UTC().Format(time.RFC3339Nano),
					"duration_ms": durationMs,
					"uuid":        requestUUID,
					"method":      r.Method,
					"path":        r.URL.Path,
					"status":      crw.StatusCode(),
					"sid":         sid,
				},
			}

			additionalData := applyRemap(remapInput, remapTemplate, requestUUID)
			payload := map[string]any{
				"requestBody":    requestBody,
				"responseBody":   responseBody,
				"additionalData": additionalData,
			}

			body, marshalErr := json.Marshal(payload)
			if marshalErr != nil {
				log.Errorf(r.Context(), marshalErr, "network-observability: failed to marshal audit payload")
				return
			}

			if parsed.Async {
				dispatcher.enqueue(r.Context(), body)
				return
			}

			dispatcher.sendNow(r.Context(), body)
		})
	}, nil
}

func parseConfigFile(configPath string) (Config, error) {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return Config{}, fmt.Errorf("network-observability: config path is empty")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return Config{}, fmt.Errorf("network-observability: failed to read config file at %s: %w", configPath, err)
	}

	var fileCfg FileConfig
	if err := yaml.Unmarshal(data, &fileCfg); err != nil {
		return Config{}, fmt.Errorf("network-observability: failed to parse YAML: %w", err)
	}

	transport := strings.ToLower(strings.TrimSpace(fileCfg.Transport))
	grpcTarget := strings.TrimSpace(fileCfg.GRPCTarget)
	if transport == "" {
		if grpcTarget != "" {
			transport = "grpc"
		} else {
			transport = "http"
		}
	}

	grpcMethod := strings.TrimSpace(fileCfg.GRPCMethod)
	if grpcMethod == "" {
		grpcMethod = "/beckn.audit.v1.AuditService/LogEvent"
	}

	auditMethod := strings.ToUpper(strings.TrimSpace(fileCfg.AuditMethod))
	if auditMethod == "" {
		auditMethod = http.MethodPost
	}
	if transport == "http" {
		if auditMethod != http.MethodPost && auditMethod != http.MethodPut {
			return Config{}, errors.New("network-observability: unsupported audit_method (allowed: POST, PUT)")
		}
	}

	async := true
	if fileCfg.Async != nil {
		async = *fileCfg.Async
	}
	timeoutMs := 5000
	if fileCfg.TimeoutMs != nil && *fileCfg.TimeoutMs > 0 {
		timeoutMs = *fileCfg.TimeoutMs
	}
	grpcTimeoutMs := timeoutMs
	if fileCfg.GRPCTimeoutMs != nil && *fileCfg.GRPCTimeoutMs > 0 {
		grpcTimeoutMs = *fileCfg.GRPCTimeoutMs
	}
	queueSize := 1000
	if fileCfg.QueueSize != nil && *fileCfg.QueueSize > 0 {
		queueSize = *fileCfg.QueueSize
	}
	workerCount := 2
	if fileCfg.WorkerCount != nil && *fileCfg.WorkerCount > 0 {
		workerCount = *fileCfg.WorkerCount
	}
	maxBodyBytes := int64(1024 * 1024)
	if fileCfg.MaxBodyBytes != nil {
		maxBodyBytes = *fileCfg.MaxBodyBytes
		if maxBodyBytes < 0 {
			maxBodyBytes = 0
		}
	}
	dropOnQueueFull := true
	if fileCfg.DropOnQueueFull != nil {
		dropOnQueueFull = *fileCfg.DropOnQueueFull
	}

	grpcInsecure := false
	if fileCfg.GRPCInsecure != nil {
		grpcInsecure = *fileCfg.GRPCInsecure
	}

	return Config{
		Transport:       transport,
		AuditURL:        strings.TrimSpace(fileCfg.AuditURL),
		AuditMethod:     auditMethod,
		Async:           async,
		Timeout:         time.Duration(timeoutMs) * time.Millisecond,
		GRPCTarget:      grpcTarget,
		GRPCInsecure:    grpcInsecure,
		GRPCTimeout:     time.Duration(grpcTimeoutMs) * time.Millisecond,
		GRPCMethod:      grpcMethod,
		QueueSize:       queueSize,
		WorkerCount:     workerCount,
		MaxBodyBytes:    maxBodyBytes,
		DropOnQueueFull: dropOnQueueFull,
		AuditHeaders:    fileCfg.AuditHeaders,
		BearerToken:     strings.TrimSpace(fileCfg.BearerToken),
		GRPCHeaders:     fileCfg.GRPCHeaders,
		GRPCBearerToken: strings.TrimSpace(fileCfg.GRPCBearerToken),
		RemapTemplate:   fileCfg.Remap,
	}, nil
}

func captureRequestBody(r *http.Request, maxBytes int64) (bool, []byte, bool) {
	if r.Body == nil {
		return false, nil, false
	}
	if maxBytes == 0 {
		return true, nil, true
	}
	limited := io.LimitReader(r.Body, maxBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return true, nil, false
	}
	truncated := int64(len(b)) > maxBytes
	if truncated {
		b = b[:maxBytes]
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return true, b, truncated
}

type captureResponseWriter struct {
	http.ResponseWriter
	status    int
	buf       bytes.Buffer
	maxBytes  int64
	truncated bool
}

func newCaptureResponseWriter(w http.ResponseWriter, maxBytes int64) *captureResponseWriter {
	return &captureResponseWriter{ResponseWriter: w, status: 0, maxBytes: maxBytes}
}

func (w *captureResponseWriter) WriteHeader(statusCode int) {
	if w.status == 0 {
		w.status = statusCode
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *captureResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.maxBytes > 0 && !w.truncated {
		remaining := w.maxBytes - int64(w.buf.Len())
		if remaining > 0 {
			if int64(len(p)) <= remaining {
				_, _ = w.buf.Write(p)
			} else {
				_, _ = w.buf.Write(p[:remaining])
				w.truncated = true
			}
		} else {
			w.truncated = true
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *captureResponseWriter) bodyBytes() ([]byte, bool) {
	return w.buf.Bytes(), w.truncated
}

func (w *captureResponseWriter) StatusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *captureResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *captureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (w *captureResponseWriter) Push(target string, opts *http.PushOptions) error {
	p, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}


func parseJSONObjectOrEmpty(b []byte) map[string]any {
	v, ok := tryParseJSON(b)
	if !ok {
		return map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return map[string]any{}
	}
	return m
}
type auditDispatcher struct {
	sender          auditSender
	ch              chan auditJob
	workerCount     int
	dropOnQueueFull bool
	startOnce       sync.Once
	baseCtx         context.Context
}

type auditJob struct {
	ctx  context.Context
	body []byte
}


type auditSender interface {
	Send(ctx context.Context, body []byte) error
}

type httpAuditSender struct {
	client  *http.Client
	url     string
	method  string
	headers map[string]string
}

func (s *httpAuditSender) Send(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, s.method, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range s.headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warnf(ctx, "network-observability: audit dispatch got status %d", resp.StatusCode)
	}
	return nil
}

type grpcAuditSender struct {
	conn    *grpc.ClientConn
	timeout time.Duration
	headers map[string]string
	method  string
}

func (s *grpcAuditSender) Send(ctx context.Context, body []byte) error {
	callCtx := ctx
	if s.timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	md := metadata.MD{}
	for k, v := range s.headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" {
			continue
		}
		md.Append(lk, v)
	}
	if len(md) > 0 {
		callCtx = metadata.NewOutgoingContext(callCtx, md)
	}

	fullMethod := strings.TrimSpace(s.method)
	if fullMethod == "" {
		fullMethod = "/beckn.audit.v1.AuditService/LogEvent"
	}

	req := wrapperspb.Bytes(body)
	res := &emptypb.Empty{}
	return s.conn.Invoke(callCtx, fullMethod, req, res)
}

func newAuditDispatcher(baseCtx context.Context, sender auditSender, queueSize, workerCount int, dropOnQueueFull bool) *auditDispatcher {
	return &auditDispatcher{
		sender:          sender,
		ch:              make(chan auditJob, queueSize),
		workerCount:     workerCount,
		dropOnQueueFull: dropOnQueueFull,
		baseCtx:         baseCtx,
	}
}

func (d *auditDispatcher) start() {
	d.startOnce.Do(func() {
		for i := 0; i < d.workerCount; i++ {
			go d.worker()
		}
	})
}

func (d *auditDispatcher) enqueue(ctx context.Context, body []byte) {
	d.start()
	job := auditJob{ctx: ctx, body: body}
	select {
	case d.ch <- job:
	default:
		if !d.dropOnQueueFull {
			d.ch <- job
			return
		}
		log.Warnf(ctx, "network-observability: audit queue full; dropping event")
	}
}

func (d *auditDispatcher) worker() {
	for job := range d.ch {
		d.sendNow(job.ctx, job.body)
	}
}

func (d *auditDispatcher) sendNow(ctx context.Context, body []byte) {
	requestCtx := ctx
	if requestCtx == nil {
		requestCtx = d.baseCtx
	}
	if err := d.sender.Send(requestCtx, body); err != nil {
		log.Errorf(requestCtx, err, "network-observability: audit dispatch failed")
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func setAuthorizationIfMissing(headers map[string]string, value string) {
	for k := range headers {
		if strings.EqualFold(strings.TrimSpace(k), "authorization") {
			return
		}
	}
	headers["Authorization"] = value
}

func buildRemapInput(r *http.Request, reqBody []byte, reqTruncated bool, crw *captureResponseWriter, resBody []byte, resTruncated bool, requestUUID string, durationMs int64) map[string]any {
	urlStr := ""
	if r.URL != nil {
		urlStr = r.URL.String()
	}

	reqHeaders, reqHeadersAll := headerMaps(r.Header)
	resHeaders, resHeadersAll := headerMaps(crw.Header())

	query, queryAll := queryMaps(r.URL)
	cookies := cookieMap(r)

	reqBodyObj, reqBodyIsJSON := tryParseJSON(reqBody)
	resBodyObj, resBodyIsJSON := tryParseJSON(resBody)

	remapInput := map[string]any{
		"req": map[string]any{
			"method":      r.Method,
			"host":        r.Host,
			"path":        r.URL.Path,
			"url":         urlStr,
			"headers":     reqHeaders,
			"headersAll":  reqHeadersAll,
			"query":       query,
			"queryAll":    queryAll,
			"cookies":     cookies,
			"body":        reqBodyObj,
			"bodyIsJSON":  reqBodyIsJSON,
			"bodyRaw":     bytesToStringOrBase64(reqBody),
			"truncated":   reqTruncated,
			"remote_addr": r.RemoteAddr,
		},
		"res": map[string]any{
			"status":     crw.StatusCode(),
			"headers":    resHeaders,
			"headersAll": resHeadersAll,
			"body":       resBodyObj,
			"bodyIsJSON": resBodyIsJSON,
			"bodyRaw":    bytesToStringOrBase64(resBody),
			"truncated":  resTruncated,
		},
		"ctx": map[string]any{
			"ts": time.Now().UTC().Format(time.RFC3339Nano),
			"gen": map[string]any{
				"uuid": requestUUID,
			},
			"duration_ms": durationMs,
		},
	}

	return remapInput
}

func headerMaps(h http.Header) (map[string]any, map[string]any) {
	first := map[string]any{}
	all := map[string]any{}
	for k, vs := range h {
		lk := strings.ToLower(k)
		if len(vs) == 0 {
			continue
		}
		all[lk] = append([]string(nil), vs...)
		first[lk] = vs[0]
	}
	return first, all
}

func queryMaps(u *url.URL) (map[string]any, map[string]any) {
	first := map[string]any{}
	all := map[string]any{}
	if u == nil {
		return first, all
	}
	q := u.Query()
	for k, vs := range q {
		if len(vs) == 0 {
			continue
		}
		all[k] = append([]string(nil), vs...)
		first[k] = vs[0]
	}
	return first, all
}

func cookieMap(r *http.Request) map[string]any {
	res := map[string]any{}
	for _, c := range r.Cookies() {
		res[c.Name] = c.Value
	}
	return res
}

func tryParseJSON(b []byte) (any, bool) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, false
	}
	return v, true
}

func bytesToStringOrBase64(b []byte) any {
	if b == nil {
		return nil
	}
	// If it's valid UTF-8, return string; otherwise return base64.
	if utf8.Valid(b) {
		return string(b)
	}
	return map[string]any{"base64": base64.StdEncoding.EncodeToString(b)}
}

func applyRemap(root any, template any, requestUUID string) any {
	switch t := template.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range t {
			out[k] = applyRemap(root, v, requestUUID)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, v := range t {
			out = append(out, applyRemap(root, v, requestUUID))
		}
		return out
	case string:
		expr := strings.TrimSpace(t)
		if expr == "" {
			return ""
		}
		// Built-ins (kept minimal): uuid() and now().
		if expr == "uuid()" {
			return requestUUID
		}
		if expr == "now()" {
			return time.Now().UTC().Format(time.RFC3339Nano)
		}
		if strings.HasPrefix(expr, "$") {
			return evalJSONPath(root, expr)
		}
		return t
	default:
		return template
	}
}

func evalJSONPath(root any, expr string) any {
	results, err := jsonpath.Retrieve(expr, root)
	if err != nil {
		return nil
	}
	if len(results) == 0 {
		return nil
	}
	if len(results) == 1 {
		return results[0]
	}
	return results
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