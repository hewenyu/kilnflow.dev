package cas

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFSStoreRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &FSStore{Root: t.TempDir()}
	data := []byte("hello kiln")
	d := Digest(data)

	has, err := s.Has(ctx, d)
	if err != nil || has {
		t.Fatalf("写入前 Has = %v, %v", has, err)
	}
	if _, _, err := s.Get(ctx, d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("写入前 Get 期望 ErrNotFound，实际 %v", err)
	}
	n, err := s.Put(ctx, d, bytes.NewReader(data))
	if err != nil || n != int64(len(data)) {
		t.Fatalf("Put = %d, %v", n, err)
	}
	if has, err := s.Has(ctx, d); err != nil || !has {
		t.Fatalf("写入后 Has = %v, %v", has, err)
	}
	rc, size, err := s.Get(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || !bytes.Equal(got, data) || size != int64(len(data)) {
		t.Fatalf("Get = %q（%d）, %v", got, size, err)
	}
	// 布局：<root>/sha256/<前 2 位>/<hex>
	p, err := s.Path(d)
	if err != nil {
		t.Fatal(err)
	}
	hx := strings.TrimPrefix(d, DigestPrefix)
	if want := filepath.Join(s.Root, "sha256", hx[:2], hx); p != want {
		t.Fatalf("Path = %s，期望 %s", p, want)
	}
}

func TestFSStorePutDigestMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &FSStore{Root: t.TempDir()}
	d := Digest([]byte("expected"))
	if _, err := s.Put(ctx, d, strings.NewReader("other")); err == nil {
		t.Fatal("内容与 digest 不一致时 Put 应当失败")
	}
	if has, err := s.Has(ctx, d); err != nil || has {
		t.Fatalf("校验失败后不应留下对象：Has = %v, %v", has, err)
	}
	// 也不应留下临时文件
	p, err := s.Path(d)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("残留文件：%v", entries)
	}
}

func TestFSStoreInvalidDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &FSStore{Root: t.TempDir()}
	for _, d := range []string{"", "sha256:abc", "md5:" + strings.Repeat("0", 64), "sha256:" + strings.Repeat("G", 64), "sha256:../../" + strings.Repeat("0", 58)} {
		t.Run(d, func(t *testing.T) {
			t.Parallel()
			if _, err := s.Has(ctx, d); err == nil {
				t.Error("Has 应当拒绝无效 digest")
			}
			if _, err := s.Put(ctx, d, strings.NewReader("")); err == nil {
				t.Error("Put 应当拒绝无效 digest")
			}
			if _, _, err := s.Get(ctx, d); err == nil {
				t.Error("Get 应当拒绝无效 digest")
			}
		})
	}
}

func TestReadJSONDetectsCorruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &FSStore{Root: t.TempDir()}
	data := []byte(`{"schema":"kiln.result.v1","outputs":{}}`)
	d := Digest(data)
	if _, err := s.Put(ctx, d, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	var rm ResultManifest
	if err := ReadJSON(ctx, s, d, &rm); err != nil || rm.Schema != ResultSchema {
		t.Fatalf("ReadJSON = %+v, %v", rm, err)
	}
	// 直接改写磁盘上的对象，模拟损坏
	p, err := s.Path(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"schema":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReadJSON(ctx, s, d, &rm); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("内容损坏时 ReadJSON 应返回 ErrInvalidObject，实际 %v", err)
	}
}

func TestHashReader(t *testing.T) {
	t.Parallel()
	d, n, err := HashReader(strings.NewReader("abc"))
	if err != nil || n != 3 || d != Digest([]byte("abc")) {
		t.Fatalf("HashReader = %s, %d, %v", d, n, err)
	}
	// sha256("abc") 的标准向量
	if want := "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; d != want {
		t.Fatalf("HashReader = %s，期望 %s", d, want)
	}
}

func TestHashReaderReadError(t *testing.T) {
	t.Parallel()
	_, n, err := HashReader(failingReader("abc"))
	if !errors.Is(err, errRead) {
		t.Fatalf("HashReader 期望返回读取错误，实际 %v", err)
	}
	if n != 3 {
		t.Errorf("出错前已读取 %d 字节，期望 3", n)
	}
}

// Root 不可用（是普通文件）时，各操作返回错误，而不是把对象当作不存在。
func TestFSStoreRootUnusable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := fileAsRoot(t)
	d := Digest([]byte("x"))
	if has, err := s.Has(ctx, d); err == nil {
		t.Errorf("Has = %v，期望返回错误", has)
	}
	if _, err := s.Put(ctx, d, strings.NewReader("x")); err == nil {
		t.Error("Put 期望返回错误")
	}
	if _, _, err := s.Get(ctx, d); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Get 期望返回非 ErrNotFound 的错误，实际 %v", err)
	}
	var v any
	if err := ReadJSON(ctx, s, d, &v); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("ReadJSON 期望返回非 ErrNotFound 的错误，实际 %v", err)
	}
}

