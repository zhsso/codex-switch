package services

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
	"github.com/gin-gonic/gin"
)

func configureErrorPolicy(t *testing.T, capacityAction, http429Action string, retryBudget int) {
	t.Helper()
	settings := NewSettingsService()
	config, err := settings.GetErrorHandlingConfig()
	if err != nil {
		t.Fatalf("读取错误策略失败: %v", err)
	}
	config.Capacity.Action = capacityAction
	config.HTTP429.Action = http429Action
	config.SharedRetryAttempts = retryBudget
	config.Blacklist.Enabled = false
	if _, err := settings.UpdateErrorHandlingConfig(config); err != nil {
		t.Fatalf("保存错误策略失败: %v", err)
	}
}

func TestCapacityAndHTTP429ShareOneRetryBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupBlacklistFixEnv(t)
	configureErrorPolicy(t, ErrorPolicyRetryThenSwitchProvider, ErrorPolicyRetryThenSwitchProvider, 1)

	var capacityHits, rateLimitHits, healthyHits atomic.Int32
	capacity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		capacityHits.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"server_overloaded"}}`)
	}))
	defer capacity.Close()
	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rateLimitHits.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit"}}`)
	}))
	defer rateLimited.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyHits.Add(1)
		_, _ = io.WriteString(w, `{"id":"healthy"}`)
	}))
	defer healthy.Close()

	response := serveResponsesRequest(t, []Provider{
		{ID: 1, Name: "capacity", APIURL: capacity.URL, APIKey: "key", Enabled: true, Level: 1},
		{ID: 2, Name: "limited", APIURL: rateLimited.URL, APIKey: "key", Enabled: true, Level: 1},
		{ID: 3, Name: "healthy", APIURL: healthy.URL, APIKey: "key", Enabled: true, Level: 1},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if capacityHits.Load() != 2 || rateLimitHits.Load() != 1 || healthyHits.Load() != 1 {
		t.Fatalf("共享预算未按请求消费: capacity=%d 429=%d healthy=%d", capacityHits.Load(), rateLimitHits.Load(), healthyHits.Load())
	}
}

func TestRetryAfterOver60SecondsDoesNotConsumeBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupBlacklistFixEnv(t)
	configureErrorPolicy(t, ErrorPolicySwitchProvider, ErrorPolicyRetryThenSwitchProvider, 1)

	var tooLongHits, retryableHits, fallbackHits atomic.Int32
	tooLong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tooLongHits.Add(1)
		w.Header().Set("Retry-After", "61")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer tooLong.Close()
	retryable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retryableHits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"id":"second-provider-retry"}`)
	}))
	defer retryable.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		_, _ = io.WriteString(w, `{"id":"fallback"}`)
	}))
	defer fallback.Close()

	response := serveResponsesRequest(t, []Provider{
		{ID: 1, Name: "too-long", APIURL: tooLong.URL, APIKey: "key", Enabled: true, Level: 1},
		{ID: 2, Name: "retryable", APIURL: retryable.URL, APIKey: "key", Enabled: true, Level: 1},
		{ID: 3, Name: "fallback", APIURL: fallback.URL, APIKey: "key", Enabled: true, Level: 1},
	})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "second-provider-retry") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if tooLongHits.Load() != 1 || retryableHits.Load() != 2 || fallbackHits.Load() != 0 {
		t.Fatalf("超长 Retry-After 错误消费预算: long=%d retryable=%d fallback=%d", tooLongHits.Load(), retryableHits.Load(), fallbackHits.Load())
	}
}

func TestPolicyRetryWaitHonorsClientCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupBlacklistFixEnv(t)
	if err := ensureRequestEventTable(); err != nil {
		t.Fatal(err)
	}
	configureErrorPolicy(t, ErrorPolicySwitchProvider, ErrorPolicyRetryThenSwitchProvider, 1)

	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	providerService := NewProviderService()
	if err := providerService.SaveProviders(CodexPlatform, []Provider{{
		ID: 1, Name: "limited", APIURL: upstream.URL, APIKey: "key", Enabled: true, Level: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	relay := newTestRelayService(providerService)
	relay.SetRequestEventService(NewRequestEventService())
	router := gin.New()
	router.POST("/responses", relay.proxyHandler(CodexPlatform, "/responses"))
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"gpt-test"}`)).WithContext(ctx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(recorder, request)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("取消后策略等待未结束")
	}
	if hits.Load() != 1 {
		t.Fatalf("取消后仍重试上游: hits=%d", hits.Load())
	}
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatal(err)
	}
	var retriedEvents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_event_log WHERE policy_outcome = 'retried'`).Scan(&retriedEvents); err != nil {
		t.Fatal(err)
	}
	if retriedEvents != 0 {
		t.Fatalf("等待期取消不得统计为已执行重试, got %d", retriedEvents)
	}
}

func TestPolicyRetriesRecordOneBlacklistFailurePerProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupBlacklistFixEnv(t)
	settings := NewSettingsService()
	config, err := settings.GetErrorHandlingConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.HTTP429.Action = ErrorPolicyRetryThenSwitchProvider
	config.SharedRetryAttempts = 2
	config.Blacklist.Enabled = true
	config.Blacklist.EnableLevelBlacklist = false
	config.Blacklist.FallbackMode = "fixed"
	config.Blacklist.FailureThreshold = 10
	config.Blacklist.DedupeWindowSeconds = 1
	if _, err := settings.UpdateErrorHandlingConfig(config); err != nil {
		t.Fatal(err)
	}

	var hits atomic.Int32
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer limited.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"healthy"}`)
	}))
	defer healthy.Close()
	response := serveResponsesRequest(t, []Provider{
		{ID: 1, Name: "limited", APIURL: limited.URL, APIKey: "key", Enabled: true, Level: 1},
		{ID: 2, Name: "healthy", APIURL: healthy.URL, APIKey: "key", Enabled: true, Level: 1},
	})
	if response.Code != http.StatusOK || hits.Load() != 3 {
		t.Fatalf("status=%d hits=%d body=%q", response.Code, hits.Load(), response.Body.String())
	}
	db, _ := xdb.DB("default")
	var failures int
	if err := db.QueryRow(`SELECT failure_count FROM provider_blacklist WHERE platform=? AND provider_name=?`, CodexPlatform, "limited").Scan(&failures); err != nil {
		t.Fatalf("读取失败计数失败: %v", err)
	}
	if failures != 1 {
		t.Fatalf("一次请求应只记一次失败, got %d", failures)
	}
}

