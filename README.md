# OpenAI Upstream Quota Control Plugin

This Go dynamic-library plugin provides quota and rate-limit control for OpenAI-compatible providers (`openai-compatibility`, OpenRouter, DeepSeek, self-hosted vLLM/Ollama, etc.), driven by an external RESTful quota management service.

It declares four capabilities:
- `request_interceptor`: intercepts execution requests after credential selection (`request.intercept_after`) to verify upstream credential balance and rate limits; immediately blocks requests with an OpenAI-compatible `429 Too Many Requests` error if quota is exhausted.
- `request_lifecycle_plugin`: tracks request completion states.
- `usage_plugin`: captures token accounting (`usage.handle`) after execution and asynchronously reports/deducts token consumption at the external quota API.
- `quota_provider`: exposes credential quota, balance, and billing information for CLIProxyAPI management UI and Management API (`/v1/management/quota/fetch` and `/v1/management/quota/reset`).

## Architecture & Workflow

```text
Downstream Client               CLIProxyAPI (Host)           openai-quota-control Plugin        External Quota API
      |                                 |                                 |                             |
      |-- 1. POST /v1/chat/completions ->|                                 |                             |
      |                                 |-- 2. request.intercept_after -->|                             |
      |                                 |      (Selected Auth ID/Index)   |-- 3. POST /quota/check ---->|
      |                                 |                                 |<-- 4. {allowed: false} -----|
      |                                 |<-- 5. Terminate: 429 JSON ------|                             |
      |<- 6. HTTP 429 Quota Exceeded ---|                                 |                             |
```

When quota is healthy:
1. `request.intercept_after` queries the local in-memory TTL cache. On cache hit, it passes through immediately without adding network latency.
2. If cache misses, it queries the external API `POST /api/v1/quota/check`. Healthy responses are cached.
3. Upon stream or non-stream completion, `usage.handle` extracts `Detail.InputTokens`, `Detail.OutputTokens`, and `Detail.TotalTokens` and calls `POST /api/v1/quota/deduct` asynchronously.
4. If deduction reveals that the remaining quota has dropped to zero or below, the local cache is immediately updated to block subsequent requests.

## External Quota API Contract

The external quota service should implement the following RESTful endpoints:

### 1. Quota Pre-check (`POST /api/v1/quota/check`)

**Request:**
```json
{
  "auth_id": "auth-openai-compat-prod-1",
  "auth_index": "0",
  "provider": "openai-compatibility",
  "model": "deepseek-chat"
}
```

**Response (Allowed):**
```json
{
  "allowed": true,
  "remaining_quota": 500000,
  "reset_at": "2026-10-01T00:00:00Z"
}
```

**Response (Denied):**
```json
{
  "allowed": false,
  "remaining_quota": 0,
  "reason": "balance_exhausted"
}
```

### 2. Usage Deduction (`POST /api/v1/quota/deduct`)

**Request:**
```json
{
  "request_id": "req-123456",
  "auth_id": "auth-openai-compat-prod-1",
  "auth_index": "0",
  "provider": "openai-compatibility",
  "model": "deepseek-chat",
  "usage": {
    "prompt_tokens": 120,
    "completion_tokens": 350,
    "reasoning_tokens": 100,
    "cached_tokens": 0,
    "total_tokens": 470
  },
  "duration_ms": 1250,
  "status": "succeeded"
}
```

**Response:**
```json
{
  "ok": true,
  "remaining_quota": 499530
}
```

### 3. Management Quota Query (`GET /api/v1/quota/query?auth_id=...&provider=...`)

**Response:**
```json
{
  "subscription": {
    "plan": "team",
    "tier_name": "tier-2"
  },
  "summary": [
    {
      "key": "remaining_quota",
      "label": "Remaining Quota",
      "value": 499530,
      "unit": "tokens",
      "format": "number"
    }
  ],
  "groups": [
    {
      "displayName": "Token Quota",
      "buckets": [
        {
          "window": "monthly",
          "remainingFraction": 0.85,
          "description": "Monthly token limit"
        }
      ]
    }
  ]
}
```

### 4. Management Quota Reset (`POST /api/v1/quota/reset`)

**Request:**
```json
{
  "auth_id": "auth-openai-compat-prod-1",
  "auth_index": "0",
  "provider": "openai-compatibility"
}
```

**Response:**
```json
{
  "success": true,
  "message": "Quota reset successfully"
}
```

## Configuration

### Mode 1: Xiaomi MiMo Usage Integration (`api_type: "mimo"`)

Integrate directly with an upstream [wx2020/mimo-usage](https://github.com/wx2020/mimo-usage) instance. The plugin fetches `/api/v1/summary`, enforces token limits, plan expiry, and account balance, and displays live metrics on the CLIProxyAPI management page.

In `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    openai-quota-control:
      enabled: true
      priority: 10
      api_type: "mimo" # "mimo" or "standard"
      external_api_url: "http://127.0.0.1:8000" # mimo-usage service base URL
      external_api_key: "your-mimo-api-key" # Optional, passed as X-API-Key
      target_providers:
        - "openai-compatibility"
      mimo_max_usage_percent: 1.0 # 1.0 = 100% token usage limit
      mimo_min_balance: 0.0 # Minimum balance required for pay-as-you-go
      mimo_enforce_expiry: true # Block requests if the plan has expired
      failure_mode: "fail-closed"
      timeout_ms: 3000
      cache_ttl_seconds: 30
      max_cache_entries: 1000
```

### Mode 2: Standard Generic RESTful Service (`api_type: "standard"`)

In `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    openai-quota-control:
      enabled: true
      priority: 10
      api_type: "standard"
      external_api_url: "https://quota.example.com"
      external_api_key: "sec-..."
      target_providers:
        - "openai-compatibility"
        - "openrouter"
        - "deepseek"
      failure_mode: "fail-closed" # "fail-closed" (block on API outage) or "fail-open" (allow on outage)
      timeout_ms: 3000
      cache_ttl_seconds: 30
      max_cache_entries: 1000
```

## Build

From repository root:

### Windows (DLL):
```powershell
mkdir -p plugins/windows/amd64
go build -buildmode=c-shared -o plugins/windows/amd64/openai-quota-control.dll ./examples/plugin/openai-quota-control/go
rm plugins/windows/amd64/openai-quota-control.h
```

### Linux (SO):
```bash
mkdir -p plugins/linux/amd64
go build -buildmode=c-shared -o plugins/linux/amd64/openai-quota-control.so ./examples/plugin/openai-quota-control/go
rm -f plugins/linux/amd64/openai-quota-control.h
```

### macOS (Dylib):
```bash
mkdir -p plugins/darwin/$(go env GOARCH)
go build -buildmode=c-shared -o plugins/darwin/$(go env GOARCH)/openai-quota-control.dylib ./examples/plugin/openai-quota-control/go
rm -f plugins/darwin/$(go env GOARCH)/openai-quota-control.h
```

## Relevant RPC Methods

- `plugin.register`
- `plugin.reconfigure`
- `request.intercept_before` (pass-through)
- `request.intercept_after` (checks selected credential quota)
- `request.complete` (observational lifecycle completion)
- `usage.handle` (token usage deduction)
- `quota.identifier`
- `quota.describe`
- `quota.fetch`
- `quota.reset`

