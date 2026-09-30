import type { Account } from '@/types'

export function matchesAccountSearch(account: Account, search: string): boolean {
  const keyword = search.trim().toLowerCase()
  if (!keyword) return true

  const credentials = account.credentials ?? {}
  const values: unknown[] = [account.name, credentials.base_url]
  const mapping = credentials.model_mapping
  if (mapping && typeof mapping === 'object' && !Array.isArray(mapping)) {
    values.push(...Object.keys(mapping), ...Object.values(mapping))
  }
  if (Array.isArray(credentials.model_whitelist)) {
    values.push(...credentials.model_whitelist)
  }
  return values.some(value => typeof value === 'string' && value.toLowerCase().includes(keyword))
}
