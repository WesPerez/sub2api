# 生产网络连接保护

原 `/root/sub2api-prod-deploy/restore-network.sh` 与 `scripts/restore-network.py` 的规范来源。
每次完成后 30 秒检查，沿用 `sub2api-upgrade.lock`、原网络恢复锁和
`/var/lib/sub2api-network-restore/pending.json` 恢复记录，与受控发布互斥。

已有正常连接时只核对健康与代理可达性；异常时恢复既定 Docker bridge 连接，拒绝挤占未知容器。
保留原事务和中断恢复，不调用 Compose、不重建容器、不改公网入口。
源码和 systemd 单元随本项目 operations 发布，计划和运行日志在 `/schedule/projects/sub2api/` 查看。
网络地址、依赖容器与代理地址属于固定部署约定，不作为用户日常参数；暂停前页面显示影响。

`tests/test_restore_network.py` 使用假 Docker 运行时验证事务、冲突、中断和既有连接保护。
验证在 GitHub Actions 执行；服务器只做现网状态读回，不为测试主动断开连接。

`legacy-entrypoints/` 保留原部署目录的两个入口：安装到
`/root/sub2api-prod-deploy/restore-network.sh` 和 `scripts/restore-network.py`。
它们只转发到本模块，避免旧维护入口继续运行一份独立实现。先发布并核对 operations、保存原文件，
再安装入口；不能在 operations 尚未就位时替换。systemd 直接使用本模块，不经过转发入口。
