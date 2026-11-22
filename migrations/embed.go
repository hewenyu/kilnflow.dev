// Package migrations 内嵌数据库迁移文件。
package migrations

import "embed"

// FS 包含全部迁移文件（*.sql）。文件名以版本号开头（如 0001_init.sql），
// 由 internal/db 按版本号顺序执行。
//
//go:embed *.sql
var FS embed.FS
