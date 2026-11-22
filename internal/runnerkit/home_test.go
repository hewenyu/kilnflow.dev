package runnerkit

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDataDir(t *testing.T) {
	t.Setenv(testProgram.DirEnv, "/data/kiln")
	if d := testProgram.DataDir(); d != "/data/kiln" {
		t.Errorf("DataDir = %q", d)
	}
	t.Setenv(testProgram.DirEnv, "")
	home, _ := os.UserHomeDir()
	if d := testProgram.DataDir(); d != filepath.Join(home, "kiln") {
		t.Errorf("DataDir = %q", d)
	}
}

func TestServerURL(t *testing.T) {
	t.Parallel()
	st := &State{Server: "https://enrolled.example.com"}
	if u := (&Common{}).ServerURL(st); u != st.Server {
		t.Errorf("config 没有 server 时用注册时的地址：%q", u)
	}
	if u := (&Common{Server: "https://override.example.com/"}).ServerURL(st); u != "https://override.example.com" {
		t.Errorf("config 中的 server 优先：%q", u)
	}
}

func TestLoadState(t *testing.T) {
	t.Parallel()
	h := &Home{Dir: t.TempDir(), Prog: testProgram}
	if _, err := h.LoadState(); err == nil || !strings.Contains(err.Error(), "kiln-runner enroll") {
		t.Errorf("未注册时 LoadState = %v", err)
	}
	mustWrite(t, h.Path(stateFileName), "{not json")
	if _, err := h.LoadState(); err == nil || !strings.Contains(err.Error(), "解析") {
		t.Errorf("文件损坏时 LoadState = %v", err)
	}
	want := State{Server: "https://s", RunnerID: 9, Name: "n", Pool: "p"}
	if err := h.SaveState(&want); err != nil {
		t.Fatal(err)
	}
	got, err := h.LoadState()
	if err != nil || *got != want {
		t.Errorf("LoadState = %+v, %v", got, err)
	}
}

// 私钥：第一次生成并以 600 权限保存，之后读回同一个；格式不对时报错。
func TestKey(t *testing.T) {
	t.Parallel()
	h := &Home{Dir: t.TempDir(), Prog: testProgram}
	if _, err := h.Key(false); err == nil {
		t.Error("没有私钥且不创建时应报错")
	}
	k1, err := h.Key(true)
	if err != nil {
		t.Fatal(err)
	}
	p := h.Path(keysDirName, keyFileName)
	for _, f := range []string{p, filepath.Dir(p)} {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if st.IsDir() {
			want = 0o700
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s 的权限 %o，期望 %o", f, st.Mode().Perm(), want)
		}
	}
	k2, err := h.Key(true)
	if err != nil {
		t.Fatal(err)
	}
	if !k1.Equal(k2) {
		t.Error("再次读取得到了不同的私钥")
	}
	for _, content := range []string{"not hex", "abcd"} {
		mustWrite(t, p, content)
		if _, err := h.Key(true); err == nil || !strings.Contains(err.Error(), "格式不正确") {
			t.Errorf("私钥 %q：%v", content, err)
		}
	}
	if len(k1) != ed25519.PrivateKeySize {
		t.Errorf("私钥长度 %d", len(k1))
	}
}

func TestSetDrain(t *testing.T) {
	t.Parallel()
	h := &Home{Dir: t.TempDir(), Prog: testProgram}
	if h.Draining() {
		t.Fatal("初始不应 drain")
	}
	if err := h.SetDrain(true); err != nil || !h.Draining() {
		t.Fatalf("SetDrain(true) = %v，Draining %v", err, h.Draining())
	}
	if err := h.SetDrain(false); err != nil || h.Draining() {
		t.Fatalf("SetDrain(false) = %v，Draining %v", err, h.Draining())
	}
	if err := h.SetDrain(false); err != nil {
		t.Fatalf("重复清除 drain：%v", err)
	}
}

// 首次启动时多个调用者同时生成私钥：只有一个写入，所有调用者拿到同一把（不会互相覆盖，覆盖会让先写入的一方失去身份）。
func TestKeyConcurrentCreate(t *testing.T) {
	t.Parallel()
	h := &Home{Dir: t.TempDir(), Prog: testProgram}
	const n = 8
	keys := make([]ed25519.PrivateKey, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { keys[i], errs[i] = h.Key(true) })
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Key(%d) = %v", i, errs[i])
		}
		if !keys[i].Equal(keys[0]) {
			t.Fatalf("并发生成的私钥不一致：%d", i)
		}
	}
}
