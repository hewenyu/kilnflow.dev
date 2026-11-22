package store

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

// auditRow 是 audit_log 中的一行。
type auditRow struct {
	Actor, Action string
	Detail        map[string]any
}

func readAudit(t *testing.T, db *DB) []auditRow {
	t.Helper()
	rows, err := db.Query(t.Context(), `SELECT actor, action, detail FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[auditRow])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecordConfigChange(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	detail := map[string]any{"scope": "alice/app", "name": "TOKEN"}
	original := maps.Clone(detail)
	err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		return RecordConfigChange(t.Context(), tx, 42, "admin", "secret.set", detail)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(detail, original) {
		t.Errorf("不应修改调用方的 detail：%v", detail)
	}
	want := map[string]any{"scope": "alice/app", "name": "TOKEN", "op": "secret.set"}

	var ownerID int64
	var actor string
	var change []byte
	if err := db.QueryRow(t.Context(), `SELECT owner_id, changed_by, change FROM config_versions`).Scan(&ownerID, &actor, &change); err != nil {
		t.Fatalf("读取 config_versions：%v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(change, &got); err != nil {
		t.Fatal(err)
	}
	if ownerID != 42 || actor != "admin" || !reflect.DeepEqual(got, want) {
		t.Errorf("config_versions = (%d, %s, %v)，应为 (42, admin, %v)", ownerID, actor, got, want)
	}

	audit := readAudit(t, db)
	wantAudit := []auditRow{{Actor: "admin", Action: "config.secret.set", Detail: want}}
	if !reflect.DeepEqual(audit, wantAudit) {
		t.Errorf("audit_log = %+v，应为 %+v", audit, wantAudit)
	}
}

// detail 为 nil 时两处记录的都只有 op。
func TestRecordConfigChange_NilDetail(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	if err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		return RecordConfigChange(t.Context(), tx, 1, "cli", "env.delete", nil)
	}); err != nil {
		t.Fatal(err)
	}
	audit := readAudit(t, db)
	if len(audit) != 1 || !reflect.DeepEqual(audit[0].Detail, map[string]any{"op": "env.delete"}) {
		t.Fatalf("audit_log = %+v", audit)
	}
}

// 事务回滚时配置版本和审计日志都不留下。
func TestRecordConfigChange_RolledBackWithTx(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	errAbort := errors.New("后续写入失败")
	err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		if err := RecordConfigChange(t.Context(), tx, 1, "admin", "var.set", map[string]any{"name": "X"}); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("Tx 应返回 f 的错误：%v", err)
	}
	var versions int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM config_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 || len(readAudit(t, db)) != 0 {
		t.Fatalf("回滚后不应留下记录：config_versions %d 行，audit_log %v", versions, readAudit(t, db))
	}
}

func TestAudit(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	type detail struct {
		RunID int64  `json:"run_id"`
		Mode  string `json:"mode"`
	}
	// 可以直接写在连接池上，也可以写在事务中
	if err := Audit(t.Context(), db, "alice", "run.rerun", detail{RunID: 7, Mode: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(t.Context(), func(tx pgx.Tx) error {
		return Audit(t.Context(), tx, "bob", "runner.revoke", map[string]any{})
	}); err != nil {
		t.Fatal(err)
	}
	want := []auditRow{
		{Actor: "alice", Action: "run.rerun", Detail: map[string]any{"run_id": float64(7), "mode": "failed"}},
		{Actor: "bob", Action: "runner.revoke", Detail: map[string]any{}},
	}
	if got := readAudit(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("audit_log = %+v，应为 %+v", got, want)
	}
}
