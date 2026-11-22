package workflow

import (
	"reflect"
	"slices"
	"testing"
)

// 某个 macOS App 的打包 job：证书、profile 和公证凭据来自 secret（PLAN #8.10）。
const signingWorkflow = `on: push
jobs:
  dmg:
    runs-on: darwin/arm64
    environment: macos-release
    cache: false
    signing:
      certificates:
        - p12: ${{ secrets.DEVELOPER_ID_P12 }}
          password: ${{ secrets.DEVELOPER_ID_P12_PASSWORD }}
      provisioning-profiles:
        - ${{ secrets.PROFILE }}
      notary:
        profile: example-notary
        apple-id: ${{ vars.X }}@example.com
        team-id: TEAMID0123
        password: ${{ secrets.NOTARY_PASSWORD }}
    steps:
      - run: make dmg
`

func TestExpandSigning(t *testing.T) {
	p := mustExpand(t, signingWorkflow, ctx())
	j := planJob(t, p, "dmg")
	want := &Signing{
		Certificates:         []SigningCertificate{{P12: "${{ secrets.DEVELOPER_ID_P12 }}", Password: "${{ secrets.DEVELOPER_ID_P12_PASSWORD }}"}},
		ProvisioningProfiles: []string{"${{ secrets.PROFILE }}"},
		Notary: &Notary{
			Profile: "example-notary", AppleID: "1@example.com", TeamID: "TEAMID0123",
			Password: "${{ secrets.NOTARY_PASSWORD }}",
		},
	}
	if !reflect.DeepEqual(j.Spec.Signing, want) {
		t.Errorf("signing = %+v\n期望 %+v", j.Spec.Signing, want)
	}
	if j.Spec.Executor != ExecutorHost || j.Spec.Platform != "darwin/arm64" {
		t.Errorf("executor = %s，平台 = %s", j.Spec.Executor, j.Spec.Platform)
	}
	// 引用的 secret 会下发给 Runner，也让 job 独占 Runner（PLAN #8.5）
	wantSecrets := []string{"DEVELOPER_ID_P12", "DEVELOPER_ID_P12_PASSWORD", "NOTARY_PASSWORD", "PROFILE"}
	if !slices.Equal(j.Spec.Secrets, wantSecrets) {
		t.Errorf("secrets = %v，期望 %v", j.Spec.Secrets, wantSecrets)
	}

	t.Run("API key", func(t *testing.T) {
		p := mustExpand(t, `on: push
jobs:
  a:
    runs-on: darwin/arm64
    signing:
      notary: { profile: n, key: "${{ secrets.ASC_KEY }}", key-id: ABC123, issuer: "${{ vars.X }}" }
    steps: [{ run: x }]
`, ctx())
		n := planJob(t, p, "a").Spec.Signing.Notary
		if n.Key != "${{ secrets.ASC_KEY }}" || n.KeyID != "ABC123" || n.Issuer != "1" || n.Password != "" {
			t.Errorf("notary = %+v", n)
		}
	})

	t.Run("Bot 触发时跳过", func(t *testing.T) {
		pc := ctx()
		pc.SenderIsBot = true
		j := planJob(t, mustExpand(t, signingWorkflow, pc), "dmg")
		if j.SkipReason == "" {
			t.Error("Bot 触发的 run 不应下发证书")
		}
	})
}

func TestSigningDiags(t *testing.T) {
	const head = "on: push\njobs:\n  a:\n    runs-on: darwin/arm64\n    steps: [{ run: x }]\n"
	cases := []struct {
		name, job, want string
	}{
		{"空的 signing", "    signing: {}", "signing 至少需要 certificates、provisioning-profiles、notary 之一"},
		{"缺少 p12", "    signing: { certificates: [{ password: \"${{ secrets.P }}\" }] }", "缺少 p12"},
		{"p12 不是 secret", "    signing: { certificates: [{ p12: MIIabc }] }", "signing.certificates.p12 必须引用 secret"},
		{"证书密码不是 secret", "    signing: { certificates: [{ p12: \"${{ secrets.P12 }}\", password: hunter2 }] }", "signing.certificates.password 必须引用 secret"},
		{"未知字段", "    signing: { keychain: x, certificates: [{ p12: \"${{ secrets.P12 }}\" }] }", "KILN_KEYCHAIN"},
		{"notary 缺少 profile", "    signing: { notary: { key: \"${{ secrets.K }}\", key-id: A } }", "signing.notary 缺少 profile"},
		{"notary profile 不合法", "    signing: { notary: { profile: \"a b\", key: \"${{ secrets.K }}\", key-id: A } }", "只能包含字母、数字"},
		{"notary 两组都写", "    signing: { notary: { profile: n, key: \"${{ secrets.K }}\", key-id: A, apple-id: a, team-id: t, password: \"${{ secrets.P }}\" } }", "只能用一组"},
		{"notary Apple ID 不全", "    signing: { notary: { profile: n, apple-id: a, password: \"${{ secrets.P }}\" } }", "需要 apple-id、team-id 和 password"},
		{"notary API key 不全", "    signing: { notary: { profile: n, key: \"${{ secrets.K }}\" } }", "需要 key 和 key-id"},
		{"notary 没有凭据", "    signing: { notary: { profile: n } }", "signing.notary 需要"},
		{"notary 密码不是 secret", "    signing: { notary: { profile: n, apple-id: a, team-id: t, password: pw } }", "signing.notary.password 必须引用 secret"},
		{"与 container 同时使用", "    container: a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n    signing: { certificates: [{ p12: \"${{ secrets.P12 }}\" }] }", "不能与 container、vm 同时使用"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := Parse("x.yml", []byte(head+c.job+"\n"))
			wantDiag(t, diags, c.want)
		})
	}

	t.Run("发布 job 不能写 signing", func(t *testing.T) {
		_, diags := Parse("x.yml", []byte(`on: push
jobs:
  b: { runs-on: darwin/arm64, steps: [{run: x}], outputs: { bin: dist/ } }
  p:
    needs: [b]
    signing: { certificates: [{ p12: "${{ secrets.P12 }}" }] }
    release: { github: { assets: [{ from: b.bin }] } }
`))
		wantDiag(t, diags, "signing 只能用于在 Runner 上执行 steps 的 job")
	})

	t.Run("非 macOS 平台", func(t *testing.T) {
		p := expandSrc(t, `on: push
jobs:
  a:
    runs-on: linux/arm64
    signing: { certificates: [{ p12: "${{ secrets.P12 }}" }] }
    steps: [{ run: x }]
`, ctx())
		wantDiag(t, p.Diags, "signing 只能用于 macOS")
	})
}
