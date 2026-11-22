package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
)

// macOS 代码签名（PLAN #8.10）：Runner 在执行 step 前为 job 建立临时 keychain，导入 signing 中的证书、
// provisioning profile 和公证凭据，job 结束后删除。
//
// keychain 的用户搜索列表和默认 keychain 保存在 $HOME/Library/Preferences/com.apple.security.plist 中，
// security 和 Security framework 按 HOME 找它。host executor 的 HOME 是 attempt 目录下的 home，
// 所以这里以 job 的 HOME 修改搜索列表：job 中查找签名身份时只看得到临时 keychain，看不到登录 keychain 里的证书。
//
// 但 codesign 的证书链是 trustd（Runner 用户的独立进程）建立的，trustd 只看 Runner 用户真正 HOME 中的搜索列表，
// 不认 job 的 HOME：临时 keychain 只在 job 的搜索列表中时，其中的中间证书（例如 Developer ID G2，系统不自带）
// 用不上，codesign 报 unable to build chain to self-signed root 和 errSecInternalComponent。
// 所以 job 期间还要把临时 keychain 追加到 Runner 用户的搜索列表末尾，job 结束后移除（默认 keychain 不变）。
const (
	securityTool = "/usr/bin/security"
	xcrunTool    = "/usr/bin/xcrun"
	plutilTool   = "/usr/bin/plutil"
	codesignTool = "/usr/bin/codesign"
	// signingDirName 是 attempt 目录下存放临时 keychain 和解码出的文件的目录（0700）。
	signingDirName = "signing"
	keychainName   = "kiln-signing.keychain-db"
	// keychainPartitions 允许 Apple 的签名工具（codesign、productbuild 等）不弹框使用私钥。
	keychainPartitions = "apple-tool:,apple:,codesign:"
	// searchListLockFile 是 Runner 用户主目录中串行化修改搜索列表的锁文件（同一用户下可能有多个 Runner 进程）。
	searchListLockFile = "Library/Caches/kiln-runner/keychain-search-list.lock"
	// trustd 缓存搜索列表，最多大约每 30 秒重新读取一次：试签名失败后重新写入搜索列表，等这么久再试（实测见 PLAN #8.10）。
	chainProbeWait     = 31 * time.Second
	chainProbeAttempts = 3
)

// signingSupported 判断本机能否执行 signing 的 job：macOS 且有 security 命令。
func signingSupported() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := os.Stat(securityTool)
	return err == nil
}

// keychain 是一个 job 的临时 keychain。
type keychain struct {
	dir  string // attempt 目录下的 signing/（0700）
	path string // keychain 文件
	home string // job 的 HOME（宿主机路径）
	tmp  string // job 的 TMPDIR
	env  []string
	// user 是 Runner 用户的搜索列表（trustd 建立证书链时只看它）。
	user userKeychains
	// listed 为 true 表示临时 keychain 已加入 Runner 用户的搜索列表，cleanup 时移除。
	listed bool
	log    *runnerkit.LogShipper
	// password 是随机生成的 keychain 密码（已登记脱敏）。
	password string
	// installed 是 provisioning profile 的路径（按声明顺序），通过 KILN_PROVISIONING_PROFILES 传给 step。
	installed []string
	// profiles 是本次新安装到用户 Library 中、结束时要删除的 provisioning profile 文件。
	profiles []string
}

// signingParams 是建立临时 keychain 所需的参数。
type signingParams struct {
	spec  *runnerv1.Signing
	subst func(string) string // 替换 secret 占位符
	dirs  *attemptDirs
	// profileDirs 是安装 provisioning profile 的目录（Xcode 在用户 Library 中查找，不认 HOME）。
	profileDirs []string
	// userHome 是 Runner 用户真正的主目录（trustd 读这里的搜索列表）。
	userHome string
	// developerDir 非空时作为 DEVELOPER_DIR 传给 xcrun（来自 job 的 env）。
	developerDir string
	log          *runnerkit.LogShipper
}

// userProfileDirs 返回 Xcode 查找 provisioning profile 的目录：Xcode 16 起为 UserData 下的目录，之前为 MobileDevice。
func userProfileDirs(home string) []string {
	return []string{
		filepath.Join(home, "Library", "Developer", "Xcode", "UserData", "Provisioning Profiles"),
		filepath.Join(home, "Library", "MobileDevice", "Provisioning Profiles"),
	}
}

