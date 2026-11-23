#!/bin/sh
set -eu

umask 077
profile=muster-center
profile_dir="$DSH_HOME/profiles/$profile"
mkdir -p "$MUSTER_DSH_WORKSPACE"

# 首次初始化会替换整个 profile，后续启动沿用卷中的配置和会话。
if [ ! -f "$profile_dir/package.json" ]; then
  dsh "$profile" --from-default-profile web --dump-config >/dev/null
fi

# 插件链接指向镜像内的固定位置。中断后缺少链接时可安全重试。
if [ ! -f "$profile_dir/node_modules/muster-dsh/dist/index.js" ]; then
  dsh plugin --profile "$profile" add link:/opt/muster/muster-dsh
fi

cd "$MUSTER_DSH_WORKSPACE"
exec dsh --profile "$profile" --host 127.0.0.1 --port 3080 --no-open
