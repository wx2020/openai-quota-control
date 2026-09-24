package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginRegisterAndReconfigure(t *testing.T) {
	// Rejects schema version < 2
	legacyReq, errMarshal := json.Marshal(lifecycleRequest{SchemaVersion: 1})
	if errMarshal != nil {
		t.Fatalf("marshal legacy request: %v", errMarshal)
	}
	if errConfigure := configure(legacyReq); errConfigure == nil {
		t.Fatal("expected error for schema version 1, got nil")
	}

	// Accepts valid configuration
	configYAML := []byte(`
external_api_url: "https://quota.example.com"
external_api_key: "test-key"
target_providers: ["openai-compatibility", "openrouter"]
failure_mode: "fail-open"
timeout_ms: 1500
cache_ttl_seconds: 60
`)
	validReq, _ := json.Marshal(lifecycleRequest{
		SchemaVersion: 2,
		ConfigYAML:    configYAML,
	})
	if errConfigure := configure(validReq); errConfigure != nil {
		t.Fatalf("configure failed: %v", errConfigure)
	}

	state.mu.RLock()
	cfg := state.config
	state.mu.RUnlock()

	if cfg.ExternalAPIURL != "https://quota.example.com" {
		t.Fatalf("external_api_url = %q, want https://quota.example.com", cfg.ExternalAPIURL)
	}
	if cfg.FailureMode != "fail-open" {
		t.Fatalf("failure_mode = %q, want fail-open", cfg.FailureMode)
	}
	if len(cfg.TargetProviders) != 2 {
		t.Fatalf("target_providers length = %d, want 2", len(cfg.TargetProviders))
	}

	reg := pluginRegistration()
	if !reg.Capabilities.RequestInterceptor {
		t.Fatal("expected RequestInterceptor to be true")
	}
	if !reg.Capabilities.UsagePlugin {
		t.Fatal("expected UsagePlugin to be true")
	}
	if !reg.Capabilities.QuotaProvider {
		t.Fatal("expected QuotaProvider to be true")
	}

	// Verify ConfigFields are properly defined for Management UI
	fieldMap := make(map[string]pluginapi.ConfigField)
	for _, f := range reg.Metadata.ConfigFields {
		fieldMap[f.Name] = f
	}
	expectedFields := []string{
		"api_type", "external_api_url", "external_api_key", "target_providers",
		"failure_mode", "timeout_ms", "cache_ttl_seconds", "max_cache_entries",
		"mimo_max_usage_percent", "mimo_min_balance", "mimo_enforce_expiry",
	}
	for _, name := range expectedFields {
		if _, ok := fieldMap[name]; !ok {
			t.Errorf("missing config field in metadata: %s", name)
		}
	}
	if apiTypeField := fieldMap["api_type"]; apiTypeField.Type != pluginapi.ConfigFieldTypeEnum || len(apiTypeField.EnumValues) != 2 {
		t.Errorf("api_type should be enum with 2 options, got %+v", apiTypeField)
	}
	if failureModeField := fieldMap["failure_mode"]; failureModeField.Type != pluginapi.ConfigFieldTypeEnum || len(failureModeField.EnumValues) != 2 {
		t.Errorf("failure_mode should be enum with 2 options, got %+v", failureModeField)
	}
	if cacheEntriesField := fieldMap["max_cache_entries"]; cacheEntriesField.Type != pluginapi.ConfigFieldTypeInteger {
		t.Errorf("max_cache_entries should be integer, got %+v", cacheEntriesField)
	}
}

func TestInterceptAfterAuth_NonTargetProvider(t *testing.T) {
	resetStateForTest(t, pluginConfig{
		TargetProviders: []string{"openai-compatibility"},
	}, "")

	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-1",
		SourceFormat: "claude",
		ToFormat:     "claude",
		Headers:      http.Header{"X-Test": {"val"}},
		Body:         []byte(`{"prompt":"hello"}`),
		Metadata: map[string]any{
			"provider": "claude",
		},
	}
	resp := callInterceptAfterAuth(t, req)
	if resp.Terminate {
		t.Fatalf("expected non-target provider to pass through, got terminated: %+v", resp)
	}
}

