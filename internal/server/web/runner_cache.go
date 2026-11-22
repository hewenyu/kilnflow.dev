package web

import (
	"context"
	"errors"
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
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// Runner 的本地缓存页面（PLAN #8.3）：Runner 最近一次上报的缓存清单、上一次清理的结果和清理请求；
// 管理员可以按仓库、类别和执行身份预览（GET 参数，服务端算好）并发起清理（POST，Runner 空闲时执行）。

// defaultCacheDomains 是执行身份下拉框中总是列出的值（清单中出现的其他值也会列出）。
var defaultCacheDomains = []string{"host", "oci-root"}

// runnerCacheURL 返回 Runner 缓存页的地址。
func runnerCacheURL(name string) string { return "/runners/cache/" + url.PathEscape(name) }

// cacheEntryView 是清单中的一项。
// Kind 是缓存类别（admin.CacheKinds），类别的名称由前端按语言显示。
type cacheEntryView struct {
	Kind string
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
	// Notes 是预览的说明（admin.CacheNote 的原因码映射的消息）。
	Notes []uiText
	// Error 不为空时预览失败（例如仓库不在 Kiln 中）。
	Error uiText
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
	// 清理表单的选项和当前取值；Kinds 是缓存类别（名称由前端按语言显示）。
	Kinds       []string
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
			Kind: e.Kind, Repo: repo, Platform: e.Platform, Domain: e.Domain,
			Name: e.Name, Path: e.Path, Size: e.SizeBytes, LastUsed: unixTime(e.LastUsedAt), Expires: unixTime(e.ExpiresAt),
		})
	}
	return out
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
		d.notFoundText(w, r, message(msgRunnerNotFound, strArg(argName, name)))
		return
	}
	c, err := d.admin.GetRunnerCache(ctx, name)
	if err != nil {
		d.serverError(w, r, err)
		return
	}
	p := &runnerCachePage{
		Runner: v, Generated: unixTime(c.GeneratedAt), Entries: cacheEntryViews(c.Entries), DiskTotal: c.DiskTotalBytes, DiskFree: c.DiskFreeBytes,
		Kinds: admin.CacheKinds(), Self: runnerCacheURL(name),
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
			resp, notes, err := d.admin.PreviewRunnerCachePurge(ctx, &apiv1.PurgeRunnerCacheRequest{
				Name: name, Repo: p.Repo, Kinds: p.KindsPicked, Domain: p.Domain,
			}, se.Login)
			switch {
			case errors.Is(err, store.ErrRepoNotFound):
				p.Preview.Error = message(msgCacheRepoNotFound, strArg(argRepo, p.Repo))
			case err != nil:
				p.Preview.Error = d.failureText(r, err)
			default:
				p.Preview.Matched, p.Preview.MatchedBytes = cacheEntryViews(resp.Matched), resp.MatchedBytes
				p.Preview.Shared, p.Preview.Notes = cacheEntryViews(resp.RetainedShared), cacheNoteTexts(notes)
			}
		}
	}
	d.renderPage(w, r, "runner_cache", message(msgRunnerCacheTitle, strArg(argName, name)), p, http.StatusOK)
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
		d.flashMsg(w, r, "/runners", message(msgRunnerOpNotFound, strArg(argName, name)))
		return
	}
	if err := r.ParseForm(); err != nil {
		d.flashMsg(w, r, back, message(msgCacheFormInvalid))
		return
	}
	repo, kinds, domain := purgeScope(r.PostForm)
	resp, err := d.admin.PurgeRunnerCache(ctx, &apiv1.PurgeRunnerCacheRequest{Name: name, Repo: repo, Kinds: kinds, Domain: domain}, se.Login)
	if err != nil {
		d.flashMsg(w, r, back, d.failureFlash(r, err, msgCachePurgeFailed, msgCachePurgeFailedRef))
		return
	}
	d.flashMsg(w, r, back, message(msgCachePurgeRequested, intArg(argID, resp.PurgeId), strArg(argName, name)))
}
