package runner

import (
	"strings"
	"testing"
)

// doctorItems 运行 Doctor，返回 检查项 → 是否通过 和总结果。
func doctorItems(t *testing.T, cfg *Config) (map[string]bool, map[string]string, bool) {
	t.Helper()
	ok := map[string]bool{}
	detail := map[string]string{}
	all := Doctor(t.Context(), cfg, DoctorOptions{}, func(pass bool, item, d string) {
		ok[item] = pass
		detail[item] = d
	})
	return ok, detail, all
}

// doctor：未注册时不通过；注册后检查私钥和与 Server 的连接。
func TestDoctor(t *testing.T) {
	t.Parallel()
	t.Run("未注册", func(t *testing.T) {
		t.Parallel()
		ok, detail, all := doctorItems(t, &Config{Dir: t.TempDir(), Docker: dockerDisabled})
		if all || ok["注册"] || !strings.Contains(detail["注册"], "还没有注册") {
			t.Errorf("all = %v，注册 = %v（%s）", all, ok["注册"], detail["注册"])
		}
		if !ok["git"] || !ok["bash"] {
			t.Errorf("git = %v，bash = %v", ok["git"], ok["bash"])
		}
		if _, checked := ok["docker（oci executor，可选）"]; checked {
			t.Error("关闭 docker 时不检查 docker")
		}
		for _, item := range []string{"平台", "executor", "资源", "宿主机指纹"} {
			if _, reported := ok[item]; !reported {
				t.Errorf("没有报告 %s", item)
			}
		}
	})
	t.Run("已注册", func(t *testing.T) {
		t.Parallel()
		fs := newFakeServer(t)
		docker, _ := fakeDocker(t)
		cfg := &Config{Dir: t.TempDir(), Docker: docker}
		if _, err := Enroll(t.Context(), cfg, fs.URL(), "enroll-token", "mini"); err != nil {
			t.Fatal(err)
		}
		ok, detail, _ := doctorItems(t, cfg)
		if !ok["注册"] || !ok["连接 Server"] || detail["连接 Server"] != fs.URL() {
			t.Errorf("注册 = %v，连接 Server = %v（%s）", ok["注册"], ok["连接 Server"], detail["连接 Server"])
		}
		if !ok["docker（oci executor，可选）"] || !strings.Contains(detail["executor"], "oci") {
			t.Errorf("docker = %v，executor = %s", ok["docker（oci executor，可选）"], detail["executor"])
		}
		// Server 拒绝认证
		fs.mu.Lock()
		fs.revoked = true
		fs.mu.Unlock()
		ok, _, all := doctorItems(t, cfg)
		if ok["连接 Server"] || all {
			t.Errorf("被吊销时连接 Server = %v，all = %v", ok["连接 Server"], all)
		}
	})
	t.Run("缺少私钥", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{Dir: t.TempDir(), Docker: dockerDisabled}
		if err := cfg.home().SaveState(&State{Server: "http://127.0.0.1:1", RunnerID: 1}); err != nil {
			t.Fatal(err)
		}
		ok, _, all := doctorItems(t, cfg)
		if !ok["注册"] || ok["私钥"] || all {
			t.Errorf("注册 = %v，私钥 = %v，all = %v", ok["注册"], ok["私钥"], all)
		}
	})
}
