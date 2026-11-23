FROM node:22.23.2-bookworm-slim AS web
WORKDIR /src
RUN npm install --global pnpm@12.3.4
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY web/package.json web/
COPY plugins/muster-dsh/package.json plugins/muster-dsh/
COPY cmd/muster-daemon/frontend/package.json cmd/muster-daemon/frontend/
RUN pnpm --filter @agent-muster/web install --frozen-lockfile
COPY web/ web/
COPY ui/ ui/
RUN pnpm --filter @agent-muster/web build

FROM golang:1.26.0-bookworm AS hub
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/muster-hub/ cmd/muster-hub/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -mod=readonly -o /out/muster-hub ./cmd/muster-hub

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=hub /out/muster-hub /usr/local/bin/muster-hub
COPY --from=web /src/web/dist/ /opt/muster/web/
USER 65534:65534
EXPOSE 13011
ENTRYPOINT ["muster-hub", "-listen", "0.0.0.0:13011", "-web-dir", "/opt/muster/web"]