func TestRequestPolicyRecordsAtMostOneFailurePerProvider(t *testing.T) {
	setupBlacklistFixEnv(t)
	settings := NewSettingsService()
	config, err := settings.GetErrorHandlingConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.Blacklist.Enabled = true
	config.Blacklist.EnableLevelBlacklist = false
	config.Blacklist.FallbackMode = "fixed"
	config.Blacklist.FailureThreshold = 10
	if _, err := settings.UpdateErrorHandlingConfig(config); err != nil {
		t.Fatal(err)
	}

	appSettings, err := NewAppSettingsService()
	if err != nil {
		t.Fatal(err)
	}
	relay := NewProviderRelayService(
		NewProviderService(),
		NewBlacklistService(settings, nil),
		nil,
		appSettings,
		"",
	)
	state := newRequestErrorPolicyState(config)
	for i := 0; i < 3; i++ {
		if err := relay.recordFailureWithPolicySnapshot(CodexPlatform, "limited", "upstream 500", state); err != nil {
			t.Fatal(err)
		}
	}
	if err := relay.recordFailureWithPolicySnapshot(CodexPlatform, "other", "upstream 500", state); err != nil {
		t.Fatal(err)
	}

	db, _ := xdb.DB("default")
	for provider, want := range map[string]int{"limited": 1, "other": 1} {
		var got int
		if err := db.QueryRow(`SELECT failure_count FROM provider_blacklist WHERE platform=? AND provider_name=?`, CodexPlatform, provider).Scan(&got); err != nil {
			t.Fatalf("读取 %s 失败计数失败: %v", provider, err)
		}
		if got != want {
			t.Fatalf("%s failure_count=%d, want %d", provider, got, want)
		}
	}
}