func TestInterceptAfterAuth_QuotaAllowed(t *testing.T) {
	var checkHit atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/quota/check" {
			checkHit.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(CheckQuotaResponse{
				Allowed:        true,
				RemainingQuota: 50000,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		TargetProviders: []string{"openai-compatibility"},
		FailureMode:     "fail-closed",
		TimeoutMS:       2000,
		CacheTTLSeconds: 30,
	}, server.URL)

	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-allowed",
		SourceFormat: "openai",
		ToFormat:     "openai-compatibility",
		Model:        "deepseek-chat",
		Metadata: map[string]any{
			"selected_auth_id":    "auth-user-1",
			"selected_auth_index": "0",
			"provider":            "openai-compatibility",
		},
	}

	resp := callInterceptAfterAuth(t, req)
	if resp.Terminate {
		t.Fatalf("expected request to be allowed, got terminated: %+v", resp)
	}
	if checkHit.Load() != 1 {
		t.Fatalf("expected 1 hit to check API, got %d", checkHit.Load())
	}

	// Second request should hit cache and NOT make a second HTTP request
	resp2 := callInterceptAfterAuth(t, req)
	if resp2.Terminate {
		t.Fatalf("expected cached request to be allowed, got terminated: %+v", resp2)
	}
	if checkHit.Load() != 1 {
		t.Fatalf("expected check hit count to remain 1 due to cache, got %d", checkHit.Load())
	}
}

func TestInterceptAfterAuth_QuotaExceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/quota/check" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(CheckQuotaResponse{
				Allowed:        false,
				RemainingQuota: 0,
				Reason:         "balance_exhausted",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		TargetProviders: []string{"openai-compatibility"},
		FailureMode:     "fail-closed",
		TimeoutMS:       2000,
		CacheTTLSeconds: 30,
	}, server.URL)

	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-blocked",
		SourceFormat: "openai",
		ToFormat:     "openai-compatibility",
		Model:        "deepseek-chat",
		Metadata: map[string]any{
			"selected_auth_id":    "auth-broke-user",
			"selected_auth_index": "1",
			"provider":            "openai-compatibility",
		},
	}

	resp := callInterceptAfterAuth(t, req)
	if !resp.Terminate {
		t.Fatal("expected request to be terminated, got allowed")
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
	if resp.ResponseHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", resp.ResponseHeaders.Get("Content-Type"))
	}
	if resp.ResponseHeaders.Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q, want 60", resp.ResponseHeaders.Get("Retry-After"))
	}

	var errResp openAIErrorResponse
	if errUnmarshal := json.Unmarshal(resp.ResponseBody, &errResp); errUnmarshal != nil {
		t.Fatalf("unmarshal error response: %v, raw: %s", errUnmarshal, string(resp.ResponseBody))
	}
	if errResp.Error.Code != "quota_exceeded" {
		t.Fatalf("error.code = %q, want quota_exceeded", errResp.Error.Code)
	}
	if errResp.Error.Type != "insufficient_quota" {
		t.Fatalf("error.type = %q, want insufficient_quota", errResp.Error.Type)
	}
}

func TestInterceptAfterAuth_CacheExpirationWithMockClock(t *testing.T) {
	var checkHit atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkHit.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CheckQuotaResponse{
			Allowed:        true,
			RemainingQuota: 1000,
		})
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		TargetProviders: []string{"openai-compatibility"},
		CacheTTLSeconds: 10,
	}, server.URL)

	// Inject controllable clock to avoid time.Sleep
	currentTime := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	state.cache.nowFunc = func() time.Time {
		return currentTime
	}

	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-clock",
		SourceFormat: "openai",
		ToFormat:     "openai-compatibility",
		Metadata: map[string]any{
			"selected_auth_id": "auth-clock-user",
		},
	}

	// 1st request -> hits server
	callInterceptAfterAuth(t, req)
	if checkHit.Load() != 1 {
		t.Fatalf("expected 1 hit, got %d", checkHit.Load())
	}

	// Advance time by 5s (still within 10s TTL) -> hits cache
	currentTime = currentTime.Add(5 * time.Second)
	callInterceptAfterAuth(t, req)
	if checkHit.Load() != 1 {
		t.Fatalf("expected 1 hit (cache hit), got %d", checkHit.Load())
	}

	// Advance time past TTL (total 15s) -> cache expired, hits server again
	currentTime = currentTime.Add(10 * time.Second)
	callInterceptAfterAuth(t, req)
	if checkHit.Load() != 2 {
		t.Fatalf("expected 2 hits after TTL expiry, got %d", checkHit.Load())
	}
}