// setupSigning 建立临时 keychain 并导入 signing 中的内容。返回的 keychain 不为 nil 时，
// 无论是否出错，调用方都要调用 cleanup。
func setupSigning(ctx context.Context, p signingParams) (*keychain, error) {
	k := &keychain{
		dir:  filepath.Join(p.dirs.root, signingDirName),
		home: p.dirs.home,
		tmp:  p.dirs.tmp,
		user: userKeychains{home: p.userHome},
		log:  p.log,
	}
	k.path = filepath.Join(k.dir, keychainName)
	k.env = []string{"HOME=" + k.home, "TMPDIR=" + k.tmp, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8"}
	if p.developerDir != "" {
		k.env = append(k.env, "DEVELOPER_DIR="+p.developerDir)
	}
	if !signingSupported() {
		return nil, userErrorf("signing 只能在 macOS 上执行（没有找到 %s）", securityTool)
	}
	if missing := missingSecrets(p.spec, p.subst); len(missing) > 0 {
		return nil, userErrorf("signing 引用的 secret %s 不存在：在 Kiln 中为这个仓库（或 job 的 environment）配置，例如 "+
			"base64 -i cert.p12 | kiln secret set NAME --repo owner/name [--env E]", strings.Join(missing, "、"))
	}
	// 搜索列表写在 $HOME/Library/Preferences 中，目录不存在时 security 不报错但不会保存
	for _, d := range []string{filepath.Join(k.home, "Library", "Preferences"), filepath.Join(k.home, "Library", "Keychains")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, infra(classPrepare, err)
		}
	}
	if err := os.Mkdir(k.dir, 0o700); err != nil {
		return nil, infra(classPrepare, err)
	}
	if err := k.create(ctx); err != nil {
		return k, err
	}
	sp := p.spec
	for i, c := range sp.Certificates {
		if err := k.importCertificate(ctx, i+1, p.subst(c.P12), p.subst(c.Password)); err != nil {
			return k, err
		}
	}
	if len(sp.Certificates) > 0 {
		if err := k.allowCodesign(ctx); err != nil {
			return k, err
		}
		if err := k.addToUserSearchList(ctx); err != nil {
			return k, infra(classPrepare, fmt.Errorf("把临时 keychain 加入 Runner 用户的搜索列表：%w", err))
		}
		k.logIdentities(ctx)
	}
	for i, prof := range sp.ProvisioningProfiles {
		path, err := k.installProfile(ctx, i+1, p.subst(prof), p.profileDirs)
		if err != nil {
			return k, err
		}
		k.installed = append(k.installed, path)
	}
	if n := sp.Notary; n != nil {
		if err := k.storeNotary(ctx, n, p.subst); err != nil {
			return k, err
		}
	}
	if len(sp.Certificates) > 0 {
		k.waitForChain(ctx)
	}
	k.log.Printf("临时 keychain 已就绪：KILN_KEYCHAIN=%s（job 结束后删除）", k.path)
	return k, nil
}

// secretRefRe 匹配替换后仍然留下的 secret 占位符（Server 没有下发这个 secret，即它不存在）。
var secretRefRe = regexp.MustCompile(`\$\{\{ secrets\.([A-Za-z_][A-Za-z0-9_]*) \}\}`)

