# Network Observability Plugin (Audit + Remap)

This plugin provides an `http.Handler` middleware that:

- Captures HTTP request + response data (bounded by size).
- Builds a canonical JSON object (`$.req`, `$.res`, `$.ctx`) for remapping.
- Applies a configurable remap using JSONPath (same library/dialect used in validator).
- Posts an audit event to a configured `audit_url` either synchronously or asynchronously.

## Configuration (YAML)

This plugin loads its configuration from a YAML file.

### How the host passes config

The plugin entrypoint still receives a `map[string]string` (host/plugin-framework interface), but it only uses it to locate the YAML path.

Supported keys (first match wins):

- `configPath`
- `config_path`
- `config`

### YAML schema

| Key                  |         Type |    Default | Notes                                                                                |
| -------------------- | -----------: | ---------: | ------------------------------------------------------------------------------------ |
| `audit_url`          |       string | (required) | Destination URL to POST/PUT audit events                                             |
| `audit_method`       |       string |     `POST` | Allowed: `POST`, `PUT`                                                               |
| `async`              |         bool |     `true` | If `true`, send via background workers                                               |
| `timeout_ms`         |          int |     `5000` | HTTP client timeout                                                                  |
| `queue_size`         |          int |     `1000` | Async queue length                                                                   |
| `worker_count`       |          int |        `2` | Async worker count                                                                   |
| `drop_on_queue_full` |         bool |     `true` | Drop+log when queue is full                                                          |
| `max_body_bytes`     |        int64 |  `1048576` | Capture limit for request/response bodies                                            |
| `include_raw_req`    |         bool |    `false` | Include `req` object in audit payload                                                |
| `include_raw_res`    |         bool |    `false` | Include `res` object in audit payload                                                |
| `audit_headers`      |          map |       `{}` | Outbound headers for the audit request                                               |
| `audit_bearer_token` |       string |       `""` | Convenience to set `Authorization: Bearer …` (unless already set in `audit_headers`) |
| `remap`              | object/array |       `{}` | Structured remap template (see below)                                                |
| `remap_flatten`      |         bool |    `false` | Flattens nested JSONPath results                                                     |

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

### Bearer token + headers + mapped payload

```yaml
audit_url: https://audit.example.com/events
audit_bearer_token: YOUR_TOKEN
include_raw_req: false
include_raw_res: false
audit_headers:
  X-Env: prod
remap:
  request_id: uuid()
  txn: "$.req.body.context.transaction_id"
  status: "$.res.status"
  session: "$.req.cookies.session_id"
```

Notes:

- Header keys are stored lowercased. For hyphenated headers, prefer bracket notation: `$.req.headers['x-request-id']`.
- This plugin logs failures but never blocks/fails the main request flow.
