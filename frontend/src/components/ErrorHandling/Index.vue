<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import ListRow from '../Setting/ListRow.vue'
import {
  getErrorHandlingConfig,
  getErrorHandlingTodaySummary,
  updateErrorHandlingConfig,
  type ErrorHandlingConfig,
  type ErrorHandlingTodaySummary,
  type ErrorPolicyAction,
} from '../../services/errorHandling'
import { extractErrorMessage } from '../../utils/error'

const { t } = useI18n()
const router = useRouter()

type Tab = 'policies' | 'blacklist'

const emptyConfig = (): ErrorHandlingConfig => ({
  version: 1,
  capacity: { action: 'switch_provider' },
  http429: { action: 'switch_provider' },
  sharedRetryAttempts: 0,
  blacklist: {
    enabled: false,
    enableLevelBlacklist: false,
    failureThreshold: 3,
    dedupeWindowSeconds: 2,
    retryWaitSeconds: 3,
    normalDegradeIntervalHours: 1,
    forgivenessHours: 3,
    jumpPenaltyWindowHours: 2.5,
    l1DurationMinutes: 5,
    l2DurationMinutes: 15,
    l3DurationMinutes: 60,
    l4DurationMinutes: 360,
    l5DurationMinutes: 1440,
    fallbackMode: 'fixed',
    fallbackDurationMinutes: 30,
  },
})

const draft = reactive<ErrorHandlingConfig>(emptyConfig())
const summary = ref<ErrorHandlingTodaySummary | null>(null)
const activeTab = ref<Tab>('policies')
const loading = ref(true)
const saving = ref(false)
const notice = ref<{ kind: 'success' | 'error'; text: string } | null>(null)

const actionOptions = computed<Array<{ value: ErrorPolicyAction; label: string }>>(() => [
  { value: 'switch_provider', label: t('components.errorHandling.actions.switch') },
  { value: 'retry_then_switch_provider', label: t('components.errorHandling.actions.retrySwitch') },
  { value: 'pass_through', label: t('components.errorHandling.actions.pass') },
  { value: 'return_502', label: t('components.errorHandling.actions.return502') },
])

const fixedRules = computed(() => [
  { trigger: t('components.errorHandling.fixed.content'), route: t('components.errorHandling.fixed.noFailure'), terminal: '400' },
  { trigger: '401 / 403 / 404', route: t('components.errorHandling.fixed.providerFailure'), terminal: t('components.errorHandling.fixed.nextProvider') },
  { trigger: '408 / 421 / 5xx / transport', route: t('components.errorHandling.fixed.endpointFirst'), terminal: '502' },
  { trigger: t('components.errorHandling.fixed.clientDisconnect'), route: t('components.errorHandling.fixed.stopNoFailure'), terminal: '—' },
  { trigger: t('components.errorHandling.fixed.streamCommitted'), route: t('components.errorHandling.fixed.stopWithFailure'), terminal: t('components.errorHandling.fixed.keepStream') },
  { trigger: t('components.errorHandling.fixed.concurrency'), route: t('components.errorHandling.fixed.wait'), terminal: '503 · Retry-After' },
])

const summaryItems = computed(() => {
  const value = summary.value
  return [
    { key: 'capacity', label: 'Capacity', value: value?.capacity_hits ?? 0, tone: 'capacity' },
    { key: '429', label: 'HTTP 429', value: value?.http_429_hits ?? 0, tone: 'rate' },
    { key: 'retry', label: t('components.errorHandling.summary.retries'), value: value?.retry_actions ?? 0, tone: 'neutral' },
    { key: 'switch', label: t('components.errorHandling.summary.switches'), value: value?.provider_switch_actions ?? 0, tone: 'switch' },
    { key: 'pass', label: t('components.errorHandling.summary.passed'), value: value?.pass_through_requests ?? 0, tone: 'success' },
    { key: '502', label: t('components.errorHandling.summary.returned502'), value: value?.returned_502_requests ?? 0, tone: 'failure' },
  ]
})

function assignConfig(config: ErrorHandlingConfig) {
  Object.assign(draft, config)
  Object.assign(draft.capacity, config.capacity)
  Object.assign(draft.http429, config.http429)
  Object.assign(draft.blacklist, config.blacklist)
}

