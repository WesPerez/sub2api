# Sub2API AnyRouter 直连余额备用同步

## 目的

在 METAPI 的 AnyRouter 余额链路故障时，使用 6 个 root-only session cookie 直接查询
`https://anyrouter.top/api/user/self`，把 `quota` 和 `used_quota` 换算为 USD 后更新对应的
Sub2API 账号备注。

当前核准顺序如下：

| Cookie 行号 | Sub2API 账号 ID | 名称校验 |
|---:|---:|---|
| 1 | 1 | `^![0-9A-Za-z]{2}-any-` |
| 2 | 2 | `^![0-9A-Za-z]{2}-any-` |
| 3 | 5 | `^![0-9A-Za-z]{2}-any-` |
| 4 | 4 | `^![0-9A-Za-z]{2}-any-` |
| 5 | 7 | `^![0-9A-Za-z]{2}-any-` |
| 6 | 2183 | `^![0-9A-Za-z]{2}-any-` |

前缀两位是命名技能的 base36 显示码，脚本只验证通用格式，不比较具体值；账号身份仍由核准的
Sub2API 账号 ID、Cookie 行号和 AnyRouter user ID 映射共同确定。任何映射不一致都会在数据库写入前终止。脚本还会按相同顺序校验 AnyRouter user ID：
`181380,199848,200748,213103,201326,213232`。

## 安全与数据影响

- Session cookie 只保存在 `/etc/sub2api-any-balance-sync.tsv`。
- Cookie 文件必须归 root 所有，mode 为 `600` 或 `400`。
- Session 通过 mode `600` 的临时 cookie jar 传给 curl，不进入进程参数。
- Fetcher 拒绝向 `https://anyrouter.top` 之外的主机发送 cookie，且不跟随重定向。
- ACW/ESA challenge 只用固定算法和数值解析在本地求解，不执行上游 JavaScript，也不调用 METAPI。
- 6 个上游请求必须全部成功，之后才会进入 PostgreSQL 事务。
- 事务只修改 IDs `1,2,5,4,7,2183` 的 `accounts.notes` 和 `updated_at`。
- 备注包含余额以及 `昨日耗`、`上小时耗`、`今日耗`、`本小时耗`、`刷新`、`统计`。
- 故障切换时保留同一天已有的签到状态/签到余额快照；没有当天证据时明确写 `签到状态未知`，
  不会把旧日期伪装成当天签到。
- `SUB2API_PRIMARY_ACCOUNT_KEY`（默认 `any-6945`）匹配到的账号第二行签到汇总会在故障切换期间保留，余额统计始终只解析第一行；不依赖名称排序、前缀值或本地 ID。METAPI 同步任务使用同一变量名和默认值。
- 用量来自相邻 `used_quota` 样本差值；样本跨越超过一小时或计数器回退时，无法可靠归属的小时桶归零。

生产出口从 `/etc/server-scheduled-tasks/apps-resin-anyrouter-balance.env` 读取
`AppsGlobal.sub2api-anyrouter-balance-sync` 的 root-only `RESIN_SOCKS_*` 配置，入口为
`proxy.internal:10834`。代理凭据只写入
mode `600` 的私有临时 curl config，不进入 argv 或 journal。

## 文件

- `fetch-anyrouter-balances.mjs`：直连请求、session user ID 提取、ACW 处理和响应校验。
- `sync-anyrouter-balances-to-sub2api-notes.sh`：校验样本和目标集合，事务更新备注。
- `note-render.test.sh`：离线验证多行备注的统计解析和第二行保留。
- `systemd/sub2api-anyrouter-balance-sync.service`
- `systemd/sub2api-anyrouter-balance-sync.timer`

Cookie 文件每行一个账号：

```text
<sub2api-account-id><whitespace><raw-session-cookie>
```

## 调度

- timer 定义为每小时 `:33:00`
- `AccuracySec=1s`
- `Persistent=true`
- 生产已安装 unit，但 METAPI 正常时 timer 必须保持 `disabled/inactive`
- 与 METAPI 主同步共用 `/run/lock/sub2api-balance-notes.lock`，禁止并发覆盖备注
- 锁已被主同步占用时以临时失败码 `75` 退出，不把“本轮未执行”误报为成功

## 验证

静态检查：

```bash
node --check fetch-anyrouter-balances.mjs
bash -n sync-anyrouter-balances-to-sub2api-notes.sh
./note-render.test.sh
systemd-analyze verify \
  systemd/sub2api-anyrouter-balance-sync.service \
  systemd/sub2api-anyrouter-balance-sync.timer
```

只读预览会真实查询 AnyRouter，但不写 PostgreSQL：

```bash
APPLY=0 ./sync-anyrouter-balances-to-sub2api-notes.sh
```

故障切换时才启用 timer。手工真实执行前必须先备份 6 条当前备注，并确认主同步没有运行。
