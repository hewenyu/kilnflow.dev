package release

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// scanChunk 是源仓库名检查每次读取的字节数。
const scanChunk = 1 << 20

// SourceNameCheck 检查要发布到公开仓库的内容中是否包含私有源仓库名（按字节搜索，不区分大小写，PLAN #13.6）。
// 检查 asset 名、包内路径、原始文件内容和 Release 说明。任何文件读取失败都返回错误：检查不完整时不能放行。
func SourceNameCheck(fullName string, assets []Asset, notes string) error {
	needle := strings.ToLower(fullName)
	var hits []string
	for _, a := range assets {
		if strings.Contains(strings.ToLower(a.Name), needle) {
			hits = append(hits, "asset 名 "+a.Name)
		}
		for _, n := range a.Names {
			if strings.Contains(strings.ToLower(n), needle) {
				hits = append(hits, fmt.Sprintf("%s 中的路径 %s", a.Name, n))
			}
		}
		if len(a.Files) == 0 {
			hit, err := scanFile(a.Name, a.Path, needle)
			if err != nil {
				return err
			}
			hits = appendHit(hits, hit)
		}
		for i, p := range a.Files {
			hit, err := scanFile(fmt.Sprintf("%s 中的第 %d 个文件", a.Name, i+1), p, needle)
			if err != nil {
				return err
			}
			hits = appendHit(hits, hit)
		}
	}
	if strings.Contains(strings.ToLower(notes), needle) {
		hits = append(hits, "Release 说明")
	}
	if len(hits) > 0 {
		return fmt.Errorf("源仓库名检查失败：要发布到公开仓库的内容中包含私有仓库名 %s：%s。确认可以公开时设置 allow-source-name: true",
			fullName, strings.Join(hits, "；"))
	}
	return nil
}

func appendHit(hits []string, hit string) []string {
	if hit == "" {
		return hits
	}
	return append(hits, hit)
}

// scanFile 在文件中搜索 needle（needle 已是小写），返回命中描述；没有命中时返回空字符串。
func scanFile(label, p, needle string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("源仓库名检查无法读取 %s：%w", label, err)
	}
	defer f.Close()
	hit, err := scanReader(f, []byte(needle))
	if err != nil {
		return "", fmt.Errorf("源仓库名检查无法读取 %s：%w", label, err)
	}
	if hit < 0 {
		return "", nil
	}
	return fmt.Sprintf("%s（偏移 %d）", label, hit), nil
}

// scanReader 按块读取 r，返回 needle 第一次出现的偏移（不区分 ASCII 大小写），没有出现时返回 -1。
// 相邻块之间保留 len(needle)-1 字节，跨块的匹配也能找到。
func scanReader(r io.Reader, needle []byte) (int64, error) {
	if len(needle) == 0 {
		return 0, nil
	}
	keepMax := len(needle) - 1
	buf := make([]byte, keepMax+scanChunk)
	carried := 0  // buf 开头保留的上一块末尾字节数（已转换为小写）
	var off int64 // buf[carried] 在 r 中的偏移
	for {
		n, err := r.Read(buf[carried:])
		if n > 0 {
			data := buf[:carried+n]
			asciiLower(data[carried:])
			if i := bytes.Index(data, needle); i >= 0 {
				return off - int64(carried) + int64(i), nil
			}
			keep := min(keepMax, len(data))
			copy(buf, data[len(data)-keep:])
			carried = keep
			off += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return -1, nil
		}
		if err != nil {
			return -1, err
		}
	}
}

// asciiLower 原地把 ASCII 大写字母转为小写（GitHub 的仓库名只含 ASCII，按字节转换不会改变偏移）。
func asciiLower(b []byte) {
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
}
