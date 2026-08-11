package services

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/daodao97/xgo/xdb"
)

// GetBlacklistLevelConfigPath 获取等级拉黑配置文件路径
func GetBlacklistLevelConfigPath() (string, error) {
	configDir, err := getUserConfigDir()
	if err != nil {
		return "", fmt.Errorf("获取用户目录失败: %w", err)
	}

	return filepath.Join(configDir, "blacklist-config.json"), nil
}

// loadLegacyBlacklistLevelConfig reads the pre-v1 effective configuration.
// It is used only when the canonical error_handling_config key is absent or corrupt.
func loadLegacyBlacklistLevelConfig() (*BlacklistLevelConfig, error) {
	configPath, err := GetBlacklistLevelConfigPath()
	if err != nil {
		return nil, err
	}

	config := DefaultBlacklistLevelConfig()

	// 如果配置文件存在，用其内容覆盖默认值
	if _, err := os.Stat(configPath); err == nil {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}

		// JSON Unmarshal 只会覆盖 JSON 中存在的字段，未出现的字段保持默认值
		if err := json.Unmarshal(data, config); err != nil {
			return nil, fmt.Errorf("解析配置文件失败: %w", err)
		}
	}
	if db, dbErr := xdb.DB("default"); dbErr == nil {
		var enabled string
		if queryErr := db.QueryRow(`SELECT value FROM app_settings WHERE key = 'blacklist_level_enabled'`).Scan(&enabled); queryErr == nil {
			config.EnableLevelBlacklist = enabled == "true"
		}
		var threshold int
		if queryErr := db.QueryRow(`SELECT CAST(value AS INTEGER) FROM app_settings WHERE key = 'blacklist_failure_threshold'`).Scan(&threshold); queryErr == nil && threshold > 0 {
			config.FailureThreshold = threshold
		}
	}
	return config, nil
}

// GetBlacklistLevelConfig keeps the legacy RPC shape while reading the
// canonical configuration.
func (ss *SettingsService) GetBlacklistLevelConfig() (*BlacklistLevelConfig, error) {
	config, err := ss.GetErrorHandlingConfig()
	if err != nil {
		return nil, err
	}
	return blacklistLevelConfigFromCanonical(config.Blacklist), nil
}

func blacklistLevelConfigFromCanonical(config ErrorHandlingBlacklistConfig) *BlacklistLevelConfig {
	return &BlacklistLevelConfig{
		EnableLevelBlacklist:       config.EnableLevelBlacklist,
		FailureThreshold:           config.FailureThreshold,
		DedupeWindowSeconds:        config.DedupeWindowSeconds,
		RetryWaitSeconds:           config.RetryWaitSeconds,
		NormalDegradeIntervalHours: config.NormalDegradeIntervalHours,
		ForgivenessHours:           config.ForgivenessHours,
		JumpPenaltyWindowHours:     config.JumpPenaltyWindowHours,
		L1DurationMinutes:          config.L1DurationMinutes,
		L2DurationMinutes:          config.L2DurationMinutes,
		L3DurationMinutes:          config.L3DurationMinutes,
		L4DurationMinutes:          config.L4DurationMinutes,
		L5DurationMinutes:          config.L5DurationMinutes,
		FallbackMode:               config.FallbackMode,
		FallbackDurationMinutes:    config.FallbackDurationMinutes,
	}
}

// SaveBlacklistLevelConfig is retained for callers outside the RPC registry.
// Fields which were historically overridden by database settings remain owned
// by their dedicated legacy mutations.
func (ss *SettingsService) SaveBlacklistLevelConfig(config *BlacklistLevelConfig) error {
	if config == nil {
		return fmt.Errorf("等级拉黑配置不能为空")
	}
	return ss.mutateErrorHandlingConfig(func(current *ErrorHandlingConfig) {
		current.Blacklist.DedupeWindowSeconds = config.DedupeWindowSeconds
		current.Blacklist.RetryWaitSeconds = config.RetryWaitSeconds
		current.Blacklist.NormalDegradeIntervalHours = config.NormalDegradeIntervalHours
		current.Blacklist.ForgivenessHours = config.ForgivenessHours
		current.Blacklist.JumpPenaltyWindowHours = config.JumpPenaltyWindowHours
		current.Blacklist.L1DurationMinutes = config.L1DurationMinutes
		current.Blacklist.L2DurationMinutes = config.L2DurationMinutes
		current.Blacklist.L3DurationMinutes = config.L3DurationMinutes
		current.Blacklist.L4DurationMinutes = config.L4DurationMinutes
		current.Blacklist.L5DurationMinutes = config.L5DurationMinutes
		current.Blacklist.FallbackMode = config.FallbackMode
	})
}

// UpdateBlacklistLevelConfig 更新等级拉黑配置
func (ss *SettingsService) UpdateBlacklistLevelConfig(config *BlacklistLevelConfig) error {
	// 验证配置
	if err := validateBlacklistLevelConfig(config); err != nil {
		return err
	}

	return ss.SaveBlacklistLevelConfig(config)
}

// validateBlacklistLevelConfig 验证等级拉黑配置
func validateBlacklistLevelConfig(config *BlacklistLevelConfig) error {
	if config.FailureThreshold < 1 || config.FailureThreshold > 10 {
		return fmt.Errorf("失败阈值必须在 1-10 之间")
	}

	if config.DedupeWindowSeconds < 1 || config.DedupeWindowSeconds > 300 {
		return fmt.Errorf("去重窗口必须在 1-300 秒之间")
	}

	if config.RetryWaitSeconds < 1 || config.RetryWaitSeconds > 300 {
		return fmt.Errorf("重试等待时间必须在 1-300 秒之间")
	}

	if config.NormalDegradeIntervalHours < 0.1 || config.NormalDegradeIntervalHours > 24 {
		return fmt.Errorf("正常降级间隔必须在 0.1-24 小时之间")
	}

	if config.ForgivenessHours < 0.5 || config.ForgivenessHours > 72 {
		return fmt.Errorf("宽恕触发时间必须在 0.5-72 小时之间")
	}

	if config.JumpPenaltyWindowHours < 0.1 || config.JumpPenaltyWindowHours > 24 {
		return fmt.Errorf("跳级惩罚窗口必须在 0.1-24 小时之间")
	}

	// 验证等级时长（必须递增）
	if config.L1DurationMinutes < 1 || config.L1DurationMinutes > 10080 {
		return fmt.Errorf("L1 拉黑时长必须在 1-10080 分钟之间")
	}
	if config.L2DurationMinutes <= config.L1DurationMinutes {
		return fmt.Errorf("L2 拉黑时长必须大于 L1")
	}
	if config.L3DurationMinutes <= config.L2DurationMinutes {
		return fmt.Errorf("L3 拉黑时长必须大于 L2")
	}
	if config.L4DurationMinutes <= config.L3DurationMinutes {
		return fmt.Errorf("L4 拉黑时长必须大于 L3")
	}
	if config.L5DurationMinutes <= config.L4DurationMinutes {
		return fmt.Errorf("L5 拉黑时长必须大于 L4")
	}

	if config.FallbackMode != "fixed" && config.FallbackMode != "none" {
		return fmt.Errorf("fallbackMode 只支持 'fixed' 或 'none'")
	}

	if config.FallbackDurationMinutes < 1 || config.FallbackDurationMinutes > 10080 {
		return fmt.Errorf("fallback 拉黑时长必须在 1-10080 分钟之间")
	}

	return nil
}
