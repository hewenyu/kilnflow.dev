package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Docker Hub 连接（DEPLOY #7.1）：每个账号一份，存放在账号级 environment docker-release 中的两个变量和一个 secret
// （名称见 admin.DockerEnvName 等）。
// 页面把它当成一个"连接"展示和编辑：显示已保存的用户名、命名空间和令牌状态，编辑时预填非敏感字段，替换令牌是显式动作。
// 保存和删除由 admin.Service 在一个事务中完成；保存只把配置存进 Kiln，不访问 Docker Hub；
// 只有用当前这套配置（用户名、命名空间和这一版令牌）成功推送过镜像后才显示"已验证"，
// 依据是发布时记录在 image_publishes 中的实际凭据（见 dockerVerified）。

// dockerDefaultRef 是新建 docker-release 时默认的 ref 策略。
const dockerDefaultRef = "refs/tags/v*"

// 连接字段的显示名。
const (
	dockerUserLabel  = "Docker Hub 用户名"
	dockerNSLabel    = "镜像命名空间"
	dockerTokenLabel = "访问令牌" //nolint:gosec // 字段的显示名，不是凭据
)

// dockerFields 是连接的全部字段（显示名），顺序即"还缺少"中的顺序。
var dockerFields = []string{dockerUserLabel, dockerNSLabel, dockerTokenLabel}

// Docker Hub 连接的状态。
const (
	dockerUnconfigured = "unconfigured" // 用户名、命名空间和令牌都没有
	dockerIncomplete   = "incomplete"   // 缺少其中一部分
	dockerSaved        = "saved"        // 都已保存，但还没有用当前这套配置成功推送过镜像
	dockerVerified     = "verified"     // 用当前这套配置成功推送过镜像
)

// dockerNamespaceRe 是 Docker Hub 命名空间（用户名或组织名）的格式：镜像名要求小写。
var dockerNamespaceRe = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// dockerPublish 是最近一次用当前这套配置成功推送的镜像。
type dockerPublish struct {
	Image, Version string
	At             time.Time
}

// dockerView 是一个账号的 Docker Hub 连接。
type dockerView struct {
	Account string
	// Env 是账号级 environment docker-release（不存在时为 nil）。
	Env                 *envView
	Username, Namespace string
	// Token 是访问令牌的元数据（没有设置时为 nil），从不包含令牌本身。
	Token *store.SecretMeta
	// Extra 是 docker-release 中除连接字段外的变量和 secret 数（在 environment 页面管理）。
	Extra    int
	Verified *dockerPublish
	Status   string
	// Missing 是缺少的字段（显示名）。
	Missing []string
}

// URL 返回连接编辑页的地址。
func (v *dockerView) URL() string { return dockerURL(v.Account) }

func dockerURL(account string) string { return "/settings/docker?account=" + url.QueryEscape(account) }

// dockerStatus 读取账号的 Docker Hub 连接。
func (d *Dashboard) dockerStatus(ctx context.Context, account string) (*dockerView, error) {
	v := &dockerView{Account: account, Status: dockerUnconfigured}
	envs, err := d.admin.ListEnvironments(ctx, account)
	if err != nil {
		return nil, err
	}
	for _, e := range envViews(envs) {
		if e.Docker {
			v.Env = &e
			break
		}
	}
	if v.Env == nil {
		v.Missing = slices.Clone(dockerFields)
		return v, nil
	}
	ref, err := store.ResolveScope(ctx, d.db, account, "", v.Env.Name)
	if err != nil {
		return nil, err
	}
	vars, err := store.ListVariables(ctx, d.db, ref)
	if err != nil {
		return nil, err
	}
	for _, x := range vars {
		switch x.Name {
		case admin.DockerUserVar:
			v.Username = x.Value
		case admin.DockerNamespaceVar:
			v.Namespace = x.Value
		default:
			v.Extra++
		}
	}
	secrets, err := store.ListSecretMeta(ctx, d.db, ref)
	if err != nil {
		return nil, err
	}
	for i := range secrets {
		if secrets[i].Name == admin.DockerTokenSecret {
			v.Token = &secrets[i]
		} else {
			v.Extra++
		}
	}
	for _, m := range []struct {
		missing bool
		label   string
	}{{v.Username == "", dockerUserLabel}, {v.Namespace == "", dockerNSLabel}, {v.Token == nil, dockerTokenLabel}} {
		if m.missing {
			v.Missing = append(v.Missing, m.label)
		}
	}
	switch {
	case len(v.Missing) == len(dockerFields):
		return v, nil
	case len(v.Missing) > 0:
		v.Status = dockerIncomplete
		return v, nil
	}
	v.Status = dockerSaved
	if v.Verified, err = d.dockerVerified(ctx, v); err != nil {
		return nil, err
	}
	if v.Verified != nil {
		v.Status = dockerVerified
	}
	return v, nil
}

