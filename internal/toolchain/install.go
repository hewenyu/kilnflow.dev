package toolchain

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/cas"
)

// Ref 是 job 使用的一个 runtime。
type Ref struct {
	Name     string
	Version  string
	Platform string
	Digest   string
}

// Meta 写在解包目录中，记录 runtime 的环境变量设置。
type Meta struct {
	Name     string             `json:"name"`
	Version  string             `json:"version"`
	Platform string             `json:"platform"`
	Digest   string             `json:"digest"`
	Env      map[string]EnvSpec `json:"env"`
}

const metaFile = ".kiln-runtime.json"

// shimDir 是 writeShims 生成启动脚本的目录（相对 runtime 根目录）。
const shimDir = ".kiln-bin"

// Installed 是 Install 的结果。
type Installed struct {
	// Dir 是 runtime 的目录（<root>/<digest hex>）。
	Dir  string
	Meta *Meta
	// Reused 为 true 表示目录中已经有完整的安装，这次没有访问下载源。
	Reused bool
}

// Install 确保 runtime 已经下载并解包到 <root>/<digest hex>（PLAN #12.8）：已经有完整的安装时直接复用，不访问下载源；
// 否则下载、校验、在临时目录中解包，写入元数据后改名到位（没有元数据的目录不算安装好）。同一个 runtime 同一时刻只安装
// 一次：靠 <root>/.locks/<digest hex> 上的文件锁，同一进程内的调用之间和进程之间都互斥（每次调用各自打开锁文件），
// 等锁时 ctx 结束即返回它的错误。错误中带有 runtime 的名称、版本和平台。
func Install(ctx context.Context, root string, ref Ref) (*Installed, error) {
	in, err := install(ctx, root, ref)
	if err != nil {
		return nil, fmt.Errorf("runtime %s@%s（%s）：%w", ref.Name, ref.Version, ref.Platform, err)
	}
	return in, nil
}

func install(ctx context.Context, root string, ref Ref) (*Installed, error) {
	imp, ok := importers[ref.Name]
	if !ok {
		return nil, errors.New("v0 只支持内置 runtime（go、node、pnpm、bun）")
	}
	if !cas.ValidDigest(ref.Digest) {
		return nil, fmt.Errorf("digest 无效：%s", ref.Digest)
	}
	hex := strings.TrimPrefix(ref.Digest, cas.DigestPrefix)
	dir := filepath.Join(root, hex)
	// 不加文件锁先看一次：元数据是最后写入、随目录一起改名到位的，读到就是完整的安装
	if m, err := readMeta(dir); err == nil {
		return &Installed{Dir: dir, Meta: m, Reused: true}, nil
	}
	unlock, err := lockInstall(ctx, root, hex)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// 等锁期间另一个进程可能已经装好
	if m, err := readMeta(dir); err == nil {
		return &Installed{Dir: dir, Meta: m, Reused: true}, nil
	}
	art, err := imp.Artifact(ctx, ref.Version, ref.Platform)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	archive, err := downloadVerified(ctx, root, art.URL, ref.Digest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(archive) }() // 临时文件，删除失败只会留下垃圾

	tmpDir, err := os.MkdirTemp(root, ".extract-*")
	if err != nil {
		return nil, err
	}
	// 成功时已经改名，失败时尽力清理
	defer func() { _ = os.RemoveAll(tmpDir) }()
	// 容器中以非 root 用户运行时也要能读取
	if err := os.Chmod(tmpDir, 0o755); err != nil { //nolint:gosec // runtime 目录需要所有用户可读
		return nil, err
	}
	if err := extract(archive, art.Format, art.Strip, tmpDir); err != nil {
		return nil, fmt.Errorf("解包 %s：%w", art.URL, err)
	}
	if err := imp.PostExtract(tmpDir); err != nil {
		return nil, err
	}
	m := &Meta{Name: ref.Name, Version: ref.Version, Platform: ref.Platform, Digest: ref.Digest, Env: imp.Env()}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, metaFile), b, 0o644); err != nil {
		return nil, err
	}
	// 没有 meta 的旧目录是上次失败的残留；删不掉时下面的 Rename 会报错
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmpDir, dir); err != nil {
		return nil, err
	}
	return &Installed{Dir: dir, Meta: m}, nil
}

// downloadVerified 把发行包下载到 dir 下的临时文件，校验 sha256 与 lockfile 一致后返回文件路径。
func downloadVerified(ctx context.Context, dir, url, want string) (string, error) {
	f, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", err
	}
	digest, err := download(ctx, url, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && digest != want {
		err = fmt.Errorf("发行包的 digest 为 %s，与 lockfile 中的 %s 不一致", digest, want)
	}
	if err != nil {
		_ = os.Remove(f.Name()) // 临时文件，删除失败只会留下垃圾
		return "", err
	}
	return f.Name(), nil
}

func readMeta(dir string) (*Meta, error) {
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// download 把 url 的内容写入 w，返回内容的 digest（sha256:…）。
func download(ctx context.Context, url string, w io.Writer) (string, error) {
	resp, err := httpGet(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), resp.Body); err != nil {
		return "", fmt.Errorf("下载 %s：%w", url, err)
	}
	return cas.FormatDigest(h.Sum(nil)), nil
}

// hashURL 下载并返回 digest（sha256:…），用于上游没有直接给出 sha256 的 runtime（例如 pnpm）；
// 同时用 npm 的 integrity（sha512-…）校验内容，没有可用的 integrity 时报错。
func hashURL(ctx context.Context, url, integrity string) (string, error) {
	want, ok := strings.CutPrefix(integrity, "sha512-")
	if !ok {
		return "", fmt.Errorf("%s 没有 sha512 integrity（%q），无法校验", url, integrity)
	}
	h512 := sha512.New()
	digest, err := download(ctx, url, h512)
	if err != nil {
		return "", err
	}
	if base64.StdEncoding.EncodeToString(h512.Sum(nil)) != want {
		return "", fmt.Errorf("%s 的 sha512 与 npm registry 的 integrity 不一致", url)
	}
	return digest, nil
}

func writeShims(dir string, shims map[string]string) error {
	for name, target := range shims {
		if target == "" {
			// bunx → bun
			if err := os.Symlink("bun", filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			continue
		}
		bin := filepath.Join(dir, shimDir)
		if err := os.MkdirAll(bin, 0o755); err != nil {
			return err
		}
		script := fmt.Sprintf("#!/bin/sh\nexec node \"$(dirname \"$0\")/../%s\" \"$@\"\n", target)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			return err
		}
	}
	return nil
}
