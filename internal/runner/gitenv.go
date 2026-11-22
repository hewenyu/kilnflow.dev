package runner

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// step 中 git 使用凭据通道的设置（PLAN #8.4）：不写任何文件，全部经环境变量。
//   - GIT_CONFIG_COUNT 等环境变量配置（追加在 workflow 自己的之后）：对任务仓库的 <协议>://<主机> 先清空 credential
//     helper，再设为 kiln-runner git-credential，按路径区分仓库（useHttpPath）；core.askPass 为空；
//   - GIT_TERMINAL_PROMPT=0，去掉 GIT_ASKPASS、SSH_ASKPASS：不给凭据时不回落到个人的交互凭据；
//   - GIT_EXEC_PATH 指向 git exec 镜像：原目录的文件逐个符号链接，远程助手换成 kiln-runner（传输包装）。host 由 Runner
//     按 step 的 PATH 中的 git 建好；oci 由 step 的启动脚本按容器中的 git 建（每个镜像的 exec 目录不同）。

// gitAccess 是 step 中 git 访问凭据通道的设置（执行环境中看到的路径）。
type gitAccess struct {
	// scope 是 credential helper 生效的地址（<协议>://<主机>）。
	scope string
	// helper 是 credential helper（git 配置的值）。
	helper string
	// socket 是凭据通道的 socket；为空表示通道在这个执行环境中不可用（helper 只说明原因）。
	socket string
	// execPath、realExecPath 是 git exec 镜像和它的来源（host；oci 由启动脚本设置，这里为空）。
	execPath, realExecPath string
	// configInScript 为真时（oci）git 配置不直接写进环境变量配置，而是交给 step 的启动脚本（ociGitConfigScript）追加：
	// 镜像的 ENV 中可能已有 GIT_CONFIG_COUNT，合并之后实际生效的值只在容器中才知道。
	configInScript bool
}

// gitHelperCmd 返回执行 kiln-runner git-credential 的 credential helper（git 用 shell 执行以 ! 开头的值，路径按 shell 引用）。
func gitHelperCmd(exe string) string { return "!" + shQuote(exe) + " " + gitCredentialCmd }

// gitRefusingHelper 返回只说明原因、不给凭据的 credential helper（POSIX sh）：凭据通道不可用时，git 要凭据会得到这个说明。
func gitRefusingHelper(reason string) string {
	return "!f() { printf '%s\\n' " + shQuote("kiln-runner："+reason) + " >&2; echo quit=1; }; f"
}

// gitWrapperEnvKeys 和 gitWrapperEnvPrefixes 是外层 kiln-orca task exec 与凭据通道的私有变量：继承下来会让 step 中的 git
// 递归套用外层的传输包装，被外层按宿主凭据冲突拒绝。按变量名和前缀（配置项编号不定）清理，之后再注入本次
// 合法的值。GIT_CONFIG_KEY_<n> / GIT_CONFIG_VALUE_<n> 不在这里：workflow 与 step 用它们追加配置是既有契约，照常保留。
var gitWrapperEnvKeys = []string{"GIT_EXEC_PATH", "KILN_ORCA_GIT_OP", gitOpEnv, gitOpErrorEnv, gitSocketEnv, gitRealExecEnv, ociGitConfigN}

var gitWrapperEnvPrefixes = []string{ociGitConfigKey, ociGitConfigValue}

// clearGitWrapperEnv 从 env 中删除外层包装与凭据通道的私有变量。在合并 workflow / step 的 env 之后调用（step 的 env 不是
// 安全边界，但不能让它把外层的包装递归套进来）。
func clearGitWrapperEnv(env map[string]string) {
	for k := range env {
		if slices.Contains(gitWrapperEnvKeys, k) || slices.ContainsFunc(gitWrapperEnvPrefixes, func(p string) bool { return strings.HasPrefix(k, p) }) {
			delete(env, k)
		}
	}
}

// gitAccessConfig 返回 step 中追加的 git 配置：对任务仓库的主机先清空 credential helper，再设为 Kiln 的 helper，按路径区分
// 仓库，core.askPass 为空。host 直接追加到环境变量配置，oci 交给启动脚本追加（ociGitConfigScript）。
func gitAccessConfig(scope, helper string) [][2]string {
	return [][2]string{
		{scope + ".helper", ""},
		{scope + ".helper", helper},
		{scope + ".useHttpPath", "true"},
		{"core.askPass", ""},
	}
}

