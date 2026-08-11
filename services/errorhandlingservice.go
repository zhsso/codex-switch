package services

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/daodao97/xgo/xdb"
)

const (
	errorHandlingConfigKey      = "error_handling_config"
	currentErrorHandlingVersion = 1

	ErrorPolicyPassThrough             = "pass_through"
	ErrorPolicyReturn502               = "return_502"
	ErrorPolicySwitchProvider          = "switch_provider"
	ErrorPolicyRetryThenSwitchProvider = "retry_then_switch_provider"
)

// ErrorPolicyConfig describes the terminal action for one upstream error class.
type ErrorPolicyConfig struct {
	Action string `json:"action"`
}

// ErrorHandlingBlacklistConfig is the canonical blacklist configuration.
// The former database keys and blacklist-config.json are migration inputs only.
type ErrorHandlingBlacklistConfig struct {
	Enabled                    bool    `json:"enabled"`
	EnableLevelBlacklist       bool    `json:"enableLevelBlacklist"`
	FailureThreshold           int     `json:"failureThreshold"`
	DedupeWindowSeconds        int     `json:"dedupeWindowSeconds"`
	RetryWaitSeconds           int     `json:"retryWaitSeconds"`
	NormalDegradeIntervalHours float64 `json:"normalDegradeIntervalHours"`
	ForgivenessHours           float64 `json:"forgivenessHours"`
	JumpPenaltyWindowHours     float64 `json:"jumpPenaltyWindowHours"`
	L1DurationMinutes          int     `json:"l1DurationMinutes"`
	L2DurationMinutes          int     `json:"l2DurationMinutes"`
	L3DurationMinutes          int     `json:"l3DurationMinutes"`
	L4DurationMinutes          int     `json:"l4DurationMinutes"`
	L5DurationMinutes          int     `json:"l5DurationMinutes"`
	FallbackMode               string  `json:"fallbackMode"`
	FallbackDurationMinutes    int     `json:"fallbackDurationMinutes"`
}

// ErrorHandlingConfig is snapshotted once for every incoming relay request.
type ErrorHandlingConfig struct {
	Version             int                          `json:"version"`
	Capacity            ErrorPolicyConfig            `json:"capacity"`
	HTTP429             ErrorPolicyConfig            `json:"http429"`
	SharedRetryAttempts int                          `json:"sharedRetryAttempts"`
	Blacklist           ErrorHandlingBlacklistConfig `json:"blacklist"`
	Warning             string                       `json:"warning,omitempty"`
}

var errorHandlingConfigMu sync.Mutex

func defaultErrorHandlingConfig() *ErrorHandlingConfig {
	legacy := DefaultBlacklistLevelConfig()
	return &ErrorHandlingConfig{
		Version:             currentErrorHandlingVersion,
		Capacity:            ErrorPolicyConfig{Action: ErrorPolicySwitchProvider},
		HTTP429:             ErrorPolicyConfig{Action: ErrorPolicySwitchProvider},
		SharedRetryAttempts: 0,
		Blacklist: ErrorHandlingBlacklistConfig{
			Enabled:                    false,
			EnableLevelBlacklist:       legacy.EnableLevelBlacklist,
			FailureThreshold:           legacy.FailureThreshold,
			DedupeWindowSeconds:        legacy.DedupeWindowSeconds,
			RetryWaitSeconds:           legacy.RetryWaitSeconds,
			NormalDegradeIntervalHours: legacy.NormalDegradeIntervalHours,
			ForgivenessHours:           legacy.ForgivenessHours,
			JumpPenaltyWindowHours:     legacy.JumpPenaltyWindowHours,
			L1DurationMinutes:          legacy.L1DurationMinutes,
			L2DurationMinutes:          legacy.L2DurationMinutes,
			L3DurationMinutes:          legacy.L3DurationMinutes,
			L4DurationMinutes:          legacy.L4DurationMinutes,
			L5DurationMinutes:          legacy.L5DurationMinutes,
			FallbackMode:               legacy.FallbackMode,
			FallbackDurationMinutes:    legacy.FallbackDurationMinutes,
		},
	}
}

// GetErrorHandlingConfig returns the canonical config. The first call imports
// the effective legacy values exactly once and stores the resulting snapshot.
func (ss *SettingsService) GetErrorHandlingConfig() (*ErrorHandlingConfig, error) {
	errorHandlingConfigMu.Lock()
	defer errorHandlingConfigMu.Unlock()
	return ss.getErrorHandlingConfigLocked()
}