// 上传中途读取失败：返回读取错误，不留下对象和临时文件。
func TestFSStorePutReadError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &FSStore{Root: t.TempDir()}
	d := Digest([]byte("abcdef"))
	if _, err := s.Put(ctx, d, failingReader("abc")); !errors.Is(err, errRead) {
		t.Fatalf("Put 期望返回读取错误，实际 %v", err)
	}
	if has, err := s.Has(ctx, d); err != nil || has {
		t.Fatalf("读取失败后不应留下对象：Has = %v, %v", has, err)
	}
	p, err := s.Path(d)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("残留文件：%v", entries)
	}
}

// 重复写入同一个对象是幂等的。
func TestFSStorePutIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &FSStore{Root: t.TempDir()}
	data := []byte("same content")
	d := Digest(data)
	for i := range 2 {
		if n, err := s.Put(ctx, d, bytes.NewReader(data)); err != nil || n != int64(len(data)) {
			t.Fatalf("第 %d 次 Put = %d, %v", i+1, n, err)
		}
	}
}

func TestReadJSONErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	notJSON := []byte("not json")
	cases := []struct {
		name   string
		store  Store
		digest string
		want   error  // 非 nil 时要求 errors.Is
		text   string // 错误信息应包含的内容
	}{
		{name: "对象不存在", store: &FSStore{Root: t.TempDir()}, digest: Digest([]byte("missing")), want: ErrNotFound},
		{name: "读取失败", store: readerStore{r: failingReader("{")}, digest: Digest([]byte("{")), want: errRead},
		{name: "内容不是 JSON", store: readerStore{r: bytes.NewReader(notJSON)}, digest: Digest(notJSON), want: ErrInvalidObject},
		{name: "内容损坏", store: readerStore{r: bytes.NewReader([]byte("{}"))}, digest: Digest(notJSON), want: ErrInvalidObject},
		{
			name: "超过大小上限", store: readerStore{r: bytes.NewReader([]byte("{}")), size: maxJSONSize + 1}, digest: Digest([]byte("{}")),
			want: ErrInvalidObject, text: "超过 JSON 对象的大小上限",
		},
		{name: "后端错误", store: readerStore{err: errRead}, digest: Digest(notJSON), want: errRead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var v map[string]any
			err := ReadJSON(ctx, tc.store, tc.digest, &v)
			if err == nil {
				t.Fatal("ReadJSON 期望返回错误")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("ReadJSON 错误 = %v，期望包含 %v", err, tc.want)
			}
			if !errors.Is(tc.want, ErrInvalidObject) && errors.Is(err, ErrInvalidObject) {
				t.Errorf("暂时的错误不应包装 ErrInvalidObject：%v", err)
			}
			if !strings.Contains(err.Error(), tc.text) {
				t.Errorf("ReadJSON 错误 = %v，期望包含 %q", err, tc.text)
			}
		})
	}
}

// Delete 删除对象，不存在时不报错；Walk 列出对象和上传临时文件，跳过不认识的文件。
func TestFSStoreDeleteWalk(t *testing.T) {
	t.Parallel()
	s := &FSStore{Root: t.TempDir()}
	// 存储还是空的：Walk 什么也不列出
	if err := s.Walk(t.Context(), func(Object) error { t.Error("空存储不应列出对象"); return nil }); err != nil {
		t.Fatal(err)
	}
	a, b := Digest([]byte("a")), Digest([]byte("b"))
	for _, d := range []struct {
		digest string
		data   string
	}{{a, "a"}, {b, "b"}} {
		if _, err := s.Put(t.Context(), d.digest, strings.NewReader(d.data)); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.Path(a)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(p)
	for _, name := range []string{".upload-1", "README"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var digests []string
	temps := 0
	if err := s.Walk(t.Context(), func(o Object) error {
		if o.Digest == "" {
			temps++
			if filepath.Base(o.Path) != ".upload-1" {
				t.Errorf("临时文件 %s", o.Path)
			}
			return nil
		}
		if o.Size != 1 || o.ModTime.IsZero() {
			t.Errorf("对象 %+v", o)
		}
		digests = append(digests, o.Digest)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(digests)
	want := []string{a, b}
	slices.Sort(want)
	if !slices.Equal(digests, want) || temps != 1 {
		t.Errorf("Walk 列出 %v 和 %d 个临时文件", digests, temps)
	}

	if err := s.Delete(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Has(t.Context(), a); ok {
		t.Error("删除后对象仍然存在")
	}
	if err := s.Delete(t.Context(), a); err != nil {
		t.Errorf("删除不存在的对象不应报错：%v", err)
	}
	if err := s.Delete(t.Context(), "md5:x"); err == nil {
		t.Error("无效的 digest 应返回错误")
	}
	stop := errors.New("stop")
	if err := s.Walk(t.Context(), func(Object) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("fn 的错误应返回：%v", err)
	}
}
