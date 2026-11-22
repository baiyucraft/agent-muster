---
title: Git 协作流程
description: 工作区管理、提交与推送授权，以及变更交付记录
updated: 2026-11-22
owner: project
---

# Git 协作流程

[返回开发指南](INDEX.md)

## 工作区与改动范围

- 修改前确认当前仓库、分支、工作区和暂存区的状态。以用户请求为边界处理文件，保留已有修改与暂存状态。
- 需要隔离并行修改时，在仓库的 `.worktree/` 目录内用特性名创建 worktree。交付后确认其中无须保留的修改和提交，再按仓库约定及时清理。
- 改动完成后检查差异和相关文档引用，按[验证与测试](01-验证与测试.md)记录实际结果。

## 提交与交付

- 提交、推送及创建或合并 Pull Request 分别按当前任务授权执行。提交前检查暂存范围，提交说明写明变更意图。
- 推送前核对目标远端和分支。出现冲突、分叉或远端拒绝时，先确认原因、最新引用与授权范围，再决定如何继续。
- 交付记录包含变更范围与验证结果。若已提交，记录提交 SHA。若已推送，核对远端实际引用。若创建了 Pull Request，附上链接。

## 本地检查

在仓库根目录用 `pnpm install --frozen-lockfile` 安装开发依赖，再运行 `pnpm check` 检查 Go、ESLint、TypeScript、插件、工程工具和文档。按候选另跑 Go 注释检查。命令的工具前置条件和集成测试入口见[验证与测试](01-验证与测试.md#工程检查入口)。

- 提交时 Review 最终暂存内容，修复前保护部分暂存和无关改动，提交后核对提交树。[提交前流程](../../.agents/skills/agent-muster-develop/references/commit.md)
- 推送时 Review 每个目标引用的累计差异及待推历史，推送已审查的 SHA 并回读远端。发现已提交代码需要修复时，按授权建立新候选并复验。[推送前流程](../../.agents/skills/agent-muster-develop/references/push.md)
- 由开发规范 Skill 调用检查工具，核实行为和注释语义。[Review 与交付流程](../../.agents/skills/agent-muster-develop/references/git-delivery.md)

## Muster 管理的项目交付

部署者为接入的 Git 项目配置自动交付规则。项目配置可授权测试通过后自动推送指定目标，也可要求独立 Review。候选 SHA、测试和 Review 证据应相互对应。远端引用的核对方式、返修预算与人工请示条件见[首版设计稿](../../.docs/agent-muster-v0.1.md)第 7.2 节。
