# Sub2API 原生任务目录

`project.json` 是供 `/schedule` 读取的公开任务目录，发布到 `/opt/sub2api-operations/operations/`。
它只描述入口和用途，不包含凭据、数据库路径，也不启动调度器。

AgentRouter 账号恢复由 `backend/internal/service/agentrouter_recovery_*` 注册到应用生命周期，
策略、持久化执行锁与历史、管理 API、账号页配置各自分层。详见 `ACCOUNT_MAINTENANCE.zh-CN.md`。
已有计划测试、OAuth 刷新、数据维护及渠道监测继续使用各自的原生服务。

停用的余额直写与代理批量对齐任务不再发布；生产 bridge 的宿主救援由
`server-scheduled-tasks/tasks/sub2api-network-attachment/` 唯一维护。
新增业务任务在本应用独立服务中注册生命周期、配置和执行结果，并更新此目录；
不要重新创建外部常驻业务 worker 或宿主业务 timer。
