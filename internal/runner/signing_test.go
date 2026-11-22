package runner

import (
	"encoding/base64"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
)

func TestDecodeBase64(t *testing.T) {
	t.Parallel()
	raw := []byte("\x00\x01binary\xffdata")
	enc := base64.StdEncoding.EncodeToString(raw)
	for name, in := range map[string]string{
		"标准":    enc,
		"换行和空白": " " + enc[:8] + "\n" + enc[8:] + "\r\n",
		"无填充":   strings.TrimRight(enc, "="),
	} {
		got, err := decodeBase64(in)
		if err != nil || string(got) != string(raw) {
			t.Errorf("%s：decodeBase64 = %q, %v", name, got, err)
		}
	}
	for _, bad := range []string{"", "  \n", "not base64!"} {
		if _, err := decodeBase64(bad); err == nil {
			t.Errorf("decodeBase64(%q) 应失败", bad)
		}
	}
}

func TestDecodeKey(t *testing.T) {
	t.Parallel()
	pem := "-----BEGIN PRIVATE KEY-----\nMIGT\n-----END PRIVATE KEY-----"
	for _, in := range []string{pem, base64.StdEncoding.EncodeToString([]byte(pem))} {
		got, err := decodeKey(in)
		if err != nil || !strings.HasPrefix(string(got), "-----BEGIN PRIVATE KEY-----") {
			t.Errorf("decodeKey = %q, %v", got, err)
		}
	}
	for _, bad := range []string{"", base64.StdEncoding.EncodeToString([]byte("plain"))} {
		if _, err := decodeKey(bad); err == nil {
			t.Errorf("decodeKey(%q) 应失败", bad)
		}
	}
}

// signingFixture 是用 openssl 生成的代码签名证书（.p12，含私钥、证书和证书链）和一个 macOS provisioning profile。
// 证书链与 Developer ID 一样有三级：自建的根证书 → 中间证书 → 代码签名证书，
// 根证书和中间证书都不在系统中，只在 .p12 里（codesign 要能用临时 keychain 中的中间证书建立证书链）。
type signingFixture struct {
	p12, password, profile string // base64
}

const (
	testIdentity     = "Kiln Test Signing"
	testIntermediate = "Kiln Test Intermediate CA"
	testRoot         = "Kiln Test Root CA"
	testProfileUUID  = "12345678-ABCD-4321-ABCD-1234567890AB"
)

