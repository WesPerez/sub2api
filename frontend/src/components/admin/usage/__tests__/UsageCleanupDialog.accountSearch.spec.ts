import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import UsageCleanupDialog from '../UsageCleanupDialog.vue'

const { createCleanupTask, showError } = vi.hoisted(() => ({
  createCleanupTask: vi.fn().mockResolvedValue({}),
  showError: vi.fn(),
}))

vi.mock('@/api/admin/usage', () => ({
  adminUsageAPI: {
    createCleanupTask,
    listCleanupTasks: vi.fn().mockResolvedValue({ items: [], total: 0 }),
  },
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess: vi.fn() }),
}))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('../UsageFilters.vue', () => ({
  default: {
    name: 'UsageFilters',
    props: ['modelValue', 'allowAccountSearch'],
    emits: ['update:modelValue'],
    template: '<div />',
  },
}))

describe('cleanup with an account keyword', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    createCleanupTask.mockClear()
    showError.mockClear()
  })
  afterEach(() => vi.useRealTimers())

  it('rejects a keyword instead of dropping it, then allows an explicitly selected account', async () => {
    const wrapper = mount(UsageCleanupDialog, {
      props: { show: false, filters: { account_search: 'windhub' }, startDate: '2026-10-01', endDate: '2026-10-12' },
      global: { stubs: {
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
        ConfirmDialog: { props: ['show'], emits: ['confirm'], template: '<button v-if="show" class="confirm" @click="$emit(\'confirm\')">Confirm</button>' },
        Pagination: true,
      } },
    })
    await wrapper.setProps({ show: true })
    await flushPromises()
    const filters = wrapper.findComponent({ name: 'UsageFilters' })
    expect(filters.props('allowAccountSearch')).toBe(false)

    await wrapper.get('.btn-danger').trigger('click')
    await wrapper.get('.confirm').trigger('click')
    await flushPromises()
    expect(createCleanupTask).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.usage.cleanup.selectAccountForKeyword')

    filters.vm.$emit('update:modelValue', { account_id: 2351 })
    await flushPromises()
    await wrapper.get('.btn-danger').trigger('click')
    await wrapper.get('.confirm').trigger('click')
    await flushPromises()
    expect(createCleanupTask).toHaveBeenCalledWith(expect.objectContaining({ account_id: 2351 }))
    expect(createCleanupTask.mock.calls[0][0]).not.toHaveProperty('account_search')
    wrapper.unmount()
  })
})
