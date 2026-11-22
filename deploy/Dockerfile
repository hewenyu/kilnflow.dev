# kiln-server 镜像（在仓库根目录执行：docker build -f deploy/Dockerfile -t kiln-server .）

# Dashboard 前端（web/）：构建到 internal/server/web/ui/dist/，下面的 go build 把它内嵌进 kiln-server。
FROM node:26-bookworm-slim AS web
RUN npm install --global pnpm@12.14.0
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/server/web/ui/dist ./internal/server/web/ui/dist
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
# 构建阶段使用目标平台镜像；禁止通过设置 GOOS / GOARCH 交叉编译。
RUN test "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" = "${TARGETOS}/${TARGETARCH}"
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/hewenyu/kilnflow.dev/internal/buildinfo.version=${VERSION}" -o /kiln-server ./cmd/kiln-server

FROM gcr.io/distroless/static-debian12
COPY --from=build /kiln-server /kiln-server
ENV KILN_LISTEN=:8080 KILN_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/kiln-server"]
