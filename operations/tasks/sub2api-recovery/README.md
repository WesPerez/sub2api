# AgentRouter 账号恢复

北京时间每小时整点，从 AgentRouter 的 GPT、Claude、GLM、DeepSeek 四类账号中，分别开放
备注余额最高的 3 个。GLM 属于 OpenAI 平台，DeepSeek 使用独立账号 ID；同一上游身份的余额
可能共享，不应把四套账号的备注余额相加当成站点总余额。

## 执行与保护

`sub2api-recovery.timer` 是生产调度来源，触发 `sub2api-recovery.service --once`。
默认不补跑错过的整点；页面可以修改执行时间。`--once` 状态文件不再计算下一次执行时间，
下次时间以 systemd 为准。脚本保留旧常驻模式供兼容，不得与 timer 同时启用。

1. 分页读取账号，经凭据导出核对 HTTPS 主机名严格等于 `agentrouter.org`，并跳过软删除账号。
2. 按名称最后一个 `-` 后的后缀归类，先检查账号和余额，再执行变更。
3. 对已核实的目标逐个调用管理页面相同的 `recover-state`，复核为 active。
4. 关闭这些账号的调度，逐个核验；按余额降序、ID 升序，为每类选择配置数量的账号重新开启。
5. 每个修改阶段重新核对名称、平台、类型、上游地址和凭据指纹。恢复或关闭失败的账号不入选。

未归类账号保持原开关，结果明确提示待归类数量。整类为空或所有余额无法识别时保留该类原开关，
报告规则异常；不能把错误的空集合当作成功。如果一类还有有效候选，同类缺少余额的账号会关闭，
预览中也明确列出。候选不足时按实际数量开放。

任务不修改余额、额度或使用流水，不发送模型探针，不更新备注。备注余额来自上游同步任务，
只代表最近一次同步值，不是本任务实时查询上游得到的余额。

## 配置

- `config.env.example`：受保护的管理接口地址、管理令牌文件和请求超时。
- `policy.example.json`：非秘密规则模板；生产保存为
  `/etc/server-scheduled-tasks/agentrouter-recovery.json`，root 所有、权限 `0600`。
- 类型包含稳定的 `id`、显示名称 `label`、后缀别名 `aliases` 和 `top_n`（1–20）。
  别名不区分大小写，不包括前面的连字符，各类不能重叠。
- 站点固定为已核实的 AgentRouter。扩展其他站点应先实现相应适配和边界核验。
- 执行时间、精度、补跑和开关由 `/etc/server-scheduled-tasks/schedules.toml` 管理；
  [Schedule 管理台](../schedule-console/README.md) 保存前会检查账号匹配、配置版本和任务来源。

## 首次从常驻服务迁移

先核对现场 service 的来源、当前一轮是否已完成，并留存 unit、规则及状态恢复点。
禁止直接并行启动第二个 worker。下面命令从仓库根目录执行：

```bash
install -m 0600 tasks/sub2api-recovery/policy.example.json /etc/server-scheduled-tasks/agentrouter-recovery.json
systemctl disable --now sub2api-recovery.service
install -m 0644 tasks/sub2api-recovery/systemd/sub2api-recovery.service /etc/systemd/system/
install -m 0644 tasks/sub2api-recovery/systemd/sub2api-recovery.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now sub2api-recovery.timer
```

将 `config/schedules.toml.example` 中的 recovery 条目加入现有生产 TOML，再使用
`manage-schedules.py apply --adopt` 接管当前一致的 timer；不要覆盖其他任务的配置。
`/run/server-scheduled-tasks/sub2api-recovery.lock` 跨常驻、oneshot 和手工执行互斥。

## 验证与诊断

只读预览需显式传入 `--agentrouter-enabled true`、`--admin-key-file` 及现有接口地址；
使用 `--preview`，不调用恢复或启停接口。生产只使用 systemd 服务执行变更，避免遗漏环境配置。

```bash
systemctl status sub2api-recovery.timer sub2api-recovery.service --no-pager
journalctl -u sub2api-recovery.service --since today --no-pager
python3 -B -m pytest -q tests/test_sub2api_recovery_worker.py
```

状态文件 `/var/lib/server-scheduled-tasks/sub2api-agentrouter-recovery-state.json` 原子写入，权限
`0600`；包含最近开始、结束、耗时、逐阶段成功/失败 ID、规则异常和本轮规则版本。
单个账号失败不阻断其余账号，最终退出非零并在页面显示失败或部分异常。
