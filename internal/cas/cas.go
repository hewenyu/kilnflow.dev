// Package cas 定义内容寻址存储的对象格式（manifest）和 Server 端的存储实现（PLAN #10）。
// v0 的存储放在 Server 本地磁盘；接口与 R2 实现一致，之后可以替换。
package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DigestPrefix 是 digest 的算法前缀。
const DigestPrefix = "sha256:"

// maxJSONSize 是 ReadJSON 读取的对象大小上限。
const maxJSONSize = 64 << 20

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ValidDigest 判断是否是 "sha256:<hex>" 形式的 digest。
func ValidDigest(d string) bool { return digestRe.MatchString(d) }

// Digest 计算字节的 digest。
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return FormatDigest(s[:])
}

// FormatDigest 把 sha256 的原始摘要格式化为 "sha256:<hex>"。
func FormatDigest(sum []byte) string {
	return DigestPrefix + hex.EncodeToString(sum)
}

// HashReader 读完 r，返回内容的 digest 和字节数。
func HashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return FormatDigest(h.Sum(nil)), n, nil
}

// FileEntry 是 manifest 中的一个文件。
type FileEntry struct {
	Path   string `json:"path"`
	Digest string `json:"digest,omitempty"`
	Size   int64  `json:"size"`
	// Exec 表示可执行位；只保留文件内容和可执行位（PLAN #4.6）。
	Exec bool `json:"exec,omitempty"`
	// Symlink 非空时表示符号链接，值为链接目标（必须在输出目录之内）。
	Symlink string `json:"symlink,omitempty"`
}

// OutputManifest 是一个命名输出的文件清单。路径相对于输出的根目录。
type OutputManifest struct {
	Schema string      `json:"schema"`
	Files  []FileEntry `json:"files"`
}

// ResultManifest 是一个 job 的结果：每个命名输出对应一个 OutputManifest 的 digest。
type ResultManifest struct {
	Schema  string            `json:"schema"`
	Outputs map[string]string `json:"outputs"`
}

// manifest 的 schema 标识。
const (
	OutputSchema = "kiln.output.v1"
	ResultSchema = "kiln.result.v1"
)

// Store 是 CAS 的存储后端。
type Store interface {
	Has(ctx context.Context, digest string) (bool, error)
	// Put 写入对象，内容的 sha256 必须与 digest 一致。
	Put(ctx context.Context, digest string, r io.Reader) (int64, error)
	Get(ctx context.Context, digest string) (io.ReadCloser, int64, error)
}

// ErrNotFound 表示对象不存在。
var ErrNotFound = errors.New("cas: 对象不存在")

// FSStore 把对象存放在本地目录 <root>/sha256/<前 2 位>/<hex>。
// Server 用它做 v0 的存储，Runner 用它做本地缓存。
type FSStore struct{ Root string }

var _ Store = (*FSStore)(nil)

// Path 返回对象在本地磁盘上的路径（不检查是否存在）。
func (s *FSStore) Path(digest string) (string, error) {
	if !ValidDigest(digest) {
		return "", fmt.Errorf("无效的 digest %q", digest)
	}
	hx := strings.TrimPrefix(digest, DigestPrefix)
	return filepath.Join(s.Root, "sha256", hx[:2], hx), nil
}

// Has 判断对象是否存在。
func (s *FSStore) Has(_ context.Context, digest string) (bool, error) {
	p, err := s.Path(digest)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// Put 先写入临时文件，校验 digest 后再原子地改名到最终位置。
func (s *FSStore) Put(_ context.Context, digest string, r io.Reader) (int64, error) {
	p, err := s.Path(digest)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), tempPrefix+"*")
	if err != nil {
		return 0, err
	}
	// 成功时临时文件已经改名，这里的删除只在失败时有意义，忽略其错误
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if got := FormatDigest(h.Sum(nil)); got != digest {
		return n, fmt.Errorf("内容的 digest 为 %s，与声明的 %s 不一致", got, digest)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return n, err
	}
	return n, nil
}

// Get 打开对象，返回内容和大小；对象不存在时返回 ErrNotFound。
func (s *FSStore) Get(_ context.Context, digest string) (io.ReadCloser, int64, error) {
	p, err := s.Path(digest)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close() // 只读打开的文件，关闭失败不影响结果
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// ErrInvalidObject 表示读到的对象本身不能用：内容与 digest 不一致（损坏）、超过 JSON 对象的大小上限或者无法解析。
// 与读取失败等暂时的错误不同，重试也不会好。
var ErrInvalidObject = errors.New("cas: 对象无效")

// ReadJSON 读取并解析一个 JSON 对象（manifest），同时校验内容与 digest 一致。
// 对象本身无效时返回的错误包装 ErrInvalidObject，不存在时包装 ErrNotFound。
func ReadJSON(ctx context.Context, s Store, digest string, v any) error {
	rc, size, err := s.Get(ctx, digest)
	if err != nil {
		return err
	}
	defer rc.Close()
	tooLarge := func() error {
		return fmt.Errorf("%w：%s 超过 JSON 对象的大小上限 %d 字节", ErrInvalidObject, digest, maxJSONSize)
	}
	if size > maxJSONSize {
		return tooLarge()
	}
	// 多读一个字节：超过上限的对象明确报告，而不是截断后当作内容损坏
	b, err := io.ReadAll(io.LimitReader(rc, maxJSONSize+1))
	if err != nil {
		return err
	}
	if len(b) > maxJSONSize {
		return tooLarge()
	}
	if Digest(b) != digest {
		return fmt.Errorf("%w：%s 内容损坏", ErrInvalidObject, digest)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w：%s 无法解析：%w", ErrInvalidObject, digest, err)
	}
	return nil
}

// Delete 删除对象；对象不存在时不报错。
func (s *FSStore) Delete(_ context.Context, digest string) error {
	p, err := s.Path(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// tempPrefix 是 Put 写入时临时文件名的前缀。
const tempPrefix = ".upload-"

// Object 是 Walk 列出的一个本地文件：对象（Digest 非空）或者上传中断留下的临时文件（Digest 为空）。
type Object struct {
	Digest  string
	Path    string
	Size    int64
	ModTime time.Time
}

// Walk 列出存储中的对象和临时文件；不认识的文件跳过。fn 返回错误时停止并返回该错误。
func (s *FSStore) Walk(ctx context.Context, fn func(Object) error) error {
	root := filepath.Join(s.Root, "sha256")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // 对象在列出期间被删除，或者存储还是空的
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		o := Object{Path: p, Size: info.Size(), ModTime: info.ModTime()}
		name := d.Name()
		switch {
		case strings.HasPrefix(name, tempPrefix):
		case ValidDigest(DigestPrefix + name):
			o.Digest = DigestPrefix + name
		default:
			return nil
		}
		return fn(o)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
