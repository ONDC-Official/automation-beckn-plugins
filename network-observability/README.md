# Network Observability Plugin (Audit + Remap)

This plugin provides an `http.Handler` middleware that:

- Captures HTTP request + response data (bounded by size).
- Emits a fixed JSON payload `{ requestBody, responseBody, additionalData }`.
- Builds a canonical JSON object (`$.requestBody`, `$.responseBody`, `$.ctx`) for remapping `additionalData`.
- Applies a configurable remap using JSONPath (same library/dialect used in validator).
- Sends the payload to a configured destination (HTTP or gRPC) either synchronously or asynchronously.

## Reusable remap package

The JSONPath remap + request parsing helpers are also available as a standalone Go module:

- `github.com/beckn-one/beckn-onix/httprequestremap`

For local development in this repo, use a `replace` like:

```go
require github.com/beckn-one/beckn-onix/httprequestremap v0.0.0
replace github.com/beckn-one/beckn-onix/httprequestremap => ../httprequestremap
```

## Configuration (YAML)

This plugin loads its configuration from a YAML file.

### How the host passes config

The plugin entrypoint still receives a `map[string]string` (host/plugin-framework interface), but it only uses it to locate the YAML path.

Supported keys (first match wins):

- `configPath`
- `config_path`
- `config`

### YAML schema

| Key                  |         Type |      Default | Notes                                                                                      |
| -------------------- | -----------: | -----------: | ------------------------------------------------------------------------------------------ |
| `transport`          |       string |       `http` | Allowed: `http`, `grpc`. If omitted and `grpc_target` is set, transport defaults to `grpc` |
| `audit_url`          |       string |   (required) | **HTTP only**. Destination URL to POST/PUT audit events                                    |
| `audit_method`       |       string |       `POST` | **HTTP only**. Allowed: `POST`, `PUT`                                                      |
| `timeout_ms`         |          int |       `5000` | **HTTP only**. HTTP client timeout                                                         |
| `audit_headers`      |          map |         `{}` | **HTTP only**. Outbound headers for the audit request                                      |
| `audit_bearer_token` |       string |         `""` | **HTTP only**. Convenience to set `Authorization: Bearer …` (unless already set)           |
| `grpc_target`        |       string |         `""` | **gRPC only**. Target passed to `grpc.Dial` (e.g. `host:port`)                             |
| `grpc_insecure`      |         bool |      `false` | **gRPC only**. If `true`, uses insecure transport credentials                              |
| `grpc_timeout_ms`    |          int | `timeout_ms` | **gRPC only**. Per-RPC timeout (and dial timeout)                                          |
| `grpc_method`        |       string |      (audit) | **gRPC only**. Full gRPC method (e.g. `/beckn.audit.v1.AuditService/LogEvent`)             |
| `grpc_headers`       |          map |         `{}` | **gRPC only**. Outgoing metadata (lowercased keys)                                         |
| `grpc_bearer_token`  |       string |         `""` | **gRPC only**. Convenience to set `authorization: Bearer …` (unless already set)           |
| `async`              |         bool |       `true` | If `true`, send via background workers                                                     |
| `queue_size`         |          int |       `1000` | Async queue length                                                                         |
| `worker_count`       |          int |          `2` | Async worker count                                                                         |
| `drop_on_queue_full` |         bool |       `true` | Drop+log when queue is full                                                                |
| `max_body_bytes`     |        int64 |    `1048576` | Capture limit for request/response bodies                                                  |
| `remap`              | object/array |         `{}` | Structured remap template (see below)                                                      |

## Outgoing payload schema

The plugin always sends exactly this JSON object (for both HTTP and gRPC):

```json
{
	"requestBody": {},
	"responseBody": {},
	"additionalData": {}
}
```

Notes:

- `requestBody` is always a JSON object. If the incoming request body is empty or not a JSON object, it becomes `{}`.
- `responseBody` is always a JSON object. If the response body is empty or not a JSON object, it becomes `{}`.
- `additionalData` is the result of applying the `remap` template.

## Remap input schema