// apply 在 step 的环境变量中加入 git 的设置（在合并 workflow 的 env 之后调用，workflow 不能覆盖）。
func (a *gitAccess) apply(env map[string]string) {
	clearGitWrapperEnv(env)
	for _, k := range []string{"GIT_ASKPASS", "SSH_ASKPASS"} {
		delete(env, k)
	}
	env["GIT_TERMINAL_PROMPT"] = "0"
	scope := "credential." + a.scope
	kv := gitAccessConfig(scope, a.helper)
	if a.configInScript {
		for i, p := range kv {
			env[ociGitConfigKey+strconv.Itoa(i)] = p[0]
			env[ociGitConfigValue+strconv.Itoa(i)] = p[1]
		}
		env[ociGitConfigN] = strconv.Itoa(len(kv))
	} else {
		appendGitConfig(env, kv)
	}
	if a.socket != "" {
		env[gitSocketEnv] = a.socket
	}
	if a.execPath != "" {
		env["GIT_EXEC_PATH"] = a.execPath
		env[gitRealExecEnv] = a.realExecPath
	}
}

// appendGitConfig 把 kv 追加到环境变量配置（GIT_CONFIG_COUNT、GIT_CONFIG_KEY_<n>、GIT_CONFIG_VALUE_<n>）中已有的项之后：
// git 按顺序读取，后面的生效。已有的 GIT_CONFIG_COUNT 是 git 会拒绝的值时什么都不改（git 照样以 bogus count 失败，与没有
// Kiln 时相同；不能从 0 开始覆盖已有的项）。
func appendGitConfig(env map[string]string, kv [][2]string) {
	n, ok := gitConfigCount(env["GIT_CONFIG_COUNT"])
	if !ok {
		return
	}
	for _, p := range kv {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(n)] = p[0]
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(n)] = p[1]
		n++
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(n)
}

// gitConfigCount 按 git 的规则（config.c：strtoul 十进制，之后不能有别的字符，不超过 INT_MAX）解析 GIT_CONFIG_COUNT：开头
// 可以有空白（C 的 isspace）和一个正负号，之后是十进制数字（前导 0 不是八进制）；空字符串（及没有设置）是 0；"-0" 是 0，
// 其余负数、超过 INT_MAX、没有数字、有别的字符的 git 拒绝（ok 为 false）。在 git 2.39.5、2.47.3 上逐项核实；step 启动脚本中
// 的 ociGitConfigScript 与 kiln-orca 的 gitConfigCount 按同一规则。
func gitConfigCount(s string) (int, bool) {
	d := strings.TrimLeft(s, " \t\n\v\f\r")
	neg := false
	if d != "" && (d[0] == '+' || d[0] == '-') {
		neg, d = d[0] == '-', d[1:]
	}
	if d == "" {
		return 0, s == ""
	}
	if strings.Trim(d, "0123456789") != "" {
		return 0, false
	}
	d = strings.TrimLeft(d, "0")
	if d == "" {
		return 0, true
	}
	if neg || len(d) > maxGitConfigCountDigits {
		return 0, false
	}
	n, err := strconv.Atoi(d)
	if err != nil || n > math.MaxInt32 {
		return 0, false
	}
	return n, true
}

// maxGitConfigCountDigits 是 INT_MAX（2147483647）的位数：更长的（去掉前导 0 之后）一定超过它。
const maxGitConfigCountDigits = 10

// gitExecMirrorDir 是 host 上 git exec 镜像在 attempt 目录中的目录名的前缀（git-exec-<n>，每个真实的 exec 目录一个）。
const gitExecMirrorDir = "git-exec"

// buildGitExecMirror 在 dir 建 src（git 的 exec 目录）的镜像：其中的文件逐个符号链接（子模块等要用到 git-sh-i18n 之类的
// 脚本，不能只放远程助手），远程助手换成指向 exe（kiln-runner）的符号链接。
func buildGitExecMirror(dir, src, exe string) error {
	return buildGitExecMirrorWith(dir, src, exe, os.Symlink)
}

