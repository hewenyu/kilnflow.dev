// Package logstore 保存 attempt（以及发布 job）的日志。
package logstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// logLockShards 是日志写锁的分片数。同一个 attempt 总是落在同一个分片上。
	logLockShards = 64
	// maxLogSize 是单个日志的长度上限（100 MiB），避免一个输出失控的 job 写满磁盘。
	// 写入后会超出上限的数据整段不写，Append 不报错、返回原来的长度；Runner 看到长度没有增加就丢弃剩余的日志。
	maxLogSize = 100 << 20
	// tailWindow 是 Tail 读取的文件末尾字节数。
	tailWindow = 64 << 10
)

// Store 把每个 attempt 的日志保存为 <dir>/<attempt_id>.log（v0：本地磁盘，PLAN #7.7 的 R2 段存储以后再做）。
type Store struct {
	dir   string
	locks [logLockShards]sync.Mutex
}

// ErrBadOffset 表示追加日志的 offset 无效（负数，或者超过日志当前的长度、中间缺了一段）：请求本身不对，重试也不会好。
// 其他错误（打开、写入文件失败）是 Server 的存储故障。
var ErrBadOffset = errors.New("日志 offset 无效")

// PublishLogID 是发布 job 日志的编号（发布 job 没有 attempt，用负数与 attempt 区分）。
// 发布 worker 写入、管理接口和 Dashboard 读取都用这个编号。
func PublishLogID(jobID int64) int64 { return -jobID }

// New 创建日志目录并返回 Store。
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (l *Store) path(attempt int64) string {
	return filepath.Join(l.dir, fmt.Sprintf("%d.log", attempt))
}

// lock 锁住 attempt 所在的分片（发布 job 的日志编号是负数，按无符号数取模）。
func (l *Store) lock(attempt int64) func() {
	m := &l.locks[uint64(attempt)%logLockShards] //nolint:gosec // 只用于取模分片，负数转换为无符号数是有意的
	m.Lock()
	return m.Unlock
}

// Append 在 offset 处追加数据，重复的部分被忽略（按 offset 去重，重传是幂等的）。返回当前长度。
// offset 无效时返回的错误包装 ErrBadOffset。
func (l *Store) Append(attempt, offset int64, data []byte) (int64, error) {
	defer l.lock(attempt)()
	f, err := os.OpenFile(l.path(attempt), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	n, werr := appendAt(f, offset, data)
	if cerr := f.Close(); cerr != nil && werr == nil {
		werr = cerr
	}
	return n, werr
}

func appendAt(f *os.File, offset int64, data []byte) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	if offset < 0 {
		return size, fmt.Errorf("%w：不能为负数（%d）", ErrBadOffset, offset)
	}
	if offset > size {
		return size, fmt.Errorf("%w：日志不连续，已有 %d 字节，收到 offset %d", ErrBadOffset, size, offset)
	}
	end := offset + int64(len(data))
	if end <= size {
		return size, nil
	}
	data = data[size-offset:]
	if size+int64(len(data)) > maxLogSize {
		return size, nil
	}
	if _, err := f.WriteAt(data, size); err != nil {
		return size, err
	}
	return size + int64(len(data)), nil
}

// Delete 删除日志（保留策略，PLAN #10.3）；日志不存在时不报错。
func (l *Store) Delete(attempt int64) error {
	defer l.lock(attempt)()
	if err := os.Remove(l.path(attempt)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// MoveTo 把日志移到 dst（同一个编号）；日志不存在时不报错，dst 中已有时不覆盖。两个 Store 必须在同一个文件系统上。
func (l *Store) MoveTo(dst *Store, attempt int64) error {
	defer l.lock(attempt)()
	defer dst.lock(attempt)()
	if _, err := os.Stat(dst.path(attempt)); err == nil {
		return nil
	}
	if err := os.Rename(l.path(attempt), dst.path(attempt)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Size 返回日志当前的长度（不存在时为 0）。
func (l *Store) Size(attempt int64) (int64, error) {
	st, err := os.Stat(l.path(attempt))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Read 从 offset 开始读取最多 limit 字节。
func (l *Store) Read(attempt, offset, limit int64) ([]byte, int64, error) {
	f, err := os.Open(l.path(attempt))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	b, err := io.ReadAll(io.LimitReader(f, limit))
	return b, offset + int64(len(b)), err
}

// Open 打开完整的日志用于下载，返回内容和当前长度；日志不存在时返回空内容（与 Read 一致）。
func (l *Store) Open(attempt int64) (io.ReadCloser, int64, error) {
	f, err := os.Open(l.path(attempt))
	if errors.Is(err, fs.ErrNotExist) {
		return io.NopCloser(bytes.NewReader(nil)), 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close() // 只读打开的文件，关闭失败不影响结果
		return nil, 0, err
	}
	// 只发送打开时的长度：日志可能还在增长，下载的是这一刻的完整内容
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(f, st.Size()), f}, st.Size(), nil
}

// Tail 返回最后 n 行（去掉 ANSI 转义序列）。读取失败时返回空字符串：Tail 只用于摘要展示。
func (l *Store) Tail(attempt int64, n int) string {
	f, err := os.Open(l.path(attempt))
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	start := max(st.Size()-tailWindow, 0)
	b, err := io.ReadAll(io.NewSectionReader(f, start, st.Size()-start))
	if err != nil {
		return ""
	}
	b = bytes.TrimRight(b, "\n")
	lines := strings.Split(string(b), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return stripANSI(strings.Join(lines, "\n"))
}

// TailBytes 返回最后 n 字节（去掉终端控制字符）：窗口中有换行时从一个完整的行开始，没有时返回这一段的全部内容
// （最后一个换行之后的半行）。只用于摘要展示，读取失败时返回 nil。
func (l *Store) TailBytes(attempt, n int64) []byte {
	if n <= 0 {
		return nil
	}
	f, err := os.Open(l.path(attempt))
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	start := max(st.Size()-n, 0)
	b, err := io.ReadAll(io.NewSectionReader(f, start, st.Size()-start))
	if err != nil {
		return nil
	}
	b = []byte(stripANSI(string(b)))
	// 截断的位置可能在半个行或半个转义序列上：从第一个换行之后开始
	if i := bytes.IndexByte(b, '\n'); i >= 0 && start > 0 {
		b = b[i+1:]
	}
	return bytes.TrimRight(b, "\n")
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
