package fakegithub

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// 模拟 internal/github/git.go 用到的接口：git 数据（tree、blob、commit、ref、tag）和变更文件列表。

// git 在 dir 中执行 git 命令。operands 来自请求（sha、ref 等），放在 --end-of-options 之后，
// 不会被当成选项解析。
func git(ctx context.Context, dir string, args []string, operands ...string) (string, error) {
	args = append(slices.Concat(args, []string{"--end-of-options"}), operands...)
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // 不经过 shell，请求中的值都在 --end-of-options 之后
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v：%w：%s", args, err, stderr.String())
	}
	return string(out), nil
}

// lines 按行切分 git 的输出，去掉空行。
func lines(out string) []string {
	var l []string
	for s := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if s != "" {
			l = append(l, s)
		}
	}
	return l
}

// refFormat 是 for-each-ref 输出 ref 名、对象 sha 和类型的格式。
const refFormat = "--format=%(refname) %(objectname) %(objecttype)"

// refJSON 把一行 refFormat 输出转换成 ref 对象。
func refJSON(line string) (map[string]any, bool) {
	f := strings.Fields(line)
	if len(f) != 3 {
		return nil, false
	}
	return map[string]any{"ref": f[0], "object": map[string]any{"sha": f[1], "type": f[2]}}, true
}

