package store

import (
	"context"
	"maps"

	"github.com/jackc/pgx/v5"
)

// RecordConfigChange 在事务中记录一次配置变更（config_versions）并写审计日志（action 为 config.<op>）。
// 两处记录的内容都是 detail 加上 "op": op。
func RecordConfigChange(ctx context.Context, tx pgx.Tx, ownerID int64, actor, op string, detail map[string]any) error {
	change := make(map[string]any, len(detail)+1)
	maps.Copy(change, detail)
	change["op"] = op
	_, err := tx.Exec(ctx, `INSERT INTO config_versions (owner_id, change, changed_by) VALUES ($1,$2,$3)`, ownerID, MustJSON(change), actor)
	if err != nil {
		return err
	}
	return Audit(ctx, tx, actor, "config."+op, change)
}

// Audit 写一条审计日志。
func Audit(ctx context.Context, q Execer, actor, action string, detail any) error {
	_, err := q.Exec(ctx, `INSERT INTO audit_log (actor, action, detail) VALUES ($1,$2,$3)`, actor, action, MustJSON(detail))
	return err
}