func TestInterceptAfterAuth_FailOpenVsFailClosed(t *testing.T) {
	// Server returning 500
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer server.Close()

	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-error",
		SourceFormat: "openai",
		ToFormat:     "openai-compatibility",
		Metadata: map[string]any{
			"selected_auth_id": "auth-err-user",
		},
	}

	// Test 1: Fail-Closed blocks
	resetStateForTest(t, pluginConfig{
		TargetProviders: []string{"openai-compatibility"},
		FailureMode:     "fail-closed",
	}, server.URL)

	respClosed := callInterceptAfterAuth(t, req)
	if !respClosed.Terminate {
		t.Fatal("expected fail-closed to terminate request on external API failure")
	}

	// Test 2: Fail-Open allows
	resetStateForTest(t, pluginConfig{
		TargetProviders: []string{"openai-compatibility"},
		FailureMode:     "fail-open",
	}, server.URL)

	respOpen := callInterceptAfterAuth(t, req)
	if respOpen.Terminate {
		t.Fatal("expected fail-open to allow request on external API failure")
	}
}

func TestHandleUsage_DeductTokens(t *testing.T) {
	deductReceived := make(chan DeductQuotaRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/quota/deduct" {
			var deductReq DeductQuotaRequest
			_ = json.NewDecoder(r.Body).Decode(&deductReq)
			deductReceived <- deductReq

			w.Header().Set("Content-Type", "application/json")
			zero := int64(0)
			_ = json.NewEncoder(w).Encode(DeductQuotaResponse{
				OK:             true,
				RemainingQuota: &zero,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		TargetProviders: []string{"openai-compatibility"},
	}, server.URL)

	record := pluginapi.UsageRecord{
		Provider:  "openai-compatibility",
		AuthID:    "auth-deduct-test",
		AuthIndex: "2",
		Model:     "deepseek-coder",
		Detail: pluginapi.UsageDetail{
			InputTokens:     120,
			OutputTokens:    350,
			ReasoningTokens: 100,
			TotalTokens:     470,
		},
		Latency: 800 * time.Millisecond,
		Failed:  false,
	}

	recordBytes, _ := json.Marshal(record)
	_, errUsage := handleUsage(recordBytes)
	if errUsage != nil {
		t.Fatalf("handleUsage failed: %v", errUsage)
	}

	select {
	case received := <-deductReceived:
		if received.AuthID != "auth-deduct-test" {
			t.Fatalf("received AuthID = %q, want auth-deduct-test", received.AuthID)
		}
		if received.Usage.PromptTokens != 120 || received.Usage.CompletionTokens != 350 || received.Usage.TotalTokens != 470 {
			t.Fatalf("unexpected received usage: %+v", received.Usage)
		}
		if received.Status != "succeeded" {
			t.Fatalf("received status = %q, want succeeded", received.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for deduct request to external API")
	}

	// Verify that because remaining quota was 0, subsequent check will be blocked by cache
	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-after-zero",
		SourceFormat: "openai",
		ToFormat:     "openai-compatibility",
		Metadata: map[string]any{
			"selected_auth_id": "auth-deduct-test",
		},
	}
	resp := callInterceptAfterAuth(t, req)
	if !resp.Terminate {
		t.Fatal("expected request to be terminated immediately due to zero balance in cache")
	}
}

func TestQuotaDescribeFetchAndReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/quota/query":
			_ = json.NewEncoder(w).Encode(pluginapi.QuotaFetchResponse{
				Subscription: &pluginapi.QuotaSubscription{
					Plan: "enterprise",
				},
				Summary: []pluginapi.QuotaMetric{
					{
						Key:   "balance",
						Label: "Account Balance",
						Value: 250000,
					},
				},
			})
		case "/api/v1/quota/reset":
			_ = json.NewEncoder(w).Encode(ResetQuotaExternalResponse{
				Success: true,
				Message: "Reset successfully",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		TargetProviders: []string{"openai-compatibility", "openrouter"},
	}, server.URL)

	// 1. Quota Describe
	descRaw, errDesc := quotaDescribe(nil)
	if errDesc != nil {
		t.Fatalf("quotaDescribe failed: %v", errDesc)
	}
	var envDesc envelope
	_ = json.Unmarshal(descRaw, &envDesc)
	var descResp pluginapi.QuotaDescribeResponse
	_ = json.Unmarshal(envDesc.Result, &descResp)
	if len(descResp.SupportedProviders) < 2 || !descResp.SupportsReset {
		t.Fatalf("unexpected describe response: %+v", descResp)
	}

	// 2. Quota Fetch
	fetchReqBytes, _ := json.Marshal(struct {
		pluginapi.QuotaFetchRequest
	}{
		QuotaFetchRequest: pluginapi.QuotaFetchRequest{
			AuthID:   "auth-test",
			Provider: "openai-compatibility",
		},
	})
	fetchRaw, errFetch := quotaFetch(fetchReqBytes)
	if errFetch != nil {
		t.Fatalf("quotaFetch failed: %v", errFetch)
	}
	var envFetch envelope
	_ = json.Unmarshal(fetchRaw, &envFetch)
	var fetchResp pluginapi.QuotaFetchResponse
	_ = json.Unmarshal(envFetch.Result, &fetchResp)
	if fetchResp.Subscription == nil || fetchResp.Subscription.Plan != "enterprise" {
		t.Fatalf("unexpected fetch response: %+v", fetchResp)
	}

	// 3. Quota Reset
	state.cache.Set("auth-test", false, 0, "exhausted", 30*time.Second)
	resetReqBytes, _ := json.Marshal(struct {
		pluginapi.QuotaResetRequest
	}{
		QuotaResetRequest: pluginapi.QuotaResetRequest{
			AuthID:   "auth-test",
			Provider: "openai-compatibility",
		},
	})
	resetRaw, errReset := quotaReset(resetReqBytes)
	if errReset != nil {
		t.Fatalf("quotaReset failed: %v", errReset)
	}
	var envReset envelope
	_ = json.Unmarshal(resetRaw, &envReset)
	var resetResp pluginapi.QuotaResetResponse
	_ = json.Unmarshal(envReset.Result, &resetResp)
	if !resetResp.Success {
		t.Fatalf("expected reset success, got false: %+v", resetResp)
	}
	// Verify cache for auth-test was invalidated
	if _, hit := state.cache.Get("auth-test"); hit {
		t.Fatal("expected cache entry to be invalidated after quota reset")
	}
}

func resetStateForTest(t *testing.T, cfg pluginConfig, externalURL string) {
	t.Helper()
	if externalURL != "" {
		cfg.ExternalAPIURL = externalURL
	}
	if cfg.TimeoutMS <= 0 {
		cfg.TimeoutMS = 2000
	}
	if cfg.CacheTTLSeconds <= 0 {
		cfg.CacheTTLSeconds = 30
	}
	if cfg.MaxCacheEntries <= 0 {
		cfg.MaxCacheEntries = 1000
	}
	if len(cfg.TargetProviders) == 0 {
		cfg.TargetProviders = []string{"openai-compatibility"}
	}
	if cfg.FailureMode == "" {
		cfg.FailureMode = "fail-closed"
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	state.config = cfg
	state.cache = newQuotaCache(time.Duration(cfg.CacheTTLSeconds)*time.Second, cfg.MaxCacheEntries)
	state.client = newExternalQuotaClient(cfg.ExternalAPIURL, cfg.ExternalAPIKey, time.Duration(cfg.TimeoutMS)*time.Millisecond)
}

func callInterceptAfterAuth(t *testing.T, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	t.Helper()
	raw, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	envelopeRaw, errIntercept := interceptAfterAuth(raw)
	if errIntercept != nil {
		t.Fatalf("interceptAfterAuth returned error: %v", errIntercept)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(envelopeRaw, &env); errUnmarshal != nil {
		t.Fatalf("unmarshal envelope: %v", errUnmarshal)
	}
	var resp pluginapi.RequestInterceptResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	return resp
}

func float64Ptr(v float64) *float64 { return &v }

func TestMimoQuotaFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/summary" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(MimoSummaryResponse{
				Data: MimoSummaryData{
					Plan: &MimoPlanDetail{
						PlanCode:         "FREE",
						PlanName:         "Xiaomi MiMo Free Tier",
						CurrentPeriodEnd: "2026-10-01T00:00:00Z",
						Expired:          false,
					},
					MonthUsage: &MimoMonthUsageItem{
						Name:    "month_total_token",
						Used:    4500000,
						Limit:   10000000,
						Percent: 0.45,
					},
					Balance: &MimoBalance{
						Balance:  float64Ptr(50.0),
						Currency: "CNY",
					},
					TokenUsage: &MimoTokenUsageDetail{
						TotalToken:  4500000,
						InputToken:  2000000,
						OutputToken: 2500000,
					},
					CostUsage: &MimoCostUsageDetail{
						TotalCost:        "25.00",
						CurrentMonthCost: "12.50",
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		APIType:         "mimo",
		TargetProviders: []string{"openai-compatibility"},
	}, server.URL)

	fetchReqBytes, _ := json.Marshal(struct {
		pluginapi.QuotaFetchRequest
	}{
		QuotaFetchRequest: pluginapi.QuotaFetchRequest{
			AuthID:   "mimo-auth-1",
			Provider: "openai-compatibility",
		},
	})
	fetchRaw, errFetch := quotaFetch(fetchReqBytes)
	if errFetch != nil {
		t.Fatalf("quotaFetch failed: %v", errFetch)
	}
	var env envelope
	_ = json.Unmarshal(fetchRaw, &env)
	var fetchResp pluginapi.QuotaFetchResponse
	_ = json.Unmarshal(env.Result, &fetchResp)

	// Verify Subscription
	if fetchResp.Subscription == nil || fetchResp.Subscription.Plan != "Xiaomi MiMo Free Tier" || fetchResp.Subscription.TierName != "FREE" {
		t.Fatalf("unexpected subscription: %+v", fetchResp.Subscription)
	}

	// Verify Summary Metrics
	if len(fetchResp.Summary) < 4 {
		t.Fatalf("expected at least 4 summary metrics, got %d", len(fetchResp.Summary))
	}
	foundBalance := false
	foundRemaining := false
	for _, m := range fetchResp.Summary {
		if m.Key == "balance" && m.Value == 50.0 && m.Currency == "CNY" {
			foundBalance = true
		}
		if m.Key == "remaining_tokens" && m.Value == 5500000 {
			foundRemaining = true
		}
	}
	if !foundBalance || !foundRemaining {
		t.Fatalf("missing expected metrics in summary: %+v", fetchResp.Summary)
	}

	// Verify Groups and Buckets
	if len(fetchResp.Groups) != 1 || len(fetchResp.Groups[0].Buckets) != 1 {
		t.Fatalf("unexpected groups: %+v", fetchResp.Groups)
	}
	bucket := fetchResp.Groups[0].Buckets[0]
	if bucket.Window != "monthly" || bucket.RemainingFraction != 0.55 || bucket.ResetTime != "2026-10-01T00:00:00Z" {
		t.Fatalf("unexpected bucket: %+v", bucket)
	}
}

func TestMimoInterceptAfterAuth_Allowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/summary" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(MimoSummaryResponse{
				Data: MimoSummaryData{
					Plan: &MimoPlanDetail{
						PlanCode: "PRO",
						PlanName: "Pro Tier",
						Expired:  false,
					},
					MonthUsage: &MimoMonthUsageItem{
						Used:    3000000,
						Limit:   10000000,
						Percent: 0.30,
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		APIType:             "mimo",
		TargetProviders:     []string{"openai-compatibility"},
		MimoMaxUsagePercent: 1.0,
	}, server.URL)

	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-mimo-allow",
		SourceFormat: "openai",
		ToFormat:     "openai-compatibility",
		Metadata: map[string]any{
			"selected_auth_id": "auth-mimo-user-1",
		},
	}
	resp := callInterceptAfterAuth(t, req)
	if resp.Terminate {
		t.Fatalf("expected request to pass through, got terminated: %+v", resp)
	}

	// Verify cached
	entry, hit := state.cache.Get("auth-mimo-user-1")
	if !hit || !entry.allowed {
		t.Fatalf("expected positive cache hit, got hit=%v, allowed=%v", hit, entry.allowed)
	}
}

