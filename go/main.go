package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var state = pluginState{
	config: pluginConfig{
		APIType:             "standard",
		ExternalAPIURL:      "",
		ExternalAPIKey:      "",
		TargetProviders:     []string{"openai-compatibility"},
		FailureMode:         "fail-closed",
		TimeoutMS:           3000,
		CacheTTLSeconds:     30,
		MaxCacheEntries:     1000,
		MimoMaxUsagePercent: 1.0,
		MimoMinBalance:      0.0,
		MimoEnforceExpiry:   boolPtr(true),
	},
	cache: newQuotaCache(30*time.Second, 1000),
}

func boolPtr(b bool) *bool { return &b }

type pluginState struct {
	mu     sync.RWMutex
	config pluginConfig
	cache  *quotaCache
	client *externalQuotaClient
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.cache != nil {
		state.cache.Clear()
	}
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodRequestInterceptBefore:
		return passThroughRequest(request)
	case pluginabi.MethodRequestInterceptAfter:
		return interceptAfterAuth(request)
	case pluginabi.MethodRequestComplete:
		return completeRequest(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodManagementRegister:
		return managementRegister(request)
	case pluginabi.MethodManagementHandle:
		return managementHandle(request)
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(rpcIdentifierResponse{Identifier: "openai-compatibility"})
	case pluginabi.MethodQuotaDescribe:
		return quotaDescribe(request)
	case pluginabi.MethodQuotaFetch:
		return quotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return quotaReset(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if req.SchemaVersion < 2 {
		return fmt.Errorf("openai-quota-control requires host schema version 2 or newer")
	}

	cfg := pluginConfig{
		APIType:             "standard",
		ExternalAPIURL:      "",
		ExternalAPIKey:      "",
		TargetProviders:     []string{"openai-compatibility"},
		FailureMode:         "fail-closed",
		TimeoutMS:           3000,
		CacheTTLSeconds:     30,
		MaxCacheEntries:     1000,
		MimoMaxUsagePercent: 1.0,
		MimoMinBalance:      0.0,
		MimoEnforceExpiry:   boolPtr(true),
	}

	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}

	cfg.APIType = strings.ToLower(strings.TrimSpace(cfg.APIType))
	if cfg.APIType == "" {
		cfg.APIType = "standard"
	}
	if cfg.MimoMaxUsagePercent <= 0 {
		cfg.MimoMaxUsagePercent = 1.0
	}
	if cfg.MimoEnforceExpiry == nil {
		cfg.MimoEnforceExpiry = boolPtr(true)
	}

	if cfg.TimeoutMS <= 0 {
		cfg.TimeoutMS = 3000
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
	cfg.FailureMode = strings.ToLower(strings.TrimSpace(cfg.FailureMode))
	if cfg.FailureMode != "fail-open" {
		cfg.FailureMode = "fail-closed"
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	state.config = cfg
	if state.cache == nil {
		state.cache = newQuotaCache(time.Duration(cfg.CacheTTLSeconds)*time.Second, cfg.MaxCacheEntries)
	} else {
		state.cache.defaultTTL = time.Duration(cfg.CacheTTLSeconds) * time.Second
		state.cache.maxEntries = cfg.MaxCacheEntries
	}
	state.client = newExternalQuotaClient(cfg.ExternalAPIURL, cfg.ExternalAPIKey, time.Duration(cfg.TimeoutMS)*time.Millisecond)
	return nil
}

func pluginRegistration() registration {
	state.mu.RLock()
	targets := state.config.TargetProviders
	state.mu.RUnlock()

	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "openai-quota-control",
			Version:          "0.3.0",
			Author:           "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			Logo:             "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/main/docs/logo.png",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "api_type",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{"mimo", "standard"},
					Description: "API integration mode: 'mimo' (Xiaomi MiMo usage API) or 'standard' (generic RESTful quota service).",
				},
				{
					Name:        "external_api_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Base URL of the external quota management API or mimo-usage service.",
				},
				{
					Name:        "external_api_key",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Authentication bearer token or API key (sent as X-API-Key and Authorization: Bearer).",
				},
				{
					Name:        "target_providers",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Provider identifiers subject to quota checks and deduction (e.g. openai-compatibility).",
				},
				{
					Name:        "failure_mode",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{"fail-closed", "fail-open"},
					Description: "Behavior when external API fails or times out: 'fail-closed' (reject) or 'fail-open' (allow).",
				},
				{
					Name:        "timeout_ms",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "HTTP request timeout in milliseconds for external quota API calls.",
				},
				{
					Name:        "cache_ttl_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Local in-memory TTL in seconds for caching quota evaluation results.",
				},
				{
					Name:        "max_cache_entries",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Maximum number of auth cache entries kept in memory. Default 1000.",
				},
				{
					Name:        "mimo_max_usage_percent",
					Type:        pluginapi.ConfigFieldTypeNumber,
					Description: "MiMo usage threshold ratio (0.0 - 1.0) before triggering 429 quota exhaustion. Default 1.0 (100%).",
				},
				{
					Name:        "mimo_min_balance",
					Type:        pluginapi.ConfigFieldTypeNumber,
					Description: "Minimum required balance for MiMo pay-as-you-go accounts before blocking. Default 0.0.",
				},
				{
					Name:        "mimo_enforce_expiry",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Whether to block requests if the MiMo subscription plan has expired. Default true.",
				},
			},
		},
		Capabilities: registrationCapability{
			RequestInterceptor:     true,
			RequestLifecyclePlugin: true,
			UsagePlugin:            true,
			ManagementAPI:          true,
			QuotaProvider:          len(targets) > 0,
		},
	}
}

