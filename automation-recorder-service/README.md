# Automation Recorder Service (Audit Consumer)

A standalone gRPC microservice that consumes audit events from the network-observability plugin and performs 3 side-effects:

1. Update transaction cache in Redis (**sync**, part of the gRPC call)
2. Send logs to NO (**async**, best-effort)
3. Save payload to DB (**async**, best-effort)

This mirrors the behavior that previously lived in the TypeScript API service route wrapper.

## Configuration

Environment variables:

- `RECORDER_LISTEN_ADDR` (default `:8089`)
- `REDIS_ADDR` (default `127.0.0.1:6379`)
- `REDIS_PASSWORD` (optional)

Feature flags:

- `RECORDER_SKIP_CACHE_UPDATE` (default `false`)
- `RECORDER_SKIP_NO_PUSH` (default `false`)
- `RECORDER_SKIP_DB_SAVE` (default `false`)

Async worker settings (applies to NO + DB tasks):

- `RECORDER_ASYNC_QUEUE_SIZE` (default `1000`)
- `RECORDER_ASYNC_WORKERS` (default `2`)
- `RECORDER_ASYNC_DROP_ON_FULL` (default `true`)

General:

- `RECORDER_ENV` (default `dev`)

Cache defaults (used if `additionalData` does not provide values):

- `RECORDER_API_TTL_SECONDS_DEFAULT` (default `30`)
- `RECORDER_CACHE_TTL_SECONDS_DEFAULT` (default `0`)

NO settings:

- `RECORDER_NO_URL` (default empty = disabled)
- `RECORDER_NO_BEARER_TOKEN` (optional)
- `RECORDER_NO_TIMEOUT_MS` (default `5000`)
- `RECORDER_NO_ENABLED_ENVS` (optional CSV, e.g. `staging,prod`; empty means enabled in all envs)

DB settings:

- `RECORDER_DB_BASE_URL` (default empty = disabled)
- `RECORDER_DB_API_KEY` (optional)
- `RECORDER_DB_TIMEOUT_MS` (default `5000`)
- `RECORDER_DB_ENABLED_ENVS` (optional CSV; empty means enabled in all envs)

## gRPC API

Service name: `beckn.audit.v1.AuditService`

Method:

- `LogEvent(google.protobuf.BytesValue) returns (google.protobuf.Empty)`

The request is **raw JSON bytes**.

### Request JSON schema

```json
{
	"requestBody": {},
	"responseBody": {},
	"additionalData": {
		"payload_id": "...",
		"transaction_id": "t1",
		"subscriber_url": "https://buyer.example.com",
		"action": "on_search",
		"timestamp": "2026-01-07T00:00:00Z",
		"api_name": "search",
		"ttl_seconds": 30,
		"cache_ttl_seconds": 600,
		"status_code": 200,
		"is_mock": false,
		"session_id": "optional"
	}
}
```

Notes:

- `requestBody` and `responseBody` must be JSON objects.
- `additionalData.transaction_id` and `additionalData.subscriber_url` are required.
- Transaction must already exist in Redis. If missing, the server returns `NOT_FOUND` (same behavior as the TS cache update).
- Redis key format: `transaction_id + "::" + subscriber_url` after trimming spaces and trimming a trailing `/`.
- `cache_ttl_seconds` controls Redis key expiry. `0` means no expiry.

## Run

From this folder:

- `go test ./...`
- `go run .`

To point at Redis:

- `REDIS_ADDR=127.0.0.1:6379 RECORDER_LISTEN_ADDR=:8089 go run .`