func (s *Server) getTree(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	v, ok := s.cachedObject(r, rp, objectTree, func() (map[string]any, bool) {
		out, err := git(r.Context(), rp.Dir, []string{"ls-tree", "-r", "-t", "-l", "-z"}, r.PathValue("sha"))
		if err != nil {
			notFound(w)
			return nil, false
		}
		var entries []map[string]any
		for rec := range strings.SplitSeq(out, "\x00") {
			meta, path, ok := strings.Cut(rec, "\t")
			if !ok {
				continue
			}
			// mode type sha size
			f := strings.Fields(meta)
			if len(f) < 4 {
				continue
			}
			e := map[string]any{"path": path, "mode": f[0], "type": f[1], "sha": f[2]}
			if n, err := strconv.ParseInt(f[3], 10, 64); err == nil {
				e["size"] = n
			}
			entries = append(entries, e)
		}
		return map[string]any{"sha": r.PathValue("sha"), "tree": entries, "truncated": false}, true
	})
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getBlob(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	v, ok := s.cachedObject(r, rp, objectBlob, func() (map[string]any, bool) {
		out, err := git(r.Context(), rp.Dir, []string{"cat-file", "blob"}, r.PathValue("sha"))
		if err != nil {
			notFound(w)
			return nil, false
		}
		return map[string]any{"sha": r.PathValue("sha"), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(out))}, true
	})
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getCommit(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	v, ok := s.cachedObject(r, rp, objectCommit, func() (map[string]any, bool) {
		out, err := git(r.Context(), rp.Dir, []string{"show", "-s", "--format=%H%n%cI%n%T%n%P%n%s"}, r.PathValue("sha"))
		if err != nil {
			notFound(w)
			return nil, false
		}
		// sha、提交时间、tree、父提交、标题
		l := strings.SplitN(out, "\n", 5)
		if len(l) < 5 {
			writeMessage(w, http.StatusInternalServerError, "unexpected git output")
			return nil, false
		}
		parents := []any{}
		for p := range strings.FieldsSeq(l[3]) {
			parents = append(parents, map[string]any{"sha": p})
		}
		return map[string]any{
			"sha": l[0], "message": strings.TrimSpace(l[4]),
			"committer": map[string]any{"date": l[1]}, "tree": map[string]any{"sha": l[2]}, "parents": parents,
		}, true
	})
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getRef(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	ref := "refs/" + r.PathValue("ref")
	out, err := git(r.Context(), rp.Dir, []string{"for-each-ref", refFormat}, ref)
	if err != nil {
		notFound(w)
		return
	}
	// for-each-ref 按前缀匹配，只接受完全相同的 ref
	for _, l := range lines(out) {
		if j, ok := refJSON(l); ok && j["ref"] == ref {
			writeJSON(w, http.StatusOK, j)
			return
		}
	}
	notFound(w)
}

func (s *Server) matchingRefs(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	list := []any{}
	if firstPage(r) {
		// GitHub 按字符串前缀匹配（tags/v 匹配 refs/tags/v0.1.0），for-each-ref 的模式只按路径的每一级匹配：
		// 列出全部 ref 后按前缀过滤
		prefix := "refs/" + r.PathValue("prefix")
		out, err := git(r.Context(), rp.Dir, []string{"for-each-ref", refFormat}, "refs/")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, l := range lines(out) {
			j, ok := refJSON(l)
			ref, _ := j["ref"].(string)
			if ok && strings.HasPrefix(ref, prefix) && !s.refHidden(ref) {
				list = append(list, j)
			}
		}
	}
	writeJSON(w, http.StatusOK, list)
}

// createRef 模拟 POST /git/refs：ref 已经存在时返回 422 "Reference already exists"（不移动），sha 不是已有的 commit 时返回 422。
func (s *Server) createRef(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	var body struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !strings.HasPrefix(body.Ref, "refs/") || strings.Count(body.Ref, "/") < 2 {
		writeMessage(w, http.StatusUnprocessableEntity, "Reference name is invalid")
		return
	}
	if _, err := git(r.Context(), rp.Dir, []string{"cat-file", "-e"}, body.SHA+"^{commit}"); err != nil {
		writeMessage(w, http.StatusUnprocessableEntity, "Object does not exist")
		return
	}
	// update-ref 的旧值为全 0：ref 已经存在时失败，不会移动已有的 ref
	if _, err := git(r.Context(), rp.Dir, []string{"update-ref"}, body.Ref, body.SHA, strings.Repeat("0", 40)); err != nil {
		writeMessage(w, http.StatusUnprocessableEntity, "Reference already exists")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ref": body.Ref, "object": map[string]any{"type": "commit", "sha": body.SHA}})
}

func (s *Server) getTag(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	out, err := git(r.Context(), rp.Dir, []string{"cat-file", "tag"}, r.PathValue("sha"))
	if err != nil {
		notFound(w)
		return
	}
	obj := map[string]any{}
	for l := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(l, "object "); ok {
			obj["sha"] = v
		}
		if v, ok := strings.CutPrefix(l, "type "); ok {
			obj["type"] = v
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sha": r.PathValue("sha"), "object": obj})
}

// fileList 把 git diff --name-only 的输出转换成文件对象列表。
func fileList(out string) []any {
	files := []any{}
	for _, f := range lines(out) {
		files = append(files, map[string]any{"filename": f})
	}
	return files
}

func (s *Server) pullFiles(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	files := []any{}
	if firstPage(r) {
		s.mu.Lock()
		head := s.prHeads[r.PathValue("n")]
		s.mu.Unlock()
		out, _ := git(r.Context(), rp.Dir, []string{"diff", "--name-only"}, head+"~1", head)
		patches := s.pullPatches(r, rp, r.PathValue("n"))
		for _, f := range lines(out) {
			files = append(files, map[string]any{"filename": f, "patch": patches[f]})
		}
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	rp := s.repo(w, r)
	if rp == nil {
		return
	}
	out, err := git(r.Context(), rp.Dir, []string{"diff", "--name-only"}, r.PathValue("basehead"))
	if err != nil {
		notFound(w)
		return
	}
	resp := map[string]any{"files": fileList(out), "total_commits": 1}
	// ahead_by / behind_by：head 比 base 多、少的提交数；commits：head 多出的提交（从旧到新）
	if base, head, ok := strings.Cut(r.PathValue("basehead"), "..."); ok {
		if log, err := git(r.Context(), rp.Dir, []string{"log", "--reverse", "--format=%H%x00%B%x01"}, base+".."+head); err == nil {
			commits := []map[string]any{}
			for rec := range strings.SplitSeq(log, "\x01") {
				sha, msg, ok := strings.Cut(strings.TrimLeft(rec, "\n"), "\x00")
				if !ok {
					continue
				}
				commits = append(commits, map[string]any{"sha": sha, "commit": map[string]any{"message": strings.TrimSuffix(msg, "\n")}})
			}
			resp["commits"], resp["total_commits"] = commits, len(commits)
		}
		if counts, err := git(r.Context(), rp.Dir, []string{"rev-list", "--left-right", "--count"}, base+"..."+head); err == nil {
			if f := strings.Fields(counts); len(f) == 2 {
				behind, _ := strconv.Atoi(f[0])
				ahead, _ := strconv.Atoi(f[1])
				status := "diverged"
				switch {
				case ahead == 0 && behind == 0:
					status = "identical"
				case behind == 0:
					status = "ahead"
				case ahead == 0:
					status = "behind"
				}
				resp["ahead_by"], resp["behind_by"], resp["status"] = ahead, behind, status
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
