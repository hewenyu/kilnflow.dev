#!/usr/bin/env bash
# release.yml 的 build 使用：在本平台编译参数中的每个二进制到 dist/<程序名>，
# 然后直接运行每个产物，核对平台和版本（不接受只能检查字符串的异平台产物）。
# 环境变量 TARGETOS、TARGETARCH、VERSION 由 job 的 env 提供（矩阵变体和 tag）。
# 用法：bash .kiln/scripts/release-build.sh kiln-server kiln-runner kiln
set -euo pipefail

[ $# -gt 0 ] || { echo "用法：$0 <程序名>..."; exit 2; }
: "${TARGETOS:?}" "${TARGETARCH:?}" "${VERSION:?}"

# kiln-server 内嵌 Dashboard 前端（web.dist）
for b in "$@"; do
  if [ "${b}" = kiln-server ]; then
    test -f internal/server/web/ui/dist/index.html || { echo "没有取到 Dashboard 前端（web.dist）"; exit 1; }
  fi
done

expected="${TARGETOS}/${TARGETARCH}"
host="$(go env GOHOSTOS)/$(go env GOHOSTARCH)"
target="$(go env GOOS)/$(go env GOARCH)"
[ "${host}" = "${expected}" ] && [ "${target}" = "${expected}" ] || { echo "必须在 ${expected} 编译：Go host=${host} target=${target}"; exit 1; }

M=github.com/hewenyu/kilnflow.dev
mkdir -p dist
for b in "$@"; do
  go build -trimpath -ldflags "-s -w -X $M/internal/buildinfo.version=${VERSION}" -o "dist/${b}" "./cmd/${b}"
done

for b in "$@"; do
  info=$(go version -m "dist/${b}")
  for kv in "GOOS=${TARGETOS}" "GOARCH=${TARGETARCH}" "CGO_ENABLED=0"; do
    grep -Eq "^[[:space:]]+build[[:space:]]+${kv}\$" <<<"$info" || { echo "dist/${b} 不是 ${kv}"; exit 1; }
  done
  got=$("dist/${b}" version)
  [ "${got}" = "${b} ${VERSION}" ] || { echo "dist/${b} version 输出 ${got}，应为 ${b} ${VERSION}"; exit 1; }
done

ls -l dist
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum dist/*
else
  shasum -a 256 dist/*
fi
