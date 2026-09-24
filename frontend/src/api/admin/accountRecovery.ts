import { apiClient } from '../client'

export interface RecoveryGroup { id: string; label: string; aliases: string[]; top_n: number }
export interface RecoveryPolicy {
  version: number; enabled: boolean; site_host: string; cron: string; timezone: string
  max_targets: number; retention_days: number; max_runs: number; groups: RecoveryGroup[]
  balance_max_age_hours?: number
}
export interface RecoveryConfig { policy: RecoveryPolicy; revision: string; next_run_at: string | null }
export interface RecoveryPlan {
  revision: string; matched: number; selected: number; warnings: string[]
  groups: { id: string; label: string; matched: number; eligible: number; selected: number[]; top_n: number }[]
  accounts: { id: number; name: string; group: string; balance: string | null; action: string; reason?: string }[]
}
export interface RecoveryRun {
  id: number; trigger: string; status: string; started_at: string; finished_at: string | null; error?: string; warning?: string
  outcome: { enabled: number[]; disabled: number[]; errors: { id: number; stage: string; message: string }[]; plan?: RecoveryPlan }
}
const base = '/admin/agentrouter-recovery'
export const accountRecovery = {
  config: async () => (await apiClient.get<RecoveryConfig>(base)).data,
  history: async () => (await apiClient.get<RecoveryRun[]>(base + '/runs')).data,
  preview: async (policy: RecoveryPolicy) => (await apiClient.post<RecoveryPlan>(base + '/preview', policy)).data,
  save: async (policy: RecoveryPolicy, revision: string, preview_revision: string) =>
    (await apiClient.put<RecoveryConfig>(base, { policy, revision, preview_revision })).data,
  run: async (preview_revision: string, request_id: string) =>
    (await apiClient.post<RecoveryRun>(base + '/run', { preview_revision, request_id }, { timeout: 200_000 })).data
}