func passThroughRequest(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
}

func interceptAfterAuth(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	state.mu.RLock()
	cfg := state.config
	cache := state.cache
	client := state.client
	state.mu.RUnlock()

	var authID, authIndex, metaProvider string
	if req.Metadata != nil {
		if val, ok := req.Metadata["selected_auth_id"].(string); ok {
			authID = strings.TrimSpace(val)
		}
		if val, ok := req.Metadata["selected_auth_index"].(string); ok {
			authIndex = strings.TrimSpace(val)
		}
		if val, ok := req.Metadata["provider"].(string); ok {
			metaProvider = strings.TrimSpace(val)
		}
	}

	provider := metaProvider
	if provider == "" {
		provider = req.ToFormat
	}
	if provider == "" {
		provider = req.SourceFormat
	}

	// If no auth ID is available yet or provider/authID does not match target providers, allow pass-through
	if authID == "" || !isTargetProvider(provider, authID, cfg.TargetProviders) {
		return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
	}

	// 1. Check local TTL cache
	if cache != nil {
		if entry, hit := cache.Get(authID); hit {
			if !entry.allowed {
				return terminatedResponse(http.StatusTooManyRequests, entry.reason, http.Header{
					"Retry-After":      {"60"},
					"X-Quota-Exceeded": {"true"},
				})
			}
			return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
		}
	}

	// 2. Query external quota API
	if client != nil && client.baseURL != "" {
		var checkResp CheckQuotaResponse
		var errCheck error

		if cfg.APIType == "mimo" {
			checkResp, errCheck = client.CheckMimoQuota(context.Background(), cfg)
		} else {
			checkResp, errCheck = client.CheckQuota(context.Background(), CheckQuotaRequest{
				AuthID:    authID,
				AuthIndex: authIndex,
				Provider:  provider,
				Model:     req.Model,
			})
		}
		if errCheck != nil {
			if cfg.FailureMode == "fail-open" {
				return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
			}
			return terminatedResponse(http.StatusTooManyRequests, "external quota check failed: "+errCheck.Error(), http.Header{
				"Retry-After":   {"30"},
				"X-Quota-Error": {"check_failed"},
			})
		}

		if !checkResp.Allowed {
			reason := checkResp.Reason
			if reason == "" {
				reason = "quota_exceeded"
			}
			if cache != nil {
				cache.Set(authID, false, checkResp.RemainingQuota, reason, 0)
			}
			return terminatedResponse(http.StatusTooManyRequests, reason, http.Header{
				"Retry-After":      {"60"},
				"X-Quota-Exceeded": {"true"},
			})
		}

		if cache != nil {
			cache.Set(authID, true, checkResp.RemainingQuota, "", 0)
		}
	}

	return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
}

func completeRequest(raw []byte) ([]byte, error) {
	_ = raw
	return okEnvelope(struct{}{})
}

func handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	state.mu.RLock()
	cfg := state.config
	cache := state.cache
	client := state.client
	state.mu.RUnlock()

	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		return okEnvelope(struct{}{})
	}
	if !isTargetProvider(record.Provider, authID, cfg.TargetProviders) {
		return okEnvelope(struct{}{})
	}
	if cfg.APIType == "mimo" {
		// MiMo usage is accounted upstream on Xiaomi platform (read-only monitoring API)
		return okEnvelope(struct{}{})
	}

	if client != nil && client.baseURL != "" {
		deductReq := DeductQuotaRequest{
			AuthID:    authID,
			AuthIndex: record.AuthIndex,
			Provider:  record.Provider,
			Model:     record.Model,
			Usage: UsageTokenDetail{
				PromptTokens:     record.Detail.InputTokens,
				CompletionTokens: record.Detail.OutputTokens,
				ReasoningTokens:  record.Detail.ReasoningTokens,
				CachedTokens:     record.Detail.CachedTokens,
				TotalTokens:      record.Detail.TotalTokens,
			},
			DurationMS: record.Latency.Milliseconds(),
			Status:     "succeeded",
		}
		if record.Failed {
			deductReq.Status = "failed"
		}

		go func() {
			resp, errDeduct := client.DeductQuota(context.Background(), deductReq)
			if errDeduct == nil && resp.RemainingQuota != nil && *resp.RemainingQuota <= 0 {
				if cache != nil {
					cache.Set(authID, false, *resp.RemainingQuota, "balance_exhausted", 0)
				}
			}
		}()
	}

	return okEnvelope(struct{}{})
}

