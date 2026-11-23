#!/bin/sh
# 本地测试用的共享 Postgres。
#
# 设置 KILN_TEST_DATABASE_URL 时（CI 或自定义环境）原样透传，不启动任何东西。
# 否则本机有 initdb / pg_ctl 时启动一个共享实例：每个包各起一个 Postgres 会同时跑几十个实例、
# 抢 CPU 和内存；共享一个实例时 pgtest 仍然会为每个测试进程建独立的模板库和槽位库，隔离性不变。
# 既没有 KILN_TEST_DATABASE_URL 也没有 initdb 时原样透传，由 pgtest 跳过数据库测试。
#
# 用法：.kiln/scripts/with-test-pg.sh <命令...>
set -eu

if [ -n "${KILN_TEST_DATABASE_URL:-}" ]; then
	exec "$@"
fi

initdb_bin=$(command -v initdb || true)
pgctl_bin=$(command -v pg_ctl || true)
if [ -z "$initdb_bin" ] || [ -z "$pgctl_bin" ]; then
	exec "$@"
fi

# unix socket 路径有长度限制，放在 /tmp 下的短目录里。
dir=$(mktemp -d /tmp/kiln-pg.XXXXXX)
cleanup() {
	"$pgctl_bin" -D "$dir/data" -m immediate stop >/dev/null 2>&1 || true
	rm -rf "$dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"$initdb_bin" -D "$dir/data" -U postgres --auth=trust -E UTF8 --no-locale >/dev/null

# 没有可靠的"取空闲端口"命令，选一个起点后逐个尝试，pg_ctl -w 启动失败就换下一个。
port=$(( ($$ + $(date +%s)) % 20000 + 20000 ))
tries=0
while [ "$tries" -lt 20 ]; do
	if "$pgctl_bin" -D "$dir/data" -o "-p $port -k $dir -c listen_addresses=127.0.0.1 -c fsync=off -c synchronous_commit=off -c full_page_writes=off -c max_connections=1000 -c shared_buffers=256MB" -l "$dir/pg.log" -w start >/dev/null 2>&1; then
		break
	fi
	port=$((port + 1))
	tries=$((tries + 1))
done
if [ "$tries" -ge 20 ]; then
	# cleanup 会删除临时目录，先把日志打出来，否则无从排查启动失败的原因。
	if [ -f "$dir/pg.log" ]; then
		cat "$dir/pg.log" >&2 || true
	fi
	echo "with-test-pg：无法启动 Postgres（临时目录即将清理，上面的日志是最后线索）" >&2
	exit 1
fi

KILN_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:$port/postgres?sslmode=disable"
export KILN_TEST_DATABASE_URL

"$@"
