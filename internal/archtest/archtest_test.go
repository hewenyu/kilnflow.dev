package archtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 领域（PLAN #30.3）。
type domain string

const (
	// shared 是共用的机制（Runner 身份、outbox 投递、日志、通知、仓库等共用表），不依赖任何领域。
	shared domain = "共用"
	ci     domain = "CI"
	aiDom  domain = "AI"
	merge  domain = "合并"
	// assembly 是组装层与展示层（server、ingest、web、admin、CLI），可以依赖全部领域。
	assembly domain = "组装"
	// lib 是不含领域判断的库（GitHub 客户端、CAS、发布协议等），不依赖任何领域。
	lib domain = "库"
)

// modulePrefix 是本模块的导入路径前缀。
const modulePrefix = "github.com/hewenyu/kilnflow.dev/"

// serverRoot 是组装层的包；它的子包各有归类，所以只按完整路径匹配。
const serverRoot = "internal/server"

// domains 按包路径前缀（相对模块根）归类，最长的前缀优先。没有归类的包让 TestDependencies 失败（新包要先归类）。
var domains = map[string]domain{
	// 共用
	"internal/server/auth":           shared,
	"internal/server/upgradectl":     shared,
	"internal/server/logstore":       shared,
	"internal/server/notify":         shared,
	"internal/server/outbox":         shared,
	"internal/server/store":          shared,
	"internal/server/repodir":        shared,
	"internal/server/runnerauth":     shared,
	"internal/server/runnerprogress": shared,
	"internal/server/checkrun":       shared,
	"internal/server/gitread":        shared,
	"internal/server/gitcred":        shared,
	"internal/runnerkit":             shared,
	"internal/upgrade":               shared,
	// CI
	"internal/server/ci": ci,
	"internal/workflow":  ci,
	"internal/expr":      ci,
	"internal/actionkey": ci,
	"internal/runner":    ci,
	"cmd/kiln-runner":    ci,
	// AI
	"internal/server/ai": aiDom,
	"internal/agentcfg":  aiDom,
	"internal/aireport":  aiDom,
	"internal/devreport": aiDom,
	// 合并
	"internal/server/mergequeue": merge,
	"internal/server/autotag":    merge,
	"internal/server/mqevents":   merge,
	"internal/mergecfg":          merge,
	// 组装与展示
	serverRoot:               assembly,
	"internal/server/ingest": assembly,
	"internal/server/web":    assembly,
	"internal/server/admin":  assembly,
	"internal/archtest":      assembly,
	"cmd/kiln":               assembly,
	"cmd/kiln-server":        assembly,
	// 库
	"gen":                  lib,
	"migrations":           lib,
	"internal/buildinfo":   lib,
	"internal/bytesize":    lib,
	"internal/cas":         lib,
	"internal/db":          lib,
	"internal/docprovider": lib,
	"internal/endpoint":    lib,
	"internal/fakegithub":  lib,
	"internal/fakenpm":     lib,
	"internal/fakeupload":  lib,
	"internal/forge":       lib,
	"internal/github":      lib,
	"internal/gitref":      lib,
	"internal/httpretry":   lib,
	"internal/logging":     lib,
	"internal/npm":         lib,
	"internal/oci":         lib,
	"internal/pgtest":      lib,
	"internal/randtoken":   lib,
	"internal/redact":      lib,
	"internal/release":     lib,
	"internal/rpcerr":      lib,
	"internal/secretbox":   lib,
	"internal/semver":      lib,
	"internal/shorten":     lib,
	"internal/toolchain":   lib,
	// kiln-runner toolchain 子命令：AI 执行节点的连接器借用 CI Runner 二进制中的安装代码（PLAN #33.12），只依赖库
	"internal/toolchainctl": lib,
	"internal/upload":       lib,
}