func TestBlacklistEnabledAndSuccessHandlingUseRequestSnapshot(t *testing.T) {
	setupBlacklistFixEnv(t)
	settings := NewSettingsService()
	config, err := settings.GetErrorHandlingConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.Blacklist.Enabled = true
	config.Blacklist.EnableLevelBlacklist = false
	if _, err := settings.UpdateErrorHandlingConfig(config); err != nil {
		t.Fatal(err)
	}
	state := newRequestErrorPolicyState(config)

	db, _ := xdb.DB("default")
	if _, err := db.Exec(`
		INSERT INTO provider_blacklist (
			platform, provider_name, failure_count, blacklisted_at, blacklisted_until,
			blacklist_level, last_recovered_at, last_degrade_hour
		) VALUES (?, ?, 2, ?, ?, 3, ?, 0)
	`, CodexPlatform, "snapshot-provider", time.Now(), time.Now().Add(time.Hour), time.Now().Add(-61*time.Minute)); err != nil {
		t.Fatal(err)
	}

	config.Blacklist.Enabled = false
	config.Blacklist.EnableLevelBlacklist = true
	if _, err := settings.UpdateErrorHandlingConfig(config); err != nil {
		t.Fatal(err)
	}
	appSettings, err := NewAppSettingsService()
	if err != nil {
		t.Fatal(err)
	}
	relay := NewProviderRelayService(
		NewProviderService(),
		NewBlacklistService(settings, nil),
		nil,
		appSettings,
		"",
	)

	blacklisted, _ := relay.isBlacklistedWithPolicySnapshot(CodexPlatform, "snapshot-provider", state)
	if !blacklisted {
		t.Fatal("请求快照启用拉黑时，不应被中途关闭的全局配置影响")
	}
	if err := relay.recordSuccessWithPolicySnapshot(CodexPlatform, "snapshot-provider", state); err != nil {
		t.Fatal(err)
	}
	var failures, level int
	if err := db.QueryRow(`SELECT failure_count, blacklist_level FROM provider_blacklist WHERE platform=? AND provider_name=?`,
		CodexPlatform, "snapshot-provider").Scan(&failures, &level); err != nil {
		t.Fatal(err)
	}
	if failures != 0 || level != 3 {
		t.Fatalf("成功处理未使用固定模式快照: failures=%d level=%d", failures, level)
	}

	disabledSnapshot := *state
	disabledSnapshot.config.Blacklist.Enabled = false
	blacklisted, _ = relay.isBlacklistedWithPolicySnapshot(CodexPlatform, "snapshot-provider", &disabledSnapshot)
	if blacklisted {
		t.Fatal("请求快照关闭拉黑时，不应被全局或数据库状态重新启用")
	}
}

func TestPolicyRetryJitterStartsWithOneSecondCap(t *testing.T) {
	err := &upstreamStatusError{
		status:           http.StatusTooManyRequests,
		retryAfterHeader: "not-a-delay",
	}
	for i := 0; i < 100; i++ {
		delay, retryAfterMS, permitted := policyRetryDelay(err, 0)
		if !permitted || retryAfterMS != nil {
			t.Fatalf("无效 Retry-After 应回退到 jitter: permitted=%v retryAfterMS=%v", permitted, retryAfterMS)
		}
		if delay < 0 || delay > time.Second {
			t.Fatalf("首次重试 jitter=%v, want [0, 1s]", delay)
		}
	}
}

