package services

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
)

func TestGetErrorHandlingConfigMigratesEffectiveLegacySettingsOnce(t *testing.T) {
	setupBlacklistFixEnv(t)
	setAppSetting(t, "enable_blacklist", "true")
	setAppSetting(t, "blacklist_level_enabled", "true")
	setAppSetting(t, "blacklist_failure_threshold", "7")
	setAppSetting(t, "blacklist_duration_minutes", "45")

	legacy := DefaultBlacklistLevelConfig()
	legacy.DedupeWindowSeconds = 9
	legacy.RetryWaitSeconds = 12
	legacy.NormalDegradeIntervalHours = 2
	legacy.ForgivenessHours = 6
	legacy.JumpPenaltyWindowHours = 4
	legacy.L1DurationMinutes = 8
	legacy.L2DurationMinutes = 20
	legacy.L3DurationMinutes = 80
	legacy.L4DurationMinutes = 480
	legacy.L5DurationMinutes = 1920
	legacy.FallbackMode = "none"
	legacy.FallbackDurationMinutes = 55

	path, err := GetBlacklistLevelConfigPath()
	if err != nil {
		t.Fatalf("获取旧配置路径失败: %v", err)
	}
	data := []byte(`{
		"enableLevelBlacklist": false,
		"failureThreshold": 2,
		"dedupeWindowSeconds": 9,
		"retryWaitSeconds": 12,
		"normalDegradeIntervalHours": 2,
		"forgivenessHours": 6,
		"jumpPenaltyWindowHours": 4,
		"l1DurationMinutes": 8,
		"l2DurationMinutes": 20,
		"l3DurationMinutes": 80,
		"l4DurationMinutes": 480,
		"l5DurationMinutes": 1920,
		"fallbackMode": "none",
		"fallbackDurationMinutes": 55
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写入旧配置失败: %v", err)
	}

	service := NewSettingsService()
	got, err := service.GetErrorHandlingConfig()
	if err != nil {
		t.Fatalf("首次读取失败: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("version = %d, want 1", got.Version)
	}
	if got.Capacity.Action != ErrorPolicySwitchProvider || got.HTTP429.Action != ErrorPolicySwitchProvider {
		t.Fatalf("默认策略 = capacity:%q 429:%q, want switch_provider", got.Capacity.Action, got.HTTP429.Action)
	}
	if got.SharedRetryAttempts != 0 {
		t.Fatalf("共享重试预算 = %d, want 0", got.SharedRetryAttempts)
	}
	if !got.Blacklist.Enabled || !got.Blacklist.EnableLevelBlacklist {
		t.Fatalf("数据库开关未迁移: %+v", got.Blacklist)
	}
	if got.Blacklist.FailureThreshold != 7 || got.Blacklist.FallbackDurationMinutes != 45 {
		t.Fatalf("数据库基础配置未覆盖旧 JSON: %+v", got.Blacklist)
	}
	if got.Blacklist.DedupeWindowSeconds != legacy.DedupeWindowSeconds ||
		got.Blacklist.RetryWaitSeconds != legacy.RetryWaitSeconds ||
		got.Blacklist.L5DurationMinutes != legacy.L5DurationMinutes ||
		got.Blacklist.FallbackMode != legacy.FallbackMode {
		t.Fatalf("旧 JSON 高级配置未迁移: %+v", got.Blacklist)
	}

	// 新配置落库后，旧键和旧文件只作为备份，不再成为数据源。
	setAppSetting(t, "enable_blacklist", "false")
	setAppSetting(t, "blacklist_failure_threshold", "1")
	if err := os.WriteFile(path, []byte(`{"dedupeWindowSeconds": 99}`), 0o600); err != nil {
		t.Fatalf("修改旧配置失败: %v", err)
	}
	again, err := NewSettingsService().GetErrorHandlingConfig()
	if err != nil {
		t.Fatalf("二次读取失败: %v", err)
	}
	if !again.Blacklist.Enabled || again.Blacklist.FailureThreshold != 7 || again.Blacklist.DedupeWindowSeconds != 9 {
		t.Fatalf("新配置被旧数据覆盖: %+v", again.Blacklist)
	}
}

func TestErrorHandlingTodaySummaryUsesConfiguredTimezoneAndDatabase(t *testing.T) {
	setupBlacklistFixEnv(t)
	if err := ensureRequestEventTable(); err != nil {
		t.Fatal(err)
	}
	appSettings, err := NewAppSettingsService()
	if err != nil {
		t.Fatal(err)
	}
	settings, err := appSettings.GetAppSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Timezone = "America/New_York"
	if _, err := appSettings.SaveAppSettings(settings); err != nil {
		t.Fatal(err)
	}

	events := NewRequestEventService()
	record := func(requestID, eventType, trigger, outcome string) {
		t.Helper()
		if err := events.Record(RequestEventInput{
			RequestID:     requestID,
			Platform:      CodexPlatform,
			EventType:     eventType,
			PolicyTrigger: trigger,
			PolicyAction:  ErrorPolicyRetryThenSwitchProvider,
			PolicyOutcome: outcome,
		}); err != nil {
			t.Fatalf("写入摘要事件失败: %v", err)
		}
	}
	record("capacity-one", RequestEventError, PolicyTriggerCapacity, "retried")
	record("capacity-one", RequestEventError, PolicyTriggerCapacity, "switch_requested")
	record("rate-one", RequestEventError, PolicyTriggerHTTP429, "passed_through")
	record("switch-one", RequestEventSwitch, PolicyTriggerHTTP429, "switched_provider")
	record("terminal-502", RequestEventError, PolicyTriggerCapacity, "returned_502")
	record("outside", RequestEventError, PolicyTriggerCapacity, "returned_502")
	db, _ := xdb.DB("default")
	if _, err := db.Exec(`UPDATE request_event_log SET created_at='2000-01-01 00:00:00' WHERE request_id='outside'`); err != nil {
		t.Fatal(err)
	}

	logs := NewLogService()
	logs.SetAppSettingsService(appSettings)
	summary, err := logs.GetErrorHandlingTodaySummary()
	if err != nil {
		t.Fatalf("读取今日摘要失败: %v", err)
	}
	if summary.Timezone != "America/New_York" {
		t.Fatalf("timezone=%q", summary.Timezone)
	}
	if summary.CapacityHits != 2 || summary.HTTP429Hits != 2 {
		t.Fatalf("命中请求去重错误: %+v", summary)
	}
	if summary.RetryActions != 1 || summary.ProviderSwitchActions != 1 ||
		summary.PassThroughRequests != 1 || summary.Returned502Requests != 1 {
		t.Fatalf("动作统计错误: %+v", summary)
	}
	location, _ := time.LoadLocation("America/New_York")
	now := time.Now().In(location)
	wantStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location).UTC()
	if summary.StartUTC != wantStart.Format(timeLayout) {
		t.Fatalf("start=%q want=%q", summary.StartUTC, wantStart.Format(timeLayout))
	}
}

func TestLegacyBlacklistMutationsMergeIntoCanonicalConfig(t *testing.T) {
	setupBlacklistFixEnv(t)
	service := NewSettingsService()
	if _, err := service.GetErrorHandlingConfig(); err != nil {
		t.Fatalf("初始化统一配置失败: %v", err)
	}

	level := DefaultBlacklistLevelConfig()
	level.DedupeWindowSeconds = 11
	level.RetryWaitSeconds = 15
	level.NormalDegradeIntervalHours = 2

	start := make(chan struct{})
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for _, mutate := range []func() error{
		func() error { return service.UpdateBlacklistEnabled(true) },
		func() error { return service.SetLevelBlacklistEnabled(true) },
		func() error { return service.UpdateBlacklistSettings(6, 75) },
		func() error { return service.UpdateBlacklistLevelConfig(level) },
	} {
		wg.Add(1)
		go func(mutate func() error) {
			defer wg.Done()
			<-start
			errs <- mutate()
		}(mutate)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("旧 RPC 更新失败: %v", err)
		}
	}

	got, err := service.GetErrorHandlingConfig()
	if err != nil {
		t.Fatalf("读取统一配置失败: %v", err)
	}
	if !got.Blacklist.Enabled || !got.Blacklist.EnableLevelBlacklist {
		t.Fatalf("并发开关更新丢失: %+v", got.Blacklist)
	}
	if got.Blacklist.FailureThreshold != 6 || got.Blacklist.FallbackDurationMinutes != 75 {
		t.Fatalf("并发基础配置更新丢失: %+v", got.Blacklist)
	}
	if got.Blacklist.DedupeWindowSeconds != 11 || got.Blacklist.RetryWaitSeconds != 15 || got.Blacklist.NormalDegradeIntervalHours != 2 {
		t.Fatalf("并发等级配置更新丢失: %+v", got.Blacklist)
	}
	if got.Capacity.Action != ErrorPolicySwitchProvider || got.HTTP429.Action != ErrorPolicySwitchProvider {
		t.Fatalf("旧 RPC 意外修改错误策略: %+v", got)
	}
}

func TestRequestEventPolicyMetadataRoundTrips(t *testing.T) {
	setupBlacklistFixEnv(t)
	if err := ensureRequestEventTable(); err != nil {
		t.Fatalf("初始化事件表失败: %v", err)
	}
	delayMS := int64(2500)
	retryAfterMS := int64(3000)
	budgetUsed := 1
	service := NewRequestEventService()
	if err := service.Record(RequestEventInput{
		RequestID:       "policy-request",
		Platform:        CodexPlatform,
		Model:           "gpt-test",
		EventType:       RequestEventError,
		Provider:        "limited",
		Attempt:         1,
		HTTPCode:        429,
		ErrorType:       "provider_error",
		ErrorCode:       "provider_request_failed",
		Outcome:         "continued",
		PolicyTrigger:   PolicyTriggerHTTP429,
		PolicyAction:    ErrorPolicyRetryThenSwitchProvider,
		PolicyOutcome:   "retried",
		RetryBudgetUsed: &budgetUsed,
		RetryDelayMS:    &delayMS,
		RetryAfterMS:    &retryAfterMS,
	}); err != nil {
		t.Fatalf("写入策略事件失败: %v", err)
	}

	events, err := NewLogService(nil).ListRequestEvents(CodexPlatform, "all", "", "policy-request", 1, 10, 0)
	if err != nil {
		t.Fatalf("读取策略事件失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("事件数 = %d, want 1", len(events))
	}
	got := events[0]
	if got.PolicyTrigger != PolicyTriggerHTTP429 || got.PolicyAction != ErrorPolicyRetryThenSwitchProvider || got.PolicyOutcome != "retried" {
		t.Fatalf("策略枚举未回读: %+v", got)
	}
	if got.RetryBudgetUsed == nil || *got.RetryBudgetUsed != budgetUsed ||
		got.RetryDelayMS == nil || *got.RetryDelayMS != delayMS ||
		got.RetryAfterMS == nil || *got.RetryAfterMS != retryAfterMS {
		t.Fatalf("策略数值未回读: %+v", got)
	}
}

func TestCorruptErrorHandlingConfigReturnsWarningWithoutOverwrite(t *testing.T) {
	setupBlacklistFixEnv(t)
	setAppSetting(t, errorHandlingConfigKey, `{"version":1,"capacity":`)
	service := NewSettingsService()
	config, err := service.GetErrorHandlingConfig()
	if err != nil {
		t.Fatalf("损坏配置不应阻止服务加载: %v", err)
	}
	if config.Warning == "" {
		t.Fatal("损坏配置未暴露告警")
	}
	if config.Capacity.Action != ErrorPolicySwitchProvider || config.HTTP429.Action != ErrorPolicySwitchProvider {
		t.Fatalf("损坏配置未使用兼容默认策略: %+v", config)
	}
	db, _ := xdb.DB("default")
	var raw string
	if err := db.QueryRow(`SELECT value FROM app_settings WHERE key=?`, errorHandlingConfigKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != `{"version":1,"capacity":` {
		t.Fatalf("损坏原值被覆盖: %q", raw)
	}
}

func TestUpdateErrorHandlingConfigRejectsInvalidValue(t *testing.T) {
	setupBlacklistFixEnv(t)
	service := NewSettingsService()
	before, err := service.GetErrorHandlingConfig()
	if err != nil {
		t.Fatal(err)
	}
	invalid := *before
	invalid.SharedRetryAttempts = 6
	if _, err := service.UpdateErrorHandlingConfig(&invalid); err == nil {
		t.Fatal("sharedRetryAttempts=6 应被拒绝")
	}
	after, err := service.GetErrorHandlingConfig()
	if err != nil {
		t.Fatal(err)
	}
	if after.SharedRetryAttempts != before.SharedRetryAttempts {
		t.Fatalf("无效更新修改了持久配置: before=%d after=%d", before.SharedRetryAttempts, after.SharedRetryAttempts)
	}
}

func TestBlacklistRetryWaitIsIndependentFromCrossRequestDedupe(t *testing.T) {
	setupBlacklistFixEnv(t)
	service := NewSettingsService()
	config, err := service.GetErrorHandlingConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.Blacklist.DedupeWindowSeconds = 300
	config.Blacklist.RetryWaitSeconds = 1
	if _, err := service.UpdateErrorHandlingConfig(config); err != nil {
		t.Fatalf("请求内失败已单独去重，重试等待不应再受跨请求窗口限制: %v", err)
	}
}