// allowed 是每个领域可以依赖的领域（PLAN #30.3：CI 与 AI 互不依赖，共用不依赖任何领域，合并只依赖接口）。
var allowed = map[domain][]domain{
	shared:   {shared, lib},
	lib:      {lib},
	ci:       {ci, shared, lib},
	aiDom:    {aiDom, shared, lib},
	merge:    {merge, shared, lib},
	assembly: {assembly, ci, aiDom, merge, shared, lib},
}

// classify 返回包 path（相对模块根）的领域；没有归类时 ok 为 false。
func classify(path string) (domain, bool) {
	best := ""
	for prefix := range domains {
		match := path == prefix || (prefix != serverRoot && strings.HasPrefix(path, prefix+"/"))
		if match && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return "", false
	}
	return domains[best], true
}

// pkg 是 go list 输出的一个包。
type pkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Deps       []string
}

// listPackages 返回模块中全部包（不含测试文件）和它们的传递依赖。
func listPackages(t *testing.T) []pkg {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-json=ImportPath,Dir,GoFiles,Deps", modulePrefix+"...").Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("go list：%v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list：%v", err)
	}
	var pkgs []pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		t.Fatal("go list 没有列出任何包")
	}
	return pkgs
}

// depViolations 返回违反依赖规则的依赖（"包 → 依赖（领域）"），以及没有归类的包。graph 的键和值都是相对模块根的路径。
func depViolations(graph map[string][]string) (violations, unclassified []string) {
	seen := map[string]bool{}
	for p, deps := range graph {
		from, ok := classify(p)
		if !ok {
			if !seen[p] {
				seen[p] = true
				unclassified = append(unclassified, p)
			}
			continue
		}
		for _, d := range deps {
			to, ok := classify(d)
			if !ok {
				if !seen[d] {
					seen[d] = true
					unclassified = append(unclassified, d)
				}
				continue
			}
			if !slices.Contains(allowed[from], to) {
				violations = append(violations, fmt.Sprintf("%s（%s）→ %s（%s）", p, from, d, to))
			}
		}
	}
	sort.Strings(violations)
	sort.Strings(unclassified)
	return violations, unclassified
}

// TestDependencies：领域之间没有直接或传递依赖（go list 的 Deps 是传递依赖）。
func TestDependencies(t *testing.T) {
	t.Parallel()
	graph := map[string][]string{}
	for _, p := range listPackages(t) {
		var deps []string
		for _, d := range p.Deps {
			if rel, ok := strings.CutPrefix(d, modulePrefix); ok {
				deps = append(deps, rel)
			}
		}
		graph[strings.TrimPrefix(p.ImportPath, modulePrefix)] = deps
	}
	violations, unclassified := depViolations(graph)
	for _, u := range unclassified {
		t.Errorf("包 %s 没有归类：在 archtest_test.go 的 domains 中加上它的领域", u)
	}
	for _, v := range violations {
		t.Errorf("越界的依赖：%s", v)
	}
}

// TestDepViolationsDetects：检查本身能发现越界（直接和传递依赖都在 Deps 中）、能发现没有归类的包。
func TestDepViolationsDetects(t *testing.T) {
	t.Parallel()
	graph := map[string][]string{
		"internal/server/ci/engine":  {"internal/server/store", "internal/server/ai/review"},
		"internal/server/ai/aidev":   {"internal/server/store", "internal/workflow"},
		"internal/server/mergequeue": {"internal/server/ci/cistore"},
		"internal/server/outbox":     {"internal/server/mqevents"},
		"internal/server/ingest":     {"internal/server/ci/planner", "internal/server/ai/review", "internal/server/mergequeue"},
		"internal/server/newpkg":     {},
		"internal/server/ci/cistore": {"internal/server/store"},
		"internal/server/ai/review":  {"internal/server/outbox", "internal/agentcfg"},
		"internal/server/store":      {"internal/shorten"},
		"internal/shorten":           {},
		"internal/workflow":          {"internal/expr"},
		"internal/expr":              {"internal/shorten"},
		"internal/agentcfg":          {},
		"internal/server/mqevents":   {"internal/server/outbox"},
		"internal/server/ci/planner": {"internal/workflow"},
	}
	violations, unclassified := depViolations(graph)
	want := []string{
		"internal/server/ai/aidev（AI）→ internal/workflow（CI）",
		"internal/server/ci/engine（CI）→ internal/server/ai/review（AI）",
		"internal/server/mergequeue（合并）→ internal/server/ci/cistore（CI）",
		"internal/server/outbox（共用）→ internal/server/mqevents（合并）",
	}
	if !slices.Equal(violations, want) {
		t.Errorf("越界 = %q\n期望 %q", violations, want)
	}
	if !slices.Equal(unclassified, []string{"internal/server/newpkg"}) {
		t.Errorf("没有归类 = %q", unclassified)
	}
}

