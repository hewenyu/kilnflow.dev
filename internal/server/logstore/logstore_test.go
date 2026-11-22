package logstore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	l, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// content 读出日志的全部内容。
func content(t *testing.T, l *Store, attempt int64) string {
	t.Helper()
	b, _, err := l.Read(attempt, 0, maxLogSize+1)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAppendIsIdempotent(t *testing.T) {
	l, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := l.Append(1, 0, []byte("hello ")); err != nil || n != 6 {
		t.Fatalf("Append = %d, %v", n, err)
	}
	// 重传重叠的部分只追加新数据
	if n, err := l.Append(1, 0, []byte("hello world\n")); err != nil || n != 12 {
		t.Fatalf("Append = %d, %v", n, err)
	}
	if _, err := l.Append(1, 20, []byte("x")); !errors.Is(err, ErrBadOffset) {
		t.Fatalf("不连续的 offset 应返回 ErrBadOffset：%v", err)
	}
	if _, err := l.Append(1, -1, []byte("x")); !errors.Is(err, ErrBadOffset) {
		t.Fatalf("负的 offset 应返回 ErrBadOffset：%v", err)
	}
	b, next, err := l.Read(1, 0, 100)
	if err != nil || string(b) != "hello world\n" || next != 12 {
		t.Fatalf("Read = %q, %d, %v", b, next, err)
	}
	if size, err := l.Size(2); err != nil || size != 0 {
		t.Fatalf("不存在的日志 Size = %d, %v", size, err)
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	// 目录不存在时逐级创建
	dir := filepath.Join(t.TempDir(), "a", "b")
	if _, err := New(dir); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("应创建日志目录：%v", err)
	}
	// 路径被文件占用时报错
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(file, "logs")); err == nil {
		t.Fatal("无法创建目录时应报错")
	}
}

func TestPublishLogID(t *testing.T) {
	t.Parallel()
	if got := PublishLogID(42); got != -42 {
		t.Fatalf("PublishLogID(42) = %d", got)
	}
	// 发布 job 的日志与同号 attempt 的日志互不影响
	l := newStore(t)
	if _, err := l.Append(42, 0, []byte("attempt")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(PublishLogID(42), 0, []byte("publish")); err != nil {
		t.Fatal(err)
	}
	if a, p := content(t, l, 42), content(t, l, PublishLogID(42)); a != "attempt" || p != "publish" {
		t.Fatalf("attempt 日志 %q，发布日志 %q", a, p)
	}
}

func TestStore_Append(t *testing.T) {
	t.Parallel()
	// 每个用例先写入 "0123456789"，再 Append(offset, data)
	const initial = "0123456789"
	cases := []struct {
		name     string
		offset   int64
		data     string
		wantSize int64
		wantLog  string
		wantErr  bool
	}{
		{"紧接末尾追加", 10, "abc", 13, initial + "abc", false},
		{"完全重复的重传", 0, initial, 10, initial, false},
		{"重传其中一段", 3, "345", 10, initial, false},
		{"重叠部分忽略，只写新数据", 8, "89ab", 12, initial + "ab", false},
		{"重叠部分与已有内容不同时仍以已有内容为准", 8, "XXyz", 12, initial + "yz", false},
		{"空数据", 10, "", 10, initial, false},
		{"offset 超过末尾", 11, "x", 10, initial, true},
		// 负数 offset 不能当作"与已有内容重叠"处理，否则会丢掉 data 开头的若干字节写在末尾
		{"offset 为负数", -5, "0123456789abcde", 10, initial, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			l := newStore(t)
			if _, err := l.Append(1, 0, []byte(initial)); err != nil {
				t.Fatal(err)
			}
			n, err := l.Append(1, c.offset, []byte(c.data))
			if (err != nil) != c.wantErr {
				t.Fatalf("Append 的错误为 %v，wantErr=%v", err, c.wantErr)
			}
			if n != c.wantSize {
				t.Errorf("Append 返回 %d，应为 %d", n, c.wantSize)
			}
			if got := content(t, l, 1); got != c.wantLog {
				t.Errorf("日志内容 = %q，应为 %q", got, c.wantLog)
			}
			if size, err := l.Size(1); err != nil || size != c.wantSize {
				t.Errorf("Size = %d, %v，应为 %d", size, err, c.wantSize)
			}
		})
	}

	t.Run("新日志从 offset 0 开始", func(t *testing.T) {
		t.Parallel()
		l := newStore(t)
		if n, err := l.Append(7, 5, []byte("x")); err == nil || n != 0 {
			t.Fatalf("新日志的 offset 不是 0 时应报错：%d, %v", n, err)
		}
		if n, err := l.Append(7, 0, nil); err != nil || n != 0 {
			t.Fatalf("空数据：%d, %v", n, err)
		}
	})

	t.Run("目录不可写", func(t *testing.T) {
		t.Parallel()
		l := newStore(t)
		if err := os.RemoveAll(l.dir); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Append(1, 0, []byte("x")); err == nil {
			t.Fatal("日志目录不存在时应报错")
		}
	})
}

