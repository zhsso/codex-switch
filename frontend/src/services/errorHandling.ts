import { Call } from '../runtime'

export type ErrorPolicyAction =
  | 'pass_through'
  | 'return_502'
  | 'switch_provider'
  | 'retry_then_switch_provider'

export type ErrorPolicyConfig = {
  action: ErrorPolicyAction
}

export type ErrorHandlingBlacklistConfig = {
  enabled: boolean
  enableLevelBlacklist: boolean
  failureThreshold: number
  dedupeWindowSeconds: number
  retryWaitSeconds: number
  normalDegradeIntervalHours: number
  forgivenessHours: number
  jumpPenaltyWindowHours: number
  l1DurationMinutes: number
  l2DurationMinutes: number
  l3DurationMinutes: number
  l4DurationMinutes: number
  l5DurationMinutes: number
  fallbackMode: 'fixed' | 'none'
  fallbackDurationMinutes: number
}

export type ErrorHandlingConfig = {
  version: number
  capacity: ErrorPolicyConfig
  http429: ErrorPolicyConfig
  sharedRetryAttempts: number
  blacklist: ErrorHandlingBlacklistConfig
  warning?: string
}

export type ErrorHandlingTodaySummary = {
  timezone: string
  start_utc: string
  end_utc: string
  capacity_hits: number
  http_429_hits: number
  retry_actions: number
  provider_switch_actions: number
  pass_through_requests: number
  returned_502_requests: number
}

const SETTINGS_SERVICE = 'codeswitch/services.SettingsService'
const LOG_SERVICE = 'codeswitch/services.LogService'

export const getErrorHandlingConfig = async (): Promise<ErrorHandlingConfig> => {
  return Call.ByName(`${SETTINGS_SERVICE}.GetErrorHandlingConfig`)
}

export const updateErrorHandlingConfig = async (config: ErrorHandlingConfig): Promise<ErrorHandlingConfig> => {
  return Call.ByName(`${SETTINGS_SERVICE}.UpdateErrorHandlingConfig`, config)
}

export const getErrorHandlingTodaySummary = async (): Promise<ErrorHandlingTodaySummary> => {
  return Call.ByName(`${LOG_SERVICE}.GetErrorHandlingTodaySummary`)
}