func TestHugeRetryAfterDoesNotRetryOrConsumeBudget(t *testing.T) {
	state := newRequestErrorPolicyState(&ErrorHandlingConfig{
		Version:             currentErrorHandlingVersion,
		Capacity:            ErrorPolicyConfig{Action: ErrorPolicySwitchProvider},
		HTTP429:             ErrorPolicyConfig{Action: ErrorPolicyRetryThenSwitchProvider},
		SharedRetryAttempts: 1,
		Blacklist:           defaultErrorHandlingConfig().Blacklist,
	})
	err := &upstreamStatusError{
		status:           http.StatusTooManyRequests,
		retryAfterHeader: strings.Repeat("9", 100),
	}

	decision := state.decide(err)
	if decision.Retry || state.used != 0 {
		t.Fatalf("超大 Retry-After 不得消费重试预算: decision=%+v used=%d", decision, state.used)
	}
	if decision.RetryAfterMS == nil || *decision.RetryAfterMS <= int64((60*time.Second)/time.Millisecond) {
		t.Fatalf("应记录 Retry-After 超出上限: %+v", decision.RetryAfterMS)
	}
}

func TestScientificRetryAfterFallsBackToJitter(t *testing.T) {
	err := &upstreamStatusError{
		status:           http.StatusTooManyRequests,
		retryAfterHeader: "1e2",
	}
	delay, retryAfterMS, permitted := policyRetryDelay(err, 0)
	if !permitted || retryAfterMS != nil || delay < 0 || delay > time.Second {
		t.Fatalf("非十进制 Retry-After 应回退到首次 jitter: delay=%v retryAfterMS=%v permitted=%v", delay, retryAfterMS, permitted)
	}
}

func TestHealthStatusClassifiesCapacityBeforeHTTPStatus(t *testing.T) {
	service := &HealthCheckService{}
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusServiceUnavailable} {
		got, message := service.determineStatus(status, 1,
			[]byte(`{"error":{"code":"server_overloaded","message":"`+capacityMessage+`"}}`))
		if got != HealthStatusFailed || message != "模型容量不足" {
			t.Fatalf("HTTP %d capacity = (%q, %q), want failed/capacity", status, got, message)
		}
	}
	got, message := service.determineStatus(http.StatusServiceUnavailable, 1,
		[]byte(`upstream rejected the request: Selected model is at capacity. Please try a different model.`))
	if got != HealthStatusFailed || message != "模型容量不足" {
		t.Fatalf("非 2xx Capacity 文本 = (%q, %q), want failed/capacity", got, message)
	}
}

func serveResponsesRequest(t *testing.T, providers []Provider) *httptest.ResponseRecorder {
	t.Helper()
	providerService := NewProviderService()
	if err := providerService.SaveProviders(CodexPlatform, providers); err != nil {
		t.Fatalf("保存 Provider 失败: %v", err)
	}
	relay := newTestRelayService(providerService)
	router := gin.New()
	router.POST("/responses", relay.proxyHandler(CodexPlatform, "/responses"))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/responses",
		strings.NewReader(`{"model":"gpt-test","stream":false}`))
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestCapacityPassThroughUsesCapacityRepresentativeAfterEndpointPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupBlacklistFixEnv(t)
	configureErrorPolicy(t, ErrorPolicyPassThrough, ErrorPolicyReturn502, 0)

	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2.5")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit"}}`)
	}))
	defer rateLimited.Close()
	capacity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Choice", "capacity")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"server_overloaded","message":"`+capacityMessage+`"}}`)
	}))
	defer capacity.Close()

	response := serveResponsesRequest(t, []Provider{{
		ID: 1, Name: "mixed", APIURL: rateLimited.URL, FallbackAPIURLs: []string{capacity.URL},
		APIKey: "key", Enabled: true, Level: 1,
	}})

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Upstream-Choice") != "capacity" || !strings.Contains(response.Body.String(), capacityMessage) {
		t.Fatalf("未原样透传 Capacity 代表响应: headers=%v body=%q", response.Header(), response.Body.String())
	}
}