func newSigningFixture(t *testing.T) signingFixture {
	t.Helper()
	if !signingSupported() {
		t.Skip("signing 只在 macOS 上可用")
	}
	const openssl = "/usr/bin/openssl"
	if _, err := os.Stat(openssl); err != nil {
		t.Skip("需要 /usr/bin/openssl")
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "root.cnf"), `[req]
distinguished_name=dn
x509_extensions=ext
prompt=no
[dn]
CN=`+testRoot+`
[ext]
basicConstraints=critical,CA:true
keyUsage=critical,keyCertSign,cRLSign
subjectKeyIdentifier=hash
`)
	mustWrite(t, filepath.Join(dir, "int.ext"), `basicConstraints=critical,CA:true,pathlen:0
keyUsage=critical,keyCertSign,cRLSign
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid
`)
	mustWrite(t, filepath.Join(dir, "cert.ext"), `basicConstraints=critical,CA:false
keyUsage=critical,digitalSignature
extendedKeyUsage=critical,codeSigning
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid
`)
	mustWrite(t, filepath.Join(dir, "profile.plist"), `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Name</key><string>Kiln Test Profile</string>
<key>UUID</key><string>`+testProfileUUID+`</string>
<key>TeamIdentifier</key><array><string>TEAM123456</string></array>
<key>Platform</key><array><string>OSX</string></array>
<key>ExpirationDate</key><date>2030-01-01T00:00:00Z</date>
<key>DeveloperCertificates</key><array><data>AAEC</data></array>
</dict></plist>
`)
	const password = "p12-Passw0rd-for-test"
	for _, args := range [][]string{
		{"req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "root.key", "-out", "root.pem", "-days", "2", "-config", "root.cnf"},
		{"req", "-newkey", "rsa:2048", "-nodes", "-keyout", "int.key", "-out", "int.csr", "-subj", "/CN=" + testIntermediate},
		{"x509", "-req", "-in", "int.csr", "-CA", "root.pem", "-CAkey", "root.key", "-set_serial", "2", "-out", "int.pem", "-days", "2", "-extfile", "int.ext"},
		{"req", "-newkey", "rsa:2048", "-nodes", "-keyout", "key.pem", "-out", "cert.csr", "-subj", "/CN=" + testIdentity},
		{"x509", "-req", "-in", "cert.csr", "-CA", "int.pem", "-CAkey", "int.key", "-set_serial", "3", "-out", "cert.pem", "-days", "2", "-extfile", "cert.ext"},
		{"pkcs12", "-export", "-inkey", "key.pem", "-in", "cert.pem", "-certfile", "chain.pem", "-out", "cert.p12", "-passout", "pass:" + password, "-name", testIdentity},
		{"smime", "-sign", "-signer", "cert.pem", "-inkey", "key.pem", "-in", "profile.plist", "-outform", "DER", "-nodetach", "-binary", "-out", "profile.provisionprofile"},
	} {
		if args[0] == "pkcs12" {
			mustWrite(t, filepath.Join(dir, "chain.pem"), readFile(t, filepath.Join(dir, "int.pem"))+readFile(t, filepath.Join(dir, "root.pem")))
		}
		cmd := exec.Command(openssl, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("openssl %s：%v\n%s", args[0], err, out)
		}
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(b)
	}
	return signingFixture{p12: read("cert.p12"), password: password, profile: read("profile.provisionprofile")}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// userSearchList 返回当前用户（真正的 HOME）的 keychain 搜索列表，用于确认 job 没有改动它。
func userSearchList(t *testing.T) string {
	t.Helper()
	out, err := exec.Command(securityTool, "list-keychains", "-d", "user").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// realHome 返回当前用户真正的主目录（trustd 读这里的搜索列表）。
func realHome(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.HomeDir
}

// wrap 每 64 个字符换行（与 base64 -b 64 的输出一样）。
func wrap(s string) string {
	var b strings.Builder
	for len(s) > 64 {
		b.WriteString(s[:64] + "\n")
		s = s[64:]
	}
	b.WriteString(s)
	return b.String()
}

// signing：Runner 为 job 建立只属于它的临时 keychain，导入证书、安装 profile、保存公证凭据；
// job 能用证书签名，看不到也不改动 Runner 用户的 keychain；结束后全部删除（PLAN #8.10）。
func TestExecuteSigning(t *testing.T) {
	t.Parallel()
	fx := newSigningFixture(t)
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	r.userHome = t.TempDir()
	notary := exec.Command(xcrunTool, "--find", "notarytool").Run() == nil
	const notaryPassword = "abcd-efgh-ijkl-mnop"
	fs.secrets = map[string]string{
		"P12": wrap(fx.p12), "P12_PASSWORD": fx.password, "PROFILE": fx.profile, "NOTARY_PASSWORD": notaryPassword,
	}
	before := userSearchList(t)

	spec := hostJob(1,
		bash("搜索列表只有临时 keychain", `
list="$(security list-keychains -d user)"
echo "$list"
test "$(echo "$list" | wc -l | tr -d ' ')" = 1
case "$list" in *kiln-signing.keychain-db*) ;; *) exit 1;; esac
test -f "$KILN_KEYCHAIN"`),
		bash("job 期间临时 keychain 也在 Runner 用户的搜索列表中（trustd 用它建立证书链）", `
HOME="$REAL_HOME" security list-keychains -d user | grep -F kiln-signing.keychain-db`),
		bash("签名：证书链来自临时 keychain 中的中间证书和根证书", `
security find-identity -p codesigning | grep -F "`+testIdentity+`"
cp /usr/bin/true tool
codesign --force --sign "`+testIdentity+`" tool
codesign --verify tool
codesign -dvv tool 2>&1 | tee authority.txt
grep -F "Authority=`+testIdentity+`" authority.txt
grep -F "Authority=`+testIntermediate+`" authority.txt
grep -F "Authority=`+testRoot+`" authority.txt`),
		bash("provisioning profile", `
echo "$KILN_PROVISIONING_PROFILES"
case "$KILN_PROVISIONING_PROFILES" in *"/Library/Developer/Xcode/UserData/Provisioning Profiles/`+testProfileUUID+`.provisionprofile") ;; *) exit 1;; esac
test -f "$KILN_PROVISIONING_PROFILES"`),
		bash("密码不出现在日志中", `echo "pw=$P12_PASSWORD"`),
	)
	spec.Steps[1].Env = map[string]string{"REAL_HOME": realHome(t)}
	spec.Steps[4].Env = map[string]string{"P12_PASSWORD": "${{ secrets.P12_PASSWORD }}"}
	spec.Signing = &runnerv1.Signing{
		Certificates:         []*runnerv1.SigningCertificate{{P12: "${{ secrets.P12 }}", Password: "${{ secrets.P12_PASSWORD }}"}},
		ProvisioningProfiles: []string{"${{ secrets.PROFILE }}"},
	}
	if notary {
		spec.Signing.Notary = &runnerv1.Notary{Profile: "kiln-test", AppleId: "dev@example.com", TeamId: "TEAM123456", Password: "${{ secrets.NOTARY_PASSWORD }}"}
		spec.Steps = append(spec.Steps, bash("公证凭据在默认 keychain 中",
			`security find-generic-password -a com.apple.gke.notary.tool.saved-creds.kiln-test >/dev/null`))
	}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_SUCCEEDED, "")

	for _, leak := range []string{fx.password, notaryPassword} {
		if strings.Contains(log, leak) {
			t.Errorf("日志中出现了密码：\n%s", log)
		}
	}
	for _, s := range []string{"已导入证书 1", testIdentity, "Kiln Test Profile", "pw=***"} {
		if !strings.Contains(log, s) {
			t.Errorf("日志中没有 %q：\n%s", s, logTail(log))
		}
	}
	if after := userSearchList(t); after != before {
		t.Errorf("Runner 用户的 keychain 搜索列表被改动：%q → %q", before, after)
	}
	if _, err := os.Stat(filepath.Join(r.workDir(1), signingDirName)); !os.IsNotExist(err) {
		t.Errorf("临时 keychain 没有删除：%v", err)
	}
	for _, d := range userProfileDirs(r.userHome) {
		if entries, _ := os.ReadDir(d); len(entries) > 0 {
			t.Errorf("%s 中的 provisioning profile 没有删除：%v", d, entries)
		}
	}
}

// 证书密码不对：job 失败（用户错误），失败现场也不保留 keychain。
func TestExecuteSigningBadPassword(t *testing.T) {
	t.Parallel()
	fx := newSigningFixture(t)
	fs := newFakeServer(t)
	r := newTestRunner(t, fs)
	r.userHome = t.TempDir()
	fs.secrets = map[string]string{"P12": fx.p12, "P12_PASSWORD": "wrong-password"}
	spec := hostJob(1, bash("不会执行", "echo should-not-run"))
	spec.KeepOnFailure = true
	spec.Signing = &runnerv1.Signing{Certificates: []*runnerv1.SigningCertificate{{P12: "${{ secrets.P12 }}", Password: "${{ secrets.P12_PASSWORD }}"}}}
	got, log := runJob(t, r, fs, spec)
	wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_FAILED, classUser)
	if !strings.Contains(got.Message, "导入失败") || strings.Contains(log, "should-not-run") {
		t.Errorf("结果 %q；日志：\n%s", got.Message, logTail(log))
	}
	if strings.Contains(log, "wrong-password") || strings.Contains(got.Message, "wrong-password") {
		t.Errorf("密码出现在日志或结果中")
	}
	if _, err := os.Stat(filepath.Join(r.workDir(1), attemptHomeDir)); err != nil {
		t.Fatalf("应保留失败现场：%v", err)
	}
	if _, err := os.Stat(filepath.Join(r.workDir(1), signingDirName)); !os.IsNotExist(err) {
		t.Errorf("保留失败现场时也应删除临时 keychain：%v", err)
	}
}