func quotaDescribe(raw []byte) ([]byte, error) {
	_ = raw
	state.mu.RLock()
	targets := append([]string(nil), state.config.TargetProviders...)
	apiType := state.config.APIType
	state.mu.RUnlock()

	supported := expandSupportedProviders(targets, apiType)
	displayName := "OpenAI Upstream Quota Control"
	if apiType == "mimo" {
		displayName = "Xiaomi MiMo Quota Control"
	}

	return okEnvelope(pluginapi.QuotaDescribeResponse{
		SupportedProviders: supported,
		DisplayName:        displayName,
		SupportsReset:      true,
	})
}

func quotaFetch(raw []byte) ([]byte, error) {
	var req struct {
		pluginapi.QuotaFetchRequest
	}
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	state.mu.RLock()
	client := state.client
	cfg := state.config
	state.mu.RUnlock()

	if client != nil && client.baseURL != "" {
		if cfg.APIType == "mimo" {
			mimoData, errFetch := client.FetchMimoSummary(context.Background())
			if errFetch != nil {
				return nil, errFetch
			}
			fetchResp := formatMimoQuotaResponse(mimoData)
			return okEnvelope(fetchResp)
		}

		fetchResp, errQuery := client.QueryQuota(context.Background(), req.AuthID, req.AuthIndex, req.Provider)
		if errQuery != nil {
			return nil, errQuery
		}
		return okEnvelope(fetchResp)
	}

	// Fallback response when external API base URL is not yet configured
	return okEnvelope(pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan: "external-quota-control",
		},
		Summary: []pluginapi.QuotaMetric{
			{
				Key:    "status",
				Label:  "Status",
				Value:  1,
				Format: "number",
			},
		},
		Groups: []pluginapi.QuotaGroup{
			{
				DisplayName: "Upstream Quota",
				Buckets: []pluginapi.QuotaBucket{
					{
						Window:            "active",
						RemainingFraction: 1.0,
						Description:       "Managed by external quota service",
					},
				},
			},
		},
	})
}

func quotaReset(raw []byte) ([]byte, error) {
	var req struct {
		pluginapi.QuotaResetRequest
	}
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	state.mu.RLock()
	client := state.client
	cache := state.cache
	cfg := state.config
	state.mu.RUnlock()

	if cache != nil && req.AuthID != "" {
		cache.Invalidate(req.AuthID)
	}

	if cfg.APIType == "mimo" {
		return okEnvelope(pluginapi.QuotaResetResponse{
			Success: true,
			Message: "Local quota cache cleared. Upstream MiMo usage is read-only.",
		})
	}

	if client != nil && client.baseURL != "" {
		resetResp, errReset := client.ResetQuota(context.Background(), ResetQuotaExternalRequest{
			AuthID:    req.AuthID,
			AuthIndex: req.AuthIndex,
			Provider:  req.Provider,
		})
		if errReset != nil {
			return nil, errReset
		}
		return okEnvelope(pluginapi.QuotaResetResponse{
			Success: resetResp.Success,
			Message: resetResp.Message,
		})
	}

	return okEnvelope(pluginapi.QuotaResetResponse{
		Success: true,
		Message: "Quota reset locally",
	})
}

