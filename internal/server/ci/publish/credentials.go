package publish

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 跨 target 共用的发布凭据：解密 secret、npm token、secret 引用模板的替换。集中在这里，镜像、npm、上传、GitHub
// Release 的目标共用同一套规则（B1 内聚）。

// publishEnvironment 复核发布 job 的 environment：声明了就必须存在，ref 策略必须允许本次 run 的 ref。没有声明
// environment 时返回 nil。规划和就绪判定已经检查过，这里是纵深防御。
func (w *Worker) publishEnvironment(ctx context.Context, pj *publishJob) (*store.Environment, error) {
	name := pj.job.Spec.Environment
	if name == "" {
		return nil, nil
	}
	e, err := store.LookupEnvironment(ctx, w.db, pj.src.ID, name)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, fmt.Errorf("environment %s 不存在，停止发布", name)
	}
	if !e.RefAllowed(pj.run.Ref) {
		return nil, fmt.Errorf("environment %s 的 ref 策略不允许 %s，停止发布", e.Name, pj.run.Ref)
	}
	return e, nil
}

// recheckAuthorization 在阶段 A 确认 tag 之后再次校验发布授权（纵深防御，规划时已经检查过）：environment 必须存在、
// ref 策略允许本次 run 的 ref；跨仓库发布到 release.github 的目标仓库必须在 environment 的允许发布仓库列表中。
// 对所有发布 job（包括只有 release.github 的）无条件执行，不再只在需要凭据的目标之前检查。
func (w *Worker) recheckAuthorization(ctx context.Context, pj *publishJob) error {
	env, err := w.publishEnvironment(ctx, pj)
	if err != nil {
		return err
	}
	g := pj.job.Spec.Release.GitHub
	if g == nil || g.Repository == "" || strings.EqualFold(g.Repository, pj.src.FullName()) {
		return nil
	}
	if env == nil {
		return fmt.Errorf("发布到其他仓库 %s 必须声明 environment，停止发布", g.Repository)
	}
	allowed := false
	for _, r := range env.AllowedPublishRepos {
		if strings.EqualFold(r, g.Repository) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%s 不在 environment %s 的允许发布仓库列表中，停止发布", g.Repository, env.Name)
	}
	return nil
}

// publishCredentials 解密发布凭据（release.images 的 username / password、release.uploads 的 token）引用的 secret，
// 返回 secret 名称到值和版本的映射；withNpm 时还返回账号级 npm 配置（registry、scope、token 来自同一次读取）。解密前再次确认（纵深防御，规划和就绪判定已经检查过）：
// run 由 tag push 触发、environment 存在且 ref 策略允许。
func (w *Worker) publishCredentials(ctx context.Context, pj *publishJob, withNpm bool) (map[string]store.ResolvedSecret, *store.NpmCredentials, error) {
	j := pj.job
	env, err := w.publishEnvironment(ctx, pj)
	if err != nil {
		return nil, nil, err
	}
	creds, err := store.ResolveSecrets(ctx, w.db, w.cfg.MasterKey, pj.src, env, j.Spec.Secrets)
	if err != nil {
		return nil, nil, err
	}
	var missing []string
	for _, n := range j.Spec.Secrets {
		if _, ok := creds[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("secret %s 没有配置（kiln secret set）", strings.Join(missing, "、"))
	}
	detail := map[string]any{
		"job_id": j.ID, "run_id": pj.run.ID, "repo": pj.src.FullName(), "ref": pj.run.Ref, "environment": j.Spec.Environment, "secrets": j.Spec.Secrets,
	}
	var npmCreds *store.NpmCredentials
	if withNpm {
		// 账号级 npm 配置的 token（PLAN #27.2），与 secret 一样只在通过 environment 检查后解密
		nc, err := store.ResolveNpmCredentials(ctx, w.db, w.cfg.MasterKey, pj.src.OwnerID)
		if err != nil {
			return nil, nil, err
		}
		if nc == nil {
			return nil, nil, fmt.Errorf("账号 %s 没有配置 npm（kiln npm set）", pj.src.Owner)
		}
		npmCreds = nc
		detail["npm"] = map[string]any{"scope": nc.Scope, "registry": nc.Registry, "version": nc.Version}
	}
	if err := store.Audit(ctx, w.db, auditActor, "publish.credentials", detail); err != nil {
		return nil, nil, err
	}
	return creds, npmCreds, nil
}

// npmTokenKey 是 publishJob.secrets 中 npm token 的键（只用于脱敏，不会与 secret 名冲突：secret 名不含空格）。
const npmTokenKey = "npm token"

// auditActor 是发布 worker 写审计日志时使用的身份。
const auditActor = "publisher"

// username、password 模板中的 secret 引用：${{ secrets.NAME }}（规划时保留原样，发布时才替换成值）。
const (
	exprOpen        = "${{"
	secretRefPrefix = "${{ secrets."
	secretRefSuffix = " }}"
)

// secretValues 返回 secret 名称到值的映射。
func secretValues(creds map[string]store.ResolvedSecret) map[string]string {
	out := make(map[string]string, len(creds))
	for k, v := range creds {
		out[k] = v.Value
	}
	return out
}

// substSecrets 把模板中的 ${{ secrets.NAME }} 占位符替换成值；模板中有其他写法的表达式或没有配置的 secret 时报错。
// 只扫描模板一遍，替换进来的值原样保留：值中恰好含有 ${{ 时不会误报，也不会把值中的占位符再替换成别的 secret。
func substSecrets(tmpl string, creds map[string]string) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(tmpl, exprOpen)
		if i < 0 {
			b.WriteString(tmpl)
			return b.String(), nil
		}
		b.WriteString(tmpl[:i])
		ref, ok := strings.CutPrefix(tmpl[i:], secretRefPrefix)
		var name string
		if ok {
			name, tmpl, ok = strings.Cut(ref, secretRefSuffix)
		}
		v, set := creds[name]
		if !ok || !set {
			return "", errors.New("引用的 secret 没有配置")
		}
		b.WriteString(v)
	}
}
