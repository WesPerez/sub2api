import { describe, expect, it } from 'vitest'
import type { Account } from '@/types'
import { matchesAccountSearch } from '../accountSearch'

const account = {
  name: 'Primary account',
  credentials: {
    base_url: 'https://WindHub.example/v1',
    model_mapping: { 'friendly-alias': 'GLM-5.3' },
    model_whitelist: ['legacy-model'],
    api_key: 'secret-needle',
  },
  notes: 'notes-needle',
} as unknown as Account

describe('account search used when refreshing a row', () => {
  it.each(['primary', ' WINDHUB.EXAMPLE ', 'friendly', 'glm-5.3', 'legacy', ''])('keeps accounts matching %j', keyword => {
    expect(matchesAccountSearch(account, keyword)).toBe(true)
  })

  it.each(['absent', 'secret-needle', 'notes-needle', '%', '_'])('excludes unrelated fields and treats %j literally', keyword => {
    expect(matchesAccountSearch(account, keyword)).toBe(false)
  })

  it('handles accounts without model configuration', () => {
    expect(matchesAccountSearch({ name: 'empty', credentials: {} } as Account, 'glm')).toBe(false)
  })
})
