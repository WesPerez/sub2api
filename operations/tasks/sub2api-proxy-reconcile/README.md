# Sub2API Resin Proxy Profiles

该任务把 Sub2API 的账号代理收敛为三个稳定选项：`Global`、`CN`、`直连`。

```text
Sub2 account -> shared Sub2 proxy_id -> ResinPlatform.sub2-{{account_id}}
             -> selected egress policy -> shared bridge -> dynamic physical node / sticky egress IP
```

| Sub2 选项 | 默认账号 | Resin 数据面 | 说明 |
|---|---|---|---|
| `Global` | `platform=grok/openai` | `proxy.internal:10834` / `AppsGlobal` | 默认国际出口；不按 URL 或模型分流 |
| `CN` | 无默认账号，手工选择 | `proxy.internal:10834` / `AppsCN` | 中国大陆出口；不绑定具体站点 |
| `直连` | 无默认账号，手工选择 | `proxy.internal:12400` / sing-box `direct` | 通用直连，不经过 Resin、不自动回退 |

Sub2 后端在每次构造账号代理 URL 时把 `{{account_id}}` 展开为账号 ID；影子账号使用母账号 ID。因此多个账号
可以共享一个 Sub2 `proxy_id`，但 Resin 仍按不同 `Account` 保存独立 lease。物理节点更新不会改 Sub2 选项。
Global/CN 共用一个 Resin 实例；`sub2-{{account_id}}` 只作为 sticky lease 身份，不是额外策略。

## Reconcile 规则

- 后端账号级模板能力与首次迁移完成前，timer 必须保持 disabled；生产读回通过后才启用。
- 当前生产已完成模板上线和首次收敛，timer 已启用；上述门禁仍适用于新环境或重建部署。
- timer 投产后，新 Grok/OpenAI 账号最迟在下一次 5 分钟运行时绑定默认 profile。
- 只有 `Global`、`CN`、`直连` 三个受管 `proxy_id` 会被长期保留；空值或任何第四种代理绑定都会收敛到默认 `Global`。
- URL、模型名、账号名称和请求头不参与选路。需要大陆出口或直连时，由管理员在账号上显式选择对应 profile。
- 影子账号跟随母账号 profile，并在运行时共享母账号 Resin identity。
- 历史首次收敛迁移启用了 `inherit_legacy_lease` 的 profile 时，脚本会在改绑前调用 Resin 官方
  `inherit-lease`。启用该能力的 profile 会把仍有效的旧 lease 复制到对应的
  `平台.sub2-账号ID`；父 lease 不删除。父 lease 已过期或不存在的 404 只有响应明确为
  `parent lease not found` 时才记为首次分配；token 错、Platform 错、非 JSON 404 和其他错误全部
  失败关闭，禁止在身份未继承时继续改绑。旧身份已经等于目标身份时记录为 `already_stable`，不调用
  Resin 的 `inherit-lease`，也不重建现有 lease。
- profile 网络字段或 token 与配置不一致时失败关闭；轮换凭据时使用新的 versioned profile 名称完成 A/B 迁移，
  不原位覆盖唯一共享入口。
- 旧代理名只在一次性迁移配置中登记；当前长期配置不按名称猜测业务用途。
- Sub2API 的 Go DTO 在批量删除无跳过项时可能把空 slice 编码为 JSON `null`；reconcile 将
  `deleted_ids` 或 `skipped` 的 `null` 仅按空列表处理，其他非数组形状仍失败关闭。

## 安全与回滚

PostgreSQL 盘点强制 `default_transaction_read_only=on`，只读取账号 ID、平台、绑定和代理网络摘要。写入只走
Sub2API Admin API。每轮 apply 在
`/var/lib/server-scheduled-tasks/sub2api-proxy-reconcile/recovery/` 创建 0600 manifest，记录原绑定、创建的
profile ID 和本轮改动账号 ID，不记录 token、账号凭据或完整代理密码。

任一账号更新失败时，脚本按逆序恢复已经改动的账号绑定。新建但尚未引用的 profile 保留，供下一轮幂等接管。
已经复制出的子 lease 也保留：该操作是幂等覆盖，删除它反而可能破坏并发流量或下一轮恢复。
profile 创建读回、最终绑定读回或状态落盘失败也会写入失败 manifest；若已改绑则执行同样的逆序回滚，
不会把恢复记录永久留在 `running`。

## 验证

```bash
bash sub2api-proxy-reconcile.test.sh

python3 sub2api_proxy_reconcile.py \
  --config /etc/server-scheduled-tasks/sub2api-proxy-reconcile.json \
  --sub2-admin-key-file /etc/server-scheduled-tasks/sub2api-admin-key \
  --profile-token-file apps=/etc/resin-apps/proxy.token \
  --profile-token-file direct=/etc/server-scheduled-tasks/sub2-direct-proxy.token
```

第二条命令默认只输出计划；systemd unit 才带 `--apply`。