func formatMimoQuotaResponse(data *MimoSummaryData) pluginapi.QuotaFetchResponse {
	if data == nil {
		return pluginapi.QuotaFetchResponse{}
	}
	resp := pluginapi.QuotaFetchResponse{}

	// 1. Subscription
	planName := "Xiaomi MiMo"
	planCode := "STANDARD"
	if data.Plan != nil {
		if data.Plan.PlanName != "" {
			planName = data.Plan.PlanName
		}
		if data.Plan.PlanCode != "" {
			planCode = data.Plan.PlanCode
		}
	}
	resp.Subscription = &pluginapi.QuotaSubscription{
		Plan:     planName,
		TierName: planCode,
	}

	// 2. Summary metrics
	if data.Balance != nil && data.Balance.Balance != nil {
		currency := data.Balance.Currency
		if currency == "" {
			currency = "CNY"
		}
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key:      "balance",
			Label:    "Balance",
			Value:    *data.Balance.Balance,
			Format:   "currency",
			Currency: currency,
		})
	}
	if data.Cards != nil && data.Cards.Total != nil && data.Cards.Total.Remaining != nil {
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key:    "remaining_quota",
			Label:  "Remaining Quota",
			Value:  *data.Cards.Total.Remaining,
			Format: "number",
		})
	}
	if data.Cards != nil && data.Cards.Today != nil {
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key:    "today_tokens",
			Label:  "Today Tokens",
			Value:  data.Cards.Today.Tokens,
			Format: "number",
		})
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key:    "today_credits",
			Label:  "Today Credits",
			Value:  data.Cards.Today.Credits,
			Format: "number",
		})
	}
	if data.Cards != nil && data.Cards.Tokens != nil {
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key:    "month_tokens",
			Label:  "Month Tokens",
			Value:  data.Cards.Tokens.Month,
			Format: "number",
		})
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key:    "all_time_tokens",
			Label:  "All-Time Tokens",
			Value:  data.Cards.Tokens.AllTime,
			Format: "number",
		})
	} else if data.TokenUsage != nil {
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key:    "total_tokens",
			Label:  "Used Tokens",
			Value:  float64(data.TokenUsage.TotalToken),
			Format: "number",
		})
	}
	if data.CostUsage != nil && data.CostUsage.CurrentMonthCost != "" {
		monthCost, _ := strconv.ParseFloat(data.CostUsage.CurrentMonthCost, 64)
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key:      "month_cost",
			Label:    "Month Cost",
			Value:    monthCost,
			Format:   "currency",
			Currency: "CNY",
		})
	}

	// 3. Groups & Buckets
	var buckets []pluginapi.QuotaBucket
	resetTime := ""
	if data.Plan != nil {
		resetTime = data.Plan.CurrentPeriodEnd
	}

	if data.Cards != nil {
		if data.Cards.Total != nil && data.Cards.Total.Limit > 0 {
			ratio := 0.0
			if data.Cards.Total.Ratio != nil {
				ratio = *data.Cards.Total.Ratio
			} else if data.Cards.Total.Percent != nil {
				ratio = *data.Cards.Total.Percent / 100.0
			}
			remFrac := 1.0 - ratio
			if remFrac < 0 {
				remFrac = 0
			}
			if remFrac > 1 {
				remFrac = 1
			}
			if data.Plan != nil && data.Plan.Expired {
				remFrac = 0
			}
			daysStr := ""
			if data.Cards.Total.Days != nil {
				daysStr = fmt.Sprintf(" · 剩余 %d 天", *data.Cards.Total.Days)
			}
			desc := fmt.Sprintf("%.0f / %.0f Credits (%.2f%%)%s", data.Cards.Total.Used, data.Cards.Total.Limit, ratio*100, daysStr)
			buckets = append(buckets, pluginapi.QuotaBucket{
				Window:            "package",
				RemainingFraction: remFrac,
				ResetTime:         resetTime,
				Description:       desc,
			})
		}
		if data.Cards.Today != nil && data.Cards.Today.DailyQuota != nil && *data.Cards.Today.DailyQuota > 0 {
			todayRatio := 0.0
			if data.Cards.Today.Ratio != nil {
				todayRatio = *data.Cards.Today.Ratio
			}
			remFrac := 1.0 - todayRatio
			if remFrac < 0 {
				remFrac = 0
			}
			if remFrac > 1 {
				remFrac = 1
			}
			desc := fmt.Sprintf("今日 Credits: %.2f / 日均额度: %.2f (%.2f%%) · 今日 Tokens: %.0f", data.Cards.Today.Credits, *data.Cards.Today.DailyQuota, todayRatio*100, data.Cards.Today.Tokens)
			buckets = append(buckets, pluginapi.QuotaBucket{
				Window:            "today",
				RemainingFraction: remFrac,
				Description:       desc,
			})
		}
	} else if data.MonthUsage != nil {
		percent := data.MonthUsage.Percent
		if percent > 1.0 && percent <= 100.0 {
			percent = percent / 100.0
		}
		remFrac := 1.0 - percent
		if remFrac < 0 {
			remFrac = 0
		}
		if remFrac > 1 {
			remFrac = 1
		}
		if data.Plan != nil && data.Plan.Expired {
			remFrac = 0
		}

		desc := fmt.Sprintf("%.0f / %.0f tokens used (%.1f%%)", data.MonthUsage.Used, data.MonthUsage.Limit, percent*100)
		if data.MonthUsage.Limit <= 0 {
			desc = fmt.Sprintf("%.0f tokens used", data.MonthUsage.Used)
		}
		buckets = append(buckets, pluginapi.QuotaBucket{
			Window:            "monthly",
			RemainingFraction: remFrac,
			ResetTime:         resetTime,
			Description:       desc,
		})
	}

	if len(buckets) > 0 {
		resp.Groups = append(resp.Groups, pluginapi.QuotaGroup{
			DisplayName: "Xiaomi MiMo Quota",
			Buckets:     buckets,
		})
	}

	return resp
}

func expandSupportedProviders(targets []string, apiType string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(val string) {
		clean := strings.ToLower(strings.TrimSpace(val))
		if clean == "" {
			return
		}
		if _, ok := seen[clean]; !ok {
			seen[clean] = struct{}{}
			out = append(out, clean)
		}
	}

	for _, t := range targets {
		clean := strings.ToLower(strings.TrimSpace(t))
		if clean == "" {
			continue
		}
		add(clean)
		if clean == "openai-compatibility" || clean == "openai-compatible" || clean == "openai" {
			add("openai-compatibility")
			add("openai-compatible")
			add("openai-compatible-mimo")
			add("mimo")
		} else if strings.HasPrefix(clean, "openai-compatible-") {
			short := strings.TrimPrefix(clean, "openai-compatible-")
			add(short)
			add("openai-compatibility")
		} else {
			add("openai-compatible-" + clean)
			add("openai-compatibility")
		}
	}

	if strings.EqualFold(apiType, "mimo") {
		add("openai-compatibility")
		add("openai-compatible-mimo")
		add("mimo")
	}
	if len(out) == 0 {
		add("openai-compatibility")
		add("openai-compatible-mimo")
	}
	return out
}