// missingSecrets 返回 signing 中引用、但没有下发（不存在）的 secret 名称。
func missingSecrets(s *runnerv1.Signing, subst func(string) string) []string {
	vals := slices.Clone(s.ProvisioningProfiles)
	for _, c := range s.Certificates {
		vals = append(vals, c.P12, c.Password)
	}
	if n := s.Notary; n != nil {
		vals = append(vals, n.Profile, n.AppleId, n.TeamId, n.Password, n.Key, n.KeyId, n.Issuer)
	}
	var out []string
	for _, v := range vals {
		for _, m := range secretRefRe.FindAllStringSubmatch(subst(v), -1) {
			if !slices.Contains(out, m[1]) {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// addEnv 在 step 共用的环境变量中加入 KILN_KEYCHAIN 和 KILN_PROVISIONING_PROFILES。
func (k *keychain) addEnv(env map[string]string) {
	env["KILN_KEYCHAIN"] = k.path
	if len(k.installed) > 0 {
		env["KILN_PROVISIONING_PROFILES"] = strings.Join(k.installed, ":")
	}
}

// run 以 job 的 HOME 执行命令，返回输出（stdout 和 stderr）。出错时错误信息中带上脱敏后的输出。
func (k *keychain) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = k.env
	cmd.Dir = k.dir
	return k.output(cmd)
}

// runSecurity 用 `security -i` 执行一条 security 命令：命令经 stdin 传入，密码不出现在命令行里
// （同一台机器上的其他用户用 ps 看不到）。参数的引号规则见 securityCommandLine。
func (k *keychain) runSecurity(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, securityTool, "-i")
	cmd.Env = k.env
	cmd.Dir = k.dir
	cmd.Stdin = strings.NewReader(securityCommandLine(args))
	return k.output(cmd)
}

// output 执行 cmd 并返回输出（stdout 和 stderr）。出错时错误信息中带上脱敏后的输出。
// stdin 没有设置时为 /dev/null：命令需要交互输入时立即失败，不会卡住。
func (k *keychain) output(cmd *exec.Cmd) ([]byte, error) {
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(k.log.MaskString(out.String()))
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), errors.New(msg)
	}
	return out.Bytes(), nil
}

// securityCommandLine 把一条 security 命令转成 `security -i` 的输入行：每个参数加双引号，转义其中的反斜杠与双引号
// （macOS 的 security 按这条路解析，含空格、引号、反斜杠、$ 的路径都能正确传入）。换行会提前结束这一行，
// 含换行的参数不能用 runSecurity（调用方检查后改用 run）。
func securityCommandLine(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a))
		b.WriteByte('"')
	}
	return b.String() + "\n"
}

// create 建立、解锁 keychain，并把它设为 job 的唯一搜索列表和默认 keychain。
func (k *keychain) create(ctx context.Context) error {
	pw, err := randomPassword()
	if err != nil {
		return infra(classPrepare, err)
	}
	k.log.AddSecret(pw)
	// 带密码的命令经 stdin 传入（runSecurity），密码不出现在命令行里
	steps := []struct {
		args   []string
		secret bool
	}{
		{[]string{"create-keychain", "-p", pw, k.path}, true},
		// 不带 -t、-l：不自动锁定，睡眠时也不锁定
		{[]string{"set-keychain-settings", k.path}, false},
		{[]string{"unlock-keychain", "-p", pw, k.path}, true},
		{[]string{"list-keychains", "-d", "user", "-s", k.path}, false},
		{[]string{"default-keychain", "-d", "user", "-s", k.path}, false},
	}
	for _, s := range steps {
		var err error
		if s.secret {
			_, err = k.runSecurity(ctx, s.args...)
		} else {
			_, err = k.run(ctx, securityTool, s.args...)
		}
		if err != nil {
			return infra(classPrepare, fmt.Errorf("建立临时 keychain（security %s）：%w", s.args[0], err))
		}
	}
	k.password = pw
	return nil
}

// importCertificate 把 base64 编码的 .p12 导入 keychain。
func (k *keychain) importCertificate(ctx context.Context, n int, p12, password string) error {
	data, err := decodeBase64(p12)
	if err != nil {
		return userErrorf("signing.certificates 第 %d 个：p12 应为 base64 编码的 .p12（例如 base64 -i cert.p12 的输出）：%v", n, err)
	}
	f := filepath.Join(k.dir, fmt.Sprintf("certificate-%d.p12", n))
	if err := os.WriteFile(f, data, 0o600); err != nil {
		return infra(classPrepare, err)
	}
	defer func() { _ = os.Remove(f) }()
	// -A：临时 keychain 只属于这个 job（引用 secret 的 host job 独占 Runner），job 结束即删除，
	// 允许所有程序使用私钥，避免无人值守时弹出授权框
	// p12 的密码来自 job 的 secret，也经 stdin 传入；含换行时无法放进 security -i 的一行命令，退回命令行参数
	var importErr error
	if strings.ContainsAny(password, "\r\n") {
		_, importErr = k.run(ctx, securityTool, "import", f, "-k", k.path, "-f", "pkcs12", "-P", password, "-A")
	} else {
		_, importErr = k.runSecurity(ctx, "import", f, "-k", k.path, "-f", "pkcs12", "-P", password, "-A")
	}
	if importErr != nil {
		return userErrorf("signing.certificates 第 %d 个导入失败（p12 内容或密码不对？OpenSSL 3 生成的 .p12 在旧版 macOS 上要加 -legacy）：%v", n, importErr)
	}
	k.log.Printf("已导入证书 %d", n)
	return nil
}