func TestMimoInterceptAfterAuth_QuotaExceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/summary" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(MimoSummaryResponse{
				Data: MimoSummaryData{
					Plan: &MimoPlanDetail{
						PlanCode: "STANDARD",
						PlanName: "Standard Plan",
						Expired:  false,
					},
					MonthUsage: &MimoMonthUsageItem{
						Used:    10000000,
						Limit:   10000000,
						Percent: 1.0,
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		APIType:             "mimo",
		TargetProviders:     []string{"openai-compatibility"},
		MimoMaxUsagePercent: 1.0,
	}, server.URL)

	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-mimo-exceeded",
		SourceFormat: "openai",
		ToFormat:     "openai-compatibility",
		Metadata: map[string]any{
			"selected_auth_id": "auth-mimo-exceeded",
		},
	}
	resp := callInterceptAfterAuth(t, req)
	if !resp.Terminate || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected request to be terminated with 429, got %+v", resp)
	}

	// Verify cached as negative
	entry, hit := state.cache.Get("auth-mimo-exceeded")
	if !hit || entry.allowed {
		t.Fatalf("expected negative cache entry, got hit=%v, allowed=%v", hit, entry.allowed)
	}
}

func TestMimoInterceptAfterAuth_PlanExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/summary" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(MimoSummaryResponse{
				Data: MimoSummaryData{
					Plan: &MimoPlanDetail{
						PlanCode:         "FREE",
						PlanName:         "Expired Plan",
						CurrentPeriodEnd: "2026-09-01T00:00:00Z",
						Expired:          true,
					},
					MonthUsage: &MimoMonthUsageItem{
						Used:    100,
						Limit:   10000,
						Percent: 0.01,
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	enforce := true
	resetStateForTest(t, pluginConfig{
		APIType:           "mimo",
		TargetProviders:   []string{"openai-compatibility"},
		MimoEnforceExpiry: &enforce,
	}, server.URL)

	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-mimo-expired",
		SourceFormat: "openai",
		ToFormat:     "openai-compatibility",
		Metadata: map[string]any{
			"selected_auth_id": "auth-mimo-expired",
		},
	}
	resp := callInterceptAfterAuth(t, req)
	if !resp.Terminate || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected request to be terminated with 429 due to expiration, got %+v", resp)
	}
}