func isTargetProvider(provider, authID string, targets []string) bool {
	cleanProvider := strings.ToLower(strings.TrimSpace(provider))
	cleanAuthID := strings.ToLower(strings.TrimSpace(authID))

	var compatChannel string
	if strings.HasPrefix(cleanAuthID, "openai-compatibility:") {
		parts := strings.Split(cleanAuthID, ":")
		if len(parts) >= 2 {
			compatChannel = strings.TrimSpace(parts[1])
		}
	}
	if compatChannel == "" && strings.HasPrefix(cleanProvider, "openai-compatible-") {
		compatChannel = strings.TrimPrefix(cleanProvider, "openai-compatible-")
	}

	isOpenAICompat := strings.HasPrefix(cleanAuthID, "openai-compatibility:") ||
		strings.HasPrefix(cleanProvider, "openai-compatib")

	if len(targets) == 0 {
		return isOpenAICompat
	}

	for _, target := range targets {
		cleanTarget := strings.ToLower(strings.TrimSpace(target))
		if cleanTarget == "" {
			continue
		}
		if cleanTarget == "openai-compatibility" || cleanTarget == "openai-compatible" || cleanTarget == "openai" {
			if isOpenAICompat || cleanProvider == "openai" {
				return true
			}
		}
		if compatChannel != "" && (cleanTarget == compatChannel || cleanTarget == "openai-compatible-"+compatChannel || strings.Contains(compatChannel, cleanTarget)) {
			return true
		}
		if cleanProvider != "" && cleanProvider != "openai" && (cleanProvider == cleanTarget || strings.Contains(cleanProvider, cleanTarget)) {
			return true
		}
	}
	return false
}

func managementRegister(raw []byte) ([]byte, error) {
	_ = raw
	return okEnvelope(pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{
				Method:      "GET",
				Path:        "/plugins/openai-quota-control/summary",
				Description: "Fetch live MiMo / external quota summary JSON",
			},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/dashboard",
				Menu:        "MiMo 配额看板",
				Description: "实时查看 MiMo 套餐额度、Token 消耗与账户余额（适配 AI提供商 -> OpenAI兼容 渠道）",
			},
		},
	})
}

func managementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	state.mu.RLock()
	cfg := state.config
	client := state.client
	cache := state.cache
	state.mu.RUnlock()

	refreshed := false
	if req.Query != nil && req.Query.Get("action") == "refresh" {
		if cache != nil {
			cache.Clear()
		}
		refreshed = true
	}

	wantsJSON := strings.HasSuffix(req.Path, "/summary") ||
		(req.Query != nil && req.Query.Get("format") == "json") ||
		strings.Contains(strings.ToLower(req.Headers.Get("Accept")), "application/json")

	var mimoData *MimoSummaryData
	var quotaResp pluginapi.QuotaFetchResponse
	var fetchErr error

	if client != nil && client.baseURL != "" {
		if cfg.APIType == "mimo" {
			mimoData, fetchErr = client.FetchMimoSummary(context.Background())
			if fetchErr == nil && mimoData != nil {
				quotaResp = formatMimoQuotaResponse(mimoData)
			}
		} else {
			quotaResp, fetchErr = client.QueryQuota(context.Background(), "openai-compatibility", "0", "openai-compatibility")
		}
	}

	if wantsJSON {
		payload := map[string]any{
			"ok":          fetchErr == nil,
			"version":     "0.3.0",
			"refreshed":   refreshed,
			"api_type":    cfg.APIType,
			"quota":       quotaResp,
			"mimo_raw":    mimoData,
			"checked_at":  time.Now().Format(time.RFC3339),
			"target_urls": cfg.ExternalAPIURL,
		}
		if fetchErr != nil {
			payload["error"] = fetchErr.Error()
		}
		body, _ := json.MarshalIndent(payload, "", "  ")
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type": {"application/json; charset=utf-8"},
			},
			Body: body,
		})
	}

	htmlBody := renderDashboardHTML(cfg, mimoData, quotaResp, fetchErr, refreshed)
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  {"text/html; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		Body: []byte(htmlBody),
	})
}