// allowCodesign 设置私钥的 partition list，让 codesign 等 Apple 工具不弹框使用私钥（macOS 10.12 起需要）。
func (k *keychain) allowCodesign(ctx context.Context) error {
	if _, err := k.runSecurity(ctx, "set-key-partition-list", "-S", keychainPartitions, "-s", "-k", k.password, k.path); err != nil {
		return infra(classPrepare, fmt.Errorf("设置私钥的访问权限（security set-key-partition-list）：%w", err))
	}
	return nil
}

// addToUserSearchList 把临时 keychain 追加到 Runner 用户的搜索列表末尾，让 trustd 能用其中的中间证书建立证书链。
// 原有的项和顺序不变，默认 keychain 不变；job 结束时 cleanup 只移除这一项。
func (k *keychain) addToUserSearchList(ctx context.Context) error {
	if err := k.user.edit(ctx, func(l []string) []string { return withKeychain(l, k.path, fileExists) }); err != nil {
		return err
	}
	k.listed = true
	k.log.Printf("已把临时 keychain 加入 Runner 用户的搜索列表（codesign 的证书链由 trustd 建立，它只看这个列表；job 结束后移除）")
	return nil
}

// relistInUserSearchList 把临时 keychain 从 Runner 用户的搜索列表中移除再加回去。trustd 在临时 keychain 加入前后
// 刚好查询过时，之后会一直用旧的列表；重新写入一次，它在下次重新读取（约 30 秒后）时就能看到（实测）。
func (k *keychain) relistInUserSearchList(ctx context.Context) error {
	if err := k.user.edit(ctx, func(l []string) []string { return withoutKeychain(l, k.path, fileExists) }); err != nil {
		return err
	}
	return k.user.edit(ctx, func(l []string) []string { return withKeychain(l, k.path, fileExists) })
}

// removeFromUserSearchList 从 Runner 用户的搜索列表中移除临时 keychain（以及残留的、已不存在的临时 keychain）。
func (k *keychain) removeFromUserSearchList(ctx context.Context) error {
	return k.user.edit(ctx, func(l []string) []string { return withoutKeychain(l, k.path, fileExists) })
}

// userKeychains 是 Runner 用户（真正的主目录中）的 keychain 搜索列表，trustd 建立证书链时只看它。
type userKeychains struct {
	home string // Runner 用户真正的主目录
}

// edit 在持有锁时读取搜索列表，用 fn 修改，有变化时写回。锁文件在 Runner 用户的主目录中：
// 同一用户下的多个 Runner 进程不会互相覆盖对方的修改。
func (u userKeychains) edit(ctx context.Context, fn func([]string) []string) error {
	path := filepath.Join(u.home, filepath.FromSlash(searchListLockFile))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }()

	out, err := u.security(ctx, "list-keychains", "-d", "user")
	if err != nil {
		return err
	}
	list := parseSearchList(out)
	next := fn(slices.Clone(list))
	if slices.Equal(next, list) {
		return nil
	}
	_, err = u.security(ctx, append([]string{"list-keychains", "-d", "user", "-s"}, next...)...)
	return err
}

