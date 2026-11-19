# Agent Muster

Agent Muster 是自部署的多设备 Agent 调度项目。你在一个 Web 对话里交办任务，中心 dsh 将工作派给设备上已有的 Codex、dsh 等 CLI。设计目标是让日常进度留在看板，需要决定时请示，完成后回到原对话汇报。

**当前处于工程骨架阶段。** Hub 提供 HTTP 健康检查，设备 daemon 和本机命令提供命令行入口，Web 和中心 dsh 插件可单独构建。完整的单设备派发与结果回传是首个开发里程碑。

[本地构建与运行](.wiki/01-快速上手/INDEX.md#本地构建与运行) · [项目 Wiki](.wiki/INDEX.md) · [v0.1 设计](.docs/agent-muster-v0.1.md) · [四个参考项目的源码调研](.docs/参考项目调研/README.md)

## 工程目录

| 路径 | 用途 |
| --- | --- |
| `cmd/` | 三个 Go 程序入口：中心服务 `muster-hub`、设备进程 `muster-daemon`、供设备 Agent 调用的 `muster` |
| `internal/hub/` | 中心服务的对话、设备与项目登记、任务、审批、IM 和 PostgreSQL 迁移 |
| `internal/daemon/` | 设备与中心的连接、本机调用接口、CLI 进程管理、文件交接和 SQLite 迁移 |
| [api/](api/) | Hub 维护的跨进程接口契约 |
| [plugins/muster-dsh/](plugins/muster-dsh/) | 由独立中心 dsh 进程加载的 TypeScript 插件 |
| [web/](web/) | React、TypeScript 和 Vite 工作台 |
| `deploy/`、`tests/e2e/` | 分别预留给部署文件和跨进程验收用例 |

## 许可证

[MIT](LICENSE)
