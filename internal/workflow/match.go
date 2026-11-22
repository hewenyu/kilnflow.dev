package workflow

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/hewenyu/kilnflow.dev/internal/expr"
)

// ---------- inputs 匹配（PLAN #4.6）----------

// defaultExcluded 是默认 inputs（整个仓库）中排除的目录前缀。
const defaultExcluded = Dir + "/"

// InputMatcher 判断仓库中的路径是否属于 job 的 inputs。
// Planner 计算输入摘要、Runner 检出源码时使用同一份代码。
type InputMatcher struct {
	def      bool
	patterns []inputPattern
}

type inputPattern struct {
	neg  bool
	glob string // doublestar 模式
	dir  string // 不含通配符时：精确路径，或者以它为前缀的目录；空字符串表示整个仓库
}

// parseInputPattern 规范化一个 inputs 模式：! 开头表示排除，去掉开头的 ./ 和 /，
// 以 / 结尾表示目录下的全部文件。
func parseInputPattern(p string) inputPattern {
	ip := inputPattern{}
	if rest, ok := strings.CutPrefix(p, "!"); ok {
		ip.neg = true
		p = rest
	}
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	if p == "." {
		p = "" // 整个仓库，与 ./ 相同
	}
	if strings.HasSuffix(p, "/") {
		p += "**"
	}
	if strings.ContainsAny(p, "*?[{") {
		ip.glob = p
	} else {
		ip.dir = p
	}
	return ip
}

// match 判断路径是否匹配模式（不考虑 neg）。
func (p inputPattern) match(path string) bool {
	if p.glob != "" {
		// 模式在解析时已用 validInputPattern 校验；来自表达式的模式非法时视为不匹配
		ok, _ := doublestar.Match(p.glob, path)
		return ok
	}
	return p.dir == "" || path == p.dir || strings.HasPrefix(path, p.dir+"/")
}

func (p inputPattern) String() string {
	if p.glob != "" {
		return p.glob
	}
	return p.dir
}

// validInputPattern 检查 inputs 模式的 glob 语法。含 ${{ }} 的模式展开后才能确定，这里不检查。
func validInputPattern(p string) bool {
	if expr.HasExpr(p) {
		return true
	}
	ip := parseInputPattern(p)
	return ip.glob == "" || doublestar.ValidatePattern(ip.glob)
}

// NewInputMatcher 构造匹配器。inputs 为 nil 时表示默认值：整个仓库，但不含 .kiln/workflows/。
func NewInputMatcher(inputs []string) *InputMatcher {
	if inputs == nil {
		return &InputMatcher{def: true}
	}
	m := &InputMatcher{}
	for _, p := range inputs {
		m.patterns = append(m.patterns, parseInputPattern(p))
	}
	return m
}

// Default 表示是否使用默认的 inputs。
func (m *InputMatcher) Default() bool { return m.def }

// Match 判断路径是否属于 inputs：按顺序求值，最后一个匹配的模式决定结果。
func (m *InputMatcher) Match(path string) bool {
	if m.def {
		return !strings.HasPrefix(path, defaultExcluded)
	}
	matched := false
	for _, p := range m.patterns {
		if p.match(path) {
			matched = !p.neg
		}
	}
	return matched
}

// EmptyPatterns 返回没有匹配任何路径的正向模式，用于"某一项匹配为空"的警告。
func (m *InputMatcher) EmptyPatterns(paths []string) []string {
	if m.def {
		return nil
	}
	var out []string
	for _, p := range m.patterns {
		if p.neg {
			continue
		}
		if !slices.ContainsFunc(paths, p.match) {
			out = append(out, p.String())
		}
	}
	return out
}

// ---------- 事件过滤（branches、tags、paths）----------

// filter 是编译后的一组过滤模式（branches、tags、paths 或对应的 -ignore）。
type filter []filterPattern

type filterPattern struct {
	neg bool
	re  *regexp.Regexp
}

// compileFilter 编译一组过滤模式。模式在解析时已用 validFilterPattern 校验，无法编译的跳过（不影响结果）。
func compileFilter(patterns []string) filter {
	f := make(filter, 0, len(patterns))
	for _, p := range patterns {
		p, neg := strings.CutPrefix(p, "!")
		if re, err := filterRegexp(p); err == nil {
			f = append(f, filterPattern{neg: neg, re: re})
		}
	}
	return f
}

// match 按 Actions 的规则匹配：按顺序求值，以 ! 开头的是排除，最后一个匹配的模式决定结果。
func (f filter) match(value string) bool {
	matched := false
	for _, p := range f {
		if p.re.MatchString(value) {
			matched = !p.neg
		}
	}
	return matched
}

