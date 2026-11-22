# Agent Muster

Agent Muster 是自部署的多设备 Agent 调度项目。你在一个 Web 对话里交办任务，中心 dsh 将工作派给设备上已有的 Codex、dsh 等 CLI。设计目标是让日常进度留在看板，需要决定时请示，完成后回到原对话汇报。

**当前实现 M1 双设备协作。** 从同一条对话交办多项任务，中心 dsh 将它们派给已配对的 Windows 设备，用 dsh 或 Codex 在绑定目录执行。设备通过 WebRTC 交接文件和消息，直连不可用时可使用配置好的 TURN 服务。固定公钥的信令验签已在指定投递上试点。父任务等待期间释放执行名额，收到子任务结果后续接原生会话。

[本地构建与运行](.wiki/01-快速上手/INDEX.md#本地构建与运行) · [双设备协作](.wiki/01-快速上手/INDEX.md#在-windows-上配置双设备协作) · [项目 Wiki](.wiki/INDEX.md) · [v0.1 设计](.docs/agent-muster-v0.1.md) · [四个参考项目的源码调研](.docs/参考项目调研/README.md)

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
