package main

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// pluginConfig holds plugin-specific configuration loaded from config.yaml.
type pluginConfig struct {
	APIType             string   `yaml:"api_type"` // "standard" (default) or "mimo"
	ExternalAPIURL      string   `yaml:"external_api_url"`
	ExternalAPIKey      string   `yaml:"external_api_key"`
	TargetProviders     []string `yaml:"target_providers"`
	FailureMode         string   `yaml:"failure_mode"` // "fail-closed" or "fail-open"
	TimeoutMS           int      `yaml:"timeout_ms"`
	CacheTTLSeconds     int      `yaml:"cache_ttl_seconds"`
	MaxCacheEntries     int      `yaml:"max_cache_entries"`
	MimoMaxUsagePercent float64  `yaml:"mimo_max_usage_percent"`
	MimoMinBalance      float64  `yaml:"mimo_min_balance"`
	MimoEnforceExpiry   *bool    `yaml:"mimo_enforce_expiry"`
}

// CheckQuotaRequest represents the payload sent to POST /api/v1/quota/check.
type CheckQuotaRequest struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index,omitempty"`
	Provider  string `json:"provider"`
	Model     string `json:"model,omitempty"`
}

// CheckQuotaResponse represents the response received from POST /api/v1/quota/check.
type CheckQuotaResponse struct {
	Allowed        bool   `json:"allowed"`
	RemainingQuota int64  `json:"remaining_quota,omitempty"`
	Reason         string `json:"reason,omitempty"`
	ResetAt        string `json:"reset_at,omitempty"`
}

// UsageTokenDetail represents token accounting for deduction.
type UsageTokenDetail struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
	CachedTokens     int64 `json:"cached_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens"`
}

// DeductQuotaRequest represents the payload sent to POST /api/v1/quota/deduct.
type DeductQuotaRequest struct {
	RequestID  string           `json:"request_id,omitempty"`
	AuthID     string           `json:"auth_id"`
	AuthIndex  string           `json:"auth_index,omitempty"`
	Provider   string           `json:"provider"`
	Model      string           `json:"model"`
	Usage      UsageTokenDetail `json:"usage"`
	DurationMS int64            `json:"duration_ms,omitempty"`
	Status     string           `json:"status"`
}

// DeductQuotaResponse represents the response received from POST /api/v1/quota/deduct.
type DeductQuotaResponse struct {
	OK             bool   `json:"ok"`
	RemainingQuota *int64 `json:"remaining_quota,omitempty"`
	Message        string `json:"message,omitempty"`
}

// ResetQuotaExternalRequest represents the payload sent to POST /api/v1/quota/reset.
type ResetQuotaExternalRequest struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index,omitempty"`
	Provider  string `json:"provider"`
}

// ResetQuotaExternalResponse represents the response received from POST /api/v1/quota/reset.
type ResetQuotaExternalResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// envelope is the standard JSON-RPC envelope used across CLIProxyAPI C ABI plugins.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

// envelopeError represents error details inside the RPC envelope.
type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// lifecycleRequest conveys configuration payload from the host during registration or reconfiguration.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// registration declares plugin metadata and capability flags to the host.
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

// registrationCapability toggles which extension points the plugin opts into.
type registrationCapability struct {
	RequestInterceptor     bool `json:"request_interceptor"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	UsagePlugin            bool `json:"usage_plugin"`
	ManagementAPI          bool `json:"management_api,omitempty"`
	QuotaProvider          bool `json:"quota_provider"`
}

// rpcIdentifierResponse returns the identifier for provider capabilities.
type rpcIdentifierResponse struct {
	Identifier string `json:"identifier"`
}

// openAIErrorResponse formats a standard OpenAI error response payload.
type openAIErrorResponse struct {
	Error openAIErrorDetail `json:"error"`
}

// openAIErrorDetail represents the standard OpenAI error body structure.
type openAIErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

// MimoSummaryResponse wraps the response from mimo-usage GET /api/v1/summary.
type MimoSummaryResponse struct {
	Data MimoSummaryData `json:"data"`
	Meta map[string]any  `json:"meta,omitempty"`
}

// MimoSummaryData represents the aggregated data block returned by mimo-usage.
type MimoSummaryData struct {
	Plan       *MimoPlanDetail       `json:"plan,omitempty"`
	MonthUsage *MimoMonthUsageItem   `json:"monthUsage,omitempty"`
	PlanUsage  *MimoMonthUsageItem   `json:"planUsage,omitempty"`
	Balance    *MimoBalance          `json:"balance,omitempty"`
	TokenUsage *MimoTokenUsageDetail `json:"tokenUsage,omitempty"`
	CostUsage  *MimoCostUsageDetail  `json:"costUsage,omitempty"`
	RateLimit  *MimoRateLimitDetail  `json:"rateLimit,omitempty"`
}

// MimoPlanDetail conveys subscription and period status.
type MimoPlanDetail struct {
	PlanCode         string `json:"planCode"`
	PlanName         string `json:"planName"`
	CurrentPeriodEnd string `json:"currentPeriodEnd"`
	Expired          bool   `json:"expired"`
	EnableAutoRenew  bool   `json:"enableAutoRenew"`
	ClawEnabled      bool   `json:"clawEnabled"`
}

// MimoMonthUsageItem conveys quota consumption metrics.
type MimoMonthUsageItem struct {
	Name    string  `json:"name"`
	Used    float64 `json:"used"`
	Limit   float64 `json:"limit"`
	Percent float64 `json:"percent"`
}

// MimoBalance holds account balance information.
type MimoBalance struct {
	Balance                 *float64 `json:"balance"`
	FrozenBalance           *float64 `json:"frozenBalance"`
	Currency                string   `json:"currency"`
	OverdraftLimit          *float64 `json:"overdraftLimit"`
	RemainingOverdraftLimit *float64 `json:"remainingOverdraftLimit"`
	GiftBalance             *float64 `json:"giftBalance"`
	CashBalance             *float64 `json:"cashBalance"`
}

// MimoTokenUsageDetail conveys detailed token consumption.
type MimoTokenUsageDetail struct {
	TotalToken  int64 `json:"totalToken"`
	InputToken  int64 `json:"inputToken"`
	OutputToken int64 `json:"outputToken"`
	CacheToken  int64 `json:"cacheToken"`
}

// MimoCostUsageDetail conveys monetary consumption in current and total periods.
type MimoCostUsageDetail struct {
	TotalCost        string `json:"totalCost"`
	CurrentMonthCost string `json:"currentMonthCost"`
}

// MimoRateLimitDetail conveys rate limit restrictions.
type MimoRateLimitDetail struct {
	TPM         *int64 `json:"tpm"`
	RPM         *int64 `json:"rpm"`
	Concurrency *int64 `json:"concurrency"`
}
