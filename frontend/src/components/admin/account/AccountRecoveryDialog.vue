<template>
  <BaseDialog :show="show" title="AgentRouter · 定时开放账号" width="extra-wide" @close="close">
    <div class="recovery-panel">
      <p class="text-sm text-gray-500">每次按已同步的余额从高到低选取各类账号。余额过期或账号身份变化时保持原开关，等待刷新。更改配置前可核对实际账号。</p>
      <p v-if="error" role="alert" class="rounded-lg bg-red-50 p-3 text-red-700">{{ error }}</p>
      <p v-if="notice" role="status" class="rounded-lg bg-green-50 p-3 text-green-700">{{ notice }}</p>
      <p v-if="loading">正在读取计划与执行记录…</p>
      <template v-if="draft && saved">
        <div class="recovery-summary">
          <div><small>自动执行</small><strong>{{ saved.policy.enabled ? '已开启' : '已暂停' }}</strong></div>
          <div><small>下次执行</small><strong>{{ saved.next_run_at ? at(saved.next_run_at) : '等待开启' }}</strong></div>
          <div><small>每轮计划开放</small><strong>{{ draft.groups.reduce((n, g) => n + g.top_n, 0) }} 个</strong></div>
        </div>
        <fieldset :disabled="busy" class="space-y-4">
          <label class="flex items-center gap-2"><input v-model="draft.enabled" type="checkbox">开启自动执行</label>
          <div class="recovery-grid">
            <label>执行计划<select v-model="scheduleMode" class="input" @change="setScheduleMode"><option value="hourly">每小时整点</option><option value="daily">每天凌晨</option><option value="custom">自定义计划</option></select></label>
            <label>时区<select v-model="draft.timezone" class="input"><option>Asia/Shanghai</option><option>UTC</option></select></label>
          </div>
          <label v-if="scheduleMode === 'custom'" class="block">Cron 表达式<input v-model="draft.cron" class="input" maxlength="100"><small>五段格式，例如 15 * * * * 表示每小时第 15 分钟。</small></label>
          <p class="text-sm text-gray-500">站点：agentrouter.org。账号类型按名称最后一个连字符后的后缀识别；未归类账号保持原状态。</p>
          <div v-for="(group, index) in draft.groups" :key="group.id" class="recovery-group">
            <div class="recovery-grid">
              <label>显示名称<input v-model="group.label" class="input" maxlength="40"></label>
              <label>开放数量<input v-model.number="group.top_n" class="input" type="number" min="1" max="20"></label>
            </div>
            <label class="block">匹配后缀<input :value="group.aliases.join(', ')" class="input" @change="aliases(group, $event)"><small>多个别名用逗号分隔。改名时可同时保留旧后缀，例如 deepseek, ds。</small></label>
            <button v-if="draft.groups.length > 1" type="button" class="text-sm text-red-600" @click="draft.groups.splice(index, 1)">移除此类型</button>
          </div>
          <button v-if="draft.groups.length < 12" class="btn btn-secondary" @click="addGroup">添加账号类型</button>
          <details><summary>记录保留与匹配保护</summary><div class="recovery-grid mt-3">
            <label>最大匹配账号数<input v-model.number="draft.max_targets" class="input" type="number" min="1" max="500"></label>
            <label>余额有效期（小时）<input v-model.number="draft.balance_max_age_hours" class="input" type="number" min="1" max="168"><small>仅在此有效期内使用结构化余额；尚未接入的账号暂沿用旧备注。</small></label>
            <label>记录保留天数<input v-model.number="draft.retention_days" class="input" type="number" min="1" max="180"></label>
            <label>最多保留记录<input v-model.number="draft.max_runs" class="input" type="number" min="20" max="5000"></label>
          </div></details>
        </fieldset>
        <section v-if="plan" class="space-y-3" aria-label="账号匹配预览">
          <h4 class="font-semibold">匹配 {{ plan.matched }} 个，计划开放 {{ plan.selected }} 个</h4>
          <p v-for="warning in plan.warnings" :key="warning" class="text-sm text-amber-700">{{ warning }}</p>
          <details v-for="group in plan.groups" :key="group.id" class="recovery-group">
            <summary>{{ group.label }} · {{ group.matched }} 个匹配 · {{ group.selected.length }} 个入选</summary>
            <ul class="mt-3 space-y-2 text-sm"><li v-for="account in plan.accounts.filter(a => a.group === group.id)" :key="account.id">
              {{ account.name }}（{{ account.id }}） · {{ account.balance === null ? '余额未识别' : '$' + account.balance }} · {{ actionText[account.action] || account.action }}<span v-if="account.reason"> · {{ account.reason }}</span>
            </li></ul>
          </details>
        </section>
        <section aria-label="最近执行记录" class="space-y-3">
          <div class="flex items-center justify-between"><h4 class="font-semibold">最近执行</h4><button class="text-sm text-primary-600" :disabled="busy" @click="refreshHistory">刷新记录</button></div>
          <p v-if="!runs.length" class="text-sm text-gray-500">尚无应用内执行记录，开启后将在下次计划执行。</p>
          <details v-for="run in runs" :key="run.id" class="recovery-group">
            <summary>{{ at(run.started_at) }} · {{ statusText[run.status] || run.status }} · {{ run.trigger === 'manual' ? '手动' : '定时' }}</summary>
            <p class="mt-3 text-sm">开放 {{ run.outcome.enabled?.length || 0 }} 个，关闭 {{ run.outcome.disabled?.length || 0 }} 个。</p>
            <p v-if="run.finished_at" class="text-xs text-gray-500">耗时 {{ Math.max(0, Math.round((Date.parse(run.finished_at) - Date.parse(run.started_at)) / 1000)) }} 秒</p>
            <p v-if="run.error" class="text-red-600">{{ run.error }}</p>
            <p v-for="item in run.outcome.errors || []" :key="item.id + item.stage" class="text-sm text-amber-700">账号 {{ item.id }}：{{ item.message }}</p>
            <p v-for="warning in run.outcome.plan?.warnings || []" :key="warning" class="text-sm text-amber-700">{{ warning }}</p>
          </details>
        </section>
      </template>
    </div>
    <template #footer>
      <div class="flex flex-wrap items-center justify-between gap-3">
        <span class="text-xs text-gray-500">{{ dirty ? '有未保存修改' : '手动执行使用已保存配置' }}</span>
        <div class="flex flex-wrap gap-2">
          <button class="btn btn-secondary" :disabled="busy || !draft" @click="preview">核对账号</button>
          <button class="btn btn-secondary" :disabled="busy || dirty || !plan" @click="confirmRun = true">立即执行一次</button>
          <button class="btn btn-primary" :disabled="busy || !dirty || !plan" @click="save">{{ busy ? '正在处理…' : '保存配置' }}</button>
        </div>
      </div>
    </template>
  </BaseDialog>
  <BaseDialog :show="confirmDiscard" title="保留还是放弃修改" width="narrow" :z-index="60" @close="confirmDiscard = false">
    <p>关闭后将放弃尚未保存的配置。</p><template #footer><button class="btn btn-secondary" @click="confirmDiscard = false">继续编辑</button><button class="btn btn-danger" @click="confirmDiscard = false; emit('close')">放弃并关闭</button></template>
  </BaseDialog>
  <BaseDialog :show="confirmRun" title="确认执行账号恢复" width="narrow" :z-index="60" @close="confirmRun = false">
    <p>将按刚才核对的结果恢复并开放 {{ plan?.selected }} 个账号，同类其他候选关闭调度。不修改平台、分组和消费记录。</p><template #footer><button class="btn btn-secondary" @click="confirmRun = false">取消</button><button class="btn btn-primary" @click="runNow">确认执行</button></template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { accountRecovery, type RecoveryConfig, type RecoveryPolicy, type RecoveryPlan, type RecoveryRun, type RecoveryGroup } from '@/api/admin/accountRecovery'
