package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type externalQuotaClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func newExternalQuotaClient(baseURL, apiKey string, timeout time.Duration) *externalQuotaClient {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	cleanURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return &externalQuotaClient{
		baseURL: cleanURL,
		apiKey:  strings.TrimSpace(apiKey),
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (c *externalQuotaClient) CheckQuota(ctx context.Context, req CheckQuotaRequest) (CheckQuotaResponse, error) {
	if c == nil || c.baseURL == "" {
		return CheckQuotaResponse{Allowed: true}, nil
	}
	endpoint := c.baseURL + "/api/v1/quota/check"
	var resp CheckQuotaResponse
	errPost := c.postJSON(ctx, endpoint, req, &resp)
	if errPost != nil {
		return CheckQuotaResponse{}, fmt.Errorf("check quota from external API: %w", errPost)
	}
	return resp, nil
}

func (c *externalQuotaClient) DeductQuota(ctx context.Context, req DeductQuotaRequest) (DeductQuotaResponse, error) {
	if c == nil || c.baseURL == "" {
		return DeductQuotaResponse{OK: true}, nil
	}
	endpoint := c.baseURL + "/api/v1/quota/deduct"
	var resp DeductQuotaResponse
	errPost := c.postJSON(ctx, endpoint, req, &resp)
	if errPost != nil {
		return DeductQuotaResponse{}, fmt.Errorf("deduct quota from external API: %w", errPost)
	}
	return resp, nil
}

func (c *externalQuotaClient) QueryQuota(ctx context.Context, authID, authIndex, provider string) (pluginapi.QuotaFetchResponse, error) {
	if c == nil || c.baseURL == "" {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("external quota client base URL is empty")
	}
	queryParams := url.Values{}
	if authID != "" {
		queryParams.Set("auth_id", authID)
	}
	if authIndex != "" {
		queryParams.Set("auth_index", authIndex)
	}
	if provider != "" {
		queryParams.Set("provider", provider)
	}
	endpoint := c.baseURL + "/api/v1/quota/query?" + queryParams.Encode()

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("create query request: %w", errReq)
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	httpReq.Header.Set("Accept", "application/json")

	httpResp, errDo := c.httpClient.Do(httpReq)
	if errDo != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("execute query request: %w", errDo)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("external query API returned status %d: %s", httpResp.StatusCode, string(bodyBytes))
	}

	var fetchResp pluginapi.QuotaFetchResponse
	if errDecode := json.NewDecoder(httpResp.Body).Decode(&fetchResp); errDecode != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("decode quota query response: %w", errDecode)
	}
	return fetchResp, nil
}

func (c *externalQuotaClient) ResetQuota(ctx context.Context, req ResetQuotaExternalRequest) (ResetQuotaExternalResponse, error) {
	if c == nil || c.baseURL == "" {
		return ResetQuotaExternalResponse{Success: true, Message: "reset simulated without external URL"}, nil
	}
	endpoint := c.baseURL + "/api/v1/quota/reset"
	var resp ResetQuotaExternalResponse
	errPost := c.postJSON(ctx, endpoint, req, &resp)
	if errPost != nil {
		return ResetQuotaExternalResponse{}, fmt.Errorf("reset quota from external API: %w", errPost)
	}
	return resp, nil
}

func (c *externalQuotaClient) postJSON(ctx context.Context, endpoint string, payload any, out any) error {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return fmt.Errorf("marshal request: %w", errMarshal)
	}
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if errReq != nil {
		return fmt.Errorf("create request: %w", errReq)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResp, errDo := c.httpClient.Do(httpReq)
	if errDo != nil {
		return fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return fmt.Errorf("external API status %d: %s", httpResp.StatusCode, string(bodyBytes))
	}

	if out != nil {
		if errDecode := json.NewDecoder(httpResp.Body).Decode(out); errDecode != nil {
			return fmt.Errorf("decode response: %w", errDecode)
		}
	}
	return nil
}

