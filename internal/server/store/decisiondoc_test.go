package store

import (
	"bytes"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/secretbox"
)

// 「待决策清单」配置的存储：信封加密的独立 AAD、代次推进、轮转清空观测数据、CRUD。

const (
	testDecisionDocA = "11111111-1111-4111-8111-111111111111"
	testDecisionDocB = "22222222-2222-4222-8222-222222222222"
	decisionBaseURL  = "https://outline.example.com"
)

func putDecision(t *testing.T, db *DB, repoID int64, docID, token string) {
	t.Helper()
	if err := PutDecisionDoc(t.Context(), db, testMasterKey, DecisionDoc{
		RepoID: repoID, Provider: "outline", BaseURL: decisionBaseURL, DocID: docID,
		CollectionID: "col-decision", CreatedBy: "admin", UpdatedBy: "admin",
	}, token); err != nil {
		t.Fatalf("PutDecisionDoc：%v", err)
	}
}

func TestDecisionDocRoundTripAndAAD(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 10, 1, 100, "alice", "app")
	insertRepo(t, db, 11, 1, 100, "alice", "other")

	putDecision(t, db, 10, testDecisionDocA, "decision-token-abc")

	got, err := GetDecisionDoc(ctx, db, 10)
	if err != nil || got == nil {
		t.Fatalf("GetDecisionDoc = %+v, %v", got, err)
	}
	if got.DocID != testDecisionDocA || got.CollectionID != "col-decision" || got.Generation <= 0 || got.CreatedAt.IsZero() {
		t.Errorf("配置 = %+v", *got)
	}
	tk, err := ResolveDecisionDocToken(ctx, db, testMasterKey, 10)
	if err != nil || tk == nil || tk.Token != "decision-token-abc" {
		t.Fatalf("ResolveDecisionDocToken = %+v, %v", tk, err)
	}

	// 密文不包含明文 token。
	var ciphertext []byte
	if err := db.QueryRow(ctx, `SELECT token_ciphertext FROM repo_decision_docs WHERE repo_id=10`).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("decision-token-abc")) {
		t.Error("密文不应包含明文 token")
	}

	// 独立的 AAD：不能按只读绑定的 AAD 解密，也不能挪到别的仓库。
	var wrapped []byte
	if err := db.QueryRow(ctx, `SELECT token_wrapped_dek FROM repo_decision_docs WHERE repo_id=10`).Scan(&wrapped); err != nil {
		t.Fatal(err)
	}
	if _, err := secretbox.Open(testMasterKey, docTokenAAD(10), secretbox.Sealed{Ciphertext: ciphertext, WrappedDEK: wrapped}); err == nil {
		t.Error("待决策清单 token 不应能按只读绑定的 AAD 解密")
	}
	if _, err := secretbox.Open(testMasterKey, DecisionDocTokenAAD(11), secretbox.Sealed{Ciphertext: ciphertext, WrappedDEK: wrapped}); err == nil {
		t.Error("待决策清单 token 不应能挪到别的仓库解密")
	}

	// 没有配置的仓库返回 nil。
	if d, err := GetDecisionDoc(ctx, db, 11); err != nil || d != nil {
		t.Errorf("未配置仓库 = %+v, %v", d, err)
	}
	if tk, err := ResolveDecisionDocToken(ctx, db, testMasterKey, 11); err != nil || tk != nil {
		t.Errorf("未配置仓库解密 = %+v, %v", tk, err)
	}
}

func TestDecisionDocGenerationAndRotation(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 12, 1, 100, "alice", "app")

	putDecision(t, db, 12, testDecisionDocA, "token-1")
	first, _ := GetDecisionDoc(ctx, db, 12)
	if first == nil {
		t.Fatal("应读到配置")
	}

	// 只轮换 token（同文档）：推进代次，保留观测数据。
	if err := UpdateDecisionDocSize(ctx, db, 12, first.Generation, 300_000, "doc_near_limit"); err != nil {
		t.Fatal(err)
	}
	putDecision(t, db, 12, testDecisionDocA, "token-2")
	second, _ := GetDecisionDoc(ctx, db, 12)
	if second == nil || second.Generation <= first.Generation {
		t.Fatalf("轮换 token 后代次应推进：%+v", second)
	}
	if second.CreatedBy != first.CreatedBy || !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("覆盖时应保留创建信息：%+v", *second)
	}
	if second.ObservedUnits == nil || *second.ObservedUnits != 300_000 || second.WarningCode != "doc_near_limit" {
		t.Errorf("只轮换 token 不应清空观测数据：%+v", *second)
	}

	// 轮换文档：代次推进，清空观测与告警。
	putDecision(t, db, 12, testDecisionDocB, "token-3")
	third, _ := GetDecisionDoc(ctx, db, 12)
	if third == nil || third.Generation <= second.Generation || third.DocID != testDecisionDocB {
		t.Fatalf("轮换文档后 = %+v", third)
	}
	if third.ObservedUnits != nil || third.ObservedAt != nil || third.WarningCode != "" || third.WarnedAt != nil {
		t.Errorf("轮换文档应清空观测与告警：%+v", *third)
	}

	// 清除后再设置：代次不复用旧值。
	if ok, err := DeleteDecisionDoc(ctx, db, 12); err != nil || !ok {
		t.Fatalf("DeleteDecisionDoc = %t, %v", ok, err)
	}
	if ok, err := DeleteDecisionDoc(ctx, db, 12); err != nil || ok {
		t.Errorf("重复删除 = %t, %v", ok, err)
	}
	putDecision(t, db, 12, testDecisionDocA, "token-4")
	fourth, _ := GetDecisionDoc(ctx, db, 12)
	if fourth == nil || fourth.Generation <= third.Generation {
		t.Errorf("清除后再设置的代次应继续递增：%+v", fourth)
	}

	// 列表按仓库排序。
	list, err := ListDecisionDocs(ctx, db)
	if err != nil || len(list) != 1 || list[0].RepoID != 12 {
		t.Fatalf("ListDecisionDocs = %+v, %v", list, err)
	}
}

