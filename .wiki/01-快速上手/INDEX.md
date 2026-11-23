---
title: 快速上手
description: 在本机启动单设备工作台，了解首版任务流程
updated: 2026-11-23
owner: project
---

# 快速上手

在 Linux 服务器安装和维护中心服务，见 [Docker Compose 自部署](01-Docker-Compose部署.md)。

## 本地构建与运行

从仓库根目录运行。开发环境使用 Go 1.26、Node.js 22.23.2 和 pnpm 12.3.4。编译和检查健康接口只需要本机工具链。

```sh
go build ./...
pnpm install --frozen-lockfile
pnpm run build
```

运行 `go run ./cmd/muster-hub` 后，用浏览器打开 [Hub 健康检查](http://127.0.0.1:8080/healthz)，预期收到 `{"status":"ok"}`。另开终端运行 `pnpm dev:web`，按 Vite 输出的本地地址打开工作台入口。两者都可以用 Ctrl+C 退出。

Web 和中心插件由同一个 pnpm 工作区构建。设备端是独立的 Wails Go 模块。当前 HTTP 接口见 [OpenAPI 契约](../../api/openapi.yaml)。

### 在 Windows 上跑通 M0

1. 准备 PostgreSQL 数据库。在启动 Hub 的终端设置 `MUSTER_DATABASE_URL`、`MUSTER_ADMIN_TOKEN` 和 `MUSTER_DSH_PLUGIN_TOKEN`。两种令牌分别生成，至少 32 个字符。启动时 Hub 自动初始化和迁移数据库。

   ```powershell
   go run ./cmd/muster-hub -listen 127.0.0.1:13011 -web-dir web/dist
   ```

2. 在另一个终端为中心 dsh 0.2.1-rc.5 准备专用 profile，并配置可用的模型。将 `MUSTER_DSH_HUB_URL` 设为 `http://127.0.0.1:13011`，`MUSTER_DSH_PLUGIN_TOKEN` 设为 Hub 上的插件令牌，`MUSTER_DSH_WORKSPACE` 设为长期使用的中心工作目录绝对路径。中心原生会话保存在该 dsh 环境的数据目录中。

   ```powershell
   dsh muster-center --from-default-profile web --dump-config | Out-Null
   dsh plugin --profile muster-center add "link:$((Resolve-Path 'plugins/muster-dsh').Path)"
   dsh --profile muster-center --host 127.0.0.1 --port 0 --no-open
   ```

3. 在 `cmd/muster-daemon` 运行 `wails3 task build`，启动生成的 Windows 应用。用管理员令牌打开 [本机工作台](http://127.0.0.1:13011/)，在「设备」创建配对码，然后在 Windows 应用中配对并选择一个工作目录。设备上的 dsh 使用其 headless profile 和本机模型配置。
4. 在 Web「项目」把名称绑定到已登记的设备目录。回到「对话」，交办该目录里的工作。到「看板」查看结果、运行轮次和会话 ID。通过「追加要求」续用原生会话。

### 在 Windows 上配置双设备协作

1. 在两台 Windows 上安装 Agent Muster，并登录各自的 dsh 或 Codex。两台设备使用同一 Hub 的 HTTP 或 HTTPS 地址，例如 `http://<服务器IP>:13011`。
2. 分别配对并选择本机工作目录。Hub 交换 WebRTC 信令，设备优先尝试直连，直连失败时自动使用部署者配置的 TURN。两台设备只需能访问 Hub 和可选的中继服务。已有可达的局域网或 VPN 地址也可以在标题栏「设备互联」中填写，保存后从托盘退出并重开应用。
3. 在 Web「项目」为同一项目添加两条绑定，分别选定设备、工作目录和 Agent，再设一条默认绑定。不同项目也可以分别绑定。任务派发后沿用当时的绑定。
4. 在主对话交办一项需要接力的任务，再交办另一项独立任务。在看板展开父任务查看子任务和交接状态。父任务等待期间可继续受理本机工作，收到所需结果后在原生会话中继续。

运行中的 Agent 可以通过 `muster status` 查询可用目标，通过 `muster send` 交接文件，通过 `muster delegate` 建立子任务，通过 `muster wait` 声明等候条件，并在恢复时用 `muster receive` 查看已接收的材料。详细参数可用各子命令的 `-h` 查询。文件使用绑定目录内的相对路径，单次上限为 32 MiB。Hub 记录投递身份、连接描述、摘要和确认，设备间正文通过加密的数据通道传输。

在 Hub 服务进程的运行环境中设置 `MUSTER_RTC_STUN_URLS` 来配置 STUN 列表，默认使用 `stun:stun.l.google.com:19302`。显式空值关闭 STUN 探测。公网中继可使用独立的 Coturn 服务，与 Hub 同机或单独部署。配置 `MUSTER_RTC_TURN_URLS`、`MUSTER_RTC_TURN_REST_SECRET` 和 `MUSTER_RTC_TURN_DEVICE_IDS` 后，Hub 为获准设备发放十分钟短期凭据。已有私有 TURN 可用 `MUSTER_RTC_TURN_USERNAME` 和 `MUSTER_RTC_TURN_CREDENTIAL` 配置。对端离线或所有路径均不可用时，设备保留发件箱并重试，可在任务详情查看进度。[部署示例](../../deploy/.env.example)

TURN 转发 DTLS 加密流量。普通设备构建信任 Hub 控制链路返回的信令，使用 HTTP 时，设备令牌、信令和 TURN 凭据都依赖该网络的可信性。固定公钥的验收包只对指定投递互验签名并使用 TURN。排查 Coturn 返回 `486` 时，同时检查用户分配数、总分配数和带宽配额。ICE 可能为每台设备同时申请 UDP 与 TCP 两条分配，单条配额乘以预期并发分配数应在总预算内。[设备间交接设计](../../.docs/agent-muster-v0.1.md#52-控制与数据传输)

### 在 Web 答复请示

使用支持请示的 Hub 和 Windows 设备端，在「对话」交办一项确实需要你作决定的任务，例如让 Agent 在整理目录前先确认采用哪种命名方式。设备 Agent 在本轮结束时提交请示，Hub 保存问题，请示出现在主对话和任务详情。

在请示中选择选项或填写答复。Hub 保存第一条有效答复。两个页面同时提交时，都能读到已保存的答案。原设备离线时答复仍会保存，设备重新上线后在原生会话中处理。等待期间可以继续交办其他任务。

联调时，设备在 Hub 声明支持请示后告知 CLI 使用以下回执。成功退出的 CLI 最终回复以 `MUSTER_APPROVAL_V1` 单独占首行，随后直接给出一个 JSON 对象，其中 `question` 必填，`options`、`impact` 可选。需要修复格式时，设备在本机保留原始回执，并在原会话尝试补写一次。补写仍失败时，任务显示「待核对」。[运行事件契约](../../api/openapi.yaml)

### 通过飞书答复同一请示

1. 在飞书开放平台创建企业自建应用，启用机器人功能。申请以应用身份发送消息（`im:message:send_as_bot`）和接收发给机器人的单聊消息（`im:message.p2p_msg:readonly`）权限。将应用可用范围设为要接收请示的账号，发布并启用版本。
2. 在应用的事件与回调配置中选择长连接，订阅「接收消息」事件 `im.message.receive_v1` 和「卡片回传交互」回调 `card.action.trigger`。平台保存长连接配置需要 Hub 已建立连接时，先完成下一步并启动 Hub，再回平台保存、发布事件与回调配置。
3. 给 Hub 同时设置 `MUSTER_FEISHU_APP_ID`、`MUSTER_FEISHU_APP_SECRET`、`MUSTER_FEISHU_OPEN_ID`。在 API 调试台选择同一个应用，用支持 `open_id` 的用户参数打开成员选择器，复制部署者的成员 ID。三个变量留空时关闭此渠道。应用密钥只保存在 Hub 的运行配置中。每个独立部署使用自己的飞书应用，平台会把回调分配给该应用的一条长连接。[配置示例](../../deploy/.env.example) · [飞书 API 调试台](https://open.feishu.cn/document/tools-and-sdks/api-explorer-guide)
4. 等设备提出请示后，在飞书单聊卡片中选一项或提交文本。也可以引用回复该卡片。回到 Web 可查看同一条请示的答复和处理状态。

Web 与飞书并发提交时，Hub 保存首个有效答复。发送结果未确认时，Hub 使用同一个去重键重试最多 55 分钟。飞书卡片的交互时限为 14 天。卡片已过期、渠道绑定已更换或飞书未返回原消息 ID 时，可在 Web 答复。

### 控制任务

在看板打开任务详情。暂停会让当前轮自然结束，恢复后再领取排队的输入或继续等待依赖。停止本轮会要求支持该操作的 Windows 设备结束本轮进程树，收到设备回执后可恢复原生会话。已启动且缺少会话标识的轮次进入「待核对」，可查看当时的运行记录和本机文件。

取消任务会撤销尚未运行的子任务和未处理的请示。已领取的轮次等待设备确认退出。旧设备继续运行至本轮自然结束，离线设备保留「正在取消」或「正在停止」，直到中心收到回执。取消后可另行交办。 [任务控制 API](../../api/openapi.yaml)

### 配置 Git 交付规则

在「项目」编辑需要交付 Git 提交的项目，开启 Git 交付规则。填写完整的基线引用，例如 `refs/heads/main` 或 `refs/remotes/origin/main`，并选择项目仓库已配置的远端别名。目标分支留空时按任务生成。测试程序和每个参数分开填写，最多允许五次返修，也可选用独立的 dsh 或 Codex 会话审查。

规则保存后由 Hub 分配版本。在对话中明确要求生成 Git 候选提交或推送，中心会冻结本次任务的规则。Git 绑定需选择仓库根目录。设备在独立工作树修复，按候选 SHA 创建干净检出运行测试和项目要求的独立 Review。发现可修的问题时，设备按配置次数回原生修复会话返修。任务详情显示候选 SHA、退出码、日志引用和 Review 结论。每次测试最多在设备保留 1 MiB 输出。

明确要求推送时，设备按冻结规则使用单一推送地址和目标分支，保留仓库的 Git 钩子。目标分支留空时使用 `refs/heads/muster/<任务 ID>`。Hub 在取得远端读回证据后确认交付。推送响应丢失时，设备继续读回原分支。核对超过一分钟时，本轮会显示待核对并释放设备名额，后续仍可在任务详情查看读回结果。 [交付规则契约](../../api/openapi.yaml)

### 查看运行诊断

在「用量」按项目或设备查看已结束的设备运行，也可切换到「中心」查看 dsh 已完成的投递轮次。页面按 CLI 来源分别汇总输入、输出和可取得的缓存等字段，并显示统计覆盖。缺少计量的历史记录计入已结束轮数。

任务详情按运行显示 CLI 退出码、设备记录的标准输出与标准错误字节数，以及 CLI 提供的 Token 计数。dsh 按本次调用的步骤汇总，缓存读写单列。Codex 新会话使用结束时的累计计数，续接缺少起点时显示「Token 用量未知」。

原始 stdout 和 stderr 分别保存在设备工作区目录的 `logs/<运行 ID>/cli-*/`，每次 CLI 调用拥有独立文件。在已结束的运行下展开「设备原始日志」，可向在线设备读取近期输出。每次读取的正文最多 64 KiB，截取时页面会标注。Hub 在本次请求中转交正文。需要完整文件时，可在设备本机打开日志。 [诊断接口](../../api/openapi.yaml)

运行记录内的独立 Review 按首审及返修序号列出候选提交、审查 Agent、结论和可用的诊断。展开该条 Review 的「设备原始日志」可读取该次审查的输出，设备目录为 `logs/<运行 ID>/review-*/`。中断后的审查保留已确认的诊断或显示为「未验证」。

Windows 设备默认从每个 stdout 或 stderr 文件最后写入时起保留 30 天。设置运行 daemon 的 Windows 用户环境变量 `MUSTER_RUN_LOG_RETENTION_DAYS` 可修改留存天数，设为 `0` 停用自动清理。设备进程重启后读取配置，启动时和每小时清理已有 Hub 确认的过期日志。运行、会话引用与待补报记录继续保存在本机执行账中。

### Windows 托盘与启动

常规启动显示窗口并恢复最近一次普通窗口的位置和大小。关闭窗口会保留后台设备服务，从托盘单击图标或选择「打开设备状态」即可重新显示。选择「退出」会停止设备服务。

`agent-muster-device.exe --background` 直接启动到系统托盘。需要在登录 Windows 时自动启动，可勾选托盘菜单的「登录时静默启动」。该选项默认关闭，按当前 Windows 用户和设备配置保存。

### 设备端自动更新

1. 每台电脑一次性安装当前用户版 Device 安装包。安装包预置发行公钥，后续发布使用与其匹配的私钥签名。设备和 `muster.exe` 一起切换版本。
2. 将新版本 ZIP 和签名清单发布到设备能访问的 HTTP 或 HTTPS 制品地址。在 Hub「设备」选中目标设备，展开「设备端更新」，填写版本和签名清单 URL。
3. 设备按 30 秒后台周期领取更新。先下载并验证，随后暂停领新任务，等待当前 CLI 完成并保存回执。更新器启动新版本，探测本机 SQLite 工作区后确认上线。验证失败时恢复上一版，Hub 报告失败或回滚。

自部署时可以用仓库根目录的发行工具维护签名。如果已存在发行密钥，后续版本直接复用，私钥留在发行机器。

```powershell
go run ./cmd/muster-release keygen -private <私钥文件> -public <muster-update.pub>
go run ./cmd/muster-release sign -version 0.2.1 -platform windows/amd64 -url https://releases.example.com/device-0.2.1.zip -dir <新版本程序目录> -private-key <私钥文件> -public-key <muster-update.pub> -key-output <发布用公钥副本> -archive <输出ZIP> -output <输出清单JSON>
```

在 `cmd/muster-daemon` 使用 `wails3 task package INSTALL_SCOPE=user UPDATE_VERSION=0.2.1 UPDATE_PUBLIC_KEY_FILE=<公钥绝对路径>` 可生成对应安装包。待签名的程序版本要与清单一致。Codex 和 dsh 仍使用各自的安装渠道。

## 一次交办如何完成

首次参与设计或开发，可以跟着这句交办读下去：“让家里电脑的 Codex 修这个 bug，测试通过就推送，办完告诉我。” 以下是 v0.1 的预期流程。普通文件目录同样可以登记为项目，交付条件可以是整理好的索引或报告。[双设备协作示例](../../.docs/agent-muster-v0.1.md#2-从两台设备看一次完整交付)

| 阶段 | 谁负责 | 从哪里看结果 |
| --- | --- | --- |
| 交办 | Web 把消息交给 Go 中心保存。Muster 插件将消息送入中心 dsh，dsh 识别项目、设备和目标，通过插件建任务。 | 你可以继续在同一条主对话交办下一件事。第一项工作已有自己的任务卡。 |
| 设备执行 | 家里电脑的 daemon 主动领任务，在绑定目录启动已有的 Codex。工作文件和 CLI 原生会话留在该设备。 | 看板显示当前轮次、承办设备和最后确认的状态。追加要求可以在原设备、原会话继续处理。 |
| 验收 | 设备对候选提交运行项目配置的测试。项目要求独立 Review 时，另一 Agent 会话审查同一提交。有可修问题就回原会话返修，候选变化后再验。 | 任务详情能查到被测提交、测试结果和审查结论。推送遇到回执丢失时，设备读取远端分支核对交付。 |
| 请示或汇报 | 需要用户选择时，Go 中心持久化同一条请示，用户可从 Web 或 IM 答复。满足交付条件后，中心 dsh 依据设备回执撰写汇报，经插件写回主对话。 | 日常进度留在看板。最终汇报给出修改内容、验证结果和实际推送的分支及提交。 |

设备侧支持 dsh 和 Codex。 [自动修复流程图](../../.docs/agent-muster-v0.1.md#72-git-项目的自动修复与推送) · [首版范围](../../.docs/agent-muster-v0.1.md#首版范围)

## 修改一个行为时，从哪里找依据

- **改派发或会话续接。** 先区分 Go 中心保存的用户消息和任务状态、中心 dsh 的原生会话、设备 CLI 的原生会话。设备掉线后，本地 SQLite 中的回执要等 daemon 重连补报。查[系统总览](../../.docs/agent-muster-v0.1.md#系统总览)和[数据归属](../../.docs/agent-muster-v0.1.md#81-数据存放与设备离线后的可见性)。
- **改请示或看板。** Web 和飞书的答复通过请示 ID 关联任务。界面要能区分「答复已保存」和「设备已处理」。[请示接口](../../api/openapi.yaml) · [Web 与 IM 共用一次请示设计](../../.docs/agent-muster-v0.1.md#62-web-与-im-共用一次请示)
- **准备贡献。** 先看仓库的方案确认和目录约定，再查本次改动相关的开发规范及验收场景。[项目规则](../../AGENTS.md) · [开发指南](../02-开发指南/INDEX.md) · [首版验收用例](../../.docs/agent-muster-v0.1.md#102-首版验收用例)

[返回 Wiki 首页](../INDEX.md)