func (ss *SettingsService) getErrorHandlingConfigLocked() (*ErrorHandlingConfig, error) {
	db, err := xdb.DB("default")
	if err != nil {
		return nil, fmt.Errorf("获取数据库连接失败: %w", err)
	}

	var raw string
	err = db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, errorHandlingConfigKey).Scan(&raw)
	switch {
	case err == nil:
		config := defaultErrorHandlingConfig()
		if decodeErr := json.Unmarshal([]byte(raw), config); decodeErr != nil {
			return ss.corruptErrorHandlingFallback(fmt.Errorf("解析统一错误处理配置失败: %w", decodeErr)), nil
		}
		if validateErr := validateErrorHandlingConfig(config); validateErr != nil {
			return ss.corruptErrorHandlingFallback(fmt.Errorf("统一错误处理配置无效: %w", validateErr)), nil
		}
		return config, nil
	case err != sql.ErrNoRows:
		return nil, fmt.Errorf("读取统一错误处理配置失败: %w", err)
	}

	config, err := ss.migrateLegacyErrorHandlingConfig()
	if err != nil {
		return nil, err
	}
	if err := saveErrorHandlingConfigLocked(config); err != nil {
		return nil, err
	}
	return config, nil
}

func (ss *SettingsService) migrateLegacyErrorHandlingConfig() (*ErrorHandlingConfig, error) {
	config := defaultErrorHandlingConfig()
	config.Blacklist.Enabled = readLegacyBlacklistEnabled()

	legacy, err := loadLegacyBlacklistLevelConfig()
	if err != nil {
		return nil, fmt.Errorf("迁移旧等级拉黑配置失败: %w", err)
	}
	config.Blacklist.EnableLevelBlacklist = legacy.EnableLevelBlacklist
	config.Blacklist.FailureThreshold = legacy.FailureThreshold
	config.Blacklist.DedupeWindowSeconds = legacy.DedupeWindowSeconds
	config.Blacklist.RetryWaitSeconds = legacy.RetryWaitSeconds
	config.Blacklist.NormalDegradeIntervalHours = legacy.NormalDegradeIntervalHours
	config.Blacklist.ForgivenessHours = legacy.ForgivenessHours
	config.Blacklist.JumpPenaltyWindowHours = legacy.JumpPenaltyWindowHours
	config.Blacklist.L1DurationMinutes = legacy.L1DurationMinutes
	config.Blacklist.L2DurationMinutes = legacy.L2DurationMinutes
	config.Blacklist.L3DurationMinutes = legacy.L3DurationMinutes
	config.Blacklist.L4DurationMinutes = legacy.L4DurationMinutes
	config.Blacklist.L5DurationMinutes = legacy.L5DurationMinutes
	config.Blacklist.FallbackMode = legacy.FallbackMode
	config.Blacklist.FallbackDurationMinutes = legacy.FallbackDurationMinutes

	threshold, duration, err := readLegacyBlacklistSettings()
	if err != nil {
		return nil, fmt.Errorf("迁移旧基础拉黑配置失败: %w", err)
	}
	config.Blacklist.FailureThreshold = threshold
	config.Blacklist.FallbackDurationMinutes = duration
	if err := validateErrorHandlingConfig(config); err != nil {
		return nil, fmt.Errorf("迁移后的统一错误处理配置无效: %w", err)
	}
	return config, nil
}

func (ss *SettingsService) corruptErrorHandlingFallback(cause error) *ErrorHandlingConfig {
	config, err := ss.migrateLegacyErrorHandlingConfig()
	if err != nil {
		config = defaultErrorHandlingConfig()
		cause = fmt.Errorf("%v; 读取兼容配置也失败: %w", cause, err)
	}
	config.Warning = cause.Error()
	log.Printf("[SettingsService] %s；本次使用兼容默认值，原始配置未覆盖", config.Warning)
	return config
}

// UpdateErrorHandlingConfig validates and atomically replaces the canonical config.
func (ss *SettingsService) UpdateErrorHandlingConfig(config *ErrorHandlingConfig) (*ErrorHandlingConfig, error) {
	errorHandlingConfigMu.Lock()
	defer errorHandlingConfigMu.Unlock()

	if config == nil {
		return nil, fmt.Errorf("统一错误处理配置不能为空")
	}
	copyConfig := *config
	copyConfig.Warning = ""
	if err := validateErrorHandlingConfig(&copyConfig); err != nil {
		return nil, err
	}
	if err := saveErrorHandlingConfigLocked(&copyConfig); err != nil {
		return nil, err
	}
	return &copyConfig, nil
}

