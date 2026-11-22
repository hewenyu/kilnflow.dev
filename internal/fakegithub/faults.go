package fakegithub

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
)

// 故障注入（PLAN #29.8）：外部写操作至多一次的测试需要两种 GitHub 上真实会出现、但正常的模拟接口不会出现的情况——
// 写操作已经生效、响应却丢了（客户端只看到 5xx 或连接断开），以及列表接口读到稍旧的数据。

// Fault 是注入到一个请求上的故障（SetFault）。
type Fault struct {
	// Lost 为真时请求照常生效，但客户端收到 502（看不到结果）。
	Lost bool
	// Status 非 0 时请求不生效，直接返回这个状态码（Lost 为假时）。
	Status int
	// Times 是生效的次数，0 表示 1 次。
	Times int
	// Pause 非 nil 时先调用它（例如等测试放行，模拟慢请求）；Lost、Status 都没有设置时之后照常处理。
	Pause func()
}

// SetFault 在请求 route（"METHOD path"，path 是请求的完整路径，例如 "POST /repos/o/r/issues/3/comments"）上注入故障；
// 生效 Times 次之后自动去掉。
func (s *Server) SetFault(route string, f Fault) {
	if f.Times == 0 {
		f.Times = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.faults == nil {
		s.faults = map[string]*Fault{}
	}
	s.faults[route] = &f
}

// takeFault 返回请求上注入的故障（没有时为 nil），并扣减次数。
func (s *Server) takeFault(r *http.Request) *Fault {
	route := r.Method + " " + r.URL.Path
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.faults[route]
	if f == nil {
		return nil
	}
	if f.Times--; f.Times <= 0 {
		delete(s.faults, route)
	}
	out := *f
	return &out
}

// serveWithFault 按注入的故障处理请求；没有故障时返回 false（照常处理）。
func (s *Server) serveWithFault(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) bool {
	f := s.takeFault(r)
	if f == nil {
		return false
	}
	if f.Pause != nil {
		f.Pause()
		if !f.Lost && f.Status == 0 {
			return false
		}
	}
	if f.Lost {
		// 照常执行，丢掉响应
		next(httptest.NewRecorder(), r)
		writeMessage(w, http.StatusBadGateway, "fakegithub：响应丢失（注入的故障）")
		return true
	}
	writeMessage(w, f.Status, "fakegithub：注入的故障")
	return true
}

// HideRefs 让列出 ref 的接口（matching-refs）暂时看不到这些 ref（完整名字，如 refs/tags/v1.0.0），模拟读到稍旧的数据；
// 按名字读取（git/ref/…）不受影响。不带参数时恢复。
func (s *Server) HideRefs(refs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hiddenRefs = slices.Clone(refs)
}

// refHidden 判断 ref 是否被 HideRefs 隐藏。
func (s *Server) refHidden(ref string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.hiddenRefs, func(h string) bool { return strings.EqualFold(h, ref) })
}

// HideReviews 让列出 PR review 的接口暂时看不到已有的 review（stale 为真时），模拟读到稍旧的数据。
func (s *Server) HideReviews(stale bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staleReviews = stale
}