func renderDashboardHTML(cfg pluginConfig, data *MimoSummaryData, quota pluginapi.QuotaFetchResponse, fetchErr error, refreshed bool) string {
	planName := "未配置 / 等待连接"
	planCode := strings.ToUpper(cfg.APIType)
	periodEnd := "-"
	statusText := "正常 (Allowed)"
	statusColor := "#10b981"
	statusBg := "rgba(16, 185, 129, 0.15)"

	var usedTokens, limitTokens, remTokens, usagePercent float64
	var todayCredits, todayQuota, todayTokens, todayRequests, todayPercent float64
	hasToday := false

	balanceStr := "-"
	monthCostStr := "-"
	var monthTok, allTimeTok float64
	daysLeftStr := "-"

	if cfg.ExternalAPIURL == "" {
		statusText = "未配置 external_api_url"
		statusColor = "#f59e0b"
		statusBg = "rgba(245, 158, 11, 0.15)"
	} else if fetchErr != nil {
		statusText = "查询失败: " + fetchErr.Error()
		statusColor = "#ef4444"
		statusBg = "rgba(239, 68, 68, 0.15)"
	} else if data != nil {
		if data.Plan != nil {
			if data.Plan.PlanName != "" {
				planName = data.Plan.PlanName
			} else {
				planName = "Xiaomi MiMo"
			}
			if data.Plan.PlanCode != "" {
				planCode = data.Plan.PlanCode
			}
			if data.Plan.CurrentPeriodEnd != "" {
				periodEnd = data.Plan.CurrentPeriodEnd
			}
			if data.Plan.Expired {
				statusText = "套餐已过期 (Expired)"
				statusColor = "#ef4444"
				statusBg = "rgba(239, 68, 68, 0.15)"
			}
		} else {
			planName = "Xiaomi MiMo"
		}

		if data.Cards != nil {
			if data.Cards.Total != nil {
				usedTokens = data.Cards.Total.Used
				limitTokens = data.Cards.Total.Limit
				if data.Cards.Total.Remaining != nil {
					remTokens = *data.Cards.Total.Remaining
				} else {
					remTokens = limitTokens - usedTokens
				}
				if remTokens < 0 {
					remTokens = 0
				}
				if data.Cards.Total.Percent != nil {
					usagePercent = *data.Cards.Total.Percent
				} else if data.Cards.Total.Ratio != nil {
					usagePercent = *data.Cards.Total.Ratio * 100.0
				} else if limitTokens > 0 {
					usagePercent = (usedTokens / limitTokens) * 100.0
				}
				if data.Cards.Total.Days != nil {
					daysLeftStr = fmt.Sprintf("%d 天", *data.Cards.Total.Days)
				}
			}
			if data.Cards.Today != nil {
				hasToday = true
				todayCredits = data.Cards.Today.Credits
				if data.Cards.Today.DailyQuota != nil {
					todayQuota = *data.Cards.Today.DailyQuota
				}
				todayTokens = data.Cards.Today.Tokens
				todayRequests = float64(data.Cards.Today.Requests)
				if data.Cards.Today.Percent != nil {
					todayPercent = *data.Cards.Today.Percent
				} else if data.Cards.Today.Ratio != nil {
					todayPercent = *data.Cards.Today.Ratio * 100.0
				}
				if data.Cards.Today.Warn {
					statusText = "今日额度已超日均 (Today Warn)"
					statusColor = "#f59e0b"
					statusBg = "rgba(245, 158, 11, 0.15)"
				}
			}
			if data.Cards.Tokens != nil {
				monthTok = data.Cards.Tokens.Month
				allTimeTok = data.Cards.Tokens.AllTime
			}
		} else if data.MonthUsage != nil {
			usedTokens = data.MonthUsage.Used
			limitTokens = data.MonthUsage.Limit
			remTokens = limitTokens - usedTokens
			if remTokens < 0 {
				remTokens = 0
			}
			usagePercent = data.MonthUsage.Percent
			if usagePercent <= 1.0 && limitTokens > 0 && usedTokens > 0 {
				usagePercent = usagePercent * 100.0
			}
		}

		if cfg.MimoMaxUsagePercent > 0 && (usagePercent/100.0) >= cfg.MimoMaxUsagePercent {
			statusText = fmt.Sprintf("配额已达拦截阈值 (%.1f%% >= %.0f%%)", usagePercent, cfg.MimoMaxUsagePercent*100)
			statusColor = "#ef4444"
			statusBg = "rgba(239, 68, 68, 0.15)"
		}

		if data.Balance != nil && data.Balance.Balance != nil {
			cur := data.Balance.Currency
			if cur == "" {
				cur = "CNY"
			}
			balanceStr = fmt.Sprintf("%.2f %s", *data.Balance.Balance, cur)
		} else if data.Cards != nil && data.Cards.Total != nil && data.Cards.Total.Limit > 0 {
			balanceStr = "0.00 CNY (套餐包月)"
		}

		if data.CostUsage != nil && data.CostUsage.CurrentMonthCost != "" {
			monthCostStr = data.CostUsage.CurrentMonthCost + " CNY"
		}
	} else if quota.Subscription != nil {
		planName = quota.Subscription.Plan
		if quota.Subscription.TierName != "" {
			planCode = quota.Subscription.TierName
		}
	}

	barWidth := usagePercent
	if barWidth < 0 {
		barWidth = 0
	}
	if barWidth > 100 {
		barWidth = 100
	}
	barColor := "#3b82f6"
	if barWidth >= 90 {
		barColor = "#ef4444"
	} else if barWidth >= 75 {
		barColor = "#f59e0b"
	}

	todayBarWidth := todayPercent
	if todayBarWidth < 0 {
		todayBarWidth = 0
	}
	if todayBarWidth > 100 {
		todayBarWidth = 100
	}
	todayBarColor := "#10b981"
	if todayBarWidth >= 100 {
		todayBarColor = "#f59e0b"
	}

	refreshBanner := ""
	if refreshed {
		refreshBanner = `<div style="background:rgba(16,185,129,0.15);border:1px solid #10b981;color:#6ee7b7;padding:10px 16px;border-radius:8px;margin-bottom:16px;font-size:13px;">✅ 已清理插件本地缓存并拉取最新上游配额数据</div>`
	}

	todaySection := ""
	if hasToday {
		todaySection = fmt.Sprintf(`
    <div class="progress-wrap" style="margin-top:14px;padding-top:14px;border-top:1px dashed #334155;">
      <div class="progress-top">
        <span><strong>今日使用 (Today Credits)</strong> — 已用 %.2f / 日均建议 %.2f Credits (%.0f Tokens, %.0f 次请求)</span>
        <span><strong>今日占比 %.2f%%</strong></span>
      </div>
      <div class="progress-bar">
        <div class="progress-fill" style="width: %.2f%%; background: %s;"></div>
      </div>
    </div>`,
			todayCredits, todayQuota, todayTokens, todayRequests, todayPercent, todayBarWidth, todayBarColor,
		)
	}

	accountDetails := ""
	if data != nil && (data.Account != nil || data.Verification != nil || data.RateLimit != nil) {
		uid := "-"
		phone := "-"
		email := "-"
		verState := "-"
		tpmStr := "-"
		rpmStr := "-"
		concurStr := "-"
		if data.Account != nil {
			if data.Account.UserID != nil {
				uid = fmt.Sprintf("%v", data.Account.UserID)
			}
			if data.Account.Phone != "" {
				phone = data.Account.Phone
			}
			if data.Account.Email != "" {
				email = data.Account.Email
			}
		}
		if data.Verification != nil && data.Verification.State != "" {
			verState = data.Verification.State
			if data.Verification.RealName != "" {
				verState += " (" + data.Verification.RealName + ")"
			}
		}
		if data.RateLimit != nil {
			if data.RateLimit.TPM != nil {
				tpmStr = fmt.Sprintf("%d", *data.RateLimit.TPM)
			}
			if data.RateLimit.RPM != nil {
				rpmStr = fmt.Sprintf("%d", *data.RateLimit.RPM)
			}
			if data.RateLimit.Concurrency != nil {
				concurStr = fmt.Sprintf("%d", *data.RateLimit.Concurrency)
			}
		}
		accountDetails = fmt.Sprintf(`
  <div class="card">
    <div style="font-size:15px;font-weight:600;margin-bottom:12px;">账户与限速信息 (Account & Rate Limits)</div>
    <table class="meta-table">
      <tr><td>用户 ID / 绑定手机 / 邮箱</td><td><code>%s</code> / <code>%s</code> / <code>%s</code></td></tr>
      <tr><td>实名认证状态 (Verification)</td><td><code>%s</code></td></tr>
      <tr><td>速率限制 (TPM / RPM / 并发)</td><td>TPM: <code>%s</code> | RPM: <code>%s</code> | Concurrency: <code>%s</code></td></tr>
    </table>
  </div>`,
			html.EscapeString(uid), html.EscapeString(phone), html.EscapeString(email),
			html.EscapeString(verState),
			html.EscapeString(tpmStr), html.EscapeString(rpmStr), html.EscapeString(concurStr),
		)
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>MiMo 配额看板 - openai-quota-control v0.3.0</title>
<style>
  :root { color-scheme: dark; }
  body { margin: 0; padding: 24px; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0f172a; color: #f8fafc; }
  .container { max-width: 860px; margin: 0 auto; }
  .card { background: #1e293b; border: 1px solid #334155; border-radius: 12px; padding: 22px; margin-bottom: 18px; box-shadow: 0 4px 20px rgba(0,0,0,0.25); }
  .header { display: flex; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 12px; margin-bottom: 18px; }
  .title { font-size: 20px; font-weight: 700; display: flex; align-items: center; gap: 10px; }
  .badge { font-size: 12px; font-weight: 600; padding: 4px 10px; border-radius: 999px; background: %s; color: %s; border: 1px solid %s; }
  .btn { display: inline-flex; align-items: center; gap: 6px; background: #2563eb; color: #fff; text-decoration: none; padding: 8px 14px; border-radius: 8px; font-size: 13px; font-weight: 600; transition: opacity 0.15s; }
  .btn:hover { opacity: 0.9; }
  .btn-sec { background: #334155; color: #e2e8f0; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 14px; margin-top: 16px; }
  .metric { background: #0f172a; border: 1px solid #1e293b; border-radius: 10px; padding: 14px; }
  .metric-label { font-size: 12px; color: #94a3b8; margin-bottom: 6px; }
  .metric-val { font-size: 18px; font-weight: 700; color: #f1f5f9; }
  .progress-wrap { margin: 18px 0 8px; }
  .progress-top { display: flex; justify-content: space-between; font-size: 13px; color: #cbd5e1; margin-bottom: 8px; }
  .progress-bar { height: 12px; background: #0f172a; border-radius: 999px; overflow: hidden; border: 1px solid #334155; }
  .progress-fill { height: 100%%; width: %.2f%%; background: %s; transition: width 0.3s ease; }
  .meta-table { width: 100%%; border-collapse: collapse; font-size: 13px; }
  .meta-table td { padding: 8px 0; border-bottom: 1px solid #334155; }
  .meta-table td:first-child { color: #94a3b8; width: 38%%; }
  code { background: #0f172a; padding: 2px 6px; border-radius: 4px; color: #93c5fd; font-family: ui-monospace, monospace; }
</style>
</head>
<body>
<div class="container">
  %s
  <div class="card">
    <div class="header">
      <div>
        <div class="title">
          <span>Xiaomi MiMo 实时配额看板</span>
          <span class="badge">%s</span>
        </div>
        <div style="font-size:12px;color:#94a3b8;margin-top:4px;">适配 <code>AI提供商 -&gt; OpenAI兼容 (openai-compatibility)</code> · 插件版本 <code>v0.3.0</code></div>
      </div>
      <div style="display:flex;gap:8px;">
        <a class="btn" href="?action=refresh">🔄 刷新配额并清缓存</a>
        <a class="btn btn-sec" href="?format=json" target="_blank">{ } JSON 数据</a>
      </div>
    </div>

    <div class="progress-wrap">
      <div class="progress-top">
        <span><strong>套餐总进度 (%s / %s)</strong> — 已用 %.0f / 总额 %.0f (剩余 %.0f)</span>
        <span><strong>已使用 %.2f%%</strong> (剩余 %s)</span>
      </div>
      <div class="progress-bar">
        <div class="progress-fill"></div>
      </div>
    </div>

    %s

    <div class="grid">
      <div class="metric">
        <div class="metric-label">账户可用余额 (Balance)</div>
        <div class="metric-val">%s</div>
      </div>
      <div class="metric">
        <div class="metric-label">本月累计消耗 (Month Tokens)</div>
        <div class="metric-val">%.0f</div>
      </div>
      <div class="metric">
        <div class="metric-label">历史总消耗 (All-Time Tokens)</div>
        <div class="metric-val">%.0f</div>
      </div>
      <div class="metric">
        <div class="metric-label">套餐周期截止 (Period End)</div>
        <div class="metric-val" style="font-size:14px;">%s</div>
      </div>
    </div>
  </div>

  %s

  <div class="card">
    <div style="font-size:15px;font-weight:600;margin-bottom:12px;">插件拦截配置 (Quota Control Config)</div>
    <table class="meta-table">
      <tr><td>接口模式 (api_type)</td><td><code>%s</code></td></tr>
      <tr><td>外部配额服务地址 (external_api_url)</td><td><code>%s</code></td></tr>
      <tr><td>生效拦截提供商 (supported_providers)</td><td><code>%s</code></td></tr>
      <tr><td>拦截阈值 (mimo_max_usage_percent / min_balance)</td><td>达到 <code>%.0f%%</code> 触发 429 拦截 | 最低余额 <code>%.2f CNY</code> | 故障策略 <code>%s</code></td></tr>
    </table>
  </div>
</div>
</body>
</html>`,
		statusBg, statusColor, statusColor,
		barWidth, barColor,
		refreshBanner,
		html.EscapeString(statusText),
		html.EscapeString(planName), html.EscapeString(planCode),
		usedTokens, limitTokens, remTokens, usagePercent, html.EscapeString(daysLeftStr),
		todaySection,
		html.EscapeString(balanceStr),
		monthTok, allTimeTok,
		html.EscapeString(periodEnd),
		accountDetails,
		html.EscapeString(cfg.APIType),
		html.EscapeString(cfg.ExternalAPIURL),
		html.EscapeString(strings.Join(expandSupportedProviders(cfg.TargetProviders, cfg.APIType), ", ")),
		cfg.MimoMaxUsagePercent*100, cfg.MimoMinBalance, html.EscapeString(cfg.FailureMode),
	)
}

func terminatedResponse(statusCode int, reason string, headers http.Header) ([]byte, error) {
	if reason == "" {
		reason = "insufficient_quota"
	}
	errResp := openAIErrorResponse{
		Error: openAIErrorDetail{
			Message: fmt.Sprintf("Quota exceeded for upstream credential: %s", reason),
			Type:    "insufficient_quota",
			Code:    "quota_exceeded",
		},
	}
	body, errMarshal := json.Marshal(errResp)
	if errMarshal != nil {
		return nil, errMarshal
	}
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "application/json")
	return okEnvelope(pluginapi.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      statusCode,
		ResponseHeaders: headers,
		ResponseBody:    body,
	})
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