func TestDecisionDocForShare(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 13, 1, 100, "alice", "app")
	putDecision(t, db, 13, testDecisionDocA, "token")

	var got *DecisionDoc
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = GetDecisionDocForShare(ctx, tx, 13)
		return err
	})
	if err != nil || got == nil || got.DocID != testDecisionDocA {
		t.Fatalf("GetDecisionDocForShare = %+v, %v", got, err)
	}
}

func TestUpdateDecisionDocSizeGuardsGeneration(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	insertRepo(t, db, 14, 1, 100, "alice", "app")
	putDecision(t, db, 14, testDecisionDocA, "token")
	first, _ := GetDecisionDoc(ctx, db, 14)

	// 旧的代次不生效。
	if err := UpdateDecisionDocSize(ctx, db, 14, first.Generation+1, 123, "doc_near_limit"); err != nil {
		t.Fatal(err)
	}
	if d, _ := GetDecisionDoc(ctx, db, 14); d.ObservedUnits != nil {
		t.Errorf("过期的代次不应写入观测数据：%+v", d)
	}
	// 当前代次生效。
	if err := UpdateDecisionDocSize(ctx, db, 14, first.Generation, 123, ""); err != nil {
		t.Fatal(err)
	}
	if d, _ := GetDecisionDoc(ctx, db, 14); d.ObservedUnits == nil || *d.ObservedUnits != 123 || d.WarningCode != "" {
		t.Errorf("观测数据 = %+v", d)
	}
}

// base_url 的规范化让写法差异不能绕过"只读绑定与待决策清单不能同集合"的检查。
func TestNormalizeDocBaseURL(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://outline.example.com":        "https://outline.example.com",
		"https://OUTLINE.example.com:443/":   "https://outline.example.com",
		"  https://Outline.Example.com///  ": "https://outline.example.com",
		"http://outline.example.com:80":      "http://outline.example.com",
		"http://outline.example.com:8080/":   "http://outline.example.com:8080",
		"https://outline.example.com:80":     "https://outline.example.com:80",
		"https://outline.example.com/wiki/":  "https://outline.example.com/wiki",
		// IPv6 字面量：Hostname() 会剥掉方括号，去掉默认端口后必须补回来，否则结果不是合法地址。
		"http://[::1]":            "http://[::1]",
		"http://[::1]:80/":        "http://[::1]",
		"https://[::1]:443/":      "https://[::1]",
		"https://[::1]:8443/":     "https://[::1]:8443",
		"http://[::1]:8080":       "http://[::1]:8080",
		"https://[2001:DB8::1]/x": "https://[2001:db8::1]/x",
		// zone（RFC 6874 在 URL 里写作 %25）。
		"http://[fe80::1%25en0]:80/":  "http://[fe80::1%25en0]",
		"http://[fe80::1%25en0]:9000": "http://[fe80::1%25en0]:9000",
	}
	for in, want := range cases {
		got := NormalizeDocBaseURL(in)
		if got != want {
			t.Errorf("NormalizeDocBaseURL(%q) = %q，期望 %q", in, got, want)
		}
		// 二次调用必须幂等（比较两边的写法差异时可能反复规范化）。
		if again := NormalizeDocBaseURL(got); again != got {
			t.Errorf("NormalizeDocBaseURL(%q) 不幂等：%q → %q", in, got, again)
		}
	}
	if NormalizeDocBaseURL("not a url") != "not a url" {
		t.Error("无法解析时只应去掉尾部斜杠")
	}
}
