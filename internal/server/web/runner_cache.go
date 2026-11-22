package web

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/admin"
)

// Runner 的本地缓存页面（PLAN #8.3）：Runner 最近一次上报的缓存清单、上一次清理的结果和清理请求；
// 管理员可以按仓库、类别和执行身份预览（GET 参数，服务端算好）并发起清理（POST，Runner 空闲时执行）。

// cacheKindLabels 是缓存类别的说明。
var cacheKindLabels = map[string]string{
	"toolcache": "toolcache",
	"runtime":   "runtime",
	"git":       "git 镜像",
	"cas":       "CAS 缓存",
	"buildkit":  "buildctl",
	"work":      "保留的工作区",
	"image":     "镜像",
	"legacy":    "旧布局的 toolcache",
}

// defaultCacheDomains 是执行身份下拉框中总是列出的值（清单中出现的其他值也会列出）。
var defaultCacheDomains = []string{"host", "oci-root"}

// runnerCacheURL 返回 Runner 缓存页的地址。
func runnerCacheURL(name string) string { return "/runners/cache/" + url.PathEscape(name) }

// cacheEntryView 是清单中的一项。
type cacheEntryView struct {
	Kind, KindLabel string
	// Repo 是所属仓库（owner/name）；仓库不在 Kiln 中时为 #ID，不属于单个仓库时为空。
	Repo     string
	Platform string
	Domain   string
	Name     string
	Path     string
	Size     int64
	LastUsed *time.Time
	Expires  *time.Time
}

type cacheFailureView struct{ Kind, Path, Error string }

// cachePurgeView 是一次清理请求。
type cachePurgeView struct {
	ID           int64
	Repo         string
	Kinds        []string
	Domain       string
	State        string // pending | done
	Actor        string
	Created      time.Time
	Done         *time.Time
	Removed      int64
	RemovedBytes int64
	Failures     []cacheFailureView
}

// cacheGCView 是上一次清理的结果。
type cacheGCView struct {
	At           time.Time
	Removed      int64
	RemovedBytes int64
	Failures     []cacheFailureView
}

// cachePreviewView 是清理范围的预览。
type cachePreviewView struct {
	Matched      []cacheEntryView
	MatchedBytes int64
	Shared       []cacheEntryView
	Notes        []string
	// Error 不为空时预览失败（例如仓库不在 Kiln 中）。
	Error string
}

// runnerCachePage 是 Runner 缓存页的数据。
type runnerCachePage struct {
	Runner *runnerView
	// Generated 是清单生成的时间；Runner 还没有上报过时为 nil。
	Generated  *time.Time
	Entries    []cacheEntryView
	TotalBytes int64
	DiskTotal  int64
	DiskFree   int64
	GC         *cacheGCView
	Purges     []cachePurgeView
	// 清理表单的选项和当前取值。
	Kinds       []option
	Repos       []string
	Domains     []string
	Repo        string
	KindsPicked []string
	Domain      string
	// Preview 是按当前取值的预览（管理员带 preview=1 打开时）。
	Preview *cachePreviewView
	// Self 是当前页面的地址（不带预览参数）。
	Self string
}

func unixTime(sec int64) *time.Time {
	if sec == 0 {
		return nil
	}
	t := time.Unix(sec, 0)
	return &t
}

func cacheEntryViews(list []*apiv1.RunnerCacheEntry) []cacheEntryView {
	out := make([]cacheEntryView, 0, len(list))
	for _, e := range list {
		repo := e.Repo
		if repo == "" && e.RepoId != 0 {
			repo = fmt.Sprintf("#%d", e.RepoId)
		}
		out = append(out, cacheEntryView{
			Kind: e.Kind, KindLabel: cmpLabel(cacheKindLabels[e.Kind], e.Kind), Repo: repo, Platform: e.Platform, Domain: e.Domain,
			Name: e.Name, Path: e.Path, Size: e.SizeBytes, LastUsed: unixTime(e.LastUsedAt), Expires: unixTime(e.ExpiresAt),
		})
	}
	return out
}

func cmpLabel(label, fallback string) string {
	if label == "" {
		return fallback
	}
	return label
}

func cacheFailureViews(list []*apiv1.RunnerCacheFailure) []cacheFailureView {
	out := make([]cacheFailureView, 0, len(list))
	for _, f := range list {
		out = append(out, cacheFailureView{Kind: f.Kind, Path: f.Path, Error: f.Error})
	}
	return out
}

