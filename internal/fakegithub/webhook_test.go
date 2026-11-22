package fakegithub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/github"
)

// delivery 是接收端收到的一个 webhook。
type delivery struct {
	header http.Header
	body   []byte
}

// newReceiver 启动接收 webhook 的服务：签名不对时返回 401，status 非 0 时返回该状态码。
func newReceiver(t *testing.T, secret string, status int) (*httptest.Server, func() []delivery) {
	t.Helper()
	var mu sync.Mutex
	var got []delivery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, delivery{header: r.Header.Clone(), body: body})
		mu.Unlock()
		if !github.VerifySignature([]byte(secret), body, r.Header.Get(github.HeaderSignature)) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		if status != 0 {
			http.Error(w, "rejected by receiver", status)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []delivery {
		mu.Lock()
		defer mu.Unlock()
		return append([]delivery(nil), got...)
	}
}

// webhook 按 GitHub 的格式投递：事件名、delivery ID、HMAC-SHA256 签名和 JSON 内容。
func TestSendWebhook(t *testing.T) {
	srv, got := newReceiver(t, "s3cret", 0)
	payload := map[string]any{"ref": "refs/heads/main", "n": 1}
	if err := SendWebhookDelivery(t.Context(), srv.URL, "s3cret", "push", "d-1", payload); err != nil {
		t.Fatal(err)
	}
	if err := SendWebhook(t.Context(), srv.URL, "s3cret", "pull_request", payload); err != nil {
		t.Fatal(err)
	}
	ds := got()
	if len(ds) != 2 {
		t.Fatalf("收到 %d 个 webhook", len(ds))
	}
	d := ds[0]
	if d.header.Get(github.HeaderEvent) != "push" || d.header.Get(github.HeaderDelivery) != "d-1" || d.header.Get("Content-Type") != "application/json" {
		t.Errorf("头部：%v", d.header)
	}
	var body map[string]any
	if err := json.Unmarshal(d.body, &body); err != nil || body["ref"] != "refs/heads/main" {
		t.Errorf("内容：%s %v", d.body, err)
	}
	if ev, id := ds[1].header.Get(github.HeaderEvent), ds[1].header.Get(github.HeaderDelivery); ev != "pull_request" || !strings.HasPrefix(id, "delivery-") {
		t.Errorf("SendWebhook 的事件和 delivery ID：%q %q", ev, id)
	}
}

func TestSendWebhookErrors(t *testing.T) {
	tests := []struct {
		name    string
		secret  string // 发送方使用的 secret（接收方是 s3cret）
		status  int
		payload any
		wantErr string
	}{
		{"签名不对", "wrong", 0, map[string]any{}, "HTTP 401 bad signature"},
		{"接收方拒绝时带上响应内容", "s3cret", http.StatusInternalServerError, map[string]any{}, "HTTP 500 rejected by receiver"},
		{"内容无法编码", "s3cret", 0, map[string]any{"ch": make(chan int)}, "编码 webhook"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newReceiver(t, "s3cret", tt.status)
			err := SendWebhookDelivery(t.Context(), srv.URL, tt.secret, "push", "d", tt.payload)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("错误应包含 %q，实际 %v", tt.wantErr, err)
			}
		})
	}
	if err := SendWebhook(t.Context(), "http://[::1]:namedport", "s", "push", nil); err == nil || !strings.Contains(err.Error(), "构造 webhook 请求") {
		t.Errorf("无效的地址：%v", err)
	}
}

// decodePayload 用 JSON 往返 payload，按 Server 实际看到的格式检查。
func decodePayload(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// field 按路径读取嵌套的字段，不存在时返回 nil。
func field(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

// payloadFixture 返回带有 installation 888 和其中两个仓库（pub 公开、priv 私有）的 fixture。
func payloadFixture(t *testing.T) (*fixture, *Repo) {
	t.Helper()
	f := newFixture(t)
	f.s.AddInstallation(888)
	pub := &Repo{ID: 2, Owner: "tester", Name: "pub", Dir: f.dir, Installation: 888}
	f.s.AddRepo(&Repo{ID: 3, Owner: "tester", Name: "priv", Dir: f.dir, Installation: 888, Private: true})
	f.s.AddRepo(pub)
	return f, pub
}

// 仓库事件的 payload 带上仓库、installation 和发送者，与 Server 解析的字段一致。
func TestRepoPayloads(t *testing.T) {
	f, pub := payloadFixture(t)
	decode := func(v map[string]any) map[string]any { return decodePayload(t, v) }

	push := decode(f.s.PushPayload(f.repo, "refs/heads/main", "a", "b"))
	if push["ref"] != "refs/heads/main" || push["before"] != "a" || push["after"] != "b" || push["deleted"] != false ||
		field(push, "repository", "full_name") != demo || field(push, "repository", "private") != true ||
		field(push, "installation", "id") != float64(777) || field(push, "sender", "login") != "tester" {
		t.Errorf("push：%v", push)
	}
	repoEv := decode(f.s.RepositoryPayload(pub, "deleted"))
	if repoEv["action"] != "deleted" || field(repoEv, "repository", "id") != float64(2) || field(repoEv, "installation", "id") != float64(888) {
		t.Errorf("repository：%v", repoEv)
	}
	suite := decode(f.s.CheckSuitePayload(f.repo, "rerequested", "abc"))
	if suite["action"] != "rerequested" || field(suite, "check_suite", "head_sha") != "abc" {
		t.Errorf("check_suite：%v", suite)
	}
}

// installation 事件只带简要的仓库信息，按 ID 排序。
func TestInstallationPayloads(t *testing.T) {
	f, pub := payloadFixture(t)
	decode := func(v map[string]any) map[string]any { return decodePayload(t, v) }
	inst := decode(f.s.InstallationPayload("created", 888))
	repos, _ := inst["repositories"].([]any)
	if inst["action"] != "created" || field(inst, "installation", "account", "login") != "tester" || len(repos) != 2 {
		t.Fatalf("installation：%v", inst)
	}
	first, _ := repos[0].(map[string]any)
	if first["full_name"] != "tester/pub" || first["private"] != false || len(first) != 4 {
		t.Errorf("简要仓库信息应只有 id、name、full_name、private，并按 ID 排序：%v", repos)
	}
	if empty, ok := decode(f.s.InstallationPayload("created", 999))["repositories"].([]any); !ok || len(empty) != 0 {
		t.Errorf("没有仓库时应为空数组：%v", empty)
	}

	ir := decode(f.s.InstallationRepositoriesPayload("added", 888, []*Repo{pub}, nil))
	added, _ := ir["repositories_added"].([]any)
	removed, _ := ir["repositories_removed"].([]any)
	if ir["action"] != "added" || ir["repository_selection"] != "selected" || len(added) != 1 || removed == nil || len(removed) != 0 {
		t.Errorf("installation_repositories：%v", ir)
	}
}