const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ (e: 'close'): void }>()
const saved = ref<RecoveryConfig | null>(null), draft = ref<RecoveryPolicy | null>(null), plan = ref<RecoveryPlan | null>(null)
const runs = ref<RecoveryRun[]>([]), loading = ref(false), busy = ref(false), error = ref(''), notice = ref('')
const confirmDiscard = ref(false), confirmRun = ref(false), scheduleMode = ref('hourly')
const dirty = computed(() => !!draft.value && JSON.stringify(draft.value) !== JSON.stringify(saved.value?.policy))
const actionText: Record<string, string> = { enable: '开放', disable: '关闭调度', preserve: '保持原样' }
const statusText: Record<string, string> = { running: '执行中', success: '成功', partial: '部分完成', failed: '失败', interrupted: '已中断' }
const at = (value: string) => new Date(value).toLocaleString('zh-CN', { timeZone: saved.value?.policy.timezone || 'Asia/Shanghai', hour12: false })
function apply(value: RecoveryConfig) { saved.value = value; draft.value = structuredClone(value.policy); scheduleMode.value = value.policy.cron === '0 * * * *' ? 'hourly' : value.policy.cron === '0 0 * * *' ? 'daily' : 'custom' }
function close() { if (dirty.value) confirmDiscard.value = true; else emit('close') }
function aliases(group: RecoveryGroup, event: Event) { group.aliases = (event.target as HTMLInputElement).value.split(/[,，]/).map(s => s.trim()).filter(Boolean) }
function addGroup() { draft.value?.groups.push({ id: 'type_' + crypto.randomUUID().replace(/-/g, '').slice(0, 12), label: '新类型', aliases: [], top_n: 3 }) }
function setScheduleMode() { if (draft.value && scheduleMode.value !== 'custom') draft.value.cron = scheduleMode.value === 'hourly' ? '0 * * * *' : '0 0 * * *' }
async function perform(fn: () => Promise<void>) {
  if (busy.value) return
  busy.value = true; error.value = ''; notice.value = ''
  try { await fn() } catch (caught) {
    const detail = (caught as { response?: { data?: { message?: unknown } } })?.response?.data?.message
    error.value = typeof detail === 'string' && detail.length < 300 ? detail : '操作未完成；如果已经执行，请先刷新记录确认结果。'
  } finally { busy.value = false }
}
async function refreshHistory() { await perform(async () => { runs.value = await accountRecovery.history() }) }
async function preview() { await perform(async () => { if (draft.value) plan.value = await accountRecovery.preview(draft.value) }) }
async function save() { await perform(async () => { if (!draft.value || !saved.value || !plan.value) return; apply(await accountRecovery.save(draft.value, saved.value.revision, plan.value.revision)); plan.value = null; notice.value = '配置已保存，下次计划生效。' }) }
async function runNow() {
  confirmRun.value = false
  await perform(async () => {
    if (!plan.value || dirty.value) return
    const run = await accountRecovery.run(plan.value.revision, crypto.randomUUID())
    plan.value = null
    runs.value = [run, ...runs.value.filter(item => item.id !== run.id)]
    if (run.warning) { error.value = run.warning; return }
    notice.value = statusText[run.status] || run.status
    try { runs.value = await accountRecovery.history() } catch { error.value = '执行已结束，但记录刷新失败；请稍后刷新，不要重复执行。' }
  })
}
watch(draft, () => { plan.value = null }, { deep: true, flush: 'sync' })
watch(() => props.show, async show => { if (!show) return; loading.value = true; await perform(async () => { const [config, history] = await Promise.all([accountRecovery.config(), accountRecovery.history()]); apply(config); runs.value = history }); loading.value = false })
</script>

<style scoped>
.recovery-panel { display: grid; gap: 1.25rem; }
.recovery-summary { display:grid; grid-template-columns:repeat(3,minmax(0,1fr)); gap:1rem; padding:1rem; border-radius:1rem; background:rgb(99 102 241 / .07); }
.recovery-summary small,.recovery-summary strong { display:block; } .recovery-summary small { opacity:.65; margin-bottom:.4rem; } .recovery-summary strong { font-size:1rem; }
.recovery-grid { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:1rem; }
.recovery-group { border:1px solid rgb(128 128 128 / .25); padding:1rem; border-radius:.75rem; display:grid; gap:.75rem; }
label,summary { font-size:.875rem; } small { display:block; font-size:.75rem; opacity:.7; margin-top:.35rem; } summary { cursor:pointer; }
@media(max-width:600px) { .recovery-summary,.recovery-grid { grid-template-columns:1fr; } }
</style>