// filterRegexp 把 Actions 的过滤模式转换成正则表达式，规则与 Actions 的 filter pattern 一致：
// * 匹配 / 以外的任意字符；** 匹配任意字符（包括 /，例如 **.md 匹配 docs/a.md），**/ 也可以匹配零层目录；
// ? 和 + 分别表示前一个字符出现零或一次、一次或多次；[] 匹配括号中的一个字符（可以写范围）；
// \ 转义下一个字符；其他字符按字面匹配。
func filterRegexp(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); {
		c, size := utf8.DecodeRuneInString(p[i:])
		switch c {
		case '*':
			switch {
			case strings.HasPrefix(p[i:], "**/"):
				b.WriteString("(?:.*/)?")
				i += 3
			case strings.HasPrefix(p[i:], "**"):
				b.WriteString(".*")
				i += 2
			default:
				b.WriteString("[^/]*")
				i++
			}
		case '?', '+':
			b.WriteRune(c)
			i += size
		case '[':
			// 字符类原样交给 regexp，但要按转义找到真正的 ']'：[\]] 里的 ']' 属于字符类。
			j := classEnd(p, i+1)
			if j < 0 {
				return nil, fmt.Errorf("模式 %q 缺少 ']'", p)
			}
			b.WriteString(p[i : j+1])
			i = j + 1
		case '\\':
			if i+size >= len(p) {
				return nil, fmt.Errorf("模式 %q 以 \\ 结尾", p)
			}
			r, n := utf8.DecodeRuneInString(p[i+size:])
			b.WriteString(regexp.QuoteMeta(string(r)))
			i += size + n
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i += size
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// classEnd 返回字符类中结束 ']' 的位置：跳过反斜杠转义的字符；紧跟在 '[' 后的 ']' 按字面处理。
func classEnd(p string, start int) int {
	for k := start; k < len(p); k++ {
		switch p[k] {
		case '\\':
			k++
		case ']':
			return k
		}
	}
	return -1
}

// validFilterPattern 检查 branches、tags、paths 中的模式的语法。
func validFilterPattern(p string) bool {
	_, err := filterRegexp(strings.TrimPrefix(p, "!"))
	return err == nil
}

// Event 是用于匹配 on 的事件。
type Event struct {
	Name    string // push | pull_request | workflow_dispatch | schedule
	Ref     string // 完整 ref
	Action  string // pull_request 的 action
	BaseRef string // pull_request 的目标分支名
	// ChangedFiles 按需获取变更文件；complete=false 表示列表不完整（视为匹配）。
	ChangedFiles func() (files []string, complete bool, err error)
}

// MatchResult 是事件匹配的结果。
type MatchResult int

// 事件匹配的结果。
const (
	NoMatch MatchResult = iota
	Matched
	// PathsExcluded：事件类型和分支都匹配，只是被 paths 过滤掉（PLAN #13.3，仍然创建一个 skipped 的 run）。
	PathsExcluded
)

// defaultPRTypes 是没有写 on.pull_request.types 时触发的 action。
var defaultPRTypes = []string{"opened", "synchronize", "reopened"}

// Match 判断 workflow 是否被事件触发。
func (w *Workflow) Match(ev Event) (MatchResult, error) {
	switch ev.Name {
	case EventPush:
		if w.On.Push == nil {
			return NoMatch, nil
		}
		return matchPush(w.On.Push, ev)
	case EventPullRequest:
		if w.On.PullRequest == nil {
			return NoMatch, nil
		}
		return matchPR(w.On.PullRequest, ev)
	case EventWorkflowDispatch:
		if w.On.WorkflowDispatch == nil {
			return NoMatch, nil
		}
		return Matched, nil
	case EventSchedule:
		if len(w.On.Schedule) == 0 {
			return NoMatch, nil
		}
		return Matched, nil
	}
	return NoMatch, nil
}

// included 按 Actions 的规则判断 value 是否通过一对过滤条件（例如 branches 和 branches-ignore）。
func included(only, ignore []string, value string) bool {
	if only != nil && !compileFilter(only).match(value) {
		return false
	}
	return ignore == nil || !compileFilter(ignore).match(value)
}

// MatchesBranch 判断 pull_request 触发器是否会在目标分支 branch 上触发（只看 branches / branches-ignore 的过滤；
// types、paths 仍可能再过滤）。lint 用它检查允许合并的目标分支是否都能得到必需的验证（PLAN #28.5）。
func (p *PRTrigger) MatchesBranch(branch string) bool {
	return p != nil && included(p.Branches, p.BranchesIgnore, branch)
}

func matchPush(p *PushTrigger, ev Event) (MatchResult, error) {
	branchFilters := p.Branches != nil || p.BranchesIgnore != nil
	tagFilters := p.Tags != nil || p.TagsIgnore != nil
	if tag, ok := strings.CutPrefix(ev.Ref, RefPrefixTag); ok {
		switch {
		case !tagFilters && branchFilters:
			return NoMatch, nil
		case !tagFilters:
			return Matched, nil // paths 不作用于 tag push
		case !included(p.Tags, p.TagsIgnore, tag):
			return NoMatch, nil
		}
		return Matched, nil
	}
	if branch, ok := strings.CutPrefix(ev.Ref, RefPrefixBranch); ok {
		if !branchFilters && tagFilters {
			return NoMatch, nil
		}
		if !included(p.Branches, p.BranchesIgnore, branch) {
			return NoMatch, nil
		}
		return pathsMatch(p.Paths, p.PathsIgnore, ev)
	}
	return NoMatch, nil
}

func matchPR(p *PRTrigger, ev Event) (MatchResult, error) {
	types := p.Types
	if types == nil {
		types = defaultPRTypes
	}
	if !slices.Contains(types, ev.Action) {
		return NoMatch, nil
	}
	if !included(p.Branches, p.BranchesIgnore, ev.BaseRef) {
		return NoMatch, nil
	}
	return pathsMatch(p.Paths, p.PathsIgnore, ev)
}

func pathsMatch(paths, ignore []string, ev Event) (MatchResult, error) {
	if paths == nil && ignore == nil {
		return Matched, nil
	}
	if ev.ChangedFiles == nil {
		return Matched, nil
	}
	files, complete, err := ev.ChangedFiles()
	if err != nil {
		return NoMatch, err
	}
	if !complete {
		return Matched, nil
	}
	if paths != nil {
		if slices.ContainsFunc(files, compileFilter(paths).match) {
			return Matched, nil
		}
		return PathsExcluded, nil
	}
	ign := compileFilter(ignore)
	if slices.ContainsFunc(files, func(f string) bool { return !ign.match(f) }) {
		return Matched, nil
	}
	return PathsExcluded, nil
}
