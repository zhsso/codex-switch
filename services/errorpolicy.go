package services

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	PolicyTriggerCapacity = "capacity"
	PolicyTriggerHTTP429  = "http_429"
)

type requestErrorPolicyState struct {
	config           ErrorHandlingConfig
	used             int
	recordedFailures map[string]struct{}
}

type errorPolicyDecision struct {
	Trigger      string
	Action       string
	Retry        bool
	Delay        time.Duration
	RetryAfterMS *int64
	BudgetUsed   int
}

type policyForwardResult struct {
	OK       bool
	Err      error
	Terminal bool
	Duration time.Duration
	Trigger  string
	Action   string
}

func newRequestErrorPolicyState(config *ErrorHandlingConfig) *requestErrorPolicyState {
	if config == nil {
		config = defaultErrorHandlingConfig()
	}
	copyConfig := *config
	return &requestErrorPolicyState{
		config:           copyConfig,
		recordedFailures: make(map[string]struct{}),
	}
}

func (prs *ProviderRelayService) errorHandlingConfigSnapshot() *ErrorHandlingConfig {
	if prs != nil && prs.blacklistService != nil && prs.blacklistService.settingsService != nil {
		config, err := prs.blacklistService.settingsService.GetErrorHandlingConfig()
		if err == nil {
			return config
		}
		fmt.Printf("[WARN] 读取统一错误处理配置失败，本请求使用兼容默认值: %v\n", err)
	}
	return defaultErrorHandlingConfig()
}

func useFixedBlacklistMode(config ErrorHandlingBlacklistConfig) bool {
	return config.Enabled && (config.EnableLevelBlacklist || config.FallbackMode == "fixed")
}

func (prs *ProviderRelayService) recordFailureWithPolicySnapshot(kind, provider, reason string, policy *requestErrorPolicyState) error {
	if prs == nil || prs.blacklistService == nil {
		return nil
	}
	failureKey := strings.TrimSpace(kind) + "\x00" + strings.TrimSpace(provider)
	if policy != nil {
		if _, recorded := policy.recordedFailures[failureKey]; recorded {
			return nil
		}
	}
	config := defaultErrorHandlingConfig().Blacklist
	if policy != nil {
		config = policy.config.Blacklist
	}
	if err := prs.blacklistService.recordFailureWithReasonSnapshot(kind, provider, reason, config); err != nil {
		return err
	}
	if policy != nil {
		policy.recordedFailures[failureKey] = struct{}{}
	}
	return nil
}

func (prs *ProviderRelayService) recordSuccessWithPolicySnapshot(kind, provider string, policy *requestErrorPolicyState) error {
	if prs == nil || prs.blacklistService == nil {
		return nil
	}
	config := defaultErrorHandlingConfig().Blacklist
	if policy != nil {
		config = policy.config.Blacklist
	}
	return prs.blacklistService.recordSuccessWithSnapshot(kind, provider, config)
}

func (prs *ProviderRelayService) isBlacklistedWithPolicySnapshot(kind, provider string, policy *requestErrorPolicyState) (bool, *time.Time) {
	if prs == nil || prs.blacklistService == nil {
		return false, nil
	}
	enabled := defaultErrorHandlingConfig().Blacklist.Enabled
	if policy != nil {
		enabled = policy.config.Blacklist.Enabled
	}
	return prs.blacklistService.isBlacklistedWithEnabled(kind, provider, enabled)
}

func policyTriggerForError(err error) string {
	if errors.Is(err, errUpstreamStreamAborted) {
		return ""
	}
	if errors.Is(err, errUpstreamModelCapacity) {
		return PolicyTriggerCapacity
	}
	var statusErr *upstreamStatusError
	if errors.As(err, &statusErr) && statusErr.status == http.StatusTooManyRequests {
		return PolicyTriggerHTTP429
	}
	return ""
}

func classifyUpstreamPolicyTrigger(status int, body []byte) string {
	if (status < http.StatusOK || status >= http.StatusMultipleChoices) && containsModelCapacitySignal(body) {
		return PolicyTriggerCapacity
	}
	if isModelCapacityErrorEnvelope(body) {
		return PolicyTriggerCapacity
	}
	if status == http.StatusTooManyRequests {
		return PolicyTriggerHTTP429
	}
	return ""
}

