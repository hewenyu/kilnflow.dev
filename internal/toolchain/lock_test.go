package toolchain

import (
	"strings"
	"testing"
)

func TestParseLockErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, data, want string
	}{
		{name: "不是 TOML", data: "[go\n", want: "解析 .kiln/runtimes.lock"},
		{name: "缺少 version", data: "[go]\n\"linux/amd64\" = \"sha256:aa\"\n", want: "[go] 缺少 version"},
		{name: "值不是字符串", data: "[go]\nversion = 1\n", want: "[go].version 应为字符串"},
		{name: "digest 没有前缀", data: "[go]\nversion = \"1.24.4\"\n\"linux/amd64\" = \"aa\"\n", want: "应为 sha256:…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseLock([]byte(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseLock = %v，期望包含 %q", err, tc.want)
			}
		})
	}
	// 空文件是合法的空 lockfile
	if l, err := ParseLock(nil); err != nil || len(l.Entries) != 0 {
		t.Fatalf("ParseLock(空) = %+v, %v", l, err)
	}
}

func TestLockResolve(t *testing.T) {
	t.Parallel()
	l := &Lock{Entries: map[string]*LockEntry{
		"go": {Name: "go", Version: "1.24.4", Platforms: map[string]string{"linux/arm64": "sha256:aa", "darwin/arm64": "sha256:bb"}},
	}}
	cases := []struct {
		name, runtime, platform string
		lock                    *Lock
		wantPlat, wantDigest    string
		wantErr                 string
	}{
		{name: "精确平台", lock: l, runtime: "go", platform: "darwin/arm64", wantPlat: "darwin/arm64", wantDigest: "sha256:bb"},
		{name: "去掉 abi", lock: l, runtime: "go", platform: "linux/arm64/musl", wantPlat: "linux/arm64", wantDigest: "sha256:aa"},
		{name: "没有 lockfile", lock: nil, runtime: "go", platform: "linux/arm64", wantErr: "仓库中没有 .kiln/runtimes.lock"},
		{name: "runtime 不在 lockfile 中", lock: l, runtime: "node", platform: "linux/arm64", wantErr: "runtime node 不在"},
		{name: "缺少平台", lock: l, runtime: "go", platform: "linux/amd64", wantErr: "没有平台 linux/amd64 的条目"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, p, d, err := tc.lock.Resolve(tc.runtime, tc.platform)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Resolve = %v，期望包含 %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || v != "1.24.4" || p != tc.wantPlat || d != tc.wantDigest {
				t.Fatalf("Resolve = %q %q %q, %v，期望 1.24.4 %q %q", v, p, d, err, tc.wantPlat, tc.wantDigest)
			}
		})
	}
}

// Format 的输出稳定：键排序，与 map 的遍历顺序无关。
func TestLockFormatStable(t *testing.T) {
	t.Parallel()
	l := &Lock{Entries: map[string]*LockEntry{
		"node": {Name: "node", Version: "22.11.0", Platforms: map[string]string{"linux/amd64": "sha256:cc", "darwin/arm64": "sha256:dd"}},
		"go":   {Name: "go", Version: "1.24.4", Platforms: map[string]string{"linux/arm64": "sha256:aa"}},
	}}
	got := string(l.Format())
	for range 10 {
		if again := string(l.Format()); again != got {
			t.Fatalf("两次输出不同：\n%s\n%s", got, again)
		}
	}
	_, body, ok := strings.Cut(got, "\n[go]")
	if !ok {
		t.Fatalf("Format 中没有 [go]：\n%s", got)
	}
	want := "\nversion = \"1.24.4\"\n\"linux/arm64\" = \"sha256:aa\"\n" +
		"\n[node]\nversion = \"22.11.0\"\n\"darwin/arm64\" = \"sha256:dd\"\n\"linux/amd64\" = \"sha256:cc\"\n"
	if body != want {
		t.Errorf("Format =\n%s\n期望\n%s", body, want)
	}
}
