import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AccountRecoveryDialog from '../AccountRecoveryDialog.vue'

const api = vi.hoisted(() => ({ config: vi.fn(), history: vi.fn(), preview: vi.fn(), save: vi.fn(), run: vi.fn() }))
vi.mock('@/api/admin/accountRecovery', () => ({ accountRecovery: api }))

const policy = () => ({ enabled: false, cron: '0 * * * *', timezone: 'Asia/Shanghai', max_targets: 200,
  retention_days: 3, max_runs: 300, groups: [{ id: 'glm', label: 'GLM', aliases: ['glm'], top_n: 3 }] })
const config = () => ({ policy: policy(), revision: 'config-one', next_run_at: null })
const plan = () => ({ revision: 'preview-one', matched: 4, selected: 3, warnings: ['一个账号余额未识别'],
  groups: [{ id: 'glm', label: 'GLM', matched: 4, selected: [1, 2, 3] }], accounts: [] })
const result = () => ({ id: 1, status: 'success', trigger: 'manual', started_at: '2026-09-22T14:00:00Z',
  finished_at: '2026-09-22T14:00:01Z', outcome: { enabled: [1, 2, 3], disabled: [4], errors: [] } })

async function open() {
  const wrapper = mount(AccountRecoveryDialog, { props: { show: false }, global: { stubs: {
    BaseDialog: { props: ['show'], emits: ['close'], template: '<div v-if="show"><button data-close @click="$emit(\'close\')">close</button><slot /><slot name="footer" /></div>' }
  } } })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}
function button(wrapper: VueWrapper, text: string) { return wrapper.findAll('button').find(item => item.text() === text)! }

describe('AccountRecoveryDialog', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    api.config.mockResolvedValue(config())
    api.history.mockResolvedValue([])
    api.preview.mockResolvedValue(plan())
    api.save.mockImplementation(async value => ({ policy: JSON.parse(JSON.stringify(value)), revision: 'config-two', next_run_at: null }))
    api.run.mockResolvedValue(result())
  })

  it('loads policy and history, then saves only after a matching preview', async () => {
    const wrapper = await open()
    expect(api.history).toHaveBeenCalledOnce()
    expect(wrapper.text()).toContain('已暂停')
    expect(button(wrapper, '保存配置').attributes('disabled')).toBeDefined()
    await wrapper.find('input[type="checkbox"]').setValue(true)
    await button(wrapper, '核对账号').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('一个账号余额未识别')
    expect(button(wrapper, '立即执行一次').attributes('disabled')).toBeDefined()
    await button(wrapper, '保存配置').trigger('click')
    await flushPromises()
    expect(api.save).toHaveBeenCalledWith(expect.objectContaining({ enabled: true }), 'config-one', 'preview-one')
    expect(api.run).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('配置已保存')
    wrapper.unmount()
  })

  it('invalidates a stale account preview when the draft changes', async () => {
    const wrapper = await open()
    await button(wrapper, '核对账号').trigger('click')
    await flushPromises()
    expect(button(wrapper, '立即执行一次').attributes('disabled')).toBeUndefined()
    await wrapper.find('input[type="number"]').setValue(4)
    expect(wrapper.find('[aria-label="账号匹配预览"]').exists()).toBe(false)
    expect(button(wrapper, '保存配置').attributes('disabled')).toBeDefined()
    await wrapper.find('[data-close]').trigger('click')
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.text()).toContain('继续编辑')
    wrapper.unmount()
  })

  it('requires confirmation and sends the preview revision plus a request identity', async () => {
    const wrapper = await open()
    await button(wrapper, '核对账号').trigger('click')
    await flushPromises()
    await button(wrapper, '立即执行一次').trigger('click')
    expect(api.run).not.toHaveBeenCalled()
    await button(wrapper, '确认执行').trigger('click')
    await flushPromises()
    expect(api.run).toHaveBeenCalledOnce()
    expect(api.run).toHaveBeenCalledWith('preview-one', expect.stringMatching(/^[0-9a-f-]{36}$/))
    expect(button(wrapper, '立即执行一次').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })

  it('keeps the draft and explains a concurrent configuration change', async () => {
    const wrapper = await open()
    await wrapper.find('input[type="checkbox"]').setValue(true)
    await button(wrapper, '核对账号').trigger('click')
    await flushPromises()
    api.save.mockRejectedValue({ response: { data: { message: '配置已被其他窗口修改，请重新读取' } } })
    await button(wrapper, '保存配置').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').text()).toContain('其他窗口')
    expect((wrapper.find('input[type="checkbox"]').element as HTMLInputElement).checked).toBe(true)
    wrapper.unmount()
  })
})