func (ss *SettingsService) mutateErrorHandlingConfig(mutate func(*ErrorHandlingConfig)) error {
	errorHandlingConfigMu.Lock()
	defer errorHandlingConfigMu.Unlock()

	config, err := ss.getErrorHandlingConfigLocked()
	if err != nil {
		return err
	}
	mutate(config)
	config.Warning = ""
	if err := validateErrorHandlingConfig(config); err != nil {
		return err
	}
	return saveErrorHandlingConfigLocked(config)
}

func readLegacyBlacklistEnabled() bool {
	db, err := xdb.DB("default")
	if err != nil {
		return false
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM app_settings WHERE key = 'enable_blacklist'`).Scan(&value); err != nil {
		return false
	}
	return value == "true"
}

func readLegacyBlacklistSettings() (int, int, error) {
	db, err := xdb.DB("default")
	if err != nil {
		return 0, 0, err
	}
	var threshold, duration int
	if err := db.QueryRow(`SELECT CAST(value AS INTEGER) FROM app_settings WHERE key = 'blacklist_failure_threshold'`).Scan(&threshold); err != nil {
		return 0, 0, err
	}
	if err := db.QueryRow(`SELECT CAST(value AS INTEGER) FROM app_settings WHERE key = 'blacklist_duration_minutes'`).Scan(&duration); err != nil {
		return 0, 0, err
	}
	return threshold, duration, nil
}

func saveErrorHandlingConfigLocked(config *ErrorHandlingConfig) error {
	raw, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("序列化统一错误处理配置失败: %w", err)
	}
	query := `INSERT INTO app_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if GlobalDBQueue != nil {
		if err := GlobalDBQueue.Exec(query, errorHandlingConfigKey, string(raw)); err != nil {
			return fmt.Errorf("保存统一错误处理配置失败: %w", err)
		}
		return nil
	}
	db, err := xdb.DB("default")
	if err != nil {
		return fmt.Errorf("获取数据库连接失败: %w", err)
	}
	if _, err := db.Exec(query, errorHandlingConfigKey, string(raw)); err != nil {
		return fmt.Errorf("保存统一错误处理配置失败: %w", err)
	}
	return nil
}

func validateErrorHandlingConfig(config *ErrorHandlingConfig) error {
	if config.Version != currentErrorHandlingVersion {
		return fmt.Errorf("version 必须为 %d", currentErrorHandlingVersion)
	}
	for label, action := range map[string]string{
		"capacity.action": config.Capacity.Action,
		"http429.action":  config.HTTP429.Action,
	} {
		switch action {
		case ErrorPolicyPassThrough, ErrorPolicyReturn502, ErrorPolicySwitchProvider, ErrorPolicyRetryThenSwitchProvider:
		default:
			return fmt.Errorf("%s 不支持 %q", label, action)
		}
	}
	if config.SharedRetryAttempts < 0 || config.SharedRetryAttempts > 5 {
		return fmt.Errorf("sharedRetryAttempts 必须在 0-5 之间")
	}
	legacy := &BlacklistLevelConfig{
		EnableLevelBlacklist:       config.Blacklist.EnableLevelBlacklist,
		FailureThreshold:           config.Blacklist.FailureThreshold,
		DedupeWindowSeconds:        config.Blacklist.DedupeWindowSeconds,
		RetryWaitSeconds:           config.Blacklist.RetryWaitSeconds,
		NormalDegradeIntervalHours: config.Blacklist.NormalDegradeIntervalHours,
		ForgivenessHours:           config.Blacklist.ForgivenessHours,
		JumpPenaltyWindowHours:     config.Blacklist.JumpPenaltyWindowHours,
		L1DurationMinutes:          config.Blacklist.L1DurationMinutes,
		L2DurationMinutes:          config.Blacklist.L2DurationMinutes,
		L3DurationMinutes:          config.Blacklist.L3DurationMinutes,
		L4DurationMinutes:          config.Blacklist.L4DurationMinutes,
		L5DurationMinutes:          config.Blacklist.L5DurationMinutes,
		FallbackMode:               config.Blacklist.FallbackMode,
		FallbackDurationMinutes:    config.Blacklist.FallbackDurationMinutes,
	}
	return validateBlacklistLevelConfig(legacy)
}
