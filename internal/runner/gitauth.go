package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// 认证冲突的检查（与 AI 的传输包装相同）：host executor 的 step 以
// Runner 的用户运行，git 可能读到机器上的 netrc 和 git 配置。传输包装在启动真实的远程助手之前按这次传输的地址检查，有冲突
// 或查询失败时只拒绝这一次传输（不开操作、不签发）。诊断只有键和来源（地址只写 safeGitURL），不含任何值。

// gitExtraHeaderKey 是会给请求加认证头的 git 配置。
const gitExtraHeaderKey = "http.extraheader"

// gitConfigOnlyEnv 是只影响 git config 命令、不影响传输的环境变量：查询时去掉，否则查到的不是传输实际生效的配置。
var gitConfigOnlyEnv = []string{"GIT_CONFIG"}

// gitAuthConflict 是一项认证冲突：键（不含值）和能确定的来源。
type gitAuthConflict struct{ key, source string }

// joinConflicts 把冲突写成一句话（只有键和来源，没有值）。
func joinConflicts(cs []gitAuthConflict) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.key + "（来源：" + c.source + "）"
	}
	return strings.Join(parts, "；")
}

// envValue 返回 env（KEY=VALUE 列表）中 key 的值（最后一个生效）。
func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if s, ok := strings.CutPrefix(kv, key+"="); ok {
			v = s
		}
	}
	return v
}

// gitAuthConflicts 检查一次 git 传输（raw 是 insteadOf 改写之后的地址）会不会带上别的凭据：
//  1. 地址中带用户名或密码；
//  2. 生效的 http.extraheader（git config --get-urlmatch，以 env 和工作目录 dir（为空时继承本进程的，即 git 的）读取）：
//     退出码 0 且值非空是冲突，1（没有）或值为空（空值清空了列表）不是，其他退出码是查询失败；
//  3. libcurl 可能读的 netrc 中有这个主机或 default（netrcConflicts）。
//
// 查询失败也作为一项返回（调用方同样拒绝这次传输）。git config 的查询最多等 gitAuthQueryTimeout（例如配置文件在卡住的
// 网络文件系统上）：超时按查询失败处理，包装不会一直挂在传输开始之前。
func gitAuthConflicts(raw, dir string, env []string) []gitAuthConflict {
	return gitAuthConflictsWithin(raw, dir, env, gitAuthQueryTimeout)
}

// gitAuthQueryTimeout 是认证冲突检查中一次 git config 查询的期限。
const gitAuthQueryTimeout = 30 * time.Second

// gitAuthConflictsWithin 同 gitAuthConflicts，git config 的查询最多等 timeout。
func gitAuthConflictsWithin(raw, dir string, env []string, timeout time.Duration) []gitAuthConflict {
	var out []gitAuthConflict
	u, err := url.Parse(raw)
	if err != nil {
		u = nil
	}
	query := raw
	if u != nil && u.User != nil {
		out = append(out, gitAuthConflict{key: "地址中的用户名或密码", source: "地址 " + safeGitURL(raw) + "（可能来自 url.<地址>.insteadOf）"})
		// 已经认出的密码不再放进诊断子进程的参数（用户名参与 URL 匹配，保留）
		if _, ok := u.User.Password(); ok {
			v := *u
			v.User = url.User(u.User.Username())
			query = v.String()
		}
	}
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(gitConfigOnlyEnv, k)
	})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := groupCommand(ctx, "git", "config", "-z", "--get-urlmatch", gitExtraHeaderKey, query)
	cmd.Env, cmd.Dir = env, dir
	// stderr 不收集：git 的诊断中可能有配置内容，诊断只报告退出码
	val, err := cmd.Output()
	code := 0
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	switch {
	case code == 0 && hasHeaderValue(val):
		out = append(out, gitAuthConflict{key: gitExtraHeaderKey, source: "git 配置（git 2.46 及以上可以用 git config get --show-origin --url=<地址> " + gitExtraHeaderKey + " 查看来源）"})
	case code != 0 && code != 1:
		out = append(out, gitAuthConflict{key: gitExtraHeaderKey, source: fmt.Sprintf("git 配置查询失败（git config --get-urlmatch 退出码 %d，例如配置文件损坏）：在同一个目录中运行 git config --get-urlmatch %s <地址> 查看原因",
			code, gitExtraHeaderKey)})
	}
	if u != nil && u.Hostname() != "" {
		out = append(out, netrcConflicts(u.Hostname(), dir, env, timeout)...)
	}
	return out
}