// 单个日志的上限：写入后会超出上限的数据整段不写，不报错，返回原来的长度；正好达到上限可以写入。
func TestStore_AppendLimit(t *testing.T) {
	t.Parallel()
	l := newStore(t)
	// 用稀疏文件构造接近上限的日志，不真正写入 100 MiB
	if err := os.WriteFile(l.path(1), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(l.path(1), maxLogSize-3); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name     string
		offset   int64
		data     string
		wantSize int64
	}{
		{"超出上限的一段整段不写", maxLogSize - 3, "abcd", maxLogSize - 3},
		{"正好达到上限", maxLogSize - 3, "abc", maxLogSize},
		{"达到上限后不再增长", maxLogSize, "d", maxLogSize},
		{"重传已有的部分仍然幂等", maxLogSize - 3, "abc", maxLogSize},
	}
	for _, s := range steps {
		n, err := l.Append(1, s.offset, []byte(s.data))
		if err != nil || n != s.wantSize {
			t.Fatalf("%s：Append = %d, %v，应为 %d", s.name, n, err, s.wantSize)
		}
	}
	b, next, err := l.Read(1, maxLogSize-3, 10)
	if err != nil || string(b) != "abc" || next != maxLogSize {
		t.Fatalf("末尾的内容 = %q, %d, %v", b, next, err)
	}
}

