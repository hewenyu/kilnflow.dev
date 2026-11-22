package oci

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// 支持的 manifest 类型。
const (
	MediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

const (
	// layoutVersion 是 oci-layout 文件中唯一支持的版本。
	layoutVersion = "1.0.0"
	// maxManifestSize 是 manifest、index、config 的大小上限（registry 一般限制 manifest 为 4MiB）。
	maxManifestSize = 4 << 20
	// maxNesting 是 index 嵌套的层数上限。
	maxNesting   = 4
	digestPrefix = "sha256:"
	// schemaVersion 是 OCI manifest 和 index 唯一的 schemaVersion。
	schemaVersion = 2
)

// ErrInvalidLayout 表示 OCI image layout 不完整或内容与描述不一致。
var ErrInvalidLayout = errors.New("OCI image layout 无效")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w：%s", ErrInvalidLayout, fmt.Sprintf(format, args...))
}

// Platform 是镜像的平台。
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// String 返回 os/arch[/variant]。
func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Descriptor 是 OCI 描述符。
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	Platform     *Platform         `json:"platform,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// isIndex 判断描述符指向 index（多平台）。
func isIndex(mt string) bool { return mt == MediaTypeOCIIndex || mt == MediaTypeDockerList }

// isManifest 判断描述符指向单平台 manifest。
func isManifest(mt string) bool { return mt == MediaTypeOCIManifest || mt == MediaTypeDockerManifest }

type indexJSON struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []Descriptor `json:"manifests"`
}

type manifestJSON struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

type configJSON struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// Manifest 是一个 manifest 或 index 的原始内容（推送时原样上传，digest 不变）。
type Manifest struct {
	Descriptor
	Body []byte
}

// Image 是校验过、可以推送的镜像。推送顺序：先 Blobs，再按顺序推 Manifests（最后一个就是 Root）。
type Image struct {
	Root Descriptor
	// Platform 是 Root 为单平台 manifest 时的平台（取自镜像配置）；Root 是 index 时为 nil。
	Platform  *Platform
	Blobs     []Descriptor
	Manifests []Manifest
}

// Size 返回全部 blob 的总大小。
func (img *Image) Size() int64 {
	var n int64
	for _, b := range img.Blobs {
		n += b.Size
	}
	return n
}

// File 是 layout 目录中的一个文件：内容的 digest 和大小。
type File struct {
	Digest string
	Size   int64
}

// Opener 按内容 digest 打开文件。
type Opener func(ctx context.Context, digest string) (io.ReadCloser, error)

// loader 保存读取一个 layout 时的状态。
type loader struct {
	files map[string]File
	open  Opener
	img   *Image
	seen  map[string]bool
}

// LoadLayout 读取并校验 OCI image layout（buildctl --output type=oci,tar=false 的输出目录）。
// files 是 layout 目录中的文件（相对路径，/ 分隔），open 按内容 digest 读取文件。
//
// 校验：oci-layout 的版本；index.json 恰好引用一个镜像；从它出发引用到的每个 index、manifest、
// config 和 layer 都存在于 blobs/sha256/ 下，文件名、大小与描述符一致，内容的 sha256 与 digest 一致。
func LoadLayout(ctx context.Context, files map[string]File, open Opener) (*Image, error) {
	l := &loader{files: files, open: open, img: &Image{}, seen: map[string]bool{}}
	var ver struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := l.readFileJSON(ctx, "oci-layout", &ver); err != nil {
		return nil, err
	}
	if ver.ImageLayoutVersion != layoutVersion {
		return nil, invalidf("oci-layout 的版本 %q 不受支持（应为 %s）", ver.ImageLayoutVersion, layoutVersion)
	}
	var idx indexJSON
	if err := l.readFileJSON(ctx, "index.json", &idx); err != nil {
		return nil, err
	}
	switch n := len(idx.Manifests); {
	case n == 0:
		return nil, invalidf("index.json 中没有镜像")
	case n > 1:
		return nil, invalidf("index.json 中有 %d 个镜像，一个输出只能包含一个镜像（多平台请用矩阵，每个变体一个平台）", n)
	}
	root := idx.Manifests[0]
	if err := l.walk(ctx, root, 0); err != nil {
		return nil, err
	}
	// index.json 中的描述符可能带 annotations（例如 ref.name）；推送时只用基本字段
	l.img.Root = Descriptor{MediaType: root.MediaType, Digest: root.Digest, Size: root.Size}
	if isManifest(root.MediaType) {
		p, err := l.platform(ctx, root)
		if err != nil {
			return nil, err
		}
		l.img.Platform = p
	}
	return l.img, nil
}

// readFileJSON 读取 layout 中的小文件（oci-layout、index.json）并解析。
func (l *loader) readFileJSON(ctx context.Context, name string, v any) error {
	f, ok := l.files[name]
	if !ok {
		return invalidf("缺少 %s", name)
	}
	b, err := l.read(ctx, f.Digest, f.Size)
	if err != nil {
		return fmt.Errorf("读取 %s：%w", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return invalidf("%s 不是有效的 JSON：%v", name, err)
	}
	return nil
}

// blobFile 检查描述符指向的 blob 文件存在，且文件名、大小与描述符一致。
func (l *loader) blobFile(d Descriptor) error {
	hx, ok := strings.CutPrefix(d.Digest, digestPrefix)
	if !ok || len(hx) != sha256.Size*2 {
		return invalidf("不支持的 digest %q（只支持 sha256）", d.Digest)
	}
	if _, err := hex.DecodeString(hx); err != nil {
		return invalidf("digest %q 无效", d.Digest)
	}
	p := "blobs/sha256/" + hx
	f, ok := l.files[p]
	switch {
	case !ok:
		return invalidf("缺少 %s", p)
	case f.Digest != d.Digest:
		return invalidf("%s 的内容与文件名不一致（实际 %s）", p, f.Digest)
	case f.Size != d.Size:
		return invalidf("%s 的大小 %d 与描述符中的 %d 不一致", p, f.Size, d.Size)
	}
	return nil
}

// read 读取不超过 maxManifestSize 的内容，并校验 sha256 和大小。
func (l *loader) read(ctx context.Context, digest string, size int64) ([]byte, error) {
	if size > maxManifestSize {
		return nil, invalidf("%s 有 %d 字节，超过上限 %d", digest, size, maxManifestSize)
	}
	rc, err := l.open(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != size {
		return nil, invalidf("%s 实际有 %d 字节，应为 %d", digest, len(b), size)
	}
	if got := Digest(b); got != digest {
		return nil, invalidf("内容的 digest 为 %s，与 %s 不一致", got, digest)
	}
	return b, nil
}

// verifyBlob 完整读一遍 blob，校验 sha256 和大小。
func (l *loader) verifyBlob(ctx context.Context, d Descriptor) error {
	rc, err := l.open(ctx, d.Digest)
	if err != nil {
		return err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return err
	}
	if got := digestPrefix + hex.EncodeToString(h.Sum(nil)); got != d.Digest || n != d.Size {
		return invalidf("blob %s 的内容损坏（实际 %s，%d 字节）", d.Digest, got, n)
	}
	return nil
}

// walk 校验描述符指向的 manifest 或 index 及其引用的全部内容，子项先于父项加入 Manifests。
func (l *loader) walk(ctx context.Context, d Descriptor, depth int) error {
	if depth > maxNesting {
		return invalidf("index 嵌套超过 %d 层", maxNesting)
	}
	if l.seen[d.Digest] {
		return nil
	}
	if !isIndex(d.MediaType) && !isManifest(d.MediaType) {
		return invalidf("不支持的 manifest 类型 %q（%s）", d.MediaType, d.Digest)
	}
	if err := l.blobFile(d); err != nil {
		return err
	}
	body, err := l.read(ctx, d.Digest, d.Size)
	if err != nil {
		return err
	}
	if isIndex(d.MediaType) {
		err = l.walkIndex(ctx, d, body, depth)
	} else {
		err = l.walkManifest(ctx, d, body)
	}
	if err != nil {
		return err
	}
	l.seen[d.Digest] = true
	l.img.Manifests = append(l.img.Manifests, Manifest{Descriptor{MediaType: d.MediaType, Digest: d.Digest, Size: d.Size}, body})
	return nil
}

func (l *loader) walkIndex(ctx context.Context, d Descriptor, body []byte, depth int) error {
	var idx indexJSON
	if err := json.Unmarshal(body, &idx); err != nil {
		return invalidf("index %s 不是有效的 JSON：%v", d.Digest, err)
	}
	if idx.SchemaVersion != schemaVersion || (idx.MediaType != "" && idx.MediaType != d.MediaType) {
		return invalidf("index %s 的 schemaVersion 或 mediaType 与描述符不一致", d.Digest)
	}
	if len(idx.Manifests) == 0 {
		return invalidf("index %s 是空的", d.Digest)
	}
	for _, c := range idx.Manifests {
		if err := l.walk(ctx, c, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (l *loader) walkManifest(ctx context.Context, d Descriptor, body []byte) error {
	var m manifestJSON
	if err := json.Unmarshal(body, &m); err != nil {
		return invalidf("manifest %s 不是有效的 JSON：%v", d.Digest, err)
	}
	if m.SchemaVersion != schemaVersion || (m.MediaType != "" && m.MediaType != d.MediaType) {
		return invalidf("manifest %s 的 schemaVersion 或 mediaType 与描述符不一致", d.Digest)
	}
	if m.Config.Digest == "" {
		return invalidf("manifest %s 没有 config", d.Digest)
	}
	for _, b := range append([]Descriptor{m.Config}, m.Layers...) {
		if l.seen[b.Digest] {
			continue
		}
		if err := l.blobFile(b); err != nil {
			return err
		}
		if err := l.verifyBlob(ctx, b); err != nil {
			return err
		}
		l.seen[b.Digest] = true
		l.img.Blobs = append(l.img.Blobs, Descriptor{MediaType: b.MediaType, Digest: b.Digest, Size: b.Size})
	}
	return nil
}

// platform 从单平台 manifest 的镜像配置中读出平台。
func (l *loader) platform(ctx context.Context, d Descriptor) (*Platform, error) {
	var mf manifestJSON
	for _, m := range l.img.Manifests {
		if m.Digest == d.Digest {
			if err := json.Unmarshal(m.Body, &mf); err != nil {
				return nil, err
			}
		}
	}
	b, err := l.read(ctx, mf.Config.Digest, mf.Config.Size)
	if err != nil {
		return nil, fmt.Errorf("读取镜像配置：%w", err)
	}
	var c configJSON
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, invalidf("镜像配置 %s 不是有效的 JSON：%v", mf.Config.Digest, err)
	}
	if c.OS == "" || c.Architecture == "" {
		return nil, invalidf("镜像配置 %s 中没有 os / architecture", mf.Config.Digest)
	}
	return &Platform{OS: c.OS, Architecture: c.Architecture, Variant: c.Variant}, nil
}

// Merge 把多个单平台镜像合并成一个多平台 index（PLAN #13.8：矩阵的每个变体一个平台）。
// 只有一个镜像时原样返回；平台重复，或某个镜像本身已经是 index 时报错。
func Merge(images []*Image) (*Image, error) {
	switch len(images) {
	case 0:
		return nil, errors.New("没有镜像")
	case 1:
		return images[0], nil
	}
	out := &Image{}
	seen := map[string]bool{}
	platforms := map[string]bool{}
	var descs []Descriptor
	for _, img := range images {
		if img.Platform == nil {
			return nil, fmt.Errorf("镜像 %s 已经是多平台 index，不能再与其他变体合并", img.Root.Digest)
		}
		p := img.Platform.String()
		if platforms[p] {
			return nil, fmt.Errorf("多个变体的镜像平台都是 %s", p)
		}
		platforms[p] = true
		d := img.Root
		d.Platform = img.Platform
		descs = append(descs, d)
		for _, b := range img.Blobs {
			if !seen[b.Digest] {
				seen[b.Digest] = true
				out.Blobs = append(out.Blobs, b)
			}
		}
		for _, m := range img.Manifests {
			if !seen[m.Digest] {
				seen[m.Digest] = true
				out.Manifests = append(out.Manifests, m)
			}
		}
	}
	slices.SortFunc(descs, func(a, b Descriptor) int { return cmp.Compare(a.Platform.String(), b.Platform.String()) })
	body, err := json.Marshal(indexJSON{SchemaVersion: schemaVersion, MediaType: MediaTypeOCIIndex, Manifests: descs})
	if err != nil {
		return nil, err
	}
	out.Root = Descriptor{MediaType: MediaTypeOCIIndex, Digest: Digest(body), Size: int64(len(body))}
	out.Manifests = append(out.Manifests, Manifest{Descriptor: out.Root, Body: body})
	return out, nil
}

// Digest 计算内容的 sha256 digest。
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return digestPrefix + hex.EncodeToString(s[:])
}
