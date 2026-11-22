package cas

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errRead 是 failingReader 返回的读取错误。
var errRead = errors.New("读取失败")

// failingReader 先返回 prefix 的内容，然后返回 errRead。
func failingReader(prefix string) io.Reader {
	return io.MultiReader(strings.NewReader(prefix), errReader{})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errRead }

// fileAsRoot 返回一个 FSStore，它的 Root 是一个普通文件：需要在 Root 下建目录或打开对象的操作都会失败，
// 但失败原因不是"不存在"。
func fileAsRoot(t *testing.T) *FSStore {
	t.Helper()
	p := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(p, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &FSStore{Root: p}
}

// readerStore 是只有 Get 有意义的 Store：Get 返回给定的 reader、声明的大小或错误（用于模拟后端读取出错）。
type readerStore struct {
	r    io.Reader
	size int64
	err  error
}

func (readerStore) Has(context.Context, string) (bool, error) { return false, nil }

func (readerStore) Put(context.Context, string, io.Reader) (int64, error) { return 0, nil }

func (s readerStore) Get(context.Context, string) (io.ReadCloser, int64, error) {
	if s.err != nil {
		return nil, 0, s.err
	}
	return io.NopCloser(s.r), s.size, nil
}