async function load() {
  loading.value = true
  notice.value = null
  try {
    const [config, today] = await Promise.all([
      getErrorHandlingConfig(),
      getErrorHandlingTodaySummary(),
    ])
    assignConfig(config)
    summary.value = today
  } catch (error) {
    notice.value = { kind: 'error', text: t('components.errorHandling.loadFailed') + extractErrorMessage(error) }
  } finally {
    loading.value = false
  }
}

function normalizeDraft() {
  draft.version = 1
  draft.sharedRetryAttempts = Math.min(5, Math.max(0, Math.floor(Number(draft.sharedRetryAttempts) || 0)))
  const blacklist = draft.blacklist
  blacklist.failureThreshold = Math.min(10, Math.max(1, Math.floor(Number(blacklist.failureThreshold) || 3)))
  blacklist.dedupeWindowSeconds = Math.min(300, Math.max(1, Math.floor(Number(blacklist.dedupeWindowSeconds) || 2)))
  blacklist.retryWaitSeconds = Math.min(300, Math.max(1, Math.floor(Number(blacklist.retryWaitSeconds) || 3)))
  blacklist.fallbackDurationMinutes = Math.min(10080, Math.max(1, Math.floor(Number(blacklist.fallbackDurationMinutes) || 30)))
  blacklist.normalDegradeIntervalHours = Number(blacklist.normalDegradeIntervalHours) || 1
  blacklist.forgivenessHours = Number(blacklist.forgivenessHours) || 3
  blacklist.jumpPenaltyWindowHours = Number(blacklist.jumpPenaltyWindowHours) || 2.5
  for (const key of ['l1DurationMinutes', 'l2DurationMinutes', 'l3DurationMinutes', 'l4DurationMinutes', 'l5DurationMinutes'] as const) {
    blacklist[key] = Math.min(10080, Math.max(1, Math.floor(Number(blacklist[key]) || 1)))
  }
}

