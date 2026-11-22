package db

// 供 db_test 包（需要 pgtest，而 pgtest 依赖本包，只能用外部测试包）使用的内部常量。
const (
	MigrateLockID   = migrateLockID
	DefaultMaxConns = defaultMaxConns
)