// buildGitExecMirrorWith 同 buildGitExecMirror，用 link 建符号链接（测试中注入失败）。先在 dir 旁边的临时目录（以 . 开头）
// 中建完整，再改名为 dir 发布：中途失败时删除临时目录，dir 不会以不完整的样子出现，也不占用这个名字。
func buildGitExecMirrorWith(dir, src, exe string, link func(oldname, newname string) error) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".tmp-")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp) // 只删本次建的临时目录
		}
	}()
	for _, e := range entries {
		target := joinRaw(src, e.Name()) // src 是 git --exec-path 的输出：原样拼接，与 git 自己找远程助手的路径相同
		if slices.Contains(gitRemoteHelpers, e.Name()) {
			target = exe
		}
		if err := link(target, filepath.Join(tmp, e.Name())); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	ok = true
	return nil
}

// hostGit 是 host executor 中 step 的 git 设置：每个 step 按它的 PATH（合并 workflow 的 env 之后）中的 git 选 exec 镜像，
// 按真实的 exec 目录在 attempt 目录中建一次、之后复用。step 的脚本自己再换 PATH、改 GIT_EXEC_PATH 的不管（PLAN #8.4）。
type hostGit struct {
	base      gitAccess // scope、helper、socket（socket 为空：通道不可用，helper 只说明原因）
	root, exe string
	// mirrors 是 真实的 exec 目录 → 镜像目录；noted 是已经写进 job 日志的说明（每种只写一次）。
	mirrors map[string]string
	noted   map[string]bool
	// next 是下一个镜像目录的编号（已经存在的名字跳过）；link 建符号链接（os.Symlink，测试中注入失败）。
	next int
	link func(oldname, newname string) error
}

func newHostGit(base gitAccess, root, exe string) *hostGit {
	return &hostGit{base: base, root: root, exe: exe, mirrors: map[string]string{}, noted: map[string]bool{}, link: os.Symlink}
}

// access 返回一个 step 的 git 设置：env 是 step 的环境变量，stepDir 是 step 的工作目录（绝对路径，已校验；PATH 中的相对项
// 按它解析，git --exec-path 也在那里执行）。找不到 git、镜像建不成时不设置 GIT_EXEC_PATH（git 的传输不经过包装，helper
// 不给凭据），原因写进 job 日志。
func (g *hostGit) access(ctx context.Context, env map[string]string, stepDir string, log *runnerkit.LogShipper) *gitAccess {
	a := g.base
	if a.socket == "" {
		return &a
	}
	note := func(msg string) *gitAccess {
		if !g.noted[msg] {
			g.noted[msg] = true
			log.Printf("%s", msg)
		}
		return &a
	}
	// 没有设置 PATH 时 shell 用自己的默认搜索路径（各 shell 不同），不去模拟：按找不到处理
	git := "git"
	if pathEnv, ok := env["PATH"]; ok {
		git = lookPathIn("git", pathEnv, stepDir)
	}
	if !filepath.IsAbs(git) {
		return note("step 的 PATH 中没有 git：step 中的 git 访问不会经过 Kiln 的凭据通道")
	}
	src, err := gitExecPath(ctx, git, stepDir, envList(env))
	if err != nil {
		return note(fmt.Sprintf("读取 %s 的 exec 目录失败（%v）：step 中的 git 访问不会经过 Kiln 的凭据通道", git, err))
	}
	dir, ok := g.mirrors[src]
	if !ok {
		var err error
		if dir, err = g.nextMirrorDir(); err == nil {
			err = buildGitExecMirrorWith(dir, src, g.exe, g.link)
		}
		if err != nil {
			return note(fmt.Sprintf("建 %s 的 git exec 镜像失败（%v）：step 中的 git 访问不会经过 Kiln 的凭据通道", git, err))
		}
		g.mirrors[src] = dir
	}
	a.execPath, a.realExecPath = dir, src
	return &a
}

// maxMirrorDirTries 是选镜像目录名时最多跳过的已存在的名字数。
const maxMirrorDirTries = 64

// nextMirrorDir 返回下一个还没有被占用的镜像目录名（git-exec-<n>）：只有确认名字已经存在（例如之前的残留）时才换下一个
// 编号，不存在即返回；查不了（父目录不可遍历、不是目录、I/O 错误）时立即返回错误，不当作"存在"或"不存在"。最多尝试
// maxMirrorDirTries 个名字。
func (g *hostGit) nextMirrorDir() (string, error) {
	for range maxMirrorDirTries {
		dir := filepath.Join(g.root, gitExecMirrorDir+"-"+strconv.Itoa(g.next))
		g.next++
		_, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return dir, nil
		case err != nil:
			return "", err
		}
	}
	return "", fmt.Errorf("%s 下已有 %d 个 %s-<n>，找不到空的名字", g.root, maxMirrorDirTries, gitExecMirrorDir)
}