// ---------- 不依赖协议 ----------

// protocolFree 是不能依赖任何 Runner 协议、生成代码和云 SDK 的包（含传递依赖），以及禁止的导入路径前缀（PLAN #32.4、I8）：
// CI 调度核心与触发方无关，Native 的 runner.v1 和以后托管的 Provider SDK 都只出现在触发方。
var protocolFree = map[string][]string{
	"internal/server/ci/scheduler": {modulePrefix + "gen/", "connectrpc.com/", "cloud.google.com/", "google.golang.org/api/", "google.golang.org/protobuf/"},
}

// protocolViolations 返回 protocolFree 中的包违反规则的依赖（"包 → 依赖"）。deps 的键是相对模块根的包路径，值是完整的导入路径。
func protocolViolations(deps map[string][]string) []string {
	var out []string
	for p, banned := range protocolFree {
		for _, d := range deps[p] {
			for _, b := range banned {
				if strings.HasPrefix(d, b) {
					out = append(out, fmt.Sprintf("%s → %s", p, d))
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestProtocolFree：CI 调度核心不依赖任何 Runner 协议（直接或传递）。
func TestProtocolFree(t *testing.T) {
	t.Parallel()
	deps := map[string][]string{}
	for _, p := range listPackages(t) {
		deps[strings.TrimPrefix(p.ImportPath, modulePrefix)] = p.Deps
	}
	for p := range protocolFree {
		if _, ok := deps[p]; !ok {
			t.Errorf("protocolFree 中的包 %s 不存在", p)
		}
	}
	for _, v := range protocolViolations(deps) {
		t.Errorf("调度核心依赖了协议：%s", v)
	}
}

// TestProtocolViolationsDetects：检查本身能发现调度核心 import runner.v1、airunner.v2（直接或经过其他包），不误报。
func TestProtocolViolationsDetects(t *testing.T) {
	t.Parallel()
	deps := map[string][]string{
		"internal/server/ci/scheduler": {
			modulePrefix + "internal/server/store", modulePrefix + "gen/go/kiln/runner/v1", modulePrefix + "gen/go/kiln/airunner/v2", "context",
		},
		"internal/server/ci/runnerapi": {modulePrefix + "gen/go/kiln/runner/v1"},
	}
	got := protocolViolations(deps)
	want := []string{
		"internal/server/ci/scheduler → " + modulePrefix + "gen/go/kiln/airunner/v2",
		"internal/server/ci/scheduler → " + modulePrefix + "gen/go/kiln/runner/v1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("违规 = %q\n期望 %q", got, want)
	}
}

// ---------- AI 不驱动 Agent 的终端 ----------

// aiNoTerminal 是 AI 领域不能依赖的包（PLAN #33）：AI 任务在 Orca 中执行，Server 只经连接器（kiln-orca）的 RPC 交接任务和
// 结果，不驱动 Agent 的终端、不解析它的终端输出。已经删除的 kiln-ai-runner（AI Agent 适配、终端会话）不能再出现，也不能
// 引入 pty、终端控制等库。
var aiNoTerminal = []string{
	modulePrefix + "internal/aiagent",
	modulePrefix + "internal/herdr",
	modulePrefix + "internal/airunner",
	"github.com/creack/pty",
	"golang.org/x/term",
}

// aiTerminalViolations 返回 AI 领域依赖了终端适配、终端解析包的包（"包 → 依赖"）。deps 的键是相对模块根的包路径，
// 值是完整的导入路径（含传递依赖）。
func aiTerminalViolations(deps map[string][]string) []string {
	var out []string
	for p, ds := range deps {
		if d, ok := classify(p); !ok || d != aiDom {
			continue
		}
		for _, dep := range ds {
			for _, banned := range aiNoTerminal {
				if dep == banned || strings.HasPrefix(dep, banned+"/") {
					out = append(out, fmt.Sprintf("%s → %s", p, dep))
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestAINoTerminal：AI 领域不依赖终端适配、终端解析的包（直接或传递）。
func TestAINoTerminal(t *testing.T) {
	t.Parallel()
	deps := map[string][]string{}
	for _, p := range listPackages(t) {
		deps[strings.TrimPrefix(p.ImportPath, modulePrefix)] = p.Deps
	}
	for _, v := range aiTerminalViolations(deps) {
		t.Errorf("AI 领域出现了终端适配：%s", v)
	}
}

// TestAITerminalViolationsDetects：检查本身能发现 AI 领域引入 kiln-ai-runner 的包或 pty 库，不误报其他领域。
func TestAITerminalViolationsDetects(t *testing.T) {
	t.Parallel()
	deps := map[string][]string{
		"internal/server/ai":                    {modulePrefix + "internal/aiagent"},
		"internal/server/ai/aidev":              {modulePrefix + "internal/server/store", modulePrefix + "internal/herdr"},
		"internal/server/ai/review":             {"github.com/creack/pty", "golang.org/x/term"},
		"internal/server/ci/engine":             {modulePrefix + "internal/aiagent"},
		"internal/server/ai/aiprompt/prompts/x": {modulePrefix + "internal/server/store"},
	}
	got := aiTerminalViolations(deps)
	want := []string{
		"internal/server/ai → " + modulePrefix + "internal/aiagent",
		"internal/server/ai/aidev → " + modulePrefix + "internal/herdr",
		"internal/server/ai/review → github.com/creack/pty",
		"internal/server/ai/review → golang.org/x/term",
	}
	if !slices.Equal(got, want) {
		t.Errorf("违规 = %q\n期望 %q", got, want)
	}
}

// ---------- 表归属 ----------

// tables 是每张表所属的领域。迁移中新建的表要先在这里归类（TestTablesClassified）。
var tables = map[string]domain{
	// 共用
	"admin_binding": shared, "audit_log": shared, "auth_nonces": shared, "blobs": shared, "config_versions": shared,
	"enrollment_tokens": shared, "environments": shared, "events": shared, "installations": shared, "kv": shared,
	"npm_accounts": shared, "outbox": shared, "ref_heads": shared, "repos": shared, "runner_upgrade_requests": shared,
	"runners": shared, "secrets": shared, "variables": shared,
	"repo_doc_bindings": shared, "repo_decision_docs": shared,
	// CI
	"action_cache": ci, "attempts": ci, "ci_runners": ci, "image_publishes": ci, "jobs": ci, "release_targets": ci,
	"releases": ci, "run_reruns": ci, "runner_cache_purges": ci, "runs": ci,
	// AI
	"ai_attempts": aiDom, "ai_dev_requests": aiDom, "ai_repo_state": aiDom, "ai_requests": aiDom, "ai_review_requests": aiDom,
	"ai_reviews": aiDom, "ai_runner_agents": aiDom, "ai_runners": aiDom, "ai_tasks": aiDom, "dev_branches": aiDom,
	"dev_comments": aiDom, "dev_issue_skips": aiDom, "dev_offers": aiDom, "dev_pool": aiDom, "dev_pull_origins": aiDom,
	"dev_pull_origin_checks": aiDom,
	"dev_reports":            aiDom, "dev_work_item_issues": aiDom, "dev_work_items": aiDom,
	"work_item_doc_snapshots": aiDom, "work_item_doc_freezes": aiDom, "dev_decision_syncs": aiDom,
	"doc_snapshot_sets": aiDom, "doc_snapshot_docs": aiDom,
	// 合并
	"auto_tag_candidates": merge, "auto_tags": merge, "merge_auto_candidates": merge, "merge_queue": merge, "pr_plans": merge,
}

// droppedTables 是迁移中建过、之后删掉的表。
var droppedTables = []string{
	"runner_releases", "runner_upgrade_source", "runner_upgrade_targets",
	"doc_watermarks", "doc_webhook_deliveries", "doc_delta_issues",
}

// tableRef 匹配 SQL 中引用表的位置（FROM、JOIN、INTO、UPDATE、DELETE FROM、TABLE 之后的标识符）。
var tableRef = regexp.MustCompile(`(?i)\b(?:from|join|into|update|table)\s+([a-z_][a-z0-9_]*)`)

// sqlTables 返回 Go 源文件 src 的字符串常量中引用的已知表。
func sqlTables(fset *token.FileSet, name string, src any) (map[string]bool, error) {
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		for _, m := range tableRef.FindAllStringSubmatch(s, -1) {
			if _, known := tables[strings.ToLower(m[1])]; known {
				out[strings.ToLower(m[1])] = true
			}
		}
		return true
	})
	return out, nil
}

// TestTableOwnership：CI 的包不出现 AI 的表，AI 的包不出现 CI 的表，合并的包只出现合并和共用的表，共用的包只出现共用的表
// （PLAN #30.3）。组装层不限。只扫描非测试文件。
func TestTableOwnership(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	for _, p := range listPackages(t) {
		rel := strings.TrimPrefix(p.ImportPath, modulePrefix)
		from, ok := classify(rel)
		if !ok || from == assembly {
			continue // 没有归类的包由 TestDependencies 报告
		}
		for _, f := range p.GoFiles {
			refs, err := sqlTables(fset, filepath.Join(p.Dir, f), nil)
			if err != nil {
				t.Fatal(err)
			}
			for tbl := range refs {
				if to := tables[tbl]; !slices.Contains(allowed[from], to) {
					t.Errorf("%s/%s（%s）访问了 %s 的表 %s", rel, f, from, to, tbl)
				}
			}
		}
	}
}

// TestSQLTablesDetects：表的识别覆盖常见写法，忽略不是表的词和未知的名字。
func TestSQLTablesDetects(t *testing.T) {
	t.Parallel()
	src := "package x\n" +
		"const a = `SELECT r.id FROM runs r JOIN jobs j ON j.run_id=r.id LEFT JOIN ai_tasks t ON true`\n" +
		"const b = \"INSERT INTO outbox (k) VALUES (1)\"\n" +
		"const c = `update merge_queue set x=1; DELETE FROM dev_offers; ALTER TABLE ci_runners ADD x int`\n" +
		"const d = \"来自 from main 的说明，from unknown_table\"\n"
	got, err := sqlTables(token.NewFileSet(), "x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	want := []string{"ai_tasks", "ci_runners", "dev_offers", "jobs", "merge_queue", "outbox", "runs"}
	if !slices.Equal(names, want) {
		t.Fatalf("识别的表 = %v，期望 %v", names, want)
	}
}

// createdTables 匹配迁移中建表的语句。
var createdTables = regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?([a-z_][a-z0-9_]*)`)

// TestTablesClassified：迁移中建的每张表（删掉的除外）都已归类，归类的表都存在。
func TestTablesClassified(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("没有找到迁移：%v", err)
	}
	created := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range createdTables.FindAllStringSubmatch(string(data), -1) {
			created[strings.ToLower(m[1])] = true
		}
	}
	for _, d := range droppedTables {
		delete(created, d)
	}
	for tbl := range created {
		if _, ok := tables[tbl]; !ok {
			t.Errorf("表 %s 没有归类：在 archtest_test.go 的 tables 中加上它的领域", tbl)
		}
	}
	for tbl := range tables {
		if !created[tbl] {
			t.Errorf("归类的表 %s 在迁移中不存在", tbl)
		}
	}
}
