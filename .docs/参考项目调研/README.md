# 四个参考项目如何影响 Agent Muster 的设计

从「让家里电脑上的 Codex 修一个 bug，测过、推送、办完告诉我」倒推，Muster 需要回答四个问题。中心 dsh 怎么把对话变成可独立推进的任务？设备重启后怎么找到原来的工作？谁能证明要推送的版本已经测过？手机和网页如何对同一请示只答一次？四个项目各实现了其中的一部分。本文给出横向判断，四份专题记录各自的功能、实现、边界和取舍。[Agent Muster v0.1 设计文档](../agent-muster-v0.1.md)

## 1. 本地源码与阅读入口

以下是 2026-11-19 从上游克隆的单分支浅克隆。提交固定到本地检出的 HEAD。可在对应目录使用 `git show HEAD:仓库内路径` 查证。树条目包含代码、测试、文档、资源以及可能存在的子模块指针。它只描述检出规模。专题按面向用户的功能归组，围绕入口、状态写入、执行者、输出和失败路径选取证据。测试引用描述测试作者写下的预期。本次为静态调研。

| 本地目录（位于 `.upstream/`） | 上游分支与版本 | 树条目 | 打开专题 | 值得先读 |
| --- | --- | ---: | --- | --- |
| `agent-orchestrator` | [main · 0d322ae](https://github.com/OrchestratorInc/agent-orchestrator/tree/0d322ae188eff9ad41d50df634fbe78c0af552d2) | 7,853 | [01 · Agent Orchestrator](01-agent-orchestrator.md) | 看板的列与活动标签、worker 报告、当前提交的 Review、CLI 安装归属和升级 |
| `agent-autoloop` | [main · d1ec2d8](https://github.com/KKKKhazix/agent-autoloop/tree/d1ec2d8e48de62b2a49bd8dc5985c03482c753ce) | 264 | [02 · Agent Autoloop](02-agent-autoloop.md) | 远程 worker 领活、CLI 回执、飞书、故障重放、GitHub 交付插件 |
| `dsh-superagent-prototype` | [main · 8c32a9d](https://github.com/deepseek-ai/dsh-superagent-prototype/tree/8c32a9dc5d72c033816c6a8110b37f40cf94fba4) | 216 | [03 · dsh Superagent](03-dsh-superagent.md) | 主会话和执行会话的分工、追加输入归属、worker 让出名额、来源与面板 |
| `lobehub` | [canary · d86a85b](https://github.com/lobehub/lobehub/tree/d86a85bcea15a5e1c3131f1d69bb1c08769a8819) | 26,429 | [04 · LobeHub](04-lobehub.md) | 设备与目录选择、任务启动与撤销冲突、Verify 和结果回写，以及工作台其余能力的成本 |

## 2. 对 Core 草案的四处修正

1. **Autoloop 已有跨机器 worker。** Worker 通过 HTTP 客户端长轮询，携带 claim、在跑轮次和可用名额。Hub 按 worker 可接的 agent、lane 互斥及并发上限发活。结果先保存在 worker，Hub 确认后才结束上交。Muster 值得另行设计的是「一条对话中的目标、项目和设备绑定、父子依赖与设备间产物」。Muster 的 Go 中心服务自行实现调度，借鉴上游的领活、去重和补报语义。中心任务与回执写入 PostgreSQL，设备待补报记录写入本机 SQLite。[领取判定](https://github.com/KKKKhazix/agent-autoloop/blob/d1ec2d8e48de62b2a49bd8dc5985c03482c753ce/kernel/decide.ts#L306-L342) · [Worker](https://github.com/KKKKhazix/agent-autoloop/blob/d1ec2d8e48de62b2a49bd8dc5985c03482c753ce/worker/worker.ts#L575-L614)
2. **AO 也有普通目录工作区。** 其公开代码走不同 workspace 实现，交付看板深度使用 PR、CI、评审和运行事实。Muster 处理素材索引、日志诊断等任务时，完成条件必须能写成「某份产物通过某项检查」。Git 只为此增加提交和远端引用两类证据。[工作区实现](https://github.com/OrchestratorInc/agent-orchestrator/tree/0d322ae188eff9ad41d50df634fbe78c0af552d2/backend/internal/adapters/workspace) · [看板归约](https://github.com/OrchestratorInc/agent-orchestrator/blob/0d322ae188eff9ad41d50df634fbe78c0af552d2/backend/pkg/contract/kanban.go)
3. **设备心跳过期只说明中心失联。** 原 CLI 可能仍在修改文件。Autoloop 对去向不明的轮次保留 unknown，AO 通过 launch ID 核对观测属于哪次启动。Muster 得到原设备的进程或退出证据后才交出会话写权。领取幂等也无法判定此前的 push 成败，需读回远端引用。[Autoloop 轮次状态](https://github.com/KKKKhazix/agent-autoloop/blob/d1ec2d8e48de62b2a49bd8dc5985c03482c753ce/kernel/decide.ts) · [AO 进程核对](https://github.com/OrchestratorInc/agent-orchestrator/blob/0d322ae188eff9ad41d50df634fbe78c0af552d2/backend/internal/lifecycle/manager.go#L459-L531)
4. **CLI 输出、运行结果、验收结论、用户汇报是不同记录。** dsh 原型可通过 Harness 事件观察输入是否进入会话步骤，这种信号在普通 CLI 上未必存在。LobeHub 将当轮输出写入 run，再决定验证与回调时机，摘要失败还可退回原答复。Muster 可保留最终文本和本机证据，独立记录本轮处理了哪些输入、验收针对哪个版本、主对话是否已收到最终汇报。[dsh 输入送达与读取](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/8c32a9dc5d72c033816c6a8110b37f40cf94fba4/src/core/assignments.ts#L45-L79) · [LobeHub 结果保存与验收](https://github.com/lobehub/lobehub/blob/d86a85bcea15a5e1c3131f1d69bb1c08769a8819/apps/server/src/services/taskLifecycle/index.ts#L141-L212)

## 3. 按 Muster 的设计问题对照

表中省略的上游能力可在专题按名称查找。最右栏描述建议采纳的机制；写作「候选」的能力留作版本取舍。

| 设计问题 | Agent Orchestrator | Agent Autoloop | dsh Superagent | LobeHub | Muster 取舍 |
| --- | --- | --- | --- | --- | --- |
| 用户找谁、谁领工作 | 项目 orchestrator 委派 worker | 来源事件匹配规则并投给 worker | 主会话裁决，Ticket 记录目标，Assignment 承办 | 用户面对工作台、Agent、Topic 或任务 | 用户只面对中心 dsh。为一次交办生成一个根任务，中心服务保存执行权 |
| 后续一句话归谁 | 特定 session 的输入、队列与报告 | 规则与 lane 关联事件、答复、复查 | 线程订阅区分承办与仅供上下文 | 已有 task/topic 可 follow-up，旧 operation 会被取代 | 保存明确的目标任务和输入 ID。每一轮结束申明已处理的输入，再决定交付 |
| 项目与设备 | host、project 和 workspace 分层 | 规则配置工作目录，worker 声明可接角色 | 单个 Harness 宿主管理票据会话与工作区 | 设备选择影响目录解释及外部 agent 启动 | 一项目可登记多组设备路径；本次原生会话固定到首次的设备、目录、CLI 配置档 |
| 原生 CLI 接入 | TUI/PTY 与富 Chat 驱动另有注册、能力探测 | 命令、专用 CLI 插件与 ACP 各有能力边界 | 直接使用 DSH 会话、工具和事件 | 异构外部 Agent 与自身模型运行链并存 | 使用提示词、最终回复、同机续接三个契约。可选采集运行活动、用量、诊断。中心 dsh 与设备 CLI 分开管理 |
| 并行、等待和再派发 | 会话、工作区准备及启动有各自阶段 | 组、lane、agent 并发上限共同控制领取 | 执行槽、背景作业及用户跟进有独立调度 | 任务图、启动认领与取消竞争、验收回调 | 中心维护依赖关系；设备维护实际进程与目录写锁。父任务在轮末等待跨机交付时释放名额 |
| 实时状态与界面 | 状态事实派生泳道、活动和通知，CDC→SSE | CLI 状态、快照、游标增量、单卡 Web 表单 | 总览→线程→Ticket→Assignment，设置有专门视图 | 工作台会话、任务、运行面板、设备入口 | 看板突出「等你」；卡片写明阶段、承办者、活动证据和最后确认时间；一键跳到测试、请求或产物 |
| 测试、Review 和推送 | 面向 PR 当前 head 启动审查，失败与意见区分 | 已有 PR 的交付插件核验 review、CI、合并、发布 | 理解 GitHub 事件、跟随 PR，具体工作由 worker 执行 | Verify 驱动配置了验证的任务终态，支持返修与证据 | 家庭 Codex 可直接修、测、按策略推；项目可配独立审查者。普通目录换用产物摘要和验收记录 |
| IM 与请示 | 通知持久化，Grok Bot 按主机、会话、请求路由 | 飞书互动卡片与原消息核对，答复入账后再续办 | 飞书/GitHub 是可读写的事件来源，Outbox 管理回复 | Bot 路由、平台适配器、双向消息和通知 | Web 与飞书共用一条请示和一次有效答复，投递、答复、续办分别记录。普通任务进度留在看板 |
| 有结果之后给谁看 | worker 报告可积累并向项目协调者交付 | 回执、提醒、业务完成与后续动作分别处理 | worker 报告交票据，再决定是否回复来源 | 产物先落账，按稳定键将结果写回创建者话题 | 子任务先交父任务，根任务结案写主对话一次。两根任务同时结束时两条汇报均可见 |
| 断线、停机、重复事件 | 启动代次、原生身份、事件回放 | worker 本地运行账、结果重送、unknown、决策日志 | 未消费输入、持久化 effects 和发送回执 | 迟到 operation 检查，认领与撤销再核对 | 优先恢复已确认的工作和派发意图。中心离线时设备可保留结果；去向不明的本地动作先对账 |
| 日志、用量与成本 | 从原生事件/记录采集，按 scope 定价归集 | 轮次与会话累计量折算，保留本地原始运行材料 | 面板跟踪执行、重试、健康状态，信息依赖 Harness | 服务有 usage 统计、追踪和配额 | 分开记录中心规划、修复、独立 Review 的可观测用量。缺字段写「未知」，累计数跨续跑去重 |
| 安装、升级、改配置 | CLI 来源探测、计划、等待会话空闲和执行后校验 | 有自身服务安装、插件和 worker 的运维 | 部署、运行时配置、提示和策略管理 | 设备网关、外部 agent 扫描、桌面与 CLI 的管理入口 | 首版展示已安装版本、能力、项目绑定、配置档。远程改动的完整产品流程见下节，排期待定 |
| 搜索、知识、工具与生成 | 产物、历史检索、终端、浏览器与开发工具 | 可配置 skill、插件、业务模板 | 角色工具、技能覆盖、消息和资源读取 | 知识库、检索、记忆、创作、协作、市场、评测 | 首版复用设备现有 agent 的技能，展示产物位置、摘要、可用性。专门建知识库、市场与在线编辑器属独立产品面 |
| 自部署与数据归属 | 本地 Go daemon、SQLite，远程及 Cloud 各有边界 | Node 与 SQLite，可拆开 Hub 和 worker | 基于 Harness 的插件宿主、持久状态和本地面板 | 服务端、工作台、网关、数据服务等可组合部署 | Ubuntu 小 VPS 默认通过 Docker Compose 运行 Go 中心、中心 dsh 与插件、PostgreSQL。设备原始日志和原生会话留本机，双机经 LAN/VPN 传文件，2C2G 为整套默认部署的实测目标 |

## 4. 把「办完告诉我」落到交付证据上

下面是家庭项目的首版设计。设备或项目第一次登记时保存目录、CLI、允许的远端与分支、测试命令、Review 规则及返修预算。本次交办能唯一命中这些设置，就可以一路运行到交付。

| 时刻 | 谁做什么，留下什么 | 用户看到什么 |
| --- | --- | --- |
| 交办 | 中心 dsh 解析「家里电脑、项目、Codex」，服务把绑定和规则快照写入根任务。daemon 在可用时认领 | 聊天能继续发下一条；看板显示等待设备、准备目录或执行中 |
| 修复 | Codex 在本机目录或专用 worktree 交付候选提交。daemon 保存原生会话、基线与候选 SHA，核对纳入改动的范围 | 详情能打开变更清单和候选提交 |
| 测试 | 设备按项目登记的命令在对应候选的干净工作区验证，记录命令、被测 SHA、退出码和日志引用。源码与受测内容有差异时，原结论失效 | 看板显示测试通过或自动返修 |
| 按需复审 | 项目选了 dsh 做独立 Review，就创建关联子任务和独立本机会话，审查同一基线至候选的差异。返回通过、需改、或无法评审的原因；修复者接回具体意见 | 看板可见审查者和结论。可修问题沿原 Codex 会话继续，不打扰用户 |
| 推送 | 设备确认待处理要求已消费、目标 SHA 的测试/必需 Review 已满足、远端与分支符合授权，执行普通 push 并读回目标引用 | 成功回执里有仓库、分支、提交；网络中断时显示「推送待核对」 |
| 汇报 | Go 中心登记已验收的交付。插件将结果送入中心 dsh 生成汇报，Go 中心按「根任务、本次交付」保存一条回复，附测试证据、可选 Review 和实际远端引用 | 主对话收到一条可核查的结果。飞书只承接有具体选择的问题 |

**推送后掉线的判定。** 若远端正好指向候选提交，可据此登记已交付。若远端后来已向前推进且可确认包含该提交，记录「已包含」及观察到的 SHA，保留对确切推送时刻的不确定性。若远端分叉，保留本地候选与已有测试证据，按项目授权决定是否基于新基线返修；新候选重新测试并按需 Review。若远端暂不可读，继续核对并保留原操作标识。以上为 Muster 的推送设计；Autoloop 的上游实例主要处理 PR 合并和交付状态回读。[Autoloop 交付与丢回包测试](https://github.com/KKKKhazix/agent-autoloop/blob/d1ec2d8e48de62b2a49bd8dc5985c03482c753ce/tests/delivery.test.ts) · [AO 同一提交的审查判定](https://github.com/OrchestratorInc/agent-orchestrator/blob/0d322ae188eff9ad41d50df634fbe78c0af552d2/backend/internal/autoreview/coordinator.go#L186-L216)

**什么时候值得请示。** 例一，项目只授权推任务分支，这次实际目标要求写受保护分支，现有策略没有合法交付路径。例二，同名项目各有一个有效绑定，现有对话无法确定应动哪一个。两者都能列出用户可选的动作和影响。可自动修复的测试失败、设备暂时离线、回执补交、可重试的飞书发送失败留在看板。返修用尽且已经能明确说明失败原因时发失败汇报；只有用户的一项决定确实能继续任务时才创建请示。[Autoloop 卡片答复处理](https://github.com/KKKKhazix/agent-autoloop/blob/d1ec2d8e48de62b2a49bd8dc5985c03482c753ce/kernel/decide.ts#L523-L558) · [dsh 的请求和报告](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/8c32a9dc5d72c033816c6a8110b37f40cf94fba4/src/core/assignments.ts)

## 5. 原需求中的远程运维

用户提出了在 Web 安装、升级 agent、改配置，以及重启或杀掉卡住的运行。当前 v0.1 正文已安排进程控制、状态、日志、用量和项目配置管理；远程更改设备上安装的 CLI 或其原生配置暂列后续。这是版本排期建议，长期产品需求仍包括这三项。可独立安排以下能力，不必把它们设成自动修 bug 的前置条件。

| 操作 | Web 发给设备的意图 | 设备的确认与验收 |
| --- | --- | --- |
| 安装 | 已登记的 CLI、版本/渠道、设备上允许的安装方式 | daemon 在目标账号解析安装计划、运行已登记的安装命令、记录退出和版本探测，成功后才标为可派发。已有安装的来源不清时保留现场 |
| 升级 | CLI 实例、当前版本、目标版本、空闲窗口 | 识别实际安装来源和正在用它的运行（包括审查）。记录维修期间的排队，检查手头会话能在新版本上读取；有可用旧版本才提供相应回退。中断后重新探测实际版本 |
| 改配置 | 指定设备及配置档、预期旧版本、结构化差异 | 先校验本地当前版本，原子保存可恢复的变更，再探测关键能力。新派发引用新版本，旧任务保存使用过的版本。面板可展示来源和变更时间，凭据在本机处理 |
| 停止并重启 | 指定任务、运行 ID 和已知的启动代次 | 先核实进程身份，停止该轮进程树。收到退出证据后再视任务状态续接；同一 PID 被系统复用时不应影响其他程序 |

AO 源码对这些细节给出了可查的例子。路径可能是 shim，安装来源要经实际包管理器核实；Windows 上正在运行的程序会影响升级；上游某些「等待空闲」状态只在内存中，daemon 重启会丢掉等待。因此 Muster 若向用户承诺「回家电脑空闲时升级」，须将这次承诺持久化，重启后能恢复或说明原因。[来源判断](https://github.com/OrchestratorInc/agent-orchestrator/blob/0d322ae188eff9ad41d50df634fbe78c0af552d2/backend/internal/service/systeminstall/package_owner.go) · [升级条件](https://github.com/OrchestratorInc/agent-orchestrator/blob/0d322ae188eff9ad41d50df634fbe78c0af552d2/backend/internal/service/systeminstall/update_gate.go) · [等待空闲](https://github.com/OrchestratorInc/agent-orchestrator/blob/0d322ae188eff9ad41d50df634fbe78c0af552d2/backend/internal/service/systeminstall/update_when_idle.go)

## 6. 首轮联调最值得证明什么

| 故障或条件 | 观察对象 | 通过标准 |
| --- | --- | --- |
| 两个根任务同时结束，中心 dsh 正在处理新消息 | 主对话、两次交付和待汇报记录 | 两份报告都能看见，各出现一次，对话仍按持久化的顺序追加 |
| 修复者测过 A；审查者指出问题后改成 B | 候选、验证和远端引用 | B 必须取得自己的测试和项目要求的复审；报告准确指向远端包含的那个候选 |
| 设备领活或执行后失联；中心和 daemon 分别重启 | 设备账、原进程、未上交结果、当前代次 | 中心展示最后确认时间；补交回执与重新执行业务有不同动作；同一原生会话不发生并发写 |
| 推送成功但响应丢失，期间他人推进目标分支 | 候选 SHA、远端当下引用、祖先关系 | 能按第 4 节分别处理精确命中、已包含、分叉或未知，无须重复推送或覆盖他人工作 |
| 飞书和 Web 同时提交，设备继续离线 | 请示状态、答复人、卡片状态、后续输入 | 只有一次有效决定；重新投递通知不会再次启用任务；设备上线后原会话只消费这条决定一次，执行不明时能对账 |
| 两台机器出现同名目录且版本不同；非 Git 项目产物更新 | 路由、目录、产物摘要、依赖项 | 只在选定机器和目录续接；索引变动后旧说明的验收可识别为过期 |
| 同一 CLI 提供会话累计用量；首条采样或某些字段缺失 | 轮次明细、汇总覆盖范围 | 已知基线的增量只归入该轮一次；缺字段标未知，中心与设备的花费可分别查 |
| 2C2G VPS 下设备断连重报，同时用户打开看板和日志 | 中心常驻与峰值内存、CPU、队列、日志取流、SSE 补读及恢复耗时 | 记录负载和机器条件；验证中心始终能接受新消息、事件无缺口、数据库重启可恢复。容量结论取自 Muster 实测 |

LobeHub 的部署样例包含网关、数据库、缓存、文件存储和可选检索服务。工作台交互与设备选择的做法可单独参考，采用那些界面思路无需把相同服务部署到个人 VPS。设备间直传、小文件落盘去重、公司和家庭设备采用相同传输规则、Windows 上 dsh/Codex 的实际续接，都是 Muster 自己要做的联调。[LobeHub 部署样例](https://github.com/lobehub/lobehub/blob/d86a85bcea15a5e1c3131f1d69bb1c08769a8819/docker-compose/deploy/docker-compose.yml) · [累计用量增量](https://github.com/KKKKhazix/agent-autoloop/blob/d1ec2d8e48de62b2a49bd8dc5985c03482c753ce/worker/usage.ts#L34-L47)