// 容器中凭据通道的固定路径（oci executor）。
const (
	// ociGitDir 是 socket 所在目录的挂载点（读写）。
	ociGitDir = "/kiln/git"
	// ociKilnRunner 是 kiln-runner 的挂载点（只读）。
	ociKilnRunner = "/kiln/bin/kiln-runner"
	// ociGitExecPrefix 是容器中 git exec 镜像的目录（加上真实 exec 目录的校验和，换了 git 时用新的镜像）。
	ociGitExecPrefix = ociTmp + "/kiln-git-exec-"
	// ociGitSetupFunc 是 ociGitSetupScript 定义的函数。
	ociGitSetupFunc = "kiln_git_setup"
	// ociGitResetFunc 是 ociGitResetScript 定义的函数。
	ociGitResetFunc = "kiln_git_reset"
	// ociGitConfigFunc 是 ociGitConfigScript 定义的函数；ociGitConfigN、ociGitConfigKey<i>、ociGitConfigValue<i> 是交给它
	// 追加的 Kiln 的 git 配置（经 step 的环境变量文件，函数用完即 unset）。
	ociGitConfigFunc  = "kiln_git_config"
	ociGitConfigN     = "KILN_GIT_CONFIG_N"
	ociGitConfigKey   = "KILN_GIT_CONFIG_KEY_"
	ociGitConfigValue = "KILN_GIT_CONFIG_VALUE_"
)

// ociShellIFS 是脚本在子 shell 中使用的 IFS（默认值：空格、制表符、换行）：step 的环境可能改了 IFS。
const ociShellIFS = "IFS=' \t\n'\n"

// ociGitResetScript 返回 step 启动脚本开头在父 shell 中清理外层包装与凭据通道私有变量的部分（POSIX sh）：容器镜像的 ENV
// 里可能带着 Runner 进程自己的这些变量（Runner 在 kiln-orca task exec 中运行时），继承下来会让容器中的 git 递归套用外层的
// 传输包装，被外层按宿主凭据冲突拒绝。在 source 本次 step 的环境变量文件之前执行，之后由 ociGitConfigScript /
// ociGitSetupScript 重新写入本次合法的值；降级（没有 git、建镜像失败、通道不可用）时父 shell 中不残留旧的 GIT_EXEC_PATH。
// 只清 Kiln 自己的键，不碰普通用户或镜像的 GIT_CONFIG_* 配置。
//
// 暂存键 KILN_GIT_CONFIG_KEY_<n> / KILN_GIT_CONFIG_VALUE_<n> 的编号不定（镜像的 ENV 中可能是任意编号），所以按 export -p 的
// 变量名扫描并清掉所有以这两个前缀开头、且只含 [A-Za-z0-9_] 的变量。与 ociGitConfigScript / ociGitSetupScript 一样，计算在
// 子 shell（函数体是 ( … )）中完成、父 shell 用 eval 执行：父 shell 中除了被 unset 的变量之外什么都不改（脚本自己的变量和
// IFS 都在子 shell 中）。输出的变量名只来自固定清单和对 export -p 的严格校验（前缀 + 合法标识符），某个普通变量的值里即便
// 带换行、下一行长得像 export KILN_GIT_CONFIG_KEY_…=，也只会多 unset 一个（通常不存在的）私有前缀名，不会误拆普通变量名或
// 注入命令。
func ociGitResetScript() string {
	fixed := []string{"GIT_EXEC_PATH", gitRealExecEnv, gitOpEnv, gitOpErrorEnv, gitSocketEnv, "KILN_ORCA_GIT_OP", "GIT_ASKPASS", "SSH_ASKPASS", ociGitConfigN}
	// 用 ; 连接成单行：step 启动脚本的其余部分也拼在这个 sh -c 参数里，单行不改变调用方按行拆分 docker 调用的方式。
	return ociGitResetFunc + `() ( export -p | { out='unset ` + strings.Join(fixed, " ") + `'; while IFS= read -r line; do ` +
		`case $line in (*=*) ;; (*) continue ;; esac; ` +
		`n=${line%%=*}; n=${n##* }; ` +
		`case $n in (` + ociGitConfigKey + `*) s=${n#` + ociGitConfigKey + `} ;; (` + ociGitConfigValue + `*) s=${n#` + ociGitConfigValue + `} ;; (*) continue ;; esac; ` +
		`case $s in (*[!0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_]*) continue ;; esac; ` +
		`out="$out $n"; done; printf '%s\n' "$out"; } ); eval "$(` + ociGitResetFunc + `)"` + "\n"
}