async function save() {
  if (saving.value || loading.value) return
  saving.value = true
  notice.value = null
  normalizeDraft()
  try {
    const saved = await updateErrorHandlingConfig(JSON.parse(JSON.stringify(draft)) as ErrorHandlingConfig)
    assignConfig(saved)
    summary.value = await getErrorHandlingTodaySummary()
    notice.value = { kind: 'success', text: t('components.errorHandling.saved') }
  } catch (error) {
    notice.value = { kind: 'error', text: t('components.errorHandling.saveFailed') + extractErrorMessage(error) }
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="main-shell error-handling-shell">
    <header class="app-page-header">
      <div class="app-page-title-group">
        <h1 class="app-page-title">{{ t('components.errorHandling.title') }}</h1>
        <p class="app-page-subtitle">{{ t('components.errorHandling.subtitle') }}</p>
      </div>
      <div class="app-page-actions">
        <button class="save-action" type="button" :disabled="loading || saving" @click="save">
          {{ saving ? t('common.saving') : t('common.save') }}
        </button>
      </div>
    </header>

    <main class="app-page-container error-handling-page" :aria-busy="loading">
      <section class="today-band" :aria-label="t('components.errorHandling.summary.title')">
        <div class="today-heading">
          <span>{{ t('components.errorHandling.summary.title') }}</span>
          <small>{{ summary?.timezone || '—' }}</small>
        </div>
        <div class="today-metrics">
          <div v-for="item in summaryItems" :key="item.key" :class="['today-metric', `tone-${item.tone}`]">
            <strong>{{ item.value }}</strong>
            <span>{{ item.label }}</span>
          </div>
        </div>
        <button class="events-link" type="button" @click="router.push('/events')">
          {{ t('components.errorHandling.summary.details') }}
          <span aria-hidden="true">›</span>
        </button>
      </section>

      <p v-if="notice" :class="['handling-notice', notice.kind]">{{ notice.text }}</p>
      <p v-if="draft.warning" class="handling-notice warning">{{ draft.warning }}</p>

      <div class="handling-tabs" role="tablist" :aria-label="t('components.errorHandling.tabs.label')">
        <button
          type="button"
          role="tab"
          :aria-selected="activeTab === 'policies'"
          :class="{ active: activeTab === 'policies' }"
          @click="activeTab = 'policies'"
        >
          {{ t('components.errorHandling.tabs.policies') }}
        </button>
        <button
          type="button"
          role="tab"
          :aria-selected="activeTab === 'blacklist'"
          :class="{ active: activeTab === 'blacklist' }"
          @click="activeTab = 'blacklist'"
        >
          {{ t('components.errorHandling.tabs.blacklist') }}
        </button>
      </div>

      <section v-if="activeTab === 'policies'" class="tab-panel" role="tabpanel">
        <div class="section-heading">
          <h2>{{ t('components.errorHandling.policy.title') }}</h2>
          <span>{{ t('components.errorHandling.policy.scope') }}</span>
        </div>

        <div class="policy-list">
          <article class="policy-row policy-row-capacity">
            <div class="policy-identity">
              <span class="policy-code">MODEL CAPACITY</span>
              <strong>{{ t('components.errorHandling.policy.capacity') }}</strong>
            </div>
            <div class="route-rail" aria-hidden="true">
              <span>{{ t('components.errorHandling.policy.endpointPool') }}</span><i>→</i>
              <span>{{ t('components.errorHandling.policy.providerPolicy') }}</span><i>→</i>
              <span>{{ actionOptions.find((item) => item.value === draft.capacity.action)?.label }}</span>
            </div>
            <label class="policy-select-field">
              <span>{{ t('components.errorHandling.policy.action') }}</span>
              <select v-model="draft.capacity.action">
                <option v-for="option in actionOptions" :key="option.value" :value="option.value">{{ option.label }}</option>
              </select>
            </label>
          </article>

          <article class="policy-row policy-row-rate">
            <div class="policy-identity">
              <span class="policy-code">HTTP 429</span>
              <strong>{{ t('components.errorHandling.policy.rateLimit') }}</strong>
            </div>
            <div class="route-rail" aria-hidden="true">
              <span>{{ t('components.errorHandling.policy.endpointPool') }}</span><i>→</i>
              <span>{{ t('components.errorHandling.policy.providerPolicy') }}</span><i>→</i>
              <span>{{ actionOptions.find((item) => item.value === draft.http429.action)?.label }}</span>
            </div>
            <label class="policy-select-field">
              <span>{{ t('components.errorHandling.policy.action') }}</span>
              <select v-model="draft.http429.action">
                <option v-for="option in actionOptions" :key="option.value" :value="option.value">{{ option.label }}</option>
              </select>
            </label>
          </article>
        </div>

        <section class="settings-band">
          <h3>{{ t('components.errorHandling.retry.title') }}</h3>
          <ListRow :label="t('components.errorHandling.retry.sharedBudget')" :sub-label="t('components.errorHandling.retry.sharedBudgetHint')">
            <div class="number-field"><input v-model.number="draft.sharedRetryAttempts" type="number" min="0" max="5"><span>0–5</span></div>
          </ListRow>
          <div class="fixed-limits">
            <span>Retry-After ≤ 60s</span>
            <span>Full jitter ≤ 30s</span>
            <span>{{ t('components.errorHandling.retry.addressFirst') }}</span>
          </div>
        </section>

        <section class="fixed-matrix">
          <div class="section-heading">
            <h2>{{ t('components.errorHandling.fixed.title') }}</h2>
            <span>{{ t('components.errorHandling.fixed.readOnly') }}</span>
          </div>
          <div class="matrix-header" aria-hidden="true">
            <span>{{ t('components.errorHandling.fixed.trigger') }}</span>
            <span>{{ t('components.errorHandling.fixed.route') }}</span>
            <span>{{ t('components.errorHandling.fixed.terminal') }}</span>
          </div>
          <div v-for="rule in fixedRules" :key="rule.trigger" class="matrix-row">
            <strong>{{ rule.trigger }}</strong>
            <span>{{ rule.route }}</span>
            <code>{{ rule.terminal }}</code>
          </div>
        </section>
      </section>

      <section v-else class="tab-panel" role="tabpanel">
        <div class="section-heading">
          <h2>{{ t('components.errorHandling.blacklist.title') }}</h2>
          <span>{{ t('components.errorHandling.blacklist.scope') }}</span>
        </div>

        <section class="settings-band">
          <h3>{{ t('components.errorHandling.blacklist.basic') }}</h3>
          <ListRow :label="t('components.general.label.enableBlacklist')">
            <label class="mac-switch"><input v-model="draft.blacklist.enabled" type="checkbox"><span /></label>
          </ListRow>
          <ListRow :label="t('components.general.label.enableLevelBlacklist')">
            <label class="mac-switch"><input v-model="draft.blacklist.enableLevelBlacklist" type="checkbox"><span /></label>
          </ListRow>
          <ListRow :label="t('components.general.label.blacklistThreshold')">
            <div class="number-field"><input v-model.number="draft.blacklist.failureThreshold" type="number" min="1" max="10"><span>{{ t('components.general.label.times') }}</span></div>
          </ListRow>
          <ListRow :label="t('components.errorHandling.blacklist.fallbackMode')">
            <select v-model="draft.blacklist.fallbackMode" class="compact-select">
              <option value="fixed">{{ t('components.errorHandling.blacklist.fixed') }}</option>
              <option value="none">{{ t('components.errorHandling.blacklist.none') }}</option>
            </select>
          </ListRow>
          <ListRow v-if="draft.blacklist.fallbackMode === 'fixed'" :label="t('components.general.label.blacklistDuration')">
            <div class="number-field"><input v-model.number="draft.blacklist.fallbackDurationMinutes" type="number" min="1" max="10080"><span>{{ t('components.general.label.minutes') }}</span></div>
          </ListRow>
        </section>

        <section class="settings-band">
          <h3>{{ t('components.errorHandling.blacklist.retryAndDedupe') }}</h3>
          <ListRow :label="t('components.errorHandling.blacklist.dedupe')">
            <div class="number-field"><input v-model.number="draft.blacklist.dedupeWindowSeconds" type="number" min="1" max="300"><span>s</span></div>
          </ListRow>
          <ListRow :label="t('components.errorHandling.blacklist.retryWait')">
            <div class="number-field"><input v-model.number="draft.blacklist.retryWaitSeconds" type="number" min="1" max="300"><span>s</span></div>
          </ListRow>
        </section>

        <section class="settings-band">
          <h3>{{ t('components.errorHandling.blacklist.levelDurations') }}</h3>
          <div class="duration-grid">
            <label v-for="level in 5" :key="level">
              <span>L{{ level }}</span>
              <input v-model.number="draft.blacklist[`l${level}DurationMinutes` as keyof typeof draft.blacklist]" type="number" min="1" max="10080">
              <small>min</small>
            </label>
          </div>
          <ListRow :label="t('components.errorHandling.blacklist.degradeInterval')">
            <div class="number-field"><input v-model.number="draft.blacklist.normalDegradeIntervalHours" type="number" min="0.1" max="24" step="0.1"><span>h</span></div>
          </ListRow>
          <ListRow :label="t('components.errorHandling.blacklist.forgiveness')">
            <div class="number-field"><input v-model.number="draft.blacklist.forgivenessHours" type="number" min="0.5" max="72" step="0.5"><span>h</span></div>
          </ListRow>
          <ListRow :label="t('components.errorHandling.blacklist.jumpWindow')">
            <div class="number-field"><input v-model.number="draft.blacklist.jumpPenaltyWindowHours" type="number" min="0.1" max="24" step="0.1"><span>h</span></div>
          </ListRow>
        </section>
      </section>
    </main>
  </div>
</template>

<style scoped>
.error-handling-page {
  width: min(1080px, 100%);
  min-width: 0;
  margin: 0 auto;
  padding-bottom: 48px;
}

.save-action {
  min-height: 34px;
  border: 1px solid #0a84ff;
  border-radius: 6px;
  padding: 0 16px;
  color: #fff;
  background: #0a84ff;
  cursor: pointer;
}

.save-action:disabled { opacity: 0.55; cursor: not-allowed; }

.today-band {
  display: grid;
  grid-template-columns: 120px minmax(0, 1fr) auto;
  align-items: stretch;
  margin-top: 18px;
  border: 1px solid var(--mac-border);
  border-radius: 8px;
  background: var(--mac-surface);
  overflow: hidden;
}

.today-heading,
.events-link {
  display: flex;
  flex-direction: column;
  justify-content: center;
  padding: 14px 16px;
}

.today-heading { border-right: 1px solid var(--mac-divider); }
.today-heading span { font-size: 0.8rem; font-weight: 700; color: var(--mac-text); }
.today-heading small { margin-top: 4px; color: var(--mac-text-secondary); font-size: 0.68rem; }

.today-metrics { display: grid; grid-template-columns: repeat(6, minmax(72px, 1fr)); }
.today-metric { position: relative; display: flex; min-width: 0; flex-direction: column; justify-content: center; padding: 12px 14px; border-right: 1px solid var(--mac-divider); }
.today-metric::before { position: absolute; top: 11px; bottom: 11px; left: 0; width: 2px; background: var(--metric-color); content: ''; }
.today-metric strong { font-variant-numeric: tabular-nums; font-size: 1.25rem; line-height: 1; }
.today-metric span { margin-top: 6px; overflow: hidden; color: var(--mac-text-secondary); font-size: 0.68rem; text-overflow: ellipsis; white-space: nowrap; }
.tone-capacity { --metric-color: #d68b00; }
.tone-rate { --metric-color: #2383d8; }
.tone-neutral { --metric-color: #7c8291; }
.tone-switch { --metric-color: #7657c8; }
.tone-success { --metric-color: #2b9a66; }
.tone-failure { --metric-color: #d64b4b; }

.events-link { align-items: center; flex-direction: row; gap: 6px; border: 0; color: var(--mac-accent); background: transparent; cursor: pointer; font-size: 0.78rem; white-space: nowrap; }
.events-link span { font-size: 1.1rem; }

.handling-notice { margin: 14px 0 0; border-left: 3px solid currentColor; padding: 10px 12px; font-size: 0.82rem; }
.handling-notice.success { color: #16824c; background: rgba(43, 154, 102, 0.09); }
.handling-notice.error { color: #cf3e3e; background: rgba(214, 75, 75, 0.09); }
.handling-notice.warning { color: #a96b00; background: rgba(214, 139, 0, 0.09); }

.handling-tabs { display: inline-grid; grid-template-columns: 1fr 1fr; margin-top: 18px; border: 1px solid var(--mac-border); border-radius: 7px; padding: 3px; background: var(--mac-surface-strong); }
.handling-tabs button { min-width: 140px; min-height: 32px; border: 0; border-radius: 5px; padding: 0 16px; color: var(--mac-text-secondary); background: transparent; cursor: pointer; }
.handling-tabs button.active { color: var(--mac-text); background: var(--mac-surface); box-shadow: 0 1px 3px rgba(15, 23, 42, 0.12); }

.tab-panel { padding-top: 20px; }
.section-heading { display: flex; align-items: baseline; justify-content: space-between; gap: 16px; margin-bottom: 9px; }
.section-heading h2 { margin: 0; font-size: 0.92rem; }
.section-heading span { color: var(--mac-text-secondary); font-size: 0.72rem; }

.policy-list { border-top: 1px solid var(--mac-divider); }
.policy-row { position: relative; display: grid; grid-template-columns: 190px minmax(260px, 1fr) 210px; align-items: center; gap: 20px; min-height: 92px; border-bottom: 1px solid var(--mac-divider); padding: 14px 16px 14px 20px; background: color-mix(in srgb, var(--mac-surface) 72%, transparent); }
.policy-row::before { position: absolute; top: 16px; bottom: 16px; left: 0; width: 3px; background: var(--rule-color); content: ''; }
.policy-row-capacity { --rule-color: #d68b00; }
.policy-row-rate { --rule-color: #2383d8; }
.policy-identity { display: flex; min-width: 0; flex-direction: column; gap: 5px; }
.policy-code { color: var(--rule-color); font: 700 0.68rem/1.2 ui-monospace, SFMono-Regular, Menlo, monospace; }
.policy-identity strong { font-size: 0.88rem; }

.route-rail { display: flex; min-width: 0; align-items: center; gap: 8px; color: var(--mac-text-secondary); font-size: 0.72rem; }
.route-rail span { min-width: 0; border: 1px solid var(--mac-border); border-radius: 5px; padding: 6px 8px; overflow: hidden; background: var(--mac-surface); text-overflow: ellipsis; white-space: nowrap; }
.route-rail i { color: var(--rule-color); font-style: normal; }

.policy-select-field { display: flex; flex-direction: column; gap: 5px; }
.policy-select-field > span { color: var(--mac-text-secondary); font-size: 0.68rem; }
.policy-select-field select,
.compact-select { min-height: 34px; border: 1px solid var(--mac-border); border-radius: 6px; padding: 0 30px 0 9px; color: var(--mac-text); background: var(--mac-surface); }

.settings-band { margin-top: 26px; border-top: 1px solid var(--mac-divider); border-bottom: 1px solid var(--mac-divider); }
.settings-band h3 { margin: 0; padding: 12px 18px 6px; color: var(--mac-text-secondary); font-size: 0.72rem; text-transform: uppercase; }
.fixed-limits { display: flex; flex-wrap: wrap; gap: 8px 18px; border-top: 1px solid var(--mac-divider); padding: 10px 18px; color: var(--mac-text-secondary); font: 0.7rem ui-monospace, SFMono-Regular, Menlo, monospace; }

.number-field { display: flex; align-items: center; gap: 8px; }
.number-field input { width: 92px; min-height: 34px; border: 1px solid var(--mac-border); border-radius: 6px; padding: 0 9px; color: var(--mac-text); background: var(--mac-surface); }
.number-field span { color: var(--mac-text-secondary); font-size: 0.75rem; }

.fixed-matrix { margin-top: 26px; }
.matrix-header,
.matrix-row { display: grid; grid-template-columns: 1.1fr 1.5fr minmax(110px, 0.7fr); gap: 16px; align-items: center; border-bottom: 1px solid var(--mac-divider); padding: 10px 14px; }
.matrix-header { border-top: 1px solid var(--mac-divider); color: var(--mac-text-secondary); font-size: 0.68rem; text-transform: uppercase; }
.matrix-row { min-height: 46px; font-size: 0.76rem; }
.matrix-row strong { font-size: 0.76rem; }
.matrix-row > span { color: var(--mac-text-secondary); }
.matrix-row code { justify-self: start; color: var(--mac-text); font-size: 0.7rem; }

.duration-grid { display: grid; grid-template-columns: repeat(5, minmax(100px, 1fr)); gap: 10px; padding: 12px 18px; }
.duration-grid label { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: center; gap: 6px; }
.duration-grid label > span { color: var(--mac-text-secondary); font: 700 0.7rem ui-monospace, monospace; }
.duration-grid input { width: 100%; min-width: 0; min-height: 32px; border: 1px solid var(--mac-border); border-radius: 5px; padding: 0 7px; color: var(--mac-text); background: var(--mac-surface); }
.duration-grid small { color: var(--mac-text-secondary); font-size: 0.65rem; }

@media (max-width: 900px) {
  .today-band { grid-template-columns: 100px minmax(0, 1fr); }
  .events-link { grid-column: 1 / -1; justify-content: flex-end; border-top: 1px solid var(--mac-divider); padding: 8px 14px; }
  .today-metrics { grid-template-columns: repeat(3, 1fr); }
  .today-metric:nth-child(3) { border-right: 0; }
  .today-metric:nth-child(-n + 3) { border-bottom: 1px solid var(--mac-divider); }
  .policy-row { grid-template-columns: 160px minmax(220px, 1fr); }
  .policy-select-field { grid-column: 1 / -1; }
  .duration-grid { grid-template-columns: repeat(3, minmax(100px, 1fr)); }
}

@media (max-width: 680px) {
  .today-band { grid-template-columns: 1fr; margin-top: 12px; }
  .today-heading { flex-direction: row; align-items: center; justify-content: space-between; border-right: 0; border-bottom: 1px solid var(--mac-divider); padding: 10px 12px; }
  .today-heading small { margin-top: 0; }
  .today-metrics { grid-template-columns: repeat(2, 1fr); }
  .today-metric { min-height: 54px; border-bottom: 1px solid var(--mac-divider); }
  .today-metric:nth-child(odd) { border-right: 1px solid var(--mac-divider); }
  .today-metric:nth-child(even) { border-right: 0; }
  .handling-tabs { display: grid; width: 100%; }
  .handling-tabs button { min-width: 0; }
  .section-heading { align-items: flex-start; flex-direction: column; gap: 3px; }
  .policy-row { grid-template-columns: 1fr; gap: 12px; padding-right: 10px; }
  .route-rail { flex-wrap: wrap; }
  .route-rail span { white-space: normal; }
  .matrix-header { display: none; }
  .matrix-row { grid-template-columns: 1fr auto; gap: 6px 12px; padding: 11px 8px; }
  .matrix-row > span { grid-column: 1 / -1; grid-row: 2; }
  .matrix-row code { grid-column: 2; grid-row: 1; }
  .duration-grid { grid-template-columns: 1fr 1fr; padding-right: 14px; padding-left: 14px; }
}
</style>