// 引用的 secret 不存在或为空时给出可读的错误。
func TestExecuteSigningMissingSecret(t *testing.T) {
	t.Parallel()
	if !signingSupported() {
		t.Skip("signing 只在 macOS 上可用")
	}
	for p12, want := range map[string]string{
		"${{ secrets.NOPE }}":  "signing 引用的 secret NOPE 不存在",
		"${{ secrets.EMPTY }}": "p12 应为 base64 编码的 .p12",
	} {
		fs := newFakeServer(t)
		r := newTestRunner(t, fs)
		fs.secrets = map[string]string{"EMPTY": ""}
		spec := hostJob(1, bash("不会执行", "true"))
		spec.Signing = &runnerv1.Signing{Certificates: []*runnerv1.SigningCertificate{{P12: p12}}}
		got, log := runJob(t, r, fs, spec)
		wantOutcome(t, got, log, runnerv1.Outcome_OUTCOME_FAILED, classUser)
		if !strings.Contains(got.Message, want) {
			t.Errorf("%s：结果 %q，应包含 %q", p12, got.Message, want)
		}
	}
}

// Runner 用户搜索列表的修改：只追加或移除临时 keychain，顺带去掉残留的（文件已不存在的）临时 keychain，其他项和顺序不变。
func TestUserSearchListEdit(t *testing.T) {
	t.Parallel()
	out := `    "/Users/me/Library/Keychains/login.keychain-db"
    "/private/var/folders/x/kiln/1/signing/kiln-signing.keychain-db"
    "/Users/me/Library/Keychains/other.keychain-db"
    "/Users/me/kiln/work/2/signing/kiln-signing.keychain-db"
`
	list := parseSearchList(out)
	if len(list) != 4 || list[0] != "/Users/me/Library/Keychains/login.keychain-db" {
		t.Fatalf("parseSearchList = %q", list)
	}
	// 另一个 Runner 进程正在用的临时 keychain（文件存在）保留，残留的（文件不存在）去掉
	exists := func(p string) bool { return p == "/Users/me/kiln/work/2/signing/kiln-signing.keychain-db" }
	kc := "/var/folders/x/kiln/1/signing/kiln-signing.keychain-db"
	want := []string{
		"/Users/me/Library/Keychains/login.keychain-db",
		"/Users/me/Library/Keychains/other.keychain-db",
		"/Users/me/kiln/work/2/signing/kiln-signing.keychain-db",
	}
	if got := withoutKeychain(list, kc, exists); !slices.Equal(got, want) {
		t.Errorf("withoutKeychain = %q, want %q", got, want)
	}
	if got := withKeychain(list, kc, exists); !slices.Equal(got, append(slices.Clone(want), kc)) {
		t.Errorf("withKeychain = %q", got)
	}
	for a, b := range map[string]string{
		"/private/tmp/a": "/tmp/a", "/private/var/x/../y": "/var/y", "/tmp": "/private/tmp",
	} {
		if !sameKeychain(a, b) {
			t.Errorf("sameKeychain(%q, %q) = false", a, b)
		}
	}
	if sameKeychain("/private/tmpx/a", "/tmpx/a") {
		t.Error("/private/tmpx 不是 /tmpx")
	}
}