// ociGitConfigScript 返回 step 启动脚本中接续 Kiln 的 git 配置的部分（POSIX sh）：把 ociGitConfigN 项追加在容器中实际生效
// 的环境变量配置之后——镜像的 ENV 与 workflow 的 env 合并之后的 GIT_CONFIG_COUNT，按 gitConfigCount 的规则解析；是 git 会
// 拒绝的值时不追加（git 照样失败，与没有 Kiln 时相同）。
//
// 计算在子 shell（kiln_git_config 的函数体是 ( … )）中完成，它只输出代码，父 shell 用 eval 执行：父 shell 中除了约定的变量
// （GIT_CONFIG_COUNT、追加的 GIT_CONFIG_KEY_<n> / GIT_CONFIG_VALUE_<n>，以及用完即 unset 的 KILN_GIT_CONFIG_*）之外什么都
// 不改。输出的代码中只有数字和固定的变量名，值以 "$KILN_GIT_CONFIG_KEY_<i>" 的形式引用，不经过引用转义。
func ociGitConfigScript() string {
	return ociGitConfigFunc + `() (
` + ociShellIFS + `c=${GIT_CONFIG_COUNT-}
d=$c
while :; do case $d in ([[:space:]]*) d=${d#?} ;; (*) break ;; esac; done
neg=
case $d in (+*) d=${d#+} ;; (-*) d=${d#-}; neg=1 ;; esac
ok=1
case $d in
('') [ -z "$c" ] || ok= ;;
(*[!0-9]*) ok= ;;
esac
if [ -n "$ok" ]; then
d=${d#"${d%%[!0]*}"}
[ -n "$d" ] || d=0
if [ "$d" != 0 ] && { [ -n "$neg" ] || [ ${#d} -gt ` + strconv.Itoa(maxGitConfigCountDigits) + ` ] || [ "$d" -gt ` + strconv.Itoa(math.MaxInt32) + ` ]; }; then ok=; fi
fi
n=${` + ociGitConfigN + `:-0}
case $n in (''|*[!0-9]*) n=0 ;; esac
out=
i=0
while [ "$i" -lt "$n" ]; do
[ -z "$ok" ] || out="$out
export GIT_CONFIG_KEY_$((d+i))=\"\$` + ociGitConfigKey + `$i\" GIT_CONFIG_VALUE_$((d+i))=\"\$` + ociGitConfigValue + `$i\""
out="$out
unset ` + ociGitConfigKey + `$i ` + ociGitConfigValue + `$i"
i=$((i+1))
done
[ -z "$ok" ] || out="$out
export GIT_CONFIG_COUNT=$((d+n))"
printf '%s\nunset ` + ociGitConfigN + `\n' "$out"
)
eval "$(` + ociGitConfigFunc + `)"
`
}

// ociGitTools 是在容器中建 git exec 镜像要用的外部命令。
var ociGitTools = []string{"cksum", "cut", "ln", "mkdir", "mv", "rm"}

