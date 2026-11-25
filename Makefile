# Kiln 的本地构建。正常发布走 .kiln/workflows/release.yml；这里的 dist 用于应急构建（PLAN #21）。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
MOD := github.com/hewenyu/kilnflow.dev
LDFLAGS := -s -w -X $(MOD)/internal/buildinfo.version=$(VERSION)
TARGETS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64
BINS := kiln-server kiln-runner kiln
# build 构建 cmd/ 下的全部命令（kiln-server、kiln-runner、kiln）。

.PHONY: build web test test-go test-web test-orca lint lint-go lint-web lint-orca fmt generate dist clean

# Dashboard 前端（web/，React + Primer）构建到 internal/server/web/ui/dist/，由 go:embed 内嵌进 kiln-server。
# build 不依赖它：没有构建前端时 kiln-server 的 Dashboard 显示提示页；发布（dist、release.yml、镜像）都先构建前端。
web: web/node_modules
	cd web && pnpm build

web/node_modules: web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml
	cd web && pnpm install --frozen-lockfile
	@touch web/node_modules

# AI 执行节点的连接器（kiln-orca/，TypeScript，PLAN #33.3）。
kiln-orca/node_modules: kiln-orca/package.json kiln-orca/pnpm-lock.yaml kiln-orca/pnpm-workspace.yaml
	cd kiln-orca && pnpm install --frozen-lockfile
	@touch kiln-orca/node_modules

build:
	@mkdir -p dist/local
	go build -trimpath -ldflags '$(LDFLAGS)' -o dist/local/ ./cmd/...

test: test-go test-web test-orca

test-go:
	go vet ./...
	# 本地没有 KILN_TEST_DATABASE_URL 时用脚本启动一个共享 Postgres，避免每个包各起一个实例。
	.kiln/scripts/with-test-pg.sh go test -race ./...

test-web: web/node_modules
	cd web && pnpm test

test-orca: kiln-orca/node_modules
	cd kiln-orca && pnpm test

lint: lint-go lint-web lint-orca

# golangci-lint 的版本由 tools/golangci/go.mod 锁定，用 go tool 运行（不用本机安装的版本）；规则见 .golangci.yml。
GOLANGCI_TOOL := -modfile=tools/golangci/go.mod golangci-lint
GOLANGCI_LINT := go tool $(GOLANGCI_TOOL)

# 按每个发布平台（TARGETS）分别检查：只在某个平台上编译的代码（syscall 结构的字段类型、构建标签）只有按那个平台检查才会
# 被发现。golangci-lint 为本机构建（go tool -n 给出它的路径；直接 GOOS=… go tool 会把它编译成目标平台的、无法执行），
# 再带目标平台的 GOOS、GOARCH 运行。
# 各平台并发执行（Go 构建缓存和 golangci-lint 缓存都支持多进程并发，用 --allow-parallel-runners 关掉它默认的
# 单实例文件锁），每个平台输出到独立文件，全部结束后按 TARGETS 顺序打印，任意一个失败都让目标失败。
lint-go:
	@bin=$$(env -u GOOS -u GOARCH go tool -n $(GOLANGCI_TOOL)) || exit 1; \
	tmp=$$(mktemp -d) || exit 1; \
	trap 'rm -rf "$$tmp"' EXIT; \
	i=0; \
	for p in $(TARGETS); do \
		i=$$((i+1)); \
		( GOOS=$${p%/*} GOARCH=$${p#*/} "$$bin" run --allow-parallel-runners ./... >"$$tmp/out.$$i" 2>&1; echo $$? >"$$tmp/status.$$i" ) & \
	done; \
	wait; \
	status=0; i=0; \
	for p in $(TARGETS); do \
		i=$$((i+1)); \
		echo "golangci-lint run ./... (GOOS/GOARCH=$$p)"; \
		cat "$$tmp/out.$$i"; \
		[ "$$(cat "$$tmp/status.$$i")" = 0 ] || status=1; \
	done; \
	rm -rf "$$tmp"; \
	exit $$status
	go mod tidy -diff

# 规则见 web/eslint.config.js，另外做类型检查
lint-web: web/node_modules
	cd web && pnpm lint

lint-orca: kiln-orca/node_modules
	cd kiln-orca && pnpm lint

fmt:
	$(GOLANGCI_LINT) fmt ./...

# Go 的生成代码在 gen/；连接器用到的 runner.v1、airunner.v2 另外生成 TypeScript 到 kiln-orca/src/gen/。
generate: kiln-orca/node_modules
	go tool -modfile=tools/go.mod buf lint
	go tool -modfile=tools/go.mod buf generate
	go tool -modfile=tools/go.mod buf generate --template kiln-orca/buf.gen.yaml --path proto/kiln/runner/v1 --path proto/kiln/airunner/v2

# 与 release.yml 相同：一个二进制一个文件 dist/release/<程序名>-<os>-<arch>，加 SHA256SUMS。
dist: web
	@rm -rf dist/release && mkdir -p dist/release
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "构建 $$t"; \
		for b in $(BINS); do \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/release/$$b-$$os-$$arch ./cmd/$$b || exit 1; \
		done; \
	done
	@cd dist/release && shasum -a 256 * > SHA256SUMS && cat SHA256SUMS

clean:
	rm -rf dist internal/server/web/ui/dist
