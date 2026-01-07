# Network Observability Plugin (Audit + Remap)

This plugin provides an `http.Handler` middleware that:

- Captures HTTP request + response data (bounded by size).
- Builds a canonical JSON object (`$.req`, `$.res`, `$.ctx`) for remapping.
- Applies a configurable remap using JSONPath (same library/dialect used in validator).
- Posts an audit event to a configured `audit_url` either synchronously or asynchronously.

## Configuration

The plugin is constructed via:

- `NewNetworkObservabilityMiddleware(ctx, config map[string]string)`

### Config keys

- `audit_url` (string, required): Destination URL to POST/PUT audit events.
- `audit_method` (string, optional): `POST` (default) or `PUT`.
- `async` (bool, default `true`):
  - `true`: enqueue audit events and send on background worker(s).
  - `false`: send audit event inline after handler completes.
- `timeout_ms` (int, default `5000`): HTTP client timeout.
- `queue_size` (int, default `1000`): buffered queue length (async mode).
- `worker_count` (int, default `2`): number of background workers (async mode).
- `drop_on_queue_full` (bool, default `true`): if `true`, drop and log when queue is full.
- `max_body_bytes` (int64, default `1048576`): max bytes captured for request/response body.
- `include_raw_req` (bool, default `false`): include `req` object in the audit payload.
- `include_raw_res` (bool, default `false`): include `res` object in the audit payload.

Auth headers for the audit request:

- `audit_bearer_token` (string, optional): convenience key to set `Authorization: Bearer <token>`.
- `audit_headers_json` (string JSON object, optional): arbitrary outbound headers.
  - If `audit_headers_json` contains `Authorization`, it is not overwritten.

Remapping:

- `remap_json` (string JSON, optional): JSON template where string values may be JSONPath expressions.
- `remap_flatten` (bool, default `false`): if `true`, flattens nested JSONPath result slices.

## Remap input schema

The remap engine evaluates JSONPath against a canonical root object.

Top-level keys:

- `$.req`: request information
- `$.res`: response information
- `$.ctx`: middleware context (generated values, timestamps)

### `$.req`

- `$.req.method` (string)
- `$.req.host` (string)
- `$.req.path` (string)
- `$.req.url` (string)
- `$.req.remote_addr` (string)
- `$.req.headers` (object): first-value map of request headers **lowercased**
- `$.req.headersAll` (object): full header map (lowercased) as arrays
- `$.req.query` (object): first-value map
- `$.req.queryAll` (object): full query map as arrays
- `$.req.cookies` (object): cookie name -> value
- `$.req.body` (object|null): parsed JSON object if body is valid JSON; otherwise `null`
- `$.req.bodyRaw` (string|object|null): body as string (if UTF-8) or `{ "base64": "..." }`
- `$.req.truncated` (bool): body was truncated by `max_body_bytes`

### `$.res`

- `$.res.status` (int)
- `$.res.headers` / `$.res.headersAll` (same shape rules as request)
- `$.res.body` / `$.res.bodyRaw`
- `$.res.truncated` (bool)

### `$.ctx`

- `$.ctx.ts` (string RFC3339Nano)
- `$.ctx.duration_ms` (int)
- `$.ctx.gen.uuid` (string): generated UUID v4 for this request

## `remap_json` semantics

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

### Minimal audit

```json
{
	"audit_url": "https://audit.example.com/events",
	"async": "true"
}
```

### Bearer token + mapped payload

```json
{
	"audit_url": "https://audit.example.com/events",
	"audit_bearer_token": "YOUR_TOKEN",
	"include_raw_req": "false",
	"include_raw_res": "false",
	"remap_json": "{\"request_id\":\"uuid()\",\"txn\":\"$.req.body.context.transaction_id\",\"status\":\"$.res.status\",\"session\":\"$.req.cookies.session_id\"}"
}
```

Notes:

- Header keys are stored lowercased. For hyphenated headers, prefer bracket notation: `$.req.headers['x-request-id']`.
- This plugin logs failures but never blocks/fails the main request flow.