// 同一个 attempt 的并发重传（例如 Runner 超时重发）不会写坏日志。
func TestStore_AppendConcurrent(t *testing.T) {
	t.Parallel()
	l := newStore(t)
	chunks := []string{"line 1\n", "line 2\n", "line 3\n", "line 4\n"}
	var want strings.Builder
	for _, c := range chunks {
		want.WriteString(c)
	}
	var wg sync.WaitGroup
	// 8 个写入者各自按顺序发送全部数据，offset 相同的片段互为重传。每个写入者发送第 k 段之前自己已经写过（或重传过）
	// 前面的段，所以不会出现不连续的写入；返回的长度至少是这一段的末尾
	for range 8 {
		wg.Go(func() {
			var off int64
			for _, c := range chunks {
				off += int64(len(c))
				n, err := l.Append(1, off-int64(len(c)), []byte(c))
				if err != nil || n < off {
					t.Errorf("Append(offset %d) = %d, %v", off-int64(len(c)), n, err)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := content(t, l, 1); got != want.String() {
		t.Fatalf("并发重传后的日志 = %q，应为 %q", got, want.String())
	}
}

func TestStore_Read(t *testing.T) {
	t.Parallel()
	l := newStore(t)
	if _, err := l.Append(1, 0, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name          string
		attempt       int64
		offset, limit int64
		want          string
		wantNext      int64
	}{
		{"从头读取全部", 1, 0, 100, "0123456789", 10},
		{"按 limit 截断", 1, 0, 4, "0123", 4},
		{"从中间读取", 1, 4, 3, "456", 7},
		{"读到末尾", 1, 7, 100, "789", 10},
		{"offset 在末尾：没有新内容", 1, 10, 100, "", 10},
		{"offset 超过末尾", 1, 20, 100, "", 20},
		{"limit 为 0", 1, 3, 0, "", 3},
		{"日志不存在", 2, 0, 100, "", 0},
	} {
		b, next, err := l.Read(c.attempt, c.offset, c.limit)
		if err != nil {
			t.Fatalf("%s：%v", c.name, err)
		}
		if string(b) != c.want || next != c.wantNext {
			t.Errorf("%s：Read = %q, %d，应为 %q, %d", c.name, b, next, c.want, c.wantNext)
		}
	}
	if _, _, err := l.Read(1, -1, 10); err == nil {
		t.Error("负数 offset 应报错")
	}
}

func TestStore_Open(t *testing.T) {
	t.Parallel()
	l := newStore(t)
	if _, err := l.Append(1, 0, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	rc, size, err := l.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	// 打开之后日志继续增长：下载的是打开时的完整内容
	if _, err := l.Append(1, 6, []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "first\n" || size != 6 {
		t.Fatalf("Open = %q, %d，应为打开时的内容", b, size)
	}

	// 不存在的日志：空内容
	rc, size, err = l.Open(2)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, err := io.ReadAll(rc); err != nil || len(b) != 0 || size != 0 {
		t.Fatalf("不存在的日志 Open = %q, %d, %v", b, size, err)
	}
}

func TestTailStripsANSI(t *testing.T) {
	l, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(-3, 0, []byte("a\n\x1b[31mb\x1b[0m\nc\n")); err != nil {
		t.Fatal(err)
	}
	if got := l.Tail(-3, 2); got != "b\nc" {
		t.Fatalf("Tail = %q", got)
	}
}

func TestStore_Tail(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, log string
		n         int
		want      string
	}{
		{"行数不足时返回全部", "a\nb\n", 5, "a\nb"},
		{"末尾多个换行", "a\nb\n\n\n", 1, "b"},
		{"末尾没有换行", "a\nb\nc", 2, "b\nc"},
		{"空行保留", "a\n\nb\n", 3, "a\n\nb"},
		{"去掉多种 ANSI 序列", "\x1b[1;32mok\x1b[0m \x1b[2K\x1b[Gdone\n", 1, "ok done"},
		{"不完整的转义序列在末尾", "x\x1b[31", 1, "x"},
		{"单独的 ESC 保留", "a\x1bb", 1, "a\x1bb"},
		{"空日志", "", 3, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			l := newStore(t)
			if _, err := l.Append(1, 0, []byte(c.log)); err != nil {
				t.Fatal(err)
			}
			if got := l.Tail(1, c.n); got != c.want {
				t.Fatalf("Tail(%d) = %q，应为 %q", c.n, got, c.want)
			}
		})
	}

	t.Run("日志不存在", func(t *testing.T) {
		t.Parallel()
		if got := newStore(t).Tail(9, 3); got != "" {
			t.Fatalf("Tail = %q", got)
		}
	})

	t.Run("只读取末尾的窗口", func(t *testing.T) {
		t.Parallel()
		l := newStore(t)
		// 一行超长的开头加上末尾几行：窗口之前的内容不会出现在结果中
		log := "HEAD" + strings.Repeat("x", tailWindow) + "\nlast 1\nlast 2\n"
		if _, err := l.Append(1, 0, []byte(log)); err != nil {
			t.Fatal(err)
		}
		got := l.Tail(1, 100)
		if strings.Contains(got, "HEAD") || !strings.HasSuffix(got, "\nlast 1\nlast 2") {
			t.Fatalf("Tail 应只包含末尾 %d 字节，实际 %d 字节，开头 %q", tailWindow, len(got), got[:min(len(got), 10)])
		}
		if len(got) > tailWindow {
			t.Fatalf("Tail 的长度 %d 超过窗口 %d", len(got), tailWindow)
		}
	})
}

func TestStripANSI(t *testing.T) {
	t.Parallel()
	in := "\x1b[0m\x1b[38;5;196mred\x1b[0m plain \x1b[?25lhidden-cursor"
	if got := stripANSI(in); got != "red plain hidden-cursor" {
		t.Fatalf("stripANSI = %q", got)
	}
	if got := stripANSI(string(bytes.Repeat([]byte("ab"), 3))); got != "ababab" {
		t.Fatalf("没有转义序列时原样返回：%q", got)
	}
}

// Delete 删除日志（包括发布 job 的日志），不存在时不报错；删除后读取得到空内容。
func TestDelete(t *testing.T) {
	t.Parallel()
	l := newStore(t)
	for _, id := range []int64{3, PublishLogID(4)} {
		if _, err := l.Append(id, 0, []byte("x\n")); err != nil {
			t.Fatal(err)
		}
		if err := l.Delete(id); err != nil {
			t.Fatal(err)
		}
		if size, err := l.Size(id); err != nil || size != 0 {
			t.Errorf("删除后 Size = %d, %v", size, err)
		}
		if err := l.Delete(id); err != nil {
			t.Errorf("删除不存在的日志不应报错：%v", err)
		}
	}
}

// TestMoveTo：日志移到另一个 Store 后从原处消失；不存在的日志、目标已有的日志都不报错（目标不被覆盖）。
func TestMoveTo(t *testing.T) {
	t.Parallel()
	src, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dst, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Append(1, 0, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := src.MoveTo(dst, 1); err != nil {
		t.Fatal(err)
	}
	if n, _ := src.Size(1); n != 0 {
		t.Fatalf("移走后原处还有 %d 字节", n)
	}
	if b, _, err := dst.Read(1, 0, 100); err != nil || string(b) != "old" {
		t.Fatalf("目标中的日志 = %q, %v", b, err)
	}
	if err := src.MoveTo(dst, 2); err != nil {
		t.Fatalf("不存在的日志：%v", err)
	}
	if _, err := src.Append(1, 0, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := src.MoveTo(dst, 1); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := dst.Read(1, 0, 100); string(b) != "old" {
		t.Fatalf("目标已有时不应覆盖：%q", b)
	}
}

// TailBytes 只返回最后 n 字节，从一个完整的行开始，并去掉终端控制字符（PLAN #33.9 的失败日志尾部）。
func TestStore_TailBytes(t *testing.T) {
	l := newStore(t)
	if got := l.TailBytes(1, 100); got != nil {
		t.Errorf("没有日志时 TailBytes = %q", got)
	}
	if _, err := l.Append(1, 0, []byte("first line\n\x1b[32msecond\x1b[0m line\nthird line\n")); err != nil {
		t.Fatal(err)
	}
	if got := l.TailBytes(1, 64); string(got) != "first line\nsecond line\nthird line" {
		t.Errorf("全部内容 = %q", got)
	}
	if got := l.TailBytes(1, 12); string(got) != "third line" {
		t.Errorf("截断的位置应从行首开始 = %q", got)
	}
	// 窗口里没有换行：返回这一段的全部内容（半行）
	if _, err := l.Append(1, 43, []byte("tail-without-newline")); err != nil {
		t.Fatal(err)
	}
	if got := l.TailBytes(1, 4); string(got) != "line" {
		t.Errorf("没有换行的窗口 = %q", got)
	}
	if got := l.TailBytes(1, 0); got != nil {
		t.Errorf("n=0 = %q", got)
	}
}
