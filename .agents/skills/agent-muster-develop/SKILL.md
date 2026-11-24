---
name: agent-muster-develop
description: 在 Agent Muster 中开发、审查代码或文档、准备或执行 Git commit 和 push。按改动选择项目检查，Review 最终候选，在授权范围内修复并复验。
---

# Agent Muster 开发规范

先核对当前分支、暂存区和工作区，按仓库规定说明实施方案并在已确认的范围内推进。[仓库规则](../../../AGENTS.md)

| 当前任务 | 阅读入口 |
| --- | --- |
| 开发、修复或重构 | [开发规范](references/development.md) |
| 单独 Review | [开发规范](references/development.md)及[共用 Review 流程](references/git-delivery.md) |
| 准备提交或执行 commit | [提交](references/commit.md)及[共用 Review 流程](references/git-delivery.md) |
| 准备推送或执行 push | [推送](references/push.md)及[共用 Review 流程](references/git-delivery.md) |

执行开发或交付的 agent 调用检查工具，并 Review 行为、注释语义与错误恢复。用户授权范围内的明确问题直接修复，复查最终候选。提交或推送工作流由本 skill 执行。Muster 管理的外部项目沿用其自身交付规则。

先复用项目已有能力，建议参考成熟项目的源码，按当前需求选择简单、可维护的实现。以用户体验为首要依据。界面改动按开发规范核对视觉层级、动效和首次使用流程，有可见变化时在运行界面截图验收。[开发规范](references/development.md)

源码、配置和测试提供实现事实。长期项目约定归 Wiki，操作步骤归本 skill。规则涉及两处时在归属位置修改正文，其余入口链接到正文。[项目 Wiki](../../../.wiki/INDEX.md)
