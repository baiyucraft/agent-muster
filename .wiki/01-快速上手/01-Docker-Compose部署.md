---
title: Docker Compose 自部署
description: 启动、更新、备份和恢复中心服务与原生会话
updated: 2026-11-24
owner: project
---

# Docker Compose 自部署

## 默认部署

在 Linux 服务器安装 Docker Engine 和 Docker Compose v2，将仓库检出到服务器。从仓库根目录进入 `deploy/`，复制配置模板并限制其读取权限。

```sh
cd deploy
umask 077
cp .env.example .env
```

分别运行 `openssl rand -hex 32` 生成 `MUSTER_DSH_PLUGIN_TOKEN` 和 `MUSTER_POSTGRES_PASSWORD`。填写 `DEEPSEEK_API_KEY` 供中心 dsh 调用模型。设备上的 CLI 使用各自的模型设置。[环境变量模板](../../deploy/.env.example)

```sh
docker compose up -d --build
docker compose ps
curl -fsS http://127.0.0.1:13011/healthz
```

默认启动 Hub、中心 dsh 和 PostgreSQL。Hub 提供 API 与 Web，端口默认是 `13011`。`MUSTER_BIND_ADDRESS` 可指定服务器的监听网卡，`MUSTER_HTTP_PORT` 可修改宿主机端口。只有 Hub 发布端口，中心 dsh 经内部网络访问 Hub。[默认入口](../../deploy/compose.yaml) · [内置数据库](../../deploy/compose.override.yaml)