// activeRepos 返回 Kiln 中启用的仓库（清理表单的下拉框）。
func (d *Dashboard) activeRepos(ctx context.Context) ([]string, error) {
	rows, err := d.db.Query(ctx, `SELECT owner || '/' || name FROM repos WHERE active ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// cacheKindOptions 返回清理表单的类别选项。
func cacheKindOptions() []option {
	out := make([]option, 0, len(admin.CacheKinds()))
	for _, k := range admin.CacheKinds() {
		out = append(out, option{Value: k, Label: cmpLabel(cacheKindLabels[k], k)})
	}
	return out
}

// purgeScope 读取清理范围（GET 参数或表单）：仓库、类别（kind 可以重复，也可以用逗号分隔）、执行身份；
// 未知的类别忽略。
func purgeScope(v url.Values) (repo string, kinds []string, domain string) {
	repo, domain = strings.TrimSpace(v.Get("repo")), strings.TrimSpace(v.Get("domain"))
	for _, raw := range v["kind"] {
		for k := range strings.SplitSeq(raw, ",") {
			k = strings.TrimSpace(k)
			if slices.Contains(admin.CacheKinds(), k) && !slices.Contains(kinds, k) {
				kinds = append(kinds, k)
			}
		}
	}
	return repo, kinds, domain
}

func (d *Dashboard) pageRunnerCache(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	name := r.PathValue("name")
	v, err := d.runnerByName(ctx, name)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if v == nil {
		d.notFound(w, r, fmt.Sprintf("没有名为 %s 的 Runner。", name))
		return
	}
	c, err := d.admin.GetRunnerCache(ctx, name)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p := &runnerCachePage{
		Runner: v, Generated: unixTime(c.GeneratedAt), Entries: cacheEntryViews(c.Entries), DiskTotal: c.DiskTotalBytes, DiskFree: c.DiskFreeBytes,
		Kinds: cacheKindOptions(), Self: runnerCacheURL(name),
	}
	domains := map[string]bool{}
	for _, dm := range defaultCacheDomains {
		domains[dm] = true
	}
	for _, e := range c.Entries {
		p.TotalBytes += e.SizeBytes
		if e.Domain != "" {
			domains[e.Domain] = true
		}
	}
	p.Domains = slices.Sorted(maps.Keys(domains))
	if c.GcAt != 0 {
		p.GC = &cacheGCView{At: time.Unix(c.GcAt, 0), Removed: c.GcRemoved, RemovedBytes: c.GcRemovedBytes, Failures: cacheFailureViews(c.GcFailures)}
	}
	for _, pu := range c.Purges {
		p.Purges = append(p.Purges, cachePurgeView{
			ID: pu.Id, Repo: pu.Repo, Kinds: pu.Kinds, Domain: pu.Domain, State: pu.State, Actor: pu.Actor,
			Created: time.Unix(pu.CreatedAt, 0), Done: unixTime(pu.DoneAt), Removed: pu.Removed, RemovedBytes: pu.RemovedBytes,
			Failures: cacheFailureViews(pu.Failures),
		})
	}
	if se.Admin() {
		if p.Repos, err = d.activeRepos(ctx); err != nil {
			d.serverError(w, r, err)
			return
		}
		q := r.URL.Query()
		p.Repo, p.KindsPicked, p.Domain = purgeScope(q)
		if q.Get("preview") == "1" && !v.Revoked {
			p.Preview = &cachePreviewView{}
			resp, err := d.admin.PurgeRunnerCache(ctx, &apiv1.PurgeRunnerCacheRequest{
				Name: name, Repo: p.Repo, Kinds: p.KindsPicked, Domain: p.Domain, DryRun: true,
			}, se.Login)
			if err != nil {
				p.Preview.Error = d.failure(r, err)
			} else {
				p.Preview.Matched, p.Preview.MatchedBytes = cacheEntryViews(resp.Matched), resp.MatchedBytes
				p.Preview.Shared, p.Preview.Notes = cacheEntryViews(resp.RetainedShared), resp.Notes
			}
		}
	}
	d.render(w, r, "runner_cache", name+" · 本地缓存", p)
}

// postRunnerCachePurge 发起一次缓存清理请求（管理员），回到缓存页并给出结果。
func (d *Dashboard) postRunnerCachePurge(w http.ResponseWriter, r *http.Request, se *session) {
	ctx := r.Context()
	name := r.PathValue("name")
	back := runnerCacheURL(name)
	v, err := d.runnerByName(ctx, name)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	if v == nil {
		d.flashBack(w, r, "/runners", fmt.Sprintf("没有名为 %s 的 Runner，没有执行任何操作。", name))
		return
	}
	if err := r.ParseForm(); err != nil {
		d.flashBack(w, r, back, "表单无效，没有执行任何操作。")
		return
	}
	repo, kinds, domain := purgeScope(r.PostForm)
	resp, err := d.admin.PurgeRunnerCache(ctx, &apiv1.PurgeRunnerCacheRequest{Name: name, Repo: repo, Kinds: kinds, Domain: domain}, se.Login)
	if err != nil {
		d.flashBack(w, r, back, "发起清理失败："+d.failure(r, err))
		return
	}
	d.flashBack(w, r, back, fmt.Sprintf("已发起清理请求 #%d：%s 在没有正在执行的 job 时执行，完成后在下面显示结果。", resp.PurgeId, name))
}