// ociGitSetupScript 返回 oci step 启动脚本中建 git exec 镜像的部分（POSIX sh，不假设镜像中有 bash 或 GNU coreutils）。
// 与 ociGitConfigScript 一样，计算在子 shell 中完成、父 shell 只 eval 它输出的代码：父 shell 中只改约定的变量
// （GIT_EXEC_PATH、KILN_GIT_EXEC_REAL、KILN_GIT_OP_ERROR），值经单引号引用（q）原样输出。容器中没有 git 时什么都不做。有 git 时按它的 git --exec-path 在 /kiln/tmp 下建镜像：先建在临时目录，每一步
// 都检查结果，完整建好才改名发布（已有时复用），然后导出 GIT_EXEC_PATH 和 KILN_GIT_EXEC_REAL。缺少要用的命令或任何一步
// 失败时不导出 GIT_EXEC_PATH，而是导出 KILN_GIT_OP_ERROR（原因）：git 要凭据时 credential helper 给出这个说明、不给凭据。
// prepare 中用它检查（checkGit），声明了 repositories 的 job 因此失败。开头总是先 unset GIT_EXEC_PATH、KILN_GIT_EXEC_REAL、
// KILN_GIT_OP、KILN_GIT_OP_ERROR、KILN_ORCA_GIT_OP：单独调用（checkGit）或降级时不残留外层或镜像 ENV 中的旧值。
// `KILN_GIT_SOCKET` 是本次 step 合法的输入，不清。git --exec-path 的输出只去掉末尾的那一个换行（命令
// 替换会去掉全部，所以先加一个哨兵）；cksum 的结果只用作目录名中的校验值（数字），不受尾部换行影响。
func ociGitSetupScript() string {
	return ociGitSetupFunc + `() (
` + ociShellIFS + `q() {
v=$1 o=
while :; do case $v in (*"'"*) o=$o${v%%"'"*}"'\\''"; v=${v#*"'"} ;; (*) break ;; esac; done
printf "'%s%s'" "$o" "$v"
}
t=
fail() {
[ -z "$t" ] || rm -rf "$t" 2>/dev/null
printf 'export ` + gitOpErrorEnv + `=%s\n' "$(q "Kiln 无法在容器中建 git exec 镜像（$1），容器中的 git 拿不到 Kiln 的凭据")"
exit 0
}
printf 'unset GIT_EXEC_PATH ` + gitRealExecEnv + ` ` + gitOpEnv + ` ` + gitOpErrorEnv + ` KILN_ORCA_GIT_OP\n'
command -v git >/dev/null 2>&1 || exit 0
for c in ` + strings.Join(ociGitTools, " ") + `; do
command -v "$c" >/dev/null 2>&1 || fail "容器中没有 $c"
done
r=$(unset GIT_EXEC_PATH; git --exec-path 2>/dev/null; e=$?; printf x; exit $e) || fail "git --exec-path 失败"
r=${r%x}
case $r in (*"
") r=${r%?} ;; esac
[ -n "$r" ] && [ -d "$r" ] || fail "git --exec-path 给出的不是目录"
s=$(printf '%s' "$r" | cksum | cut -d ' ' -f 1) && [ -n "$s" ] || fail "cksum 失败"
d=` + ociGitExecPrefix + `$s
if [ ! -d "$d" ]; then
t=$d.$$
rm -rf "$t" && mkdir "$t" || fail "建临时目录失败"
for f in "$r"/*; do
[ -e "$f" ] || [ -L "$f" ] || continue
n=${f##*/}
case $n in
(` + strings.Join(gitRemoteHelpers, "|") + `) ln -s ` + ociKilnRunner + ` "$t/$n" ;;
(*) ln -s "$f" "$t/$n" ;;
esac || fail "ln -s 失败"
done
mv "$t" "$d" || fail "mv 失败"
t=
fi
printf 'export GIT_EXEC_PATH=%s ` + gitRealExecEnv + `=%s\n' "$(q "$d")" "$(q "$r")"
)
eval "$(` + ociGitSetupFunc + `)"
`
}

// containerRunnerProblem 返回 exe（宿主机上的 kiln-runner）不能放进 Linux 容器中运行的原因；能时为空。hostOS 是 Runner 所在
// 的操作系统（runtime.GOOS）。首期只支持与容器同内核的 Linux 上的 Runner，二进制须静态链接（发布的 kiln-runner 以
// CGO_ENABLED=0 构建）。
func containerRunnerProblem(hostOS, exe string) string {
	if hostOS != "linux" {
		return fmt.Sprintf("Runner 运行在 %s 上：OCI executor 的 git 凭据通道目前只支持 Linux 上的 Runner（与容器同一内核的 Docker），容器中需要 Linux 版的 kiln-runner", hostOS)
	}
	f, err := elf.Open(exe)
	if err != nil {
		return fmt.Sprintf("读不了 kiln-runner（%s）：%v", exe, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Sprintf("kiln-runner（%s）不是静态链接的，容器中可能无法运行（发布的 kiln-runner 以 CGO_ENABLED=0 构建）", exe)
		}
	}
	return ""
}

// errGitUnavailable 是凭据通道在执行环境中不可用、而 job 声明了 repositories 时的错误前缀。
var errGitUnavailable = errors.New("job 声明了 repositories，但 Kiln 的 git 凭据通道在这个执行环境中不可用")