Hub 启动时连接数据库并运行迁移。`/healthz` 返回 `{"status":"ok"}` 表示 HTTP 监听已启动。Compose 另检查数据库与 dsh 监听状态。创建管理员账号后，在工作台发一条普通消息，检查主对话是否收到回复。设备配对和任务交办见[双设备协作](INDEX.md#在-windows-上配置双设备协作)，飞书单聊见[飞书请示](INDEX.md#通过飞书答复同一请示)。

`DSH_HOME` 是 `/var/lib/muster/dsh`，同一卷保存 `muster-center` profile、模型设置和原生会话。已有中心 dsh 设置可随会话卷备份或恢复。

## 设置管理员账号

首次部署或从旧版令牌登录升级后，在服务器上的 `deploy/` 目录运行以下命令，在终端输入并确认密码。用户名由 1 至 64 个英文字母、数字、点、下划线或连字符组成。密码长度为 12 至 1024 个 UTF-8 字节。

```sh
docker compose exec hub muster-hub admin set --username admin
```

在浏览器打开 `http://<服务器 IP>:13011` 并用新账号登录。设备使用同一个 Hub 地址配对。Web 会话自登录起有效 24 小时，刷新页面可继续使用，退出会撤销当前会话。忘记密码时可重新运行同一条命令，重置账号并撤销所有已签发的 Web 会话。

已有设备继续使用各自的配对令牌，无需重新配对。中心 dsh 沿用 `MUSTER_DSH_PLUGIN_TOKEN`。如有使用旧管理员令牌的管理脚本，可在 Hub 保留 `MUSTER_ADMIN_TOKEN`，仅供管理 API 调用。

HTTP 会明文传输密码、会话 Cookie、配对码和设备令牌，请在受信网络内使用。经 HTTPS 反向代理公开 Web 时，在 Hub 环境设置 `MUSTER_ADMIN_COOKIE_SECURE=true`，并让代理保留浏览器请求的 `Host`。如果代理对 Web 请求使用 HTTP Basic 认证，代理验完后应移除该请求的 Basic `Authorization` 标头，使 Hub 可以使用会话 Cookie。登录限速按 Hub 看见的直接连接来源计算，经同一代理访问会共用该来源的额度。

## 使用已有 PostgreSQL

将 `.env` 的 `MUSTER_DATABASE_URL` 设为已有数据库的连接 URL，并使用能在目标数据库中创建及更新表和索引的账号。外部数据库需要允许来自 Hub 容器的连接，URL 中密码的特殊字符需要百分号编码。显示连接信息的命令输出应按凭据处理。

```sh
docker compose -f compose.yaml up -d --build
docker compose -f compose.yaml ps
curl -fsS http://127.0.0.1:13011/healthz
```

显式使用基础文件时，只启动 Hub 和中心 dsh。后续命令都加 `-f compose.yaml`。从已有内置库迁出时，先停止中心写入并完成下文备份，将备份还原到外置库。切换配置时使用原项目名，以便中心 dsh 挂载相同的会话卷。已存在的 PostgreSQL 容器需用默认模式显式停止。

## 更新与运维

更新源码前备份数据库和中心 dsh 卷。检出目标版本后运行 `docker compose up -d --build`，再复查健康接口、对话回复及之前的未完成任务。中心 dsh 可独立通过 `docker compose restart dsh` 重启，主会话沿用原生会话目录。内置 PostgreSQL 大版本升级使用数据库迁移流程。

```sh
docker compose logs --tail 100 hub dsh postgres
docker stats --no-stream $(docker compose ps -q)
```

外接库时，把日志命令中的 `postgres` 去掉并记录数据库运行环境。dsh 启动日志含用于短时浏览器登录的地址，请勿公开。TURN 独立部署时需单独采集它的资源和连接数据。[设备间连接配置](INDEX.md#在-windows-上配置双设备协作)

## 备份与恢复

安排一次维护窗口，等看板中的活动任务完成或进入可恢复状态。停止 Hub 和中心 dsh 以停止中心写入。按同一时间点备份数据库和 dsh 卷，并把 `.env` 存入受控的凭据存储。示例以默认部署为例，在 `deploy/` 下运行。

```sh
set -e
umask 077
backup_dir="$(mktemp -d "$PWD/muster-backup.XXXXXXXX")"
docker compose stop hub dsh
docker compose exec -T postgres pg_dump -U muster -d muster -Fc > "$backup_dir/muster.dump"
docker compose run --rm --no-deps -T --entrypoint tar dsh \
  -C /var/lib/muster/dsh -cf - . > "$backup_dir/dsh.tar"
docker compose start hub dsh
```

外接数据库用数据库服务方支持的 `pg_dump -Fc` 或快照工具，dsh 卷命令继续使用 `-f compose.yaml`。先在单独的 Compose 项目和空数据库演练恢复。保留相同的模型配置和容器内路径，原生会话需要能解析原工作目录。以下是内置库的恢复命令。在新环境的 `deploy/` 下设置已批准的配置，把 `backup_dir` 指向成对的备份，并确认 `muster-restore` 项目此前未启动过。将 `MUSTER_HTTP_PORT` 设为与现有服务不同的值。

```sh
set -e
docker compose -p muster-restore up -d postgres
docker compose -p muster-restore exec -T postgres pg_isready -U muster -d muster
docker compose -p muster-restore exec -T postgres pg_restore -U muster -d muster \
  --no-owner --exit-on-error < "$backup_dir/muster.dump"
docker compose -p muster-restore build dsh
docker compose -p muster-restore run --rm --no-deps -T --entrypoint tar dsh \
  -C /var/lib/muster/dsh -xf - < "$backup_dir/dsh.tar"
```

如果 `pg_isready` 暂时失败，等待 PostgreSQL 就绪再继续数据库恢复。复核目标库和原生会话数据后启动 Hub。清点设备、项目绑定、待派发任务、待投递请示和最近的主对话。正式恢复需要把设备指向确认过的 Hub，按看板核对中断任务与设备留存的原生会话，再恢复待办处理。

## 目标 VPS 验收

在 2 核 2 GB Ubuntu VPS 分别记录空闲、两台设备并行执行和断线后补报的 CPU、内存、API 响应时间及待办积压。使用相同观察时长，多次采集 `docker stats --no-stream` 和工作台请求耗时，记录宿主机峰值内存及服务重启次数。分别重启中心 dsh 和 Hub，核对会话续接、请示恢复及结果补送。外接库和同机 TURN 作为单独的部署条件记录。[阶段验收与容量设计](../../.docs/agent-muster-v0.1.md#10-技术路线部署与验收)

## 事实来源

- [Hub 进程入口](../../cmd/muster-hub/main.go) · [Hub 镜像](../../deploy/Hub.Dockerfile)
- [dsh 镜像](../../deploy/DSH.Dockerfile) · [dsh 初始化脚本](../../deploy/start-dsh.sh)
