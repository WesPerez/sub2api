import type { Account } from '@/types'

export function matchesAccountSearch(account: Account, search: string): boolean {
  // Match Go's strings.Fields: each Unicode whitespace-separated term must
  // occur in at least one public field, independently of the other terms.
  const terms = search.toLowerCase().split(/\p{White_Space}+/u).filter(Boolean)
  if (!terms.length) return true

  const credentials = account.credentials ?? {}
  const values: unknown[] = [account.name, credentials.base_url]
  const mapping = credentials.model_mapping
  if (mapping && typeof mapping === 'object' && !Array.isArray(mapping)) {
    values.push(...Object.keys(mapping), ...Object.values(mapping))
  }
  if (Array.isArray(credentials.model_whitelist)) {
    values.push(...credentials.model_whitelist)
  }
  const fields = values.filter((value): value is string => typeof value === 'string')
    .map(value => value.toLowerCase())
  return terms.every(term => fields.some(value => value.includes(term)))
}