func (c *externalQuotaClient) FetchMimoSummary(ctx context.Context) (*MimoSummaryData, error) {
	if c == nil || c.baseURL == "" {
		return nil, fmt.Errorf("external quota client base URL is empty")
	}
	endpoint := c.baseURL + "/api/v1/summary"
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("create mimo summary request: %w", errReq)
	}
	if c.apiKey != "" {
		// Note: In wx2020/mimo-usage, X-API-Key authenticates against MIMO_API_KEY,
		// whereas Authorization: Bearer overrides the upstream Xiaomi serviceToken!
		// Therefore we MUST ONLY send X-API-Key unless "apiKey|serviceToken" is explicitly specified.
		if parts := strings.SplitN(c.apiKey, "|", 2); len(parts) == 2 {
			if apiKey := strings.TrimSpace(parts[0]); apiKey != "" {
				httpReq.Header.Set("X-API-Key", apiKey)
			}
			if svcToken := strings.TrimSpace(parts[1]); svcToken != "" {
				httpReq.Header.Set("X-Mimo-Service-Token", svcToken)
			}
		} else {
			httpReq.Header.Set("X-API-Key", c.apiKey)
		}
	}
	httpReq.Header.Set("Accept", "application/json")

	httpResp, errDo := c.httpClient.Do(httpReq)
	if errDo != nil {
		return nil, fmt.Errorf("execute mimo summary request: %w", errDo)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, fmt.Errorf("mimo summary API returned status %d: %s", httpResp.StatusCode, string(bodyBytes))
	}

	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read mimo summary body: %w", errRead)
	}

	// Try unmarshaling as envelope {"data": ..., "errors": ...} first
	var env struct {
		Data   MimoSummaryData `json:"data"`
		Errors map[string]any  `json:"errors,omitempty"`
	}
	if errJSON := json.Unmarshal(bodyBytes, &env); errJSON == nil {
		normalizeMimoSummaryData(&env.Data)
		if len(env.Errors) > 0 {
			env.Data.Errors = env.Errors
		}
		hasValidData := env.Data.Cards != nil || env.Data.Plan != nil || env.Data.Account != nil ||
			env.Data.Verification != nil || env.Data.RateLimit != nil || env.Data.Balance != nil ||
			env.Data.MonthUsage != nil || env.Data.PlanUsage != nil || env.Data.TokenUsage != nil
		if hasValidData {
			return &env.Data, nil
		}
		if len(env.Errors) > 0 {
			errBytes, _ := json.Marshal(env.Errors)
			return &env.Data, fmt.Errorf("mimo-usage upstream sections failed: %s", string(errBytes))
		}
	}

	// Fallback to direct unwrapped object
	var direct MimoSummaryData
	if errJSON := json.Unmarshal(bodyBytes, &direct); errJSON == nil {
		normalizeMimoSummaryData(&direct)
		return &direct, nil
	}

	return nil, fmt.Errorf("failed to decode mimo summary response: %s", string(bodyBytes))
}

func normalizeMimoSummaryData(d *MimoSummaryData) {
	if d == nil {
		return
	}
	if d.Plan != nil && d.Plan.PlanCode == "" && d.Plan.PlanName == "" && d.Plan.CurrentPeriodEnd == "" && !d.Plan.Expired {
		d.Plan = nil
	}
	// Backfill MonthUsage from Cards.Total when using the updated mimo-usage /api/v1/summary
	if d.MonthUsage == nil && d.Cards != nil && d.Cards.Total != nil {
		pct := 0.0
		if d.Cards.Total.Percent != nil {
			pct = *d.Cards.Total.Percent
		} else if d.Cards.Total.Ratio != nil {
			pct = *d.Cards.Total.Ratio * 100.0
		} else if d.Cards.Total.Limit > 0 {
			pct = (d.Cards.Total.Used / d.Cards.Total.Limit) * 100.0
		}
		d.MonthUsage = &MimoMonthUsageItem{
			Name:    "plan_total_token",
			Used:    d.Cards.Total.Used,
			Limit:   d.Cards.Total.Limit,
			Percent: pct,
		}
	}
	// Backfill TokenUsage from Cards.Tokens when using the updated mimo-usage /api/v1/summary
	if d.TokenUsage == nil && d.Cards != nil && d.Cards.Tokens != nil {
		total := int64(d.Cards.Tokens.AllTime)
		if total == 0 {
			total = int64(d.Cards.Tokens.Month)
		}
		d.TokenUsage = &MimoTokenUsageDetail{
			TotalToken: total,
		}
	}
}

