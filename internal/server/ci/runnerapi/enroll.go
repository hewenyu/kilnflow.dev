package runnerapi

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/rpcerr"
	"github.com/hewenyu/kilnflow.dev/internal/server/runnerauth"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 注册与认证（PLAN #9.3）：身份由共用的 runnerauth 处理（CI Runner、AI 执行节点都经 runner.v1 的 Enroll、Authenticate），
// CI Runner 的领域记录（ci_runners：pool、平台、executor）由 EnrollHook 写入。

// EnrollHook 是 CI Runner 注册时写入 ci_runners 的钩子（runnerauth.EnrollHook）。e.Details 是注册请求中上报的状态
// （*runnerv1.RunnerState，可能为 nil）。
func EnrollHook(ctx context.Context, tx pgx.Tx, e runnerauth.Enrolled) error {
	st, _ := e.Details.(*runnerv1.RunnerState)
	caps := map[string]any{"host_fingerprint": st.GetHostFingerprint()}
	_, err := tx.Exec(ctx, `INSERT INTO ci_runners (runner_id, pool, platforms, executors, capabilities) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (runner_id) DO UPDATE SET pool=EXCLUDED.pool, platforms=EXCLUDED.platforms, executors=EXCLUDED.executors,
		  capabilities=EXCLUDED.capabilities, capacity=NULL, running_jobs=0`,
		e.ID, e.Pool, orEmpty(st.GetPlatforms()), orEmpty(st.GetExecutors()), store.MustJSON(caps))
	return err
}

// orEmpty 把 nil 切片换成空切片：pgx 把 nil 切片写成 NULL，而 ci_runners.platforms、executors 是 NOT NULL
// （Runner 没有上报状态，或者上报的列表为空时）。
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Enroll 用一次性 enroll token 注册 Runner（CI Runner 和 AI 执行节点共用），记录它的公钥。
func (s *Service) Enroll(ctx context.Context, req *connect.Request[runnerv1.EnrollRequest]) (*connect.Response[runnerv1.EnrollResponse], error) {
	m := req.Msg
	e, err := s.auth.Enroll(ctx, runnerauth.EnrollRequest{Token: m.Token, Name: m.Name, PublicKey: m.PublicKey, Version: m.State.GetVersion(), Details: m.State})
	if runnerauth.IsRejection(err) {
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	}
	if err != nil {
		return nil, err
	}
	s.log.Info("Runner 已注册", "name", m.Name, "id", e.ID, "pool", e.Pool, "kind", e.Kind, "role", e.Role)
	return connect.NewResponse(&runnerv1.EnrollResponse{RunnerId: e.ID, Pool: e.Pool, Kind: e.Kind, Role: e.Role}), nil
}

// Authenticate 校验 Runner 对时间戳和随机数的签名，签发短期 access token（PLAN #9.3）。
func (s *Service) Authenticate(ctx context.Context, req *connect.Request[runnerv1.AuthenticateRequest]) (*connect.Response[runnerv1.AuthenticateResponse], error) {
	m := req.Msg
	tok, exp, err := s.auth.Authenticate(ctx, m.RunnerId, m.Timestamp, m.Nonce, m.Signature)
	switch {
	case errors.Is(err, runnerauth.ErrRevoked):
		return nil, rpcerr.RunnerRevoked()
	case runnerauth.IsRejection(err):
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	case err != nil:
		return nil, err
	}
	return connect.NewResponse(&runnerv1.AuthenticateResponse{AccessToken: tok, ExpiresAt: exp.Unix()}), nil
}

// authorize 校验请求的 access token，只接受 CI Runner（AI 执行节点的执行接口是 airunner.v2，PLAN #33.6），返回 Runner ID。
func (s *Service) authorize(ctx context.Context, h interface{ Header() http.Header }) (int64, error) {
	return s.auth.AuthorizeKind(ctx, h.Header(), store.RunnerKindCI)
}