// dockerVerified 返回用当前这套配置成功推送的最近一个镜像，没有时返回 nil。只认发布时记录的实际凭据，三项都要对上：
//   - password 用的正是账号级 docker-release 中 DOCKERHUB_TOKEN 的当前版本（secrets.version 每次写入都变，
//     所以令牌被替换、删除后重建、或发布用的是别的 secret / 仓库级同名 environment 中的 secret，都对不上）；
//   - 登录用户名与当前的 DOCKERHUB_USERNAME 相同（不论它是在连接页、变量页还是 CLI 中修改的）；
//   - 镜像在 docker.io 的当前命名空间下。
//
// 没有记录凭据的发布（引用方式无法确定，或本功能上线之前的发布）一律不算。
func (d *Dashboard) dockerVerified(ctx context.Context, v *dockerView) (*dockerPublish, error) {
	var p dockerPublish
	err := d.db.QueryRow(ctx, `SELECT name, version, updated_at FROM image_publishes
		WHERE password_secret_version=$1 AND username=$2 AND starts_with(lower(name), 'docker.io/'||lower($3)||'/')
		ORDER BY updated_at DESC LIMIT 1`, v.Token.Version, v.Username, v.Namespace).Scan(&p.Image, &p.Version, &p.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// dockerPage 是 Docker Hub 连接编辑页的数据。
type dockerPage struct {
	settingsBase
	Docker *dockerView
	Form   *formState
	// DefaultRef 是新建 docker-release 时默认的 ref 策略。
	DefaultRef string
}

func (d *Dashboard) pageDocker(w http.ResponseWriter, r *http.Request, _ *session) {
	ctx := r.Context()
	account, err := d.findAccount(ctx, strings.TrimSpace(r.URL.Query().Get("account")))
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if account == "" {
		d.flashBack(w, r, "/settings", "请从连接卡片进入：账号不在 Kiln 中")
		return
	}
	v, err := d.dockerStatus(ctx, account)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	f := newForm()
	f.Values["username"], f.Values["namespace"], f.Values["ref_policy"] = v.Username, v.Namespace, dockerDefaultRef
	d.renderDocker(w, r, v, f, http.StatusOK)
}

func (d *Dashboard) renderDocker(w http.ResponseWriter, r *http.Request, v *dockerView, f *formState, status int) {
	p := &dockerPage{Section: sectionConn, Docker: v, Form: f, DefaultRef: dockerDefaultRef}
	d.renderStatus(w, r, "settings_docker", "Docker Hub 连接 · "+v.Account, p, status)
}

// readDockerForm 读取并校验 Docker Hub 连接表单。令牌只返回，不写入表单状态。
func readDockerForm(r *http.Request, v *dockerView) (f *formState, token string) {
	f = newForm()
	for _, k := range []string{"username", "namespace", "ref_policy"} {
		f.Values[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	token = strings.TrimSpace(r.PostFormValue("token"))
	switch user := f.Values["username"]; {
	case user == "":
		f.fail("username", "请填写 Docker Hub 用户名（访问令牌所属的 Docker ID）")
	case strings.ContainsAny(user, " \t/:@"):
		f.fail("username", "用户名不能包含空格、/、: 或 @，填 Docker ID 本身即可")
	}
	switch ns := f.Values["namespace"]; {
	case ns == "":
		f.fail("namespace", "请填写镜像命名空间（镜像所在的 Docker Hub 用户名或组织名）")
	case !dockerNamespaceRe.MatchString(ns):
		f.fail("namespace", "命名空间只能包含小写字母、数字和 . _ -（Docker Hub 镜像名必须小写），例如 acme")
	}
	switch {
	case token == "" && v.Token == nil:
		f.fail("token", "首次配置需要填写访问令牌")
	case token != "" && f.Failed():
		f.fail("token", "为了安全，刚才输入的访问令牌没有保留，修正上面的错误后请重新输入")
	}
	return f, token
}

// postDocker 保存账号的 Docker Hub 连接：docker-release 不存在时按表单中的 ref 策略创建（已存在时不改动它的 ref 策略），
// 只写入有变化的用户名和命名空间；令牌非空时替换，为空时保持原值。只保存配置，不访问 Docker Hub。
func (d *Dashboard) postDocker(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	account, err := d.findAccount(ctx, strings.TrimSpace(r.PostFormValue("account")))
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if account == "" {
		d.flashBack(w, r, "/settings", "账号不在 Kiln 中，没有保存")
		return
	}
	v, err := d.dockerStatus(ctx, account)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	f, token := readDockerForm(r, v)
	var msg []string
	if !f.Failed() {
		if msg, err = d.saveDocker(ctx, v, f, token, se.Login); err != nil {
			f.fail(formGeneral, "保存失败："+d.failure(r, err))
		}
	}
	if f.Failed() {
		d.renderDocker(w, r, v, f, http.StatusUnprocessableEntity)
		return
	}
	d.flashBack(w, r, dockerURL(account), strings.Join(msg, "；"))
}

// saveDocker 写入 Docker Hub 连接（一个事务），返回给用户的提示。
func (d *Dashboard) saveDocker(ctx context.Context, v *dockerView, f *formState, token, actor string) ([]string, error) {
	c := &admin.DockerConnection{Account: v.Account, Token: token}
	var msg, changed []string
	if v.Env == nil {
		c.RefPolicy = splitList(f.Values["ref_policy"])
		if len(c.RefPolicy) == 0 {
			c.RefPolicy = []string{dockerDefaultRef}
		}
		msg = append(msg, fmt.Sprintf("已创建 environment %s（只允许 %s）", admin.DockerEnvName, strings.Join(c.RefPolicy, ", ")))
	} else {
		c.Environment = v.Env.Name
	}
	if user := f.Values["username"]; user != v.Username {
		c.Username = &user
		changed = append(changed, dockerUserLabel)
	}
	if ns := f.Values["namespace"]; ns != v.Namespace {
		c.Namespace = &ns
		changed = append(changed, dockerNSLabel)
	}
	if token != "" {
		changed = append(changed, dockerTokenLabel)
	}
	if err := d.admin.SaveDockerConnection(ctx, c, actor); err != nil {
		return nil, err
	}
	switch {
	case len(changed) == 0:
		msg = append(msg, "没有修改，Docker Hub 配置保持不变")
	default:
		msg = append(msg, "已保存 Docker Hub 配置（"+strings.Join(changed, "、")+"）")
	}
	if token == "" && v.Token != nil {
		msg = append(msg, "访问令牌没有修改，继续使用原来的")
	}
	msg = append(msg, "保存只把配置存进 Kiln，没有登录或推送 Docker Hub")
	return msg, nil
}

// postDockerDelete 删除账号的 Docker Hub 连接（用户名、命名空间和访问令牌，一个事务），保留 environment docker-release
// 及其保护规则和其他变量、secret。需要勾选确认。
func (d *Dashboard) postDockerDelete(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	account, err := d.findAccount(ctx, strings.TrimSpace(r.PostFormValue("account")))
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if account == "" {
		d.flashBack(w, r, "/settings", "账号不在 Kiln 中")
		return
	}
	if r.PostFormValue("confirm") != "1" {
		d.flashBack(w, r, dockerURL(account), "没有删除：请先勾选确认")
		return
	}
	v, err := d.dockerStatus(ctx, account)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if v.Env == nil {
		d.flashBack(w, r, "/settings", "没有已保存的 Docker Hub 凭据")
		return
	}
	if err := d.admin.DeleteDockerConnection(ctx, account, v.Env.Name, se.Login); err != nil {
		d.flashBack(w, r, dockerURL(account), "删除失败："+d.failure(r, err))
		return
	}
	d.flashBack(w, r, "/settings", fmt.Sprintf("已删除 %s 的 Docker Hub 凭据；environment %s 及其保护规则保留", account, v.Env.Name))
}