// security 以 Runner 用户真正的主目录为 HOME 执行 security（输出中只有 keychain 路径，没有需要脱敏的内容）。
func (u userKeychains) security(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, securityTool, args...)
	cmd.Env = []string{"HOME=" + u.home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("security %s：%w：%s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// cleanStaleKeychains 清理 Runner 进程在 job 执行中被强行结束时留下的临时 keychain（workRoot 是这个 Runner 的工作区目录）：
// 从 Runner 用户的搜索列表中移除，再删除 keychain 和 signing/ 目录（keychain 不自动锁定，留着的话其中的私钥一直可用）。
// 只能在这个 Runner 没有正在执行的 job 时调用。
func cleanStaleKeychains(ctx context.Context, workRoot string, log *slog.Logger) {
	if !signingSupported() {
		return
	}
	stale, _ := filepath.Glob(filepath.Join(workRoot, "*", signingDirName, keychainName))
	root := canonicalPath(workRoot) + string(filepath.Separator)
	u, err := user.Current()
	if err != nil {
		log.Warn("清理残留的临时 keychain", "err", err)
		return
	}
	us := userKeychains{home: u.HomeDir}
	err = us.edit(ctx, func(l []string) []string {
		return slices.DeleteFunc(l, func(p string) bool {
			return filepath.Base(p) == keychainName && (strings.HasPrefix(canonicalPath(p), root) || !fileExists(p))
		})
	})
	if err != nil {
		log.Warn("从 Runner 用户的搜索列表中移除残留的临时 keychain", "err", err)
	}
	for _, kc := range stale {
		if _, err := us.security(ctx, "delete-keychain", kc); err != nil {
			log.Warn("删除残留的临时 keychain", "keychain", kc, "err", err)
		}
		if err := os.RemoveAll(filepath.Dir(kc)); err != nil {
			log.Warn("删除残留的临时 keychain", "dir", filepath.Dir(kc), "err", err)
		}
		log.Info("已删除残留的临时 keychain（Runner 在 job 执行中被强行结束时留下的）", "keychain", kc)
	}
}

// parseSearchList 解析 security list-keychains 的输出（每行一个带引号的路径）。
func parseSearchList(out string) []string {
	var list []string
	for line := range strings.Lines(out) {
		if p := strings.Trim(strings.TrimSpace(line), `"`); p != "" {
			list = append(list, p)
		}
	}
	return list
}

// withKeychain 返回把 kc 追加到末尾后的搜索列表（去掉已有的同一项和残留的临时 keychain）。
func withKeychain(list []string, kc string, exists func(string) bool) []string {
	return append(withoutKeychain(list, kc, exists), kc)
}

// withoutKeychain 返回去掉 kc 后的搜索列表。Runner 异常退出时临时 keychain 会留在列表中，
// 文件已经不存在的临时 keychain（文件名是 kiln-signing.keychain-db）也一并去掉；其他项和顺序不变。
func withoutKeychain(list []string, kc string, exists func(string) bool) []string {
	var out []string
	for _, p := range list {
		if sameKeychain(p, kc) || (filepath.Base(p) == keychainName && !exists(p)) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// sameKeychain 比较两个 keychain 路径：security 输出的是解析后的路径（/tmp、/var 显示为 /private/tmp、/private/var）。
func sameKeychain(a, b string) bool {
	return canonicalPath(a) == canonicalPath(b)
}

func canonicalPath(p string) string {
	p = filepath.Clean(p)
	for _, d := range []string{"/tmp", "/var", "/etc"} {
		if rest, ok := strings.CutPrefix(p, "/private"+d); ok && (rest == "" || rest[0] == '/') {
			return d + rest
		}
	}
	return p
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// identityRe 匹配 security find-identity 输出中的身份行：序号) SHA-1 "名称"。
var identityRe = regexp.MustCompile(`(?m)^\s*\d+\) ([0-9A-F]{40}) "`)

// waitForChain 用 keychain 中第一个代码签名身份试签名一个临时文件，确认 codesign 能建立证书链。
// trustd 缓存搜索列表，刚加入的临时 keychain 可能还没生效（报 unable to build chain），这时重新写入搜索列表，
// 等 chainProbeWait 再试；试几次仍然失败时只写日志，不让 job 失败（例如 .p12 中没有中间证书，
// step 中的 codesign 会给出同样的错误）。
func (k *keychain) waitForChain(ctx context.Context) {
	out, err := k.run(ctx, securityTool, "find-identity", "-p", "codesigning", k.path)
	if err != nil {
		return
	}
	m := identityRe.FindStringSubmatch(string(out))
	if m == nil {
		return
	}
	id, probe := m[1], filepath.Join(k.dir, "chain-probe")
	defer func() { _ = os.Remove(probe) }()
	for i := 1; ; i++ {
		data, err := os.ReadFile("/usr/bin/true")
		if err == nil {
			err = os.WriteFile(probe, data, 0o700) //nolint:gosec // 试签名用的可执行文件
		}
		if err != nil {
			k.log.Printf("试签名：%v", err)
			return
		}
		_, err = k.run(ctx, codesignTool, "--force", "--sign", id, "--timestamp=none", "--keychain", k.path, probe)
		if err == nil {
			if i > 1 {
				k.log.Printf("试签名成功，证书链可用")
			}
			return
		}
		if !strings.Contains(err.Error(), "unable to build chain") || i == chainProbeAttempts {
			k.log.Printf("注意：用 %s 试签名失败：%v。unable to build chain 表示缺少中间证书：导出 .p12 时包含证书链"+
				"（Developer ID 需要 Developer ID G2，Apple Development / Distribution 需要对应的 WWDR 中间证书，"+
				"见 https://www.apple.com/certificateauthority/）", id, err)
			return
		}
		if err := k.relistInUserSearchList(ctx); err != nil {
			k.log.Printf("重新写入 Runner 用户的搜索列表：%v", err)
			return
		}
		k.log.Printf("证书链暂不可用（trustd 还没有读取新的搜索列表），%s 后重试", chainProbeWait)
		runnerkit.SleepCtx(ctx, chainProbeWait)
		if ctx.Err() != nil {
			return
		}
	}
}

// logIdentities 把 keychain 中可用于代码签名的身份写入日志（证书名和 SHA-1，不含私钥）。
func (k *keychain) logIdentities(ctx context.Context) {
	out, err := k.run(ctx, securityTool, "find-identity", "-v", "-p", "codesigning", k.path)
	if err != nil {
		k.log.Printf("列出签名身份失败：%v", err)
		return
	}
	text := strings.TrimSpace(string(out))
	k.log.Printf("可用于代码签名的身份：\n%s", text)
	if strings.HasPrefix(lastLine(text), "0 valid identities") {
		k.log.Printf("注意：没有有效（证书链受信任）的代码签名身份。Developer ID / Apple Development 证书需要对应的中间证书" +
			"（https://www.apple.com/certificateauthority/）：导出 .p12 时包含证书链，或在 Runner 所在的机器上安装中间证书。" +
			"只有 Installer 证书或使用自签名证书时显示 0 是正常的")
	}
}

func lastLine(s string) string {
	if _, after, ok := strings.CutLast(s, "\n"); ok {
		return strings.TrimSpace(after)
	}
	return strings.TrimSpace(s)
}

// installProfile 解码 provisioning profile，按 Xcode 的约定（<UUID>.provisionprofile 或 .mobileprovision）
// 安装到 profileDirs，返回第一个目录中的路径。原来就存在的文件不覆盖，结束时也不删除。
func (k *keychain) installProfile(ctx context.Context, n int, b64 string, profileDirs []string) (string, error) {
	data, err := decodeBase64(b64)
	if err != nil {
		return "", userErrorf("signing.provisioning-profiles 第 %d 个应为 base64 编码的 provisioning profile：%v", n, err)
	}
	f := filepath.Join(k.dir, fmt.Sprintf("profile-%d", n))
	if err := os.WriteFile(f, data, 0o600); err != nil {
		return "", infra(classPrepare, err)
	}
	plist := f + ".plist"
	if _, err := k.run(ctx, securityTool, "cms", "-D", "-i", f, "-o", plist); err != nil {
		return "", userErrorf("signing.provisioning-profiles 第 %d 个不是有效的 provisioning profile：%v", n, err)
	}
	// profile 中有 data、date 类型，不能整体转换为 JSON，逐个取需要的字段
	field := func(key, format string) string {
		out, err := k.run(ctx, plutilTool, "-extract", key, format, "-o", "-", plist)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	uuid := field("UUID", "raw")
	if !uuidLike(uuid) {
		return "", userErrorf("signing.provisioning-profiles 第 %d 个缺少有效的 UUID", n)
	}
	var platforms []string
	_ = json.Unmarshal([]byte(field("Platform", "json")), &platforms)
	ext := ".mobileprovision"
	if slices.Contains(platforms, "OSX") {
		ext = ".provisionprofile"
	}
	var first string
	for _, dir := range profileDirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", infra(classPrepare, err)
		}
		dst := filepath.Join(dir, uuid+ext)
		if first == "" {
			first = dst
		}
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "", infra(classPrepare, err)
		}
		k.profiles = append(k.profiles, dst)
	}
	k.log.Printf("已安装 provisioning profile %d：%s（UUID %s，团队 %s，到期 %s）", n, field("Name", "raw"), uuid,
		field("TeamIdentifier.0", "raw"), field("ExpirationDate", "raw"))
	return first, nil
}

// uuidLike 检查 profile 的 UUID 可以安全地用作文件名。
func uuidLike(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') && c != '-' {
			return false
		}
	}
	return true
}

// storeNotary 把公证凭据存入临时 keychain（xcrun notarytool store-credentials），
// step 中用 xcrun notarytool <命令> --keychain-profile <profile> 使用（默认 keychain 就是临时 keychain）。
func (k *keychain) storeNotary(ctx context.Context, n *runnerv1.Notary, subst func(string) string) error {
	profile := subst(n.Profile)
	args := []string{"notarytool", "store-credentials", profile, "--keychain", k.path, "--no-validate"}
	if n.Key != "" {
		key, err := decodeKey(subst(n.Key))
		if err != nil {
			return userErrorf("signing.notary.key：%v", err)
		}
		f := filepath.Join(k.dir, "AuthKey_notary.p8")
		if err := os.WriteFile(f, key, 0o600); err != nil {
			return infra(classPrepare, err)
		}
		defer func() { _ = os.Remove(f) }()
		args = append(args, "--key", f, "--key-id", subst(n.KeyId))
		if is := subst(n.Issuer); is != "" {
			args = append(args, "--issuer", is)
		}
	} else {
		pw := subst(n.Password)
		if strings.TrimSpace(pw) == "" {
			return userErrorf("signing.notary.password 为空：引用的 secret 不存在或为空")
		}
		args = append(args, "--apple-id", subst(n.AppleId), "--team-id", subst(n.TeamId), "--password", pw)
	}
	if _, err := k.run(ctx, xcrunTool, args...); err != nil {
		return userErrorf("保存公证凭据 %s 失败（需要 Xcode 或 Command Line Tools 中的 notarytool）：%v", profile, err)
	}
	// notarytool 先查登录会话的 data protection keychain（Local Items），Runner 用户在那里有同名 profile 时会用它，
	// 所以建议在 step 中加上 --keychain "$KILN_KEYCHAIN"
	k.log.Printf(`已保存公证凭据 %s（xcrun notarytool … --keychain-profile %s --keychain "$KILN_KEYCHAIN"）`, profile, profile)
	return nil
}

// decodeKey 返回 App Store Connect API key（.p8）的内容：可以是原文（PEM），也可以是 base64 编码的原文。
func decodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("为空：引用的 secret 不存在或为空")
	}
	if strings.Contains(s, "-----BEGIN") {
		return []byte(s + "\n"), nil
	}
	b, err := decodeBase64(s)
	if err != nil || !bytes.Contains(b, []byte("-----BEGIN")) {
		return nil, errors.New("应为 .p8 文件的内容（-----BEGIN PRIVATE KEY----- 开头）或它的 base64")
	}
	return b, nil
}

// decodeBase64 解码 base64（忽略空白和换行，接受带或不带填充的标准编码）。
func decodeBase64(s string) ([]byte, error) {
	s = strings.Join(strings.Fields(s), "")
	if s == "" {
		return nil, errors.New("为空：引用的 secret 不存在或为空")
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
	}
	if err != nil {
		return nil, errors.New("不是有效的 base64")
	}
	return b, nil
}

func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// cleanup 删除临时 keychain、解码出的文件和本次安装的 provisioning profile。失败现场也不保留这些内容。
func (k *keychain) cleanup(ctx context.Context) {
	if k.listed {
		if err := k.removeFromUserSearchList(ctx); err != nil {
			k.log.Printf("从 Runner 用户的搜索列表中移除临时 keychain：%v", err)
		}
	}
	if _, err := os.Stat(k.path); err == nil {
		if _, err := k.run(ctx, securityTool, "delete-keychain", k.path); err != nil {
			k.log.Printf("删除临时 keychain：%v", err)
		}
	}
	for _, p := range k.profiles {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			k.log.Printf("删除 provisioning profile %s：%v", p, err)
		}
	}
	if err := os.RemoveAll(k.dir); err != nil {
		k.log.Printf("删除 %s：%v", k.dir, err)
	}
}