func (c *externalQuotaClient) CheckMimoQuota(ctx context.Context, cfg pluginConfig) (CheckQuotaResponse, error) {
	summary, errFetch := c.FetchMimoSummary(ctx)
	if errFetch != nil {
		return CheckQuotaResponse{}, errFetch
	}
	if summary == nil {
		return CheckQuotaResponse{Allowed: true}, nil
	}

	enforceExpiry := true
	if cfg.MimoEnforceExpiry != nil {
		enforceExpiry = *cfg.MimoEnforceExpiry
	}

	// 1. Expiration check
	if enforceExpiry && summary.Plan != nil && summary.Plan.Expired {
		resetAt := summary.Plan.CurrentPeriodEnd
		planName := summary.Plan.PlanName
		if planName == "" {
			planName = "MiMo subscription"
		}
		return CheckQuotaResponse{
			Allowed: false,
			Reason:  fmt.Sprintf("%s has expired", planName),
			ResetAt: resetAt,
		}, nil
	}

	maxPercent := cfg.MimoMaxUsagePercent
	if maxPercent <= 0 {
		maxPercent = 1.0
	}
	if maxPercent > 1.0 && maxPercent <= 100.0 {
		maxPercent = maxPercent / 100.0
	}

	// 2. Direct Cards.Total check (new mimo-usage API)
	if summary.Cards != nil && summary.Cards.Total != nil {
		ratio := 0.0
		if summary.Cards.Total.Ratio != nil {
			ratio = *summary.Cards.Total.Ratio
		} else if summary.Cards.Total.Percent != nil {
			ratio = *summary.Cards.Total.Percent / 100.0
		} else if summary.Cards.Total.Limit > 0 {
			ratio = summary.Cards.Total.Used / summary.Cards.Total.Limit
		}

		var remaining int64
		if summary.Cards.Total.Remaining != nil {
			remaining = int64(*summary.Cards.Total.Remaining)
		} else if summary.Cards.Total.Limit > 0 {
			remaining = int64(summary.Cards.Total.Limit - summary.Cards.Total.Used)
		}
		if remaining < 0 {
			remaining = 0
		}

		resetAt := ""
		if summary.Plan != nil {
			resetAt = summary.Plan.CurrentPeriodEnd
		}

		if ratio >= maxPercent || (summary.Cards.Total.Limit > 0 && summary.Cards.Total.Used >= summary.Cards.Total.Limit) {
			return CheckQuotaResponse{
				Allowed:        false,
				RemainingQuota: remaining,
				Reason:         fmt.Sprintf("MiMo token quota limit reached (%.1f%% used)", ratio*100),
				ResetAt:        resetAt,
			}, nil
		}

		return CheckQuotaResponse{
			Allowed:        true,
			RemainingQuota: remaining,
			ResetAt:        resetAt,
		}, nil
	}

	// 3. Fallback to MonthUsage / PlanUsage (legacy mimo-usage API)
	usageItem := summary.MonthUsage
	if usageItem == nil {
		usageItem = summary.PlanUsage
	}

	if usageItem != nil {
		percent := usageItem.Percent
		if percent > 1.0 && percent <= 100.0 {
			percent = percent / 100.0
		}

		remaining := int64(usageItem.Limit - usageItem.Used)
		if remaining < 0 {
			remaining = 0
		}

		resetAt := ""
		if summary.Plan != nil {
			resetAt = summary.Plan.CurrentPeriodEnd
		}

		if percent >= maxPercent || (usageItem.Limit > 0 && usageItem.Used >= usageItem.Limit) {
			return CheckQuotaResponse{
				Allowed:        false,
				RemainingQuota: remaining,
				Reason:         fmt.Sprintf("MiMo token quota limit reached (%.1f%% used)", percent*100),
				ResetAt:        resetAt,
			}, nil
		}

		return CheckQuotaResponse{
			Allowed:        true,
			RemainingQuota: remaining,
			ResetAt:        resetAt,
		}, nil
	}

	// 3. Balance check (for pay-as-you-go accounts without token plan)
	if summary.Balance != nil && summary.Balance.Balance != nil {
		if *summary.Balance.Balance <= cfg.MimoMinBalance {
			curr := summary.Balance.Currency
			if curr == "" {
				curr = "CNY"
			}
			return CheckQuotaResponse{
				Allowed: false,
				Reason:  fmt.Sprintf("MiMo account balance is insufficient (%.2f %s <= min %.2f %s)", *summary.Balance.Balance, curr, cfg.MimoMinBalance, curr),
			}, nil
		}
	}

	return CheckQuotaResponse{Allowed: true}, nil
}
