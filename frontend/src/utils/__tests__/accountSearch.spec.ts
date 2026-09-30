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
  it.each([
    'primary', ' WINDHUB.EXAMPLE ', 'friendly', 'glm-5.3', 'legacy', '', ' \t\n\u3000 ',
    'primary windhub friendly glm-5.3 legacy',
    ' LEGACY\tGLM-5.3\nFRIENDLY\u0085WindHub\u00a0primary\u3000 ',
    'primary primary',
  ])('keeps accounts matching %j', keyword => {
    expect(matchesAccountSearch(account, keyword)).toBe(true)
  })

  it.each([
    'absent', 'secret-needle', 'notes-needle', '%', '_',
    'primary absent', 'primary secret-needle', 'primary notes-needle', 'primary %', 'primary _',
    'primarywindhub',
  ])('excludes unrelated fields and treats %j literally', keyword => {
    expect(matchesAccountSearch(account, keyword)).toBe(false)
  })

  it('handles accounts without model configuration', () => {
    expect(matchesAccountSearch({ name: 'empty', credentials: {} } as Account, 'glm')).toBe(false)
  })

  const accounts = [
    { name: 'agentrouter-linux-glm', credentials: { model_mapping: { glm: 'GLM-5' } } },
    { name: 'hyxware-linux-glm', credentials: { model_whitelist: ['GLM-5'] } },
    { name: 'agentrouter-linux-gpt', credentials: { model_mapping: { friendly: 'gpt-6-astra' } } },
    { name: 'hyxware-linux-gpt', credentials: { model_mapping: { 'astra-alias': 'target' } } },
    { name: 'other-gpt', credentials: { model_mapping: { friendly: 'gpt-6-sol' }, api_key: 'astra' } },
  ] as unknown as Account[]

  it.each([
    ['ag -glm', ['agentrouter-linux-glm']],
    ['hyx  -glm', ['hyxware-linux-glm']],
    ['-gpt astra', ['agentrouter-linux-gpt', 'hyxware-linux-gpt']],
    [' ASTRA\u3000-gpt ', ['agentrouter-linux-gpt', 'hyxware-linux-gpt']],
    ['ag -gpt astra', ['agentrouter-linux-gpt']],
    ['-glm astra', []],
    ['ag hyx', []],
  ])('matches all terms in %j across public fields', (search, names) => {
    expect(accounts.filter(account => matchesAccountSearch(account, search)).map(account => account.name)).toEqual(names)
  })

  it('does not search malformed or non-string model configuration', () => {
    const malformed = {
      name: 'other-gpt',
      credentials: { model_mapping: ['astra'], model_whitelist: [{ model: 'astra' }, 123] },
    } as unknown as Account
    expect(matchesAccountSearch(malformed, '-gpt astra')).toBe(false)
  })
})