func (state *requestErrorPolicyState) decide(err error) errorPolicyDecision {
	trigger := policyTriggerForError(err)
	if trigger == "" {
		return errorPolicyDecision{}
	}
	action := state.config.HTTP429.Action
	if trigger == PolicyTriggerCapacity {
		action = state.config.Capacity.Action
	}
	decision := errorPolicyDecision{Trigger: trigger, Action: action}
	if action != ErrorPolicyRetryThenSwitchProvider || state.used >= state.config.SharedRetryAttempts {
		decision.BudgetUsed = state.used
		return decision
	}

	delay, retryAfterMS, permitted := policyRetryDelay(err, state.used)
	decision.RetryAfterMS = retryAfterMS
	if !permitted {
		decision.BudgetUsed = state.used
		return decision
	}
	state.used++
	decision.BudgetUsed = state.used
	decision.Retry = true
	decision.Delay = delay
	return decision
}

func policyRetryDelay(err error, attempt int) (time.Duration, *int64, bool) {
	var statusErr *upstreamStatusError
	if errors.As(err, &statusErr) && strings.TrimSpace(statusErr.retryAfterHeader) != "" {
		delay, ok := parsePolicyRetryAfter(statusErr.retryAfterHeader, time.Now())
		if ok {
			ms := delay.Milliseconds()
			if delay > 60*time.Second {
				return 0, &ms, false
			}
			return delay, &ms, true
		}
	}

	if attempt < 0 {
		attempt = 0
	}
	capDelay := 30 * time.Second
	if attempt < 5 {
		capDelay = time.Second * time.Duration(1<<attempt)
	}
	if capDelay <= 0 {
		return 0, nil, true
	}
	return time.Duration(rand.Int63n(int64(capDelay) + 1)), nil, true
}

