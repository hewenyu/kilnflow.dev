package oci_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/oci"
	"github.com/hewenyu/kilnflow.dev/internal/oci/ocitest"
)

// layoutBuilder 按需拼装 OCI image layout，用于构造 ocitest.NewLayout 生成不了的结构
// （多平台 index、嵌套 index、Docker 类型、各种损坏的内容）。
type layoutBuilder struct {
	t     *testing.T
	files map[string][]byte
	// sizes 覆盖文件表中记录的大小（模拟文件表与描述符声明了超大的文件）
	sizes map[string]int64
}

func newLayoutBuilder(t *testing.T) *layoutBuilder {
	t.Helper()
	return &layoutBuilder{
		t: t, files: map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`)}, sizes: map[string]int64{},
	}
}

// layout 返回 LoadLayout 的输入。
func (b *layoutBuilder) layout() (map[string]oci.File, oci.Opener) {
	files, open := memLayout(&ocitest.Layout{Files: b.files})
	for p, n := range b.sizes {
		f := files[p]
		f.Size = n
		files[p] = f
	}
	return files, open
}

func (b *layoutBuilder) json(v any) []byte {
	b.t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

// blob 把内容写到 blobs/sha256/ 下，返回它的描述符。
func (b *layoutBuilder) blob(mediaType string, body []byte) oci.Descriptor {
	d := oci.Digest(body)
	b.files["blobs/sha256/"+strings.TrimPrefix(d, "sha256:")] = body
	return oci.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(body))}
}

// manifest 生成一个单平台镜像（config + 一个 layer），返回 manifest 的描述符。
func (b *layoutBuilder) manifest(mediaType string, cfg map[string]any, layer string) oci.Descriptor {
	c := b.blob("application/vnd.oci.image.config.v1+json", b.json(cfg))
	l := b.blob("application/vnd.oci.image.layer.v1.tar", []byte(layer))
	return b.blob(mediaType, b.json(map[string]any{
		"schemaVersion": 2, "mediaType": mediaType, "config": c, "layers": []oci.Descriptor{l},
	}))
}

func (b *layoutBuilder) image(goos, arch, variant, layer string) oci.Descriptor {
	cfg := map[string]any{"os": goos, "architecture": arch}
	if variant != "" {
		cfg["variant"] = variant
	}
	return b.manifest(oci.MediaTypeOCIManifest, cfg, layer)
}

func (b *layoutBuilder) index(mediaType string, children ...oci.Descriptor) oci.Descriptor {
	return b.blob(mediaType, b.json(map[string]any{"schemaVersion": 2, "mediaType": mediaType, "manifests": children}))
}

// root 把 d 写进 index.json，作为 layout 中唯一的镜像。
func (b *layoutBuilder) root(d oci.Descriptor) {
	b.files["index.json"] = b.json(map[string]any{
		"schemaVersion": 2, "mediaType": oci.MediaTypeOCIIndex, "manifests": []oci.Descriptor{d},
	})
}

func (b *layoutBuilder) load() (*oci.Image, error) {
	files, open := b.layout()
	return oci.LoadLayout(b.t.Context(), files, open)
}

// TestLoadLayoutMultiPlatformIndex 读取多平台 index：子 manifest 先于 index 加入，共用的 layer 只出现一次。
func TestLoadLayoutMultiPlatformIndex(t *testing.T) {
	b := newLayoutBuilder(t)
	arm := b.image("linux", "arm64", "v8", "shared")
	amd := b.image("linux", "amd64", "", "shared")
	idx := b.index(oci.MediaTypeOCIIndex, arm, amd)
	// index.json 中的描述符带 annotations，推送时只保留基本字段
	withAnnotations := idx
	withAnnotations.Annotations = map[string]string{"org.opencontainers.image.ref.name": "v1"}
	b.root(withAnnotations)

	img, err := b.load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(img.Root, idx) {
		t.Fatalf("Root = %+v，期望 %+v", img.Root, idx)
	}
	if img.Platform != nil {
		t.Fatalf("Root 是 index 时 Platform 应为 nil：%v", img.Platform)
	}
	var order []string
	for _, m := range img.Manifests {
		order = append(order, m.Digest)
	}
	if want := []string{arm.Digest, amd.Digest, idx.Digest}; !slices.Equal(order, want) {
		t.Fatalf("Manifests 顺序 = %v，期望 %v（子项在前，Root 最后）", order, want)
	}
	// 两个 config + 一个共用的 layer
	if len(img.Blobs) != 3 {
		t.Fatalf("Blobs = %d 个，期望 3 个（共用的 layer 只算一次）", len(img.Blobs))
	}
	var sum int64
	for _, bl := range img.Blobs {
		sum += bl.Size
	}
	if img.Size() != sum || sum == 0 {
		t.Fatalf("Size() = %d，期望 %d", img.Size(), sum)
	}
	if img.RootManifest().Digest != idx.Digest || len(img.RootManifest().Body) == 0 {
		t.Fatalf("RootManifest = %+v", img.RootManifest().Descriptor)
	}
}

// TestLoadLayoutDockerMediaTypes 接受 Docker 的 manifest list 和 manifest v2。
func TestLoadLayoutDockerMediaTypes(t *testing.T) {
	b := newLayoutBuilder(t)
	m := b.manifest(oci.MediaTypeDockerManifest, map[string]any{"os": "linux", "architecture": "arm", "variant": "v7"}, "x")
	b.root(b.index(oci.MediaTypeDockerList, m))
	img, err := b.load()
	if err != nil {
		t.Fatal(err)
	}
	if img.Root.MediaType != oci.MediaTypeDockerList || len(img.Manifests) != 2 {
		t.Fatalf("Root = %+v，Manifests = %d", img.Root, len(img.Manifests))
	}

	// 单平台 Docker manifest 作为 Root 时读出平台（含 variant）
	b2 := newLayoutBuilder(t)
	b2.root(b2.manifest(oci.MediaTypeDockerManifest, map[string]any{"os": "linux", "architecture": "arm", "variant": "v7"}, "x"))
	img2, err := b2.load()
	if err != nil {
		t.Fatal(err)
	}
	if img2.Platform == nil || img2.Platform.String() != "linux/arm/v7" {
		t.Fatalf("Platform = %v，期望 linux/arm/v7", img2.Platform)
	}
}

// TestLoadLayoutDuplicateReferences 同一个 manifest 被引用两次时只校验和推送一次。
func TestLoadLayoutDuplicateReferences(t *testing.T) {
	b := newLayoutBuilder(t)
	m := b.image("linux", "arm64", "", "x")
	b.root(b.index(oci.MediaTypeOCIIndex, m, m))
	img, err := b.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(img.Manifests) != 2 || len(img.Blobs) != 2 {
		t.Fatalf("Manifests = %d，Blobs = %d，期望 2 和 2", len(img.Manifests), len(img.Blobs))
	}
}

func TestLoadLayoutInvalid(t *testing.T) {
	const bigSize = 5 << 20
	cases := []struct {
		name  string
		build func(b *layoutBuilder)
		want  string
	}{
		{"index.json 缺失", func(b *layoutBuilder) {}, "缺少 index.json"},
		{"oci-layout 不是 JSON", func(b *layoutBuilder) {
			b.files["oci-layout"] = []byte("1.0.0")
		}, "oci-layout 不是有效的 JSON"},
		{"index.json 不是 JSON", func(b *layoutBuilder) {
			b.files["index.json"] = []byte("{")
		}, "index.json 不是有效的 JSON"},
		{"index.json 中没有镜像", func(b *layoutBuilder) {
			b.files["index.json"] = []byte(`{"schemaVersion":2,"manifests":[]}`)
		}, "没有镜像"},
		{"index 不是 JSON", func(b *layoutBuilder) {
			b.root(b.blob(oci.MediaTypeOCIIndex, []byte("not json")))
		}, "不是有效的 JSON"},
		{"index 的 schemaVersion 不对", func(b *layoutBuilder) {
			m := b.image("linux", "arm64", "", "x")
			b.root(b.blob(oci.MediaTypeOCIIndex, b.json(map[string]any{"schemaVersion": 1, "manifests": []oci.Descriptor{m}})))
		}, "schemaVersion 或 mediaType"},
		{"index 的 mediaType 与描述符不一致", func(b *layoutBuilder) {
			m := b.image("linux", "arm64", "", "x")
			body := b.json(map[string]any{"schemaVersion": 2, "mediaType": oci.MediaTypeDockerList, "manifests": []oci.Descriptor{m}})
			b.root(b.blob(oci.MediaTypeOCIIndex, body))
		}, "schemaVersion 或 mediaType"},
		{"空的 index", func(b *layoutBuilder) {
			b.root(b.index(oci.MediaTypeOCIIndex))
		}, "是空的"},
		{"index 嵌套太深", func(b *layoutBuilder) {
			d := b.image("linux", "arm64", "", "x")
			for range 5 {
				d = b.index(oci.MediaTypeOCIIndex, d)
			}
			b.root(d)
		}, "嵌套超过"},
		{"manifest 不是 JSON", func(b *layoutBuilder) {
			b.root(b.blob(oci.MediaTypeOCIManifest, []byte("[")))
		}, "不是有效的 JSON"},
		{"manifest 的 schemaVersion 不对", func(b *layoutBuilder) {
			b.root(b.blob(oci.MediaTypeOCIManifest, b.json(map[string]any{"schemaVersion": 1})))
		}, "schemaVersion 或 mediaType"},
		{"manifest 没有 config", func(b *layoutBuilder) {
			b.root(b.blob(oci.MediaTypeOCIManifest, b.json(map[string]any{"schemaVersion": 2, "layers": []any{}})))
		}, "没有 config"},
		{"不是 sha256 的 digest", func(b *layoutBuilder) {
			b.root(oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: "sha512:" + strings.Repeat("a", 128), Size: 2})
		}, "只支持 sha256"},
		{"digest 长度不对", func(b *layoutBuilder) {
			b.root(oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: "sha256:abc", Size: 2})
		}, "只支持 sha256"},
		{"digest 不是十六进制", func(b *layoutBuilder) {
			b.root(oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: "sha256:" + strings.Repeat("z", 64), Size: 2})
		}, "无效"},
		{"layer 的 digest 无效", func(b *layoutBuilder) {
			c := b.blob("application/vnd.oci.image.config.v1+json", []byte(`{"os":"linux","architecture":"arm64"}`))
			bad := oci.Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: "md5:x", Size: 1}
			b.root(b.blob(oci.MediaTypeOCIManifest, b.json(map[string]any{
				"schemaVersion": 2, "config": c, "layers": []oci.Descriptor{bad},
			})))
		}, "只支持 sha256"},
		{"镜像配置不是 JSON", func(b *layoutBuilder) {
			c := b.blob("application/vnd.oci.image.config.v1+json", []byte("nope"))
			b.root(b.blob(oci.MediaTypeOCIManifest, b.json(map[string]any{"schemaVersion": 2, "config": c, "layers": []any{}})))
		}, "镜像配置"},
		{"镜像配置中没有 os", func(b *layoutBuilder) {
			b.root(b.manifest(oci.MediaTypeOCIManifest, map[string]any{"architecture": "arm64"}, "x"))
		}, "没有 os / architecture"},
		{"manifest 超过大小上限", func(b *layoutBuilder) {
			d := b.blob(oci.MediaTypeOCIManifest, []byte("{}"))
			// 文件表与描述符都声明超大尺寸：不读内容，直接拒绝
			d.Size = bigSize
			b.sizes["blobs/sha256/"+strings.TrimPrefix(d.Digest, "sha256:")] = bigSize
			b.root(d)
		}, "超过上限"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newLayoutBuilder(t)
			c.build(b)
			_, err := b.load()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v，应包含 %q", err, c.want)
			}
			if !errors.Is(err, oci.ErrInvalidLayout) {
				t.Errorf("应为 ErrInvalidLayout：%v", err)
			}
		})
	}
}

// TestLoadLayoutFileSizeMismatch 文件表中的大小与读出的内容长度不一致（例如读取时文件被截断）。
func TestLoadLayoutFileSizeMismatch(t *testing.T) {
	l := ocitest.NewLayout("linux", "arm64", "hi")
	files, open := memLayout(l)
	f := files["index.json"]
	f.Size++
	files["index.json"] = f
	_, err := oci.LoadLayout(t.Context(), files, open)
	if !errors.Is(err, oci.ErrInvalidLayout) || !strings.Contains(err.Error(), "实际有") {
		t.Fatalf("err = %v", err)
	}
}

// TestLoadLayoutOpenErrors 读取文件失败时原样返回错误（不是 layout 无效）。
func TestLoadLayoutOpenErrors(t *testing.T) {
	l := ocitest.NewLayout("linux", "arm64", "hi")
	files, open := memLayout(l)
	errDisk := errors.New("磁盘错误")
	cases := []struct {
		name   string
		digest string
	}{
		{"oci-layout", files["oci-layout"].Digest},
		{"manifest", l.Root},
		{"layer", l.Layer},
		{"config", l.Config},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			failing := func(ctx context.Context, d string) (io.ReadCloser, error) {
				if d == c.digest {
					return nil, errDisk
				}
				return open(ctx, d)
			}
			_, err := oci.LoadLayout(t.Context(), files, failing)
			if !errors.Is(err, errDisk) {
				t.Fatalf("err = %v，期望包含 %v", err, errDisk)
			}
		})
	}
}

func TestPlatformString(t *testing.T) {
	cases := []struct {
		p    oci.Platform
		want string
	}{
		{oci.Platform{OS: "linux", Architecture: "amd64"}, "linux/amd64"},
		{oci.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, "linux/arm/v7"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("String() = %q，期望 %q", got, c.want)
		}
	}
}

func TestMergeEdges(t *testing.T) {
	if _, err := oci.Merge(nil); err == nil {
		t.Fatal("没有镜像时应报错")
	}
	// 同一平台不同 variant 可以合并
	a := loadLayout(t, ocitest.NewLayout("linux", "arm", "a"))
	a.Platform.Variant = "v6"
	b := loadLayout(t, ocitest.NewLayout("linux", "arm", "b"))
	b.Platform.Variant = "v7"
	m, err := oci.Merge([]*oci.Image{a, b})
	if err != nil {
		t.Fatalf("不同 variant 应可合并：%v", err)
	}
	if m.Root.Digest != oci.Digest(m.RootManifest().Body) || m.Root.Size != int64(len(m.RootManifest().Body)) {
		t.Fatalf("Root 描述符与 index 内容不一致：%+v", m.Root)
	}
}

func TestRootManifestFallback(t *testing.T) {
	d := oci.Descriptor{MediaType: oci.MediaTypeOCIManifest, Digest: "sha256:x", Size: 3}
	img := &oci.Image{Root: d}
	if got := img.RootManifest(); got.Digest != d.Digest || got.Body != nil {
		t.Fatalf("RootManifest = %+v，期望只有描述符", got)
	}
}