func TestCapacityRepresentativeBeatsLaterHTTP429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupBlacklistFixEnv(t)
	configureErrorPolicy(t, ErrorPolicyPassThrough, ErrorPolicyPassThrough, 0)

	capacity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream-Choice", "capacity")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"server_overloaded"}}`)
	}))
	defer capacity.Close()
	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream-Choice", "rate-limit")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit"}}`)
	}))
	defer rateLimited.Close()

	response := serveResponsesRequest(t, []Provider{{
		ID: 1, Name: "mixed", APIURL: capacity.URL, FallbackAPIURLs: []string{rateLimited.URL},
		APIKey: "key", Enabled: true, Level: 1,
	}})
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("X-Upstream-Choice") != "capacity" {
		t.Fatalf("Capacity 未保持最高优先级: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}

func TestEndpointPoolUsesLastResponseWithinSamePolicyClass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupBlacklistFixEnv(t)
	configureErrorPolicy(t, ErrorPolicySwitchProvider, ErrorPolicyPassThrough, 0)

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream-Choice", "first")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"first"}}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream-Choice", "second")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"second"}}`)
	}))
	defer second.Close()

	response := serveResponsesRequest(t, []Provider{{
		ID: 1, Name: "limited", APIURL: first.URL, FallbackAPIURLs: []string{second.URL},
		APIKey: "key", Enabled: true, Level: 1,
	}})
	if response.Code != http.StatusTooManyRequests || response.Header().Get("X-Upstream-Choice") != "second" ||
		!strings.Contains(response.Body.String(), `"second"`) {
		t.Fatalf("同类错误未选择最后响应: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}

func TestHTTP429Return502StopsProviderSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupBlacklistFixEnv(t)
	configureErrorPolicy(t, ErrorPolicySwitchProvider, ErrorPolicyReturn502, 0)

	var fallbackHits atomic.Int32
	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit"}}`)
	}))
	defer rateLimited.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		_, _ = io.WriteString(w, `{"id":"must-not-run"}`)
	}))
	defer fallback.Close()

	response := serveResponsesRequest(t, []Provider{
		{ID: 1, Name: "limited", APIURL: rateLimited.URL, APIKey: "key", Enabled: true, Level: 1},
		{ID: 2, Name: "fallback", APIURL: fallback.URL, APIKey: "key", Enabled: true, Level: 1},
	})

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", response.Code, response.Body.String())
	}
	if fallbackHits.Load() != 0 {
		t.Fatalf("return_502 后仍切换 Provider: hits=%d", fallbackHits.Load())
	}
	for _, fragment := range []string{`"error_code":"upstream_policy_rejected"`, `"policy_trigger":"http_429"`, `"policy_action":"return_502"`} {
		if !strings.Contains(response.Body.String(), fragment) {
			t.Fatalf("结构化策略字段缺失 %s: %q", fragment, response.Body.String())
		}
	}
}

func TestHTTP429RetryThenSwitchRetriesCurrentProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupBlacklistFixEnv(t)
	configureErrorPolicy(t, ErrorPolicySwitchProvider, ErrorPolicyRetryThenSwitchProvider, 1)

	var primaryHits, fallbackHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if primaryHits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"retried-current-provider"}`)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		_, _ = io.WriteString(w, `{"id":"fallback"}`)
	}))
	defer fallback.Close()

	response := serveResponsesRequest(t, []Provider{
		{ID: 1, Name: "primary", APIURL: primary.URL, APIKey: "key", Enabled: true, Level: 1},
		{ID: 2, Name: "fallback", APIURL: fallback.URL, APIKey: "key", Enabled: true, Level: 1},
	})

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "retried-current-provider") {
		t.Fatalf("重试未成功: status=%d body=%q", response.Code, response.Body.String())
	}
	if primaryHits.Load() != 2 || fallbackHits.Load() != 0 {
		t.Fatalf("请求次数错误: primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
	}
}
