FROM node:22.23.2-bookworm-slim AS plugin
WORKDIR /src
RUN npm install --global pnpm@12.3.4
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY web/package.json web/
COPY plugins/muster-dsh/package.json plugins/muster-dsh/
COPY cmd/muster-daemon/frontend/package.json cmd/muster-daemon/frontend/
RUN pnpm --filter muster-dsh install --frozen-lockfile
COPY plugins/muster-dsh/ plugins/muster-dsh/
RUN pnpm --filter muster-dsh build \
    && pnpm --filter muster-dsh --prod deploy --legacy /out/muster-dsh

FROM node:22.23.2-bookworm-slim
RUN npm install --global pnpm@12.3.4 @deepseek-ai/dsh@0.2.1-rc.5 \
    && npm cache clean --force \
    && mkdir -p /var/lib/muster/dsh \
    && chown node:node /var/lib/muster/dsh
COPY --from=plugin /out/muster-dsh/ /opt/muster/muster-dsh/
COPY deploy/start-dsh.sh /usr/local/bin/start-muster-dsh
ENV DSH_HOME=/var/lib/muster/dsh \
    DSH_TELEMETRY_MODE=DISABLED \
    MUSTER_DSH_WORKSPACE=/var/lib/muster/dsh/workspace
USER node
WORKDIR /var/lib/muster/dsh
ENTRYPOINT ["/bin/sh", "/usr/local/bin/start-muster-dsh"]