// Runner 在 job 执行中被强行结束后，空闲时的清理把残留的临时 keychain 从 Runner 用户的搜索列表中移除并删除，
// 其他项不变。会改动 Runner 用户的搜索列表（结束时恢复），所以不与 TestExecuteSigning 并行。
func TestCleanStaleKeychains(t *testing.T) {
	if !signingSupported() {
		t.Skip("signing 只在 macOS 上可用")
	}
	before := userSearchList(t)
	work := filepath.Join(t.TempDir(), "work")
	home := filepath.Join(work, "7", attemptHomeDir)
	kc := filepath.Join(work, "7", signingDirName, keychainName)
	for _, d := range []string{filepath.Join(home, "Library", "Preferences"), filepath.Dir(kc)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(securityTool, "create-keychain", "-p", "pw", kc)
	cmd.Env = []string{"HOME=" + home}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create-keychain：%v\n%s", err, out)
	}
	us := userKeychains{home: realHome(t)}
	gone := filepath.Join(t.TempDir(), "gone", signingDirName, keychainName) // 文件已不存在的残留项
	add := func(l []string) []string { return append(l, kc, gone) }
	if err := us.edit(t.Context(), add); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // 测试失败时也恢复
		_ = us.edit(t.Context(), func(l []string) []string {
			return slices.DeleteFunc(l, func(p string) bool { return sameKeychain(p, kc) || sameKeychain(p, gone) })
		})
	})
	if l := userSearchList(t); !strings.Contains(l, kc) {
		t.Fatalf("搜索列表中没有 %s：%s", kc, l)
	}

	cleanStaleKeychains(t.Context(), work, slog.New(slog.DiscardHandler))
	if after := userSearchList(t); after != before {
		t.Errorf("搜索列表 %q，应恢复为 %q", after, before)
	}
	if _, err := os.Stat(filepath.Dir(kc)); !os.IsNotExist(err) {
		t.Errorf("残留的 signing/ 没有删除：%v", err)
	}
}

// securityCommandLine 生成 security -i 的输入行：每个参数加双引号并转义反斜杠与双引号，最后是换行。
func TestSecurityCommandLine(t *testing.T) {
	t.Parallel()
	got := securityCommandLine([]string{"create-keychain", "-p", "abc123", `/tmp/a b/"c"\d.keychain-db`})
	want := `"create-keychain" "-p" "abc123" "/tmp/a b/\"c\"\\d.keychain-db"` + "\n"
	if got != want {
		t.Errorf("securityCommandLine = %q，期望 %q", got, want)
	}
}