// netrcFiles 返回 libcurl 可能读的 netrc，全部检查（保守：不按 libcurl 的版本区分）：NETRC 环境变量指定的文件（较新的
// libcurl；相对路径按传输的工作目录 dir，为空时是本进程的），HOME 下的 .netrc，账户主目录下的 .netrc（HOME 没有设置时
// libcurl 用它）。Windows 的 _netrc 不处理。
func netrcFiles(dir string, env []string) []string {
	var files []string
	// 都按原始分量拼接（joinRaw，与 libcurl 的 "%s/.netrc" 相同），由文件系统解析符号链接和 ..：词法规整会让检查的与
	// libcurl 读的不是同一个文件
	if f := envValue(env, "NETRC"); f != "" {
		if !filepath.IsAbs(f) {
			f = joinRaw(dir, f) // dir 为空时相对本进程的当前目录（传输包装与 libcurl 在同一个目录）
		}
		files = append(files, f)
	}
	if h := envValue(env, "HOME"); h != "" {
		files = append(files, joinRaw(h, ".netrc"))
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		files = append(files, joinRaw(u.HomeDir, ".netrc"))
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// netrcSep 是 netrc 中词的分界：libcurl 的 ISSPACE（空格、\t、\n、\v、\f、\r；Go 正则的 \s 不含 \v）和引号。
const netrcSep = `[ \t\n\v\f\r"']`

// netrcWord 返回匹配以 netrcSep 或首尾为界的 w（不区分大小写）的正则。
func netrcWord(w string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|` + netrcSep + `)` + regexp.QuoteMeta(w) + `(` + netrcSep + `|$)`)
}

// netrcEscapeRe 匹配引号中的反斜杠：较新的 libcurl 在带引号的词中处理转义（"git\hub.com" 即 github.com），按词法找不出，
// 一律算冲突。
var netrcEscapeRe = regexp.MustCompile(`"[^"\n]*\\`)

// netrcDefaultRe 匹配 netrc 中对所有主机生效的 default（libcurl 不区分大小写）。
var netrcDefaultRe = netrcWord("default")

// hasHeaderValue 判断 git config -z --get-urlmatch http.extraheader 的输出（生效的最后一个值加一个 NUL）是不是会让 git 带上
// 请求头。与 git 的 http.c 一致：只有零长度的值清空请求头列表，其余的值（包括只有空白的、首尾带空白的）都追加；最后一个值
// 是零长度时列表为空，否则至少有一个请求头。按原始值判断，不去掉空白（只去掉 git 追加的 NUL）。与 kiln-orca 的
// gitexec.ts hasHeaderValue 相同。
func hasHeaderValue(out []byte) bool {
	return len(bytes.TrimSuffix(out, []byte{0})) > 0
}

// maxNetrcBytes 是检查 netrc 时最多读的字节数（正常的 netrc 只有几行；更大的按无法检查处理）。
const maxNetrcBytes = 1 << 20

// readNetrc 有界地读一个 netrc：最多等 timeout（例如主目录在无响应的网络文件系统上；到期返回错误，底层的读取在后台
// 自己结束：包装是短命的进程），以 O_NONBLOCK 打开（没有写端的 FIFO 不会卡在打开上），不是普通文件、超过 maxNetrcBytes
// 的返回错误。不存在时返回 fs.ErrNotExist。错误由调用方按查询失败（冲突）处理。
func readNetrc(file string, timeout time.Duration) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := readNetrcBounded(file)
		ch <- result{b, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-timer.C:
		return nil, fmt.Errorf("读取超时（%v）", timeout)
	}
}

func readNetrcBounded(file string) ([]byte, error) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	// ENOENT：不存在；ENOTDIR：路径中有一段不是目录（例如 HOME=/dev/null 时的 /dev/null/.netrc）——都按没有这个文件处理，
	// 不是"无法检查"。与 kiln-orca 的 readNetrc 相同。
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, errors.New("读不了（没有权限）")
		}
		return nil, errors.New("读不了（读取失败）")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, errors.New("读不了（读取失败）")
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("不是普通文件")
	}
	if st.Size() > maxNetrcBytes {
		return nil, fmt.Errorf("超过 %d 字节", maxNetrcBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxNetrcBytes+1))
	if err != nil {
		return nil, errors.New("读不了（读取失败）")
	}
	if len(b) > maxNetrcBytes {
		return nil, fmt.Errorf("超过 %d 字节", maxNetrcBytes)
	}
	return b, nil
}

// netrcConflicts 检查 netrc 中有没有可能对 host 生效的条目。有意保守、不完整解析 netrc：以 libcurl 认的空白、引号或首尾为界
// 出现这个主机名或 default（都不区分大小写），或者引号中有转义，即算冲突。这样会误报（注释、密码、macdef 中出现主机名也
// 算）；按 libcurl 的 netrc 词法不会漏报（Windows 的 _netrc 除外）。存在但读不了时是查询失败。只报告文件和键，不输出内容。
func netrcConflicts(host, dir string, env []string, timeout time.Duration) []gitAuthConflict {
	var out []gitAuthConflict
	hostRe := netrcWord(host)
	for _, file := range netrcFiles(dir, env) {
		b, err := readNetrc(file, timeout)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			out = append(out, gitAuthConflict{key: "netrc", source: fmt.Sprintf("查询失败：%s %v", file, err)})
			continue
		}
		switch {
		case hostRe.Match(b):
			out = append(out, gitAuthConflict{key: "netrc 中的主机 " + host, source: file})
		case netrcDefaultRe.Match(b):
			out = append(out, gitAuthConflict{key: "netrc 中的 default", source: file})
		case netrcEscapeRe.Match(b):
			out = append(out, gitAuthConflict{key: "netrc 中带转义的引号（无法确定对哪个主机生效）", source: file})
		}
	}
	return out
}
