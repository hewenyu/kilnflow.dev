package oci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/oci"
	"github.com/hewenyu/kilnflow.dev/internal/oci/ocitest"
)

// memLayout 把 ocitest.Layout 转换成 LoadLayout 的输入：文件表和按 digest 读取的函数。
func memLayout(l *ocitest.Layout) (map[string]oci.File, oci.Opener) {
	files := map[string]oci.File{}
	byDigest := map[string][]byte{}
	for p, b := range l.Files {
		d := oci.Digest(b)
		files[p] = oci.File{Digest: d, Size: int64(len(b))}
		byDigest[d] = b
	}
	open := func(_ context.Context, d string) (io.ReadCloser, error) {
		b, ok := byDigest[d]
		if !ok {
			return nil, fmt.Errorf("没有 %s", d)
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	return files, open
}

func loadLayout(t *testing.T, l *ocitest.Layout) *oci.Image {
	t.Helper()
	files, open := memLayout(l)
	img, err := oci.LoadLayout(t.Context(), files, open)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestLoadLayout(t *testing.T) {
	l := ocitest.NewLayout("linux", "arm64", "hi")
	img := loadLayout(t, l)
	if img.Root.Digest != l.Root || img.Root.MediaType != oci.MediaTypeOCIManifest {
		t.Fatalf("root = %+v，应为 %s", img.Root, l.Root)
	}
	if img.Platform == nil || img.Platform.String() != "linux/arm64" {
		t.Fatalf("platform = %v", img.Platform)
	}
	if len(img.Blobs) != 2 || len(img.Manifests) != 1 || img.RootManifest().Digest != l.Root {
		t.Fatalf("blobs %d manifests %d", len(img.Blobs), len(img.Manifests))
	}
}

func TestLoadLayoutRejectsBrokenLayouts(t *testing.T) {
	blob := func(d string) string { return "blobs/sha256/" + strings.TrimPrefix(d, "sha256:") }
	cases := []struct {
		name   string
		mutate func(l *ocitest.Layout)
		want   string
	}{
		{"缺少 oci-layout", func(l *ocitest.Layout) { delete(l.Files, "oci-layout") }, "缺少 oci-layout"},
		{"版本不对", func(l *ocitest.Layout) { l.Files["oci-layout"] = []byte(`{"imageLayoutVersion":"2.0.0"}`) }, "不受支持"},
		{"缺少 layer", func(l *ocitest.Layout) { delete(l.Files, blob(l.Layer)) }, "缺少 " + blob("")},
		{"layer 内容被改", func(l *ocitest.Layout) { l.Files[blob(l.Layer)] = []byte("tampered") }, "内容与文件名不一致"},
		{"manifest 内容被改", func(l *ocitest.Layout) {
			l.Files[blob(l.Root)] = bytes.Replace(l.Files[blob(l.Root)], []byte("layers"), []byte("Layers"), 1)
		}, "内容与文件名不一致"},
		{"多个镜像", func(l *ocitest.Layout) {
			var idx map[string]any
			_ = json.Unmarshal(l.Files["index.json"], &idx)
			ms, _ := idx["manifests"].([]any)
			idx["manifests"] = append(ms, ms[0])
			l.Files["index.json"], _ = json.Marshal(idx)
		}, "一个输出只能包含一个镜像"},
		{"大小不一致", func(l *ocitest.Layout) {
			l.Files["index.json"] = bytes.Replace(l.Files["index.json"], []byte(`"size":`), []byte(`"size":1`), 1)
		}, "大小"},
		{"不支持的类型", func(l *ocitest.Layout) {
			l.Files["index.json"] = bytes.Replace(l.Files["index.json"], []byte("image.manifest.v1+json\",\"size"), []byte("foo+json\",\"size"), 1)
		}, "不支持的 manifest 类型"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := ocitest.NewLayout("linux", "arm64", "hi")
			c.mutate(l)
			files, open := memLayout(l)
			_, err := oci.LoadLayout(t.Context(), files, open)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v，应包含 %q", err, c.want)
			}
			if !strings.Contains(c.want, "缺少 oci-layout") && !errors.Is(err, oci.ErrInvalidLayout) {
				t.Errorf("应为 ErrInvalidLayout：%v", err)
			}
		})
	}
}

// TestLoadLayoutCorruptBlobContent 模拟存储中的内容损坏：文件表声明的 digest 正确，读出的内容不对。
func TestLoadLayoutCorruptBlobContent(t *testing.T) {
	l := ocitest.NewLayout("linux", "arm64", "hi")
	files, open := memLayout(l)
	corrupt := func(ctx context.Context, d string) (io.ReadCloser, error) {
		if d == l.Layer {
			return io.NopCloser(strings.NewReader("bitrot")), nil
		}
		return open(ctx, d)
	}
	_, err := oci.LoadLayout(t.Context(), files, corrupt)
	if !errors.Is(err, oci.ErrInvalidLayout) || !strings.Contains(err.Error(), "内容损坏") {
		t.Fatalf("err = %v", err)
	}
}

func TestMerge(t *testing.T) {
	a := loadLayout(t, ocitest.NewLayout("linux", "arm64", "a"))
	b := loadLayout(t, ocitest.NewLayout("linux", "amd64", "b"))
	one, err := oci.Merge([]*oci.Image{a})
	if err != nil || one != a {
		t.Fatalf("单个镜像应原样返回：%v", err)
	}
	m, err := oci.Merge([]*oci.Image{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if m.Root.MediaType != oci.MediaTypeOCIIndex || m.Platform != nil || len(m.Blobs) != 4 || len(m.Manifests) != 3 {
		t.Fatalf("合并结果不对：%+v", m.Root)
	}
	var idx struct {
		Manifests []oci.Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(m.RootManifest().Body, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 2 || idx.Manifests[0].Platform.String() != "linux/amd64" {
		t.Fatalf("index 应按平台排序：%+v", idx.Manifests)
	}
	m2, err := oci.Merge([]*oci.Image{b, a})
	if err != nil || m2.Root.Digest != m.Root.Digest {
		t.Fatalf("合并结果应与顺序无关：%v", err)
	}
	if _, err := oci.Merge([]*oci.Image{a, a}); err == nil || !strings.Contains(err.Error(), "linux/arm64") {
		t.Fatalf("平台重复应报错：%v", err)
	}
	if _, err := oci.Merge([]*oci.Image{m, a}); err == nil {
		t.Fatal("已经是 index 的镜像不能再合并")
	}
}

func TestParseName(t *testing.T) {
	cases := []struct{ in, want, host, err string }{
		{"docker.io/example/kiln-server", "docker.io/example/kiln-server", "registry-1.docker.io", ""},
		{"docker.io/nginx", "docker.io/library/nginx", "registry-1.docker.io", ""},
		{"index.docker.io/a/b", "docker.io/a/b", "registry-1.docker.io", ""},
		{"ghcr.io/o/r/sub", "ghcr.io/o/r/sub", "ghcr.io", ""},
		{"127.0.0.1:5000/kiln/x", "127.0.0.1:5000/kiln/x", "127.0.0.1:5000", ""},
		{"registry:5000/org/app", "registry:5000/org/app", "registry:5000", ""},
		{"[::1]:5000/a/b", "[::1]:5000/a/b", "[::1]:5000", ""},
		{"[::1]/a/b", "[::1]/a/b", "[::1]", ""},
		{"localhost/x", "localhost/x", "localhost", ""},
		{"[example.com]:5000/a/b", "", "", "必须以 registry 开头"},
		{"[127.0.0.1]:5000/a/b", "", "", "必须以 registry 开头"},
		{"ghcr.io:+443/a/b", "", "", "必须以 registry 开头"},
		{"ghcr.io:-1/a/b", "", "", "必须以 registry 开头"},
		{"ghcr.io:0/a/b", "", "", "必须以 registry 开头"},
		{"ghcr.io:65536/a/b", "", "", "必须以 registry 开头"},
		{"example/kiln", "", "", "必须以 registry 开头"},
		{"docker.io/a/b:v1", "", "", "不能带 tag"},
		{"docker.io/A/b", "", "", "无效"},
		{"docker.io//b", "", "", "无效"},
	}
	for _, c := range cases {
		n, err := oci.ParseName(c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s：err = %v，应包含 %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || n.String() != c.want || n.Host != c.host {
			t.Errorf("%s → %+v %v", c.in, n, err)
		}
	}
	if n, _ := oci.ParseName("127.0.0.1:5000/a"); !n.Insecure() {
		t.Error("本机 registry 应使用 http")
	}
	if n, _ := oci.ParseName("docker.io/a/b"); n.Insecure() {
		t.Error("Docker Hub 必须使用 https")
	}
}

func TestCheckTag(t *testing.T) {
	for _, ok := range []string{"v1.2.3", "latest", "1.0.0-rc.1", "a_b"} {
		if err := oci.CheckTag(ok); err != nil {
			t.Errorf("%s：%v", ok, err)
		}
	}
	for _, bad := range []string{"", ".x", "-x", "a/b", "a:b", strings.Repeat("x", 129)} {
		if err := oci.CheckTag(bad); !errors.Is(err, oci.ErrInvalidTag) {
			t.Errorf("%q 应无效：%v", bad, err)
		}
	}
}

// pushFixture 是推送测试的环境：registry、镜像和 blob 读取函数。
type pushFixture struct {
	reg  *ocitest.Registry
	img  *oci.Image
	open oci.Opener
	name oci.Name
}

func newPushFixture(t *testing.T, auth string) *pushFixture {
	t.Helper()
	reg := ocitest.New(auth, "alice", "pa55-w0rd-secret")
	t.Cleanup(reg.Close)
	l := ocitest.NewLayout("linux", "arm64", "hello")
	files, open := memLayout(l)
	img, err := oci.LoadLayout(t.Context(), files, open)
	if err != nil {
		t.Fatal(err)
	}
	name, err := oci.ParseName(reg.Host + "/team/app")
	if err != nil {
		t.Fatal(err)
	}
	return &pushFixture{reg: reg, img: img, open: open, name: name}
}

func (f *pushFixture) client(user, pass string) *oci.Client {
	return oci.NewTestClient(f.name, user, pass)
}

func TestPushAndTag(t *testing.T) {
	for _, auth := range []string{ocitest.AuthBearer, ocitest.AuthBasic} {
		t.Run(auth, func(t *testing.T) {
			f := newPushFixture(t, auth)
			c := f.client("alice", "pa55-w0rd-secret")
			ctx := t.Context()
			if err := c.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			pending, err := oci.PendingTags(ctx, c, []string{"v1.0.0"}, f.img.Root.Digest)
			if err != nil || !slices.Equal(pending, []string{"v1.0.0"}) {
				t.Fatalf("pending = %v %v", pending, err)
			}
			st, err := oci.Push(ctx, c, f.img, f.open)
			if err != nil {
				t.Fatal(err)
			}
			if st.Uploaded != 2 || st.Skipped != 0 {
				t.Fatalf("stats = %+v", st)
			}
			if f.reg.Tag("team/app", "v1.0.0") != "" {
				t.Fatal("阶段 A 不应设置 tag")
			}
			if err := oci.SetTag(ctx, c, f.img, "v1.0.0"); err != nil {
				t.Fatal(err)
			}
			if got := f.reg.Tag("team/app", "v1.0.0"); got != f.img.Root.Digest {
				t.Fatalf("tag 指向 %s", got)
			}
			// 重跑：tag 已指向同一 digest → 不再需要设置；blob 已存在 → 跳过
			pending, err = oci.PendingTags(ctx, c, []string{"v1.0.0"}, f.img.Root.Digest)
			if err != nil || len(pending) != 0 {
				t.Fatalf("重跑时 pending = %v %v", pending, err)
			}
			st, err = oci.Push(ctx, c, f.img, f.open)
			if err != nil || st.Uploaded != 0 || st.Skipped != 2 {
				t.Fatalf("重跑 stats = %+v %v", st, err)
			}
		})
	}
}

func TestTagConflict(t *testing.T) {
	f := newPushFixture(t, ocitest.AuthBearer)
	other := f.reg.SetTag("team/app", "v1.0.0", []byte(`{"schemaVersion":2}`))
	c := f.client("alice", "pa55-w0rd-secret")
	_, err := oci.PendingTags(t.Context(), c, []string{"v1.0.0"}, f.img.Root.Digest)
	if !errors.Is(err, oci.ErrTagConflict) || !strings.Contains(err.Error(), other) {
		t.Fatalf("err = %v", err)
	}
}

func TestWrongCredentialsDoNotLeak(t *testing.T) {
	for _, auth := range []string{ocitest.AuthBearer, ocitest.AuthBasic} {
		t.Run(auth, func(t *testing.T) {
			f := newPushFixture(t, auth)
			const wrong = "wr0ng-pa55word-value"
			c := f.client("alice", wrong)
			err := c.Ping(t.Context())
			if !errors.Is(err, oci.ErrUnauthorized) {
				t.Fatalf("err = %v", err)
			}
			_, err = oci.Push(t.Context(), c, f.img, f.open)
			if !errors.Is(err, oci.ErrUnauthorized) {
				t.Fatalf("err = %v", err)
			}
			for _, s := range []string{wrong, "Authorization", "Basic ", "Bearer "} {
				if strings.Contains(err.Error(), s) {
					t.Errorf("错误信息中出现了 %q：%v", s, err)
				}
			}
		})
	}
}

func TestRetryTransientErrors(t *testing.T) {
	f := newPushFixture(t, ocitest.AuthBearer)
	c := f.client("alice", "pa55-w0rd-secret")
	f.reg.FailNext(3)
	if _, err := oci.Push(t.Context(), c, f.img, f.open); err != nil {
		t.Fatalf("503 应该重试：%v", err)
	}
	f.reg.FailNext(100)
	err := oci.SetTag(t.Context(), c, f.img, "v1")
	var re *oci.Error
	if !errors.As(err, &re) || re.Status != 503 {
		t.Fatalf("重试用完后应返回 503：%v", err)
	}
}

// TestPushMergedIndex 推送多平台 index：先推各平台 manifest，再推 index。
func TestPushMergedIndex(t *testing.T) {
	f := newPushFixture(t, ocitest.AuthBearer)
	l2 := ocitest.NewLayout("linux", "amd64", "other")
	files2, open2 := memLayout(l2)
	img2, err := oci.LoadLayout(t.Context(), files2, open2)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := oci.Merge([]*oci.Image{f.img, img2})
	if err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context, d string) (io.ReadCloser, error) {
		if rc, err := f.open(ctx, d); err == nil {
			return rc, nil
		}
		return open2(ctx, d)
	}
	c := f.client("alice", "pa55-w0rd-secret")
	if _, err := oci.Push(t.Context(), c, merged, open); err != nil {
		t.Fatal(err)
	}
	if err := oci.SetTag(t.Context(), c, merged, "v1"); err != nil {
		t.Fatal(err)
	}
	mt, _, ok := f.reg.Manifest("team/app", "v1")
	if !ok || mt != oci.MediaTypeOCIIndex {
		t.Fatalf("tag 应指向 index：%s", mt)
	}
}
