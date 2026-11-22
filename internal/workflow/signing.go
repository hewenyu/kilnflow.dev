package workflow

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hewenyu/kilnflow.dev/internal/expr"
)

// Signing 是 job 的 signing（PLAN #8.10）：Runner 在执行 step 前建立 job 专用的临时 keychain，
// 导入证书、provisioning profile 和公证凭据，job 结束后删除。只用于 macOS 宿主机上的 job。
// 解析后和展开后（保存在 jobs.spec 中）使用同一个类型；展开后的值中 secret 以 ${{ secrets.NAME }} 占位。
type Signing struct {
	Certificates         []SigningCertificate `json:"certificates,omitempty"`
	ProvisioningProfiles []string             `json:"provisioning_profiles,omitempty"`
	Notary               *Notary              `json:"notary,omitempty"`
	Pos                  Pos                  `json:"-"`
}

// SigningCertificate 是导入临时 keychain 的一个 PKCS#12（.p12，base64 编码）。
type SigningCertificate struct {
	P12      string `json:"p12"`
	Password string `json:"password,omitempty"`
	Pos      Pos    `json:"-"`
}

// Notary 是存入临时 keychain 的公证凭据，step 中用 xcrun notarytool --keychain-profile <Profile> 使用。
// AppleID + TeamID + Password（App 专用密码）与 Key + KeyID（+ Issuer，App Store Connect API key）二选一。
type Notary struct {
	Profile  string `json:"profile"`
	AppleID  string `json:"apple_id,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
	Password string `json:"password,omitempty"`
	Key      string `json:"key,omitempty"`
	KeyID    string `json:"key_id,omitempty"`
	Issuer   string `json:"issuer,omitempty"`
	Pos      Pos    `json:"-"`
}

// notaryProfileRe 是公证凭据名（keychain 中的 profile 名）的格式。
var notaryProfileRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (d *decoder) parseSigning(v *yaml.Node) *Signing {
	s := &Signing{Pos: pos(v)}
	d.fields(v, "signing", handlers{
		"certificates":          func(_, v *yaml.Node) { s.Certificates = parseSeq(d, v, "signing.certificates", d.parseSigningCert) },
		"provisioning-profiles": func(_, v *yaml.Node) { s.ProvisioningProfiles = d.strList(v) },
		"notary":                func(_, v *yaml.Node) { s.Notary = d.parseNotary(v) },
	}, map[string]string{
		"keychain": "不需要：Runner 为每个 job 建立临时 keychain（随机密码，job 期间保持解锁），路径在环境变量 KILN_KEYCHAIN 中（PLAN #8.10）",
	})
	if len(s.Certificates) == 0 && len(s.ProvisioningProfiles) == 0 && s.Notary == nil {
		d.errAt(v, "signing 至少需要 certificates、provisioning-profiles、notary 之一")
	}
	if slices.Contains(s.ProvisioningProfiles, "") {
		d.errAt(v, "signing.provisioning-profiles 中不能有空值")
	}
	return s
}

func (d *decoder) parseSigningCert(c *yaml.Node) SigningCertificate {
	sc := SigningCertificate{Pos: pos(c)}
	d.fields(c, "signing.certificates[]", handlers{
		"p12":      func(_, v *yaml.Node) { sc.P12 = d.str(v) },
		"password": func(_, v *yaml.Node) { sc.Password = d.str(v) },
	}, nil)
	if sc.P12 == "" {
		d.errAt(c, "signing.certificates[] 缺少 p12（base64 编码的 .p12，例如 ${{ secrets.MACOS_CERTIFICATE }}）")
	}
	return sc
}

func (d *decoder) parseNotary(v *yaml.Node) *Notary {
	n := &Notary{Pos: pos(v)}
	d.fields(v, "signing.notary", handlers{
		"profile":  func(_, v *yaml.Node) { n.Profile = d.str(v) },
		"apple-id": func(_, v *yaml.Node) { n.AppleID = d.str(v) },
		"team-id":  func(_, v *yaml.Node) { n.TeamID = d.str(v) },
		"password": func(_, v *yaml.Node) { n.Password = d.str(v) },
		"key":      func(_, v *yaml.Node) { n.Key = d.str(v) },
		"key-id":   func(_, v *yaml.Node) { n.KeyID = d.str(v) },
		"issuer":   func(_, v *yaml.Node) { n.Issuer = d.str(v) },
	}, nil)
	switch {
	case n.Profile == "":
		d.errAt(v, "signing.notary 缺少 profile（step 中 xcrun notarytool --keychain-profile 使用的名字）")
	case !expr.HasExpr(n.Profile) && !notaryProfileRe.MatchString(n.Profile):
		d.errAt(v, "signing.notary.profile %q 只能包含字母、数字、.、_ 和 -", n.Profile)
	}
	apple := n.AppleID != "" || n.TeamID != "" || n.Password != ""
	key := n.Key != "" || n.KeyID != "" || n.Issuer != ""
	switch {
	case apple && key:
		d.errAt(v, "signing.notary：apple-id、team-id、password 与 key、key-id、issuer 只能用一组")
	case apple && (n.AppleID == "" || n.TeamID == "" || n.Password == ""):
		d.errAt(v, "signing.notary 用 Apple ID 时需要 apple-id、team-id 和 password（App 专用密码）")
	case key && (n.Key == "" || n.KeyID == ""):
		d.errAt(v, "signing.notary 用 App Store Connect API key 时需要 key 和 key-id（团队 key 还需要 issuer）")
	case !apple && !key:
		d.errAt(v, "signing.notary 需要 apple-id、team-id、password，或 key、key-id（、issuer）")
	}
	return n
}

// checkSigning 检查 signing 只用于宿主机上执行 steps 的 job，敏感字段只来自 secret。
func (d *decoder) checkSigning(j *Job) {
	s := j.Signing
	if s == nil {
		return
	}
	if j.Release != nil || j.Image != nil {
		d.errJob(j, s.Pos, "job %s：signing 只能用于在 Runner 上执行 steps 的 job", j.ID)
	}
	if j.Container != "" || j.VM != "" {
		d.errJob(j, s.Pos, "job %s：signing 只能用于在 macOS 宿主机上执行的 job，不能与 container、vm 同时使用", j.ID)
	}
	mustSecret := func(p Pos, what, v string) {
		if v == "" {
			return
		}
		refs, err := expr.StringRefs(v)
		if err != nil {
			d.errJob(j, p, "job %s 的 %s：%v", j.ID, what, err)
			return
		}
		if !refersTo(refs, ctxSecrets) {
			d.errJob(j, p, "job %s 的 %s 必须引用 secret（例如 ${{ secrets.MACOS_CERTIFICATE }}），不能直接写在 workflow 中", j.ID, what)
		}
	}
	for _, c := range s.Certificates {
		mustSecret(c.Pos, "signing.certificates.p12", c.P12)
		mustSecret(c.Pos, "signing.certificates.password", c.Password)
	}
	if n := s.Notary; n != nil {
		mustSecret(n.Pos, "signing.notary.password", n.Password)
		mustSecret(n.Pos, "signing.notary.key", n.Key)
	}
}

// expandSigning 展开 signing：可以引用 vars、github、matrix 和 secrets，secret 保留占位符，由 Runner 替换。
func expandSigning(spec *Spec, j *Job, cx jobContexts, in *interpolator) {
	if j.Signing == nil {
		return
	}
	if !strings.HasPrefix(spec.Platform, "darwin/") {
		in.fail(fmt.Errorf("signing 只能用于 macOS（runs-on: darwin/arm64 等），这个 job 的平台是 %q", spec.Platform))
	}
	s := &Signing{ProvisioningProfiles: in.strs("signing.provisioning-profiles", j.Signing.ProvisioningProfiles, cx.env)}
	for _, c := range j.Signing.Certificates {
		s.Certificates = append(s.Certificates, SigningCertificate{
			P12:      in.str("signing.certificates.p12", c.P12, cx.env),
			Password: in.str("signing.certificates.password", c.Password, cx.env),
		})
	}
	if n := j.Signing.Notary; n != nil {
		s.Notary = &Notary{
			Profile:  in.str("signing.notary.profile", n.Profile, cx.env),
			AppleID:  in.str("signing.notary.apple-id", n.AppleID, cx.env),
			TeamID:   in.str("signing.notary.team-id", n.TeamID, cx.env),
			Password: in.str("signing.notary.password", n.Password, cx.env),
			Key:      in.str("signing.notary.key", n.Key, cx.env),
			KeyID:    in.str("signing.notary.key-id", n.KeyID, cx.env),
			Issuer:   in.str("signing.notary.issuer", n.Issuer, cx.env),
		}
		if p := s.Notary.Profile; p != "" && !notaryProfileRe.MatchString(p) {
			in.fail(fmt.Errorf("signing.notary.profile %q 只能包含字母、数字、.、_ 和 -", p))
		}
	}
	spec.Signing = s
}
