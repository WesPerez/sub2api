# Sub2API 任务目录

`project.json` 是供 `/schedule` 读取的公开原生任务目录，发布到 `/opt/sub2api-operations/operations/`。
它只描述入口和用途，不包含凭据，也不启动调度器。

已有计划测试、OAuth 刷新、数据维护及渠道监测继续使用上游原生服务。
AgentRouter 整点恢复迁至 `server-scheduled-tasks/tasks/sub2api-recovery/`，通过现有 Admin API
关闭调度、清除错误并按余额选择账号，不在 fork 内新增业务调度器。
生产 bridge 的宿主救援由 `server-scheduled-tasks/tasks/sub2api-network-attachment/` 维护。