func TestMimoInterceptAfterAuth_BalanceExhausted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/summary" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(MimoSummaryResponse{
				Data: MimoSummaryData{
					Balance: &MimoBalance{
						Balance:  float64Ptr(0.0),
						Currency: "CNY",
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		APIType:         "mimo",
		TargetProviders: []string{"openai-compatibility"},
		MimoMinBalance:  0.0,
	}, server.URL)

	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-mimo-balance-zero",
		SourceFormat: "openai",
		ToFormat:     "openai-compatibility",
		Metadata: map[string]any{
			"selected_auth_id": "auth-mimo-balance-zero",
		},
	}
	resp := callInterceptAfterAuth(t, req)
	if !resp.Terminate || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected request to be terminated with 429 due to zero balance, got %+v", resp)
	}
}

func TestMimoQuotaReset(t *testing.T) {
	resetStateForTest(t, pluginConfig{
		APIType:         "mimo",
		TargetProviders: []string{"openai-compatibility"},
	}, "http://127.0.0.1:9999")

	state.cache.Set("auth-mimo-reset", false, 0, "quota_exceeded", 30*time.Second)

	resetReqBytes, _ := json.Marshal(struct {
		pluginapi.QuotaResetRequest
	}{
		QuotaResetRequest: pluginapi.QuotaResetRequest{
			AuthID:   "auth-mimo-reset",
			Provider: "openai-compatibility",
		},
	})
	resetRaw, errReset := quotaReset(resetReqBytes)
	if errReset != nil {
		t.Fatalf("quotaReset failed: %v", errReset)
	}
	var env envelope
	_ = json.Unmarshal(resetRaw, &env)
	var resetResp pluginapi.QuotaResetResponse
	_ = json.Unmarshal(env.Result, &resetResp)
	if !resetResp.Success {
		t.Fatalf("expected reset success, got false: %+v", resetResp)
	}
	if _, hit := state.cache.Get("auth-mimo-reset"); hit {
		t.Fatal("expected cache to be cleared after reset")
	}
}

