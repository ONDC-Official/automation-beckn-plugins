package networkobservability

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
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
	"gopkg.in/yaml.v3"
)

type Config struct {
	AuditURL        string
	AuditMethod     string
	Async           bool
	Timeout         time.Duration
	QueueSize       int
	WorkerCount     int
	MaxBodyBytes    int64
	IncludeRawReq   bool
	IncludeRawRes   bool
	AuditHeaders    map[string]string
	BearerToken     string
	RemapFlatten    bool
	DropOnQueueFull bool
	RemapTemplate   any
}

// FileConfig is the YAML configuration schema.
// This keeps configuration structured (no JSON-in-string for remap).
type FileConfig struct {
	AuditURL        string            `yaml:"audit_url"`
	AuditMethod     string            `yaml:"audit_method"`
	Async           *bool             `yaml:"async"`
	TimeoutMs       *int              `yaml:"timeout_ms"`
	QueueSize       *int              `yaml:"queue_size"`
	WorkerCount     *int              `yaml:"worker_count"`
	MaxBodyBytes    *int64            `yaml:"max_body_bytes"`
	IncludeRawReq   *bool             `yaml:"include_raw_req"`
	IncludeRawRes   *bool             `yaml:"include_raw_res"`
	Remap           any               `yaml:"remap"`
	RemapFlatten    *bool             `yaml:"remap_flatten"`
	DropOnQueueFull *bool             `yaml:"drop_on_queue_full"`
	AuditHeaders    map[string]string `yaml:"audit_headers"`
	BearerToken     string            `yaml:"audit_bearer_token"`
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
	if parsed.AuditURL == "" {
		log.Warnf(ctx, "network-observability: audit_url is empty; middleware is a no-op")
		return func(next http.Handler) http.Handler { return next }, nil
	}

	auditURL, err := url.Parse(parsed.AuditURL)
	if err != nil {
		return nil, err
	}

	headers := map[string]string{}
	for k, v := range parsed.AuditHeaders {
		headers[k] = v
	}
	if parsed.BearerToken != "" {
		if _, ok := headers["Authorization"]; !ok {
			headers["Authorization"] = "Bearer " + parsed.BearerToken
		}
	}

	client := &http.Client{Timeout: parsed.Timeout}
	dispatcher := newAuditDispatcher(ctx, client, auditURL.String(), parsed.AuditMethod, headers, parsed.QueueSize, parsed.WorkerCount, parsed.DropOnQueueFull)

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

			_, reqBodyBytes, reqTruncated := captureRequestBody(r, parsed.MaxBodyBytes)

			crw := newCaptureResponseWriter(w, parsed.MaxBodyBytes)
			start := time.Now()
			next.ServeHTTP(crw, r)
			durationMs := time.Since(start).Milliseconds()

			resBodyBytes, resTruncated := crw.bodyBytes()

			remapInput := buildRemapInput(r, reqBodyBytes, reqTruncated, crw, resBodyBytes, resTruncated, requestUUID, durationMs)
			mapped := applyRemap(remapInput, remapTemplate, requestUUID, parsed.RemapFlatten)

			auditPayload := map[string]any{
				"ts":     time.Now().UTC().Format(time.RFC3339Nano),
				"mapped": mapped,
				"ctx": map[string]any{
					"gen": map[string]any{"uuid": requestUUID},
				},
			}

			if parsed.IncludeRawReq {
				auditPayload["req"] = remapInput["req"]
			}
			if parsed.IncludeRawRes {
				auditPayload["res"] = remapInput["res"]
			}

			body, marshalErr := json.Marshal(auditPayload)
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

	auditMethod := strings.ToUpper(strings.TrimSpace(fileCfg.AuditMethod))
	if auditMethod == "" {
		auditMethod = http.MethodPost
	}
	if auditMethod != http.MethodPost && auditMethod != http.MethodPut {
		return Config{}, errors.New("network-observability: unsupported audit_method (allowed: POST, PUT)")
	}

	async := true
	if fileCfg.Async != nil {
		async = *fileCfg.Async
	}
	timeoutMs := 5000
	if fileCfg.TimeoutMs != nil && *fileCfg.TimeoutMs > 0 {
		timeoutMs = *fileCfg.TimeoutMs
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
	includeRawReq := false
	if fileCfg.IncludeRawReq != nil {
		includeRawReq = *fileCfg.IncludeRawReq
	}
	includeRawRes := false
	if fileCfg.IncludeRawRes != nil {
		includeRawRes = *fileCfg.IncludeRawRes
	}
	remapFlatten := false
	if fileCfg.RemapFlatten != nil {
		remapFlatten = *fileCfg.RemapFlatten
	}
	dropOnQueueFull := true
	if fileCfg.DropOnQueueFull != nil {
		dropOnQueueFull = *fileCfg.DropOnQueueFull
	}

	return Config{
		AuditURL:        strings.TrimSpace(fileCfg.AuditURL),
		AuditMethod:     auditMethod,
		Async:           async,
		Timeout:         time.Duration(timeoutMs) * time.Millisecond,
		QueueSize:       queueSize,
		WorkerCount:     workerCount,
		MaxBodyBytes:    maxBodyBytes,
		IncludeRawReq:   includeRawReq,
		IncludeRawRes:   includeRawRes,
		RemapFlatten:    remapFlatten,
		DropOnQueueFull: dropOnQueueFull,
		AuditHeaders:    fileCfg.AuditHeaders,
		BearerToken:     strings.TrimSpace(fileCfg.BearerToken),
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
		return nil, nil, errors.New("hijacker not supported")
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

type auditDispatcher struct {
	client          *http.Client
	url             string
	method          string
	headers         map[string]string
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

func newAuditDispatcher(baseCtx context.Context, client *http.Client, url, method string, headers map[string]string, queueSize, workerCount int, dropOnQueueFull bool) *auditDispatcher {
	return &auditDispatcher{
		client:          client,
		url:             url,
		method:          method,
		headers:         headers,
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
	req, err := http.NewRequestWithContext(requestCtx, d.method, d.url, bytes.NewReader(body))
	if err != nil {
		log.Errorf(requestCtx, err, "network-observability: failed to create audit request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range d.headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		log.Errorf(requestCtx, err, "network-observability: audit dispatch failed")
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warnf(requestCtx, "network-observability: audit dispatch got status %d", resp.StatusCode)
	}
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

func applyRemap(root any, template any, requestUUID string, flatten bool) any {
	switch t := template.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range t {
			out[k] = applyRemap(root, v, requestUUID, flatten)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, v := range t {
			out = append(out, applyRemap(root, v, requestUUID, flatten))
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
			return evalJSONPath(root, expr, flatten)
		}
		return t
	default:
		return template
	}
}

func evalJSONPath(root any, expr string, flatten bool) any {
	results, err := jsonpath.Retrieve(expr, root)
	if err != nil {
		return nil
	}
	if flatten {
		results = flattenAnySlice(results)
	}
	if len(results) == 0 {
		return nil
	}
	if len(results) == 1 {
		return results[0]
	}
	return results
}

func flattenAnySlice(in []any) []any {
	var out []any
	var walk func(v any)
	walk = func(v any) {
		s, ok := v.([]any)
		if ok {
			for _, item := range s {
				walk(item)
			}
			return
		}
		out = append(out, v)
	}
	for _, v := range in {
		walk(v)
	}
	return out
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