The remap engine evaluates JSONPath against a canonical root object.

Top-level keys:

- `$.requestBody`: JSON object (always present)
- `$.responseBody`: JSON object (always present)
- `$.ctx`: middleware context

### `$.ctx`

- `$.ctx.ts` (string RFC3339Nano)
- `$.ctx.duration_ms` (int)
- `$.ctx.uuid` (string): generated UUID v4 for this request
- `$.ctx.method` (string)
- `$.ctx.path` (string)
- `$.ctx.status` (int)
- `$.ctx.sid` (string): cookie `sid` if present (empty string if absent)
- `$.ctx.headers` (object): request headers (lowercased keys), first value only
- `$.ctx.headers_all` (object): request headers (lowercased keys), all values as arrays
- `$.ctx.cookies` (object): cookies as a simple `{name: value}` map

## `remap` semantics

- Any string value beginning with `$` is treated as a JSONPath expression.
- Any other string value is treated as a literal string.
- Special built-ins (strings):
  - `uuid()` => the generated UUID for this request
  - `now()` => current UTC time RFC3339Nano

JSONPath result shaping:

- 0 results => `null`
- 1 result => scalar value
- > 1 results => array

## Example

### Minimal audit (YAML)

```yaml
audit_url: https://audit.example.com/events
async: true
remap: {}
```

### gRPC audit

The default gRPC method invoked is `/beckn.audit.v1.AuditService/LogEvent`.

Proto reference: `proto/audit.proto`

```yaml
transport: grpc
grpc_target: audit-collector.example.com:443
grpc_insecure: false
grpc_timeout_ms: 5000
grpc_method: /beckn.audit.v1.AuditService/LogEvent
grpc_headers:
  x-env: prod
grpc_bearer_token: YOUR_TOKEN
async: true
remap:
  request_id: uuid()
  txn: "$.requestBody.context.transaction_id"
  status: "$.ctx.status"
```

### gRPC direct to recorder service (local/dev)

If you want the plugin to call the recorder service directly, point `grpc_method` at the recorder RPC.

Recorder RPC: `/beckn.audit.v1.AuditService/LogEvent`

The plugin will still send the fixed envelope `{requestBody,responseBody,additionalData}`.

The recorder service derives cache + DB fields from `additionalData` (see its `proto/audit.proto`).

```yaml
transport: grpc
grpc_target: 127.0.0.1:8089
grpc_insecure: true
grpc_method: /beckn.audit.v1.AuditService/LogEvent
grpc_timeout_ms: 5000
async: true

# The recorder expects these values inside additionalData.
# Tip: the recorder's Redis transaction key is `transaction_id::subscriber_url` after trimming spaces
# and trimming a trailing `/` from subscriber_url.
remap:
  # Identifiers
  payload_id: uuid()
  transaction_id: "$.requestBody.context.transaction_id"
  message_id: "$.requestBody.context.message_id"

  # Choose the correct URI field for your side:
  # - BPP side: $.requestBody.context.bpp_uri
  # - BAP side: $.requestBody.context.bap_uri
  subscriber_url: "$.requestBody.context.bpp_uri"

  # Cache metadata
  action: "$.requestBody.context.action"
  timestamp: "$.requestBody.context.timestamp"
  api_name: "$.ctx.path"
  status_code: "$.ctx.status"
  ttl_seconds: 30
  cache_ttl_seconds: 600

  # Optional flags used by DB/metrics
  is_mock: false
  session_id: "$.ctx.sid" # or map your own session id

  # Optional: pass request headers into DB payload (mirrors TS `reqHeader`)
  # - Use headers_all if you want to preserve multi-value headers.
  req_header: "$.ctx.headers_all"
```

### Bearer token + headers + mapped payload

```yaml
audit_url: https://audit.example.com/events
audit_bearer_token: YOUR_TOKEN
audit_headers:
  X-Env: prod
remap:
  request_id: uuid()
  txn: "$.requestBody.context.transaction_id"
  status: "$.ctx.status"
```

Notes:

- This plugin logs failures but never blocks/fails the main request flow.