func TestOpenAICompatibilityChannelAdaptation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/summary" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"plan":{"planCode":"PRO","planName":"MiMo Pro","expired":false},"monthUsage":{"used":990,"limit":1000,"percent":99}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	// Configure with target_providers = ["mimo"] (user sets channel name "mimo" in AI提供商 -> OpenAI兼容)
	resetStateForTest(t, pluginConfig{
		APIType:             "mimo",
		TargetProviders:     []string{"mimo"},
		MimoMaxUsagePercent: 0.95,
		FailureMode:         "fail-closed",
	}, server.URL)

	// 1. Verify quotaDescribe expands SupportedProviders to include "openai-compatible-mimo" and "openai-compatibility"
	descRaw, errDesc := quotaDescribe(nil)
	if errDesc != nil {
		t.Fatalf("quotaDescribe failed: %v", errDesc)
	}
	var descEnv envelope
	_ = json.Unmarshal(descRaw, &descEnv)
	var descResp pluginapi.QuotaDescribeResponse
	_ = json.Unmarshal(descEnv.Result, &descResp)

	hasCompatMimo := false
	for _, p := range descResp.SupportedProviders {
		if p == "openai-compatible-mimo" {
			hasCompatMimo = true
			break
		}
	}
	if !hasCompatMimo {
		t.Fatalf("expected SupportedProviders to contain openai-compatible-mimo, got %v", descResp.SupportedProviders)
	}

	// 2. Verify interceptAfterAuth matches synthesized OpenAI-compatibility authID ("openai-compatibility:mimo:8f4b2c1a9d0e") even when ToFormat is "openai"
	req := pluginapi.RequestInterceptRequest{
		RequestID:    "req-compat-mimo",
		SourceFormat: "openai",
		ToFormat:     "openai",
		Model:        "mimo-v2-flash",
		Metadata: map[string]any{
			"selected_auth_id":    "openai-compatibility:mimo:8f4b2c1a9d0e",
			"selected_auth_index": "8f4b2c1a9d0e1234",
		},
	}
	resp := callInterceptAfterAuth(t, req)
	if !resp.Terminate || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected request on openai-compatibility:mimo channel to be intercepted and blocked with 429, got %+v", resp)
	}

	// 3. Verify non-target provider (e.g. claude) is NOT intercepted
	claudeReq := pluginapi.RequestInterceptRequest{
		RequestID:    "req-claude",
		SourceFormat: "claude",
		ToFormat:     "claude",
		Model:        "claude-sonnet-4-20250514",
		Metadata: map[string]any{
			"selected_auth_id": "claude:default",
		},
	}
	claudeResp := callInterceptAfterAuth(t, claudeReq)
	if claudeResp.Terminate {
		t.Fatalf("expected claude request to pass through unblocked, got %+v", claudeResp)
	}
}

func TestManagementDashboardAndSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/summary" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"plan":{"planCode":"PRO","planName":"MiMo Pro","currentPeriodEnd":"2026-10-24","expired":false},"monthUsage":{"used":2500,"limit":10000,"percent":25},"balance":{"balance":88.5,"currency":"CNY"}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		APIType:             "mimo",
		TargetProviders:     []string{"openai-compatibility"},
		MimoMaxUsagePercent: 1.0,
	}, server.URL)

	// 1. Register management routes
	regRaw, errReg := handleMethod(pluginabi.MethodManagementRegister, nil)
	if errReg != nil {
		t.Fatalf("management.register failed: %v", errReg)
	}
	var regEnv envelope
	_ = json.Unmarshal(regRaw, &regEnv)
	var regResp pluginapi.ManagementRegistrationResponse
	_ = json.Unmarshal(regEnv.Result, &regResp)
	if len(regResp.Resources) == 0 || regResp.Resources[0].Path != "/dashboard" {
		t.Fatalf("expected /dashboard resource route, got %+v", regResp.Resources)
	}

	// 2. Render HTML dashboard
	dashReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/v0/resource/plugins/openai-quota-control/dashboard",
	})
	dashRaw, errDash := handleMethod(pluginabi.MethodManagementHandle, dashReq)
	if errDash != nil {
		t.Fatalf("management.handle dashboard failed: %v", errDash)
	}
	var dashEnv envelope
	_ = json.Unmarshal(dashRaw, &dashEnv)
	var dashResp pluginapi.ManagementResponse
	_ = json.Unmarshal(dashEnv.Result, &dashResp)
	if dashResp.StatusCode != http.StatusOK || !strings.Contains(string(dashResp.Body), "MiMo Pro") || !strings.Contains(string(dashResp.Body), "88.50 CNY") {
		t.Fatalf("unexpected dashboard HTML response: status=%d body=%s", dashResp.StatusCode, string(dashResp.Body))
	}
}

func TestMimoNewCardsSummaryAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/summary" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"data": {
					"plan": {
						"planCode": "lite:year",
						"planName": "Lite",
						"currentPeriodEnd": "2027-09-22 23:59:59",
						"expired": false,
						"enableAutoRenew": true,
						"clawEnabled": false
					},
					"cards": {
						"today": {
							"credits": 25.50,
							"dailyQuota": 1000.0,
							"percent": 2.55,
							"ratio": 0.0255,
							"bar": 0.0255,
							"warn": false,
							"tokens": 25500,
							"requests": 15
						},
						"total": {
							"used": 304041752,
							"limit": 49200000000,
							"remaining": 48895958248,
							"percent": 0.618,
							"ratio": 0.00618,
							"bar": 0.00618,
							"days": 360
						},
						"tokens": {
							"today": 25500,
							"todayRequests": 15,
							"month": 304041752,
							"monthRequests": 120,
							"monthDays": 10,
							"monthAverage": 30404175.2,
							"allTime": 500000000
						},
						"credits": {
							"today": 25.50,
							"month": 304041.75,
							"used": 304041752,
							"delta": 0.0
						}
					},
					"account": {
						"userId": "100000001",
						"phone": "138****0000",
						"email": "test@xiaomi.com"
					},
					"verification": {
						"state": "AUTHORIZED",
						"realName": "张三"
					},
					"rateLimit": {
						"tpm": 3000000,
						"rpm": 1000,
						"concurrency": 50
					}
				}
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	resetStateForTest(t, pluginConfig{
		APIType:             "mimo",
		TargetProviders:     []string{"openai-compatibility"},
		MimoMaxUsagePercent: 0.95,
	}, server.URL)

	// 1. Fetch quota
	fetchRaw, errFetch := quotaFetch([]byte(`{}`))
	if errFetch != nil {
		t.Fatalf("quotaFetch failed with new cards schema: %v", errFetch)
	}
	var env envelope
	_ = json.Unmarshal(fetchRaw, &env)
	var quotaResp pluginapi.QuotaFetchResponse
	_ = json.Unmarshal(env.Result, &quotaResp)
	if quotaResp.Subscription == nil || quotaResp.Subscription.Plan != "Lite" {
		t.Fatalf("expected Subscription.Plan=Lite, got %+v", quotaResp.Subscription)
	}
	if len(quotaResp.Groups) == 0 || len(quotaResp.Groups[0].Buckets) < 2 {
		t.Fatalf("expected package and today buckets, got %+v", quotaResp.Groups)
	}

	// 2. Fetch JSON summary via management endpoint
	summaryReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/openai-quota-control/summary",
	})
	summaryRaw, errSum := handleMethod(pluginabi.MethodManagementHandle, summaryReq)
	if errSum != nil {
		t.Fatalf("management.handle summary failed: %v", errSum)
	}
	var sumEnv envelope
	_ = json.Unmarshal(summaryRaw, &sumEnv)
	var sumResp pluginapi.ManagementResponse
	_ = json.Unmarshal(sumEnv.Result, &sumResp)
	if !strings.Contains(string(sumResp.Body), `"version": "0.3.0"`) || !strings.Contains(string(sumResp.Body), `"Lite"`) {
		t.Fatalf("unexpected summary body: %s", string(sumResp.Body))
	}

	// 3. Render HTML dashboard with new cards
	dashReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/v0/resource/plugins/openai-quota-control/dashboard",
	})
	dashRaw, errDash := handleMethod(pluginabi.MethodManagementHandle, dashReq)
	if errDash != nil {
		t.Fatalf("management.handle dashboard failed: %v", errDash)
	}
	var dashEnv envelope
	_ = json.Unmarshal(dashRaw, &dashEnv)
	var dashResp pluginapi.ManagementResponse
	_ = json.Unmarshal(dashEnv.Result, &dashResp)
	bodyStr := string(dashResp.Body)
	if !strings.Contains(bodyStr, "Lite") || !strings.Contains(bodyStr, "v0.3.0") || !strings.Contains(bodyStr, "AUTHORIZED") {
		t.Fatalf("unexpected dashboard HTML body: %s", bodyStr)
	}
}