func parsePolicyRetryAfter(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if isPolicyRetryAfterSeconds(header) {
		seconds, err := strconv.ParseFloat(header, 64)
		if err != nil {
			if math.IsInf(seconds, 1) {
				return 60*time.Second + time.Millisecond, true
			}
			return 0, false
		}
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return 0, false
		}
		// Values beyond Duration's range still mean "longer than the legal
		// retry window". Preserve that fact without overflowing negative.
		if seconds > float64(math.MaxInt64/int64(time.Second)) {
			return 60*time.Second + time.Millisecond, true
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	parsed, err := http.ParseTime(header)
	if err != nil {
		return 0, false
	}
	delay := parsed.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func isPolicyRetryAfterSeconds(value string) bool {
	dot := -1
	for i := 0; i < len(value); i++ {
		switch {
		case value[i] >= '0' && value[i] <= '9':
		case value[i] == '.' && dot < 0 && i > 0 && i < len(value)-1:
			dot = i
		default:
			return false
		}
	}
	return len(value) > 0
}

func waitForPolicyRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (prs *ProviderRelayService) forwardRequestWithPolicy(
	c *gin.Context,
	kind string,
	provider Provider,
	endpoint string,
	query map[string]string,
	clientHeaders map[string]string,
	bodyBytes []byte,
	isStream bool,
	model string,
	configGen int64,
	trace *relayRequestTrace,
	retryOrdinal int,
	policy *requestErrorPolicyState,
) policyForwardResult {
	for {
		attempt := trace.BeforeAttempt(provider.Name)
		started := time.Now()
		ok, err := prs.forwardRequest(c, kind, provider, endpoint, query, clientHeaders, bodyBytes, isStream, model, configGen)
		duration := time.Since(started)
		if ok {
			return policyForwardResult{OK: true, Duration: duration}
		}

		decision := policy.decide(err)
		metadata := policyMetadataForDecision(decision)
		if decision.Retry {
			waitErr := waitForPolicyRetry(c.Request.Context(), decision.Delay)
			if waitErr == nil {
				waitErr = c.Request.Context().Err()
			}
			if waitErr != nil {
				metadata.Outcome = "retry_cancelled"
				trace.RecordForwardErrorWithPolicy(provider.Name, err, attempt, retryOrdinal, duration, metadata)
				return policyForwardResult{
					Err:      fmt.Errorf("%w: %v", errClientAbort, waitErr),
					Terminal: true,
					Duration: duration,
					Trigger:  decision.Trigger,
					Action:   decision.Action,
				}
			}
			trace.RecordForwardErrorWithPolicy(provider.Name, err, attempt, retryOrdinal, duration, metadata)
			continue
		}
		trace.RecordForwardErrorWithPolicy(provider.Name, err, attempt, retryOrdinal, duration, metadata)
		if decision.Trigger != "" && !decision.Retry &&
			(decision.Action == ErrorPolicySwitchProvider || decision.Action == ErrorPolicyRetryThenSwitchProvider) {
			trace.SetPendingPolicySwitch(metadata)
		}
		if decision.Trigger == "" {
			return policyForwardResult{Err: err, Duration: duration}
		}

		result := policyForwardResult{
			Err:      err,
			Duration: duration,
			Trigger:  decision.Trigger,
			Action:   decision.Action,
		}
		switch decision.Action {
		case ErrorPolicyPassThrough:
			writePolicyPassThrough(c, err)
			result.Terminal = true
		case ErrorPolicyReturn502:
			writePolicy502(c, decision, policy.used)
			result.Terminal = true
		case ErrorPolicySwitchProvider, ErrorPolicyRetryThenSwitchProvider:
			// Retry budget exhausted (or Retry-After too long): switch provider.
		default:
			writePolicy502(c, decision, policy.used)
			result.Terminal = true
		}
		if result.Terminal {
			trace.MarkFailed(err)
			if prs.blacklistService != nil {
				if recordErr := prs.recordFailureWithPolicySnapshot(kind, provider.Name, safeRelayError(err), policy); recordErr != nil {
					fmt.Printf("[ERROR] 记录策略终态失败到黑名单失败: %v\n", recordErr)
				}
			}
		}
		return result
	}
}

func policyMetadataForDecision(decision errorPolicyDecision) PolicyEventMetadata {
	if decision.Trigger == "" {
		return PolicyEventMetadata{}
	}
	budgetUsed := decision.BudgetUsed
	metadata := PolicyEventMetadata{
		Trigger:         decision.Trigger,
		Action:          decision.Action,
		RetryBudgetUsed: &budgetUsed,
		RetryAfterMS:    decision.RetryAfterMS,
	}
	if decision.Retry {
		delayMS := decision.Delay.Milliseconds()
		metadata.Outcome = "retried"
		metadata.RetryDelayMS = &delayMS
		return metadata
	}
	switch decision.Action {
	case ErrorPolicyPassThrough:
		metadata.Outcome = "passed_through"
	case ErrorPolicyReturn502:
		metadata.Outcome = "returned_502"
	default:
		metadata.Outcome = "switch_requested"
	}
	return metadata
}

func writePolicyPassThrough(c *gin.Context, err error) {
	var statusErr *upstreamStatusError
	if !errors.As(err, &statusErr) {
		writePolicy502(c, errorPolicyDecision{Trigger: policyTriggerForError(err), Action: ErrorPolicyPassThrough}, 0)
		return
	}
	for key, values := range statusErr.responseHeaders {
		c.Writer.Header()[key] = append([]string(nil), values...)
	}
	status := statusErr.status
	if status < 100 {
		status = http.StatusBadGateway
	}
	c.Status(status)
	if len(statusErr.responseBody) > 0 {
		_, _ = c.Writer.Write(statusErr.responseBody)
	}
}

func writePolicy502(c *gin.Context, decision errorPolicyDecision, retryBudgetUsed int) {
	message := "上游错误命中终止策略"
	c.JSON(http.StatusBadGateway, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    "server_error",
			"code":    "upstream_policy_rejected",
			"message": message,
		},
		"message":           message,
		"error_code":        "upstream_policy_rejected",
		"policy_trigger":    decision.Trigger,
		"policy_action":     decision.Action,
		"retry_budget_used": retryBudgetUsed,
	})
}
