package admin

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/server/upgradectl"
)

// Runner 按台升级（PLAN #27.13、#27.14）：Dashboard 和 CLI 共用 upgradectl 的操作，这里只做 RPC 的转换；改动请求、
// registry 和检查序号后唤醒长轮询中的 Runner，让新的指令尽快下发。

// GetRunnerUpgrade 返回 registry 设置。
func (s *Service) GetRunnerUpgrade(ctx context.Context) (*apiv1.GetRunnerUpgradeResponse, error) {
	return upgradectl.Overview(ctx, s.db)
}

// SetRunnerUpgradeRegistry 设置 Runner 查询和下载升级包用的 registry（空表示官方 registry）。
func (s *Service) SetRunnerUpgradeRegistry(ctx context.Context, registry, actor string) error {
	err := upgradectl.SetRegistry(ctx, s.db, registry, actor)
	if err == nil {
		s.notifyRunners()
	}
	return err
}

// RequestRunnerUpgrade 请求把 Runner 升级到它上报的最新版本；已有进行中的请求时返回那一个（created 为 false）。
// version 不为空时必须等于 Runner 上报的最新版本（管理员确认的版本，见 upgradectl.RequestUpgrade）。
func (s *Service) RequestRunnerUpgrade(ctx context.Context, runner, version, actor string) (*apiv1.RunnerUpgradeRequest, bool, error) {
	req, created, err := upgradectl.RequestUpgrade(ctx, s.db, runner, version, actor)
	if err == nil {
		s.notifyRunners()
	}
	return req, created, err
}

// CancelRunnerUpgrade 取消 Runner 进行中的升级请求。
func (s *Service) CancelRunnerUpgrade(ctx context.Context, runner, actor string) (*apiv1.RunnerUpgradeRequest, error) {
	req, err := upgradectl.CancelUpgrade(ctx, s.db, runner, actor)
	if err == nil {
		s.notifyRunners()
	}
	return req, err
}

// CheckRunnerUpgrade 让 Runner 立即查询最新版本（runner 为空表示全部），返回请求检查的数量。
func (s *Service) CheckRunnerUpgrade(ctx context.Context, runner, actor string) (int, error) {
	n, err := upgradectl.RequestCheck(ctx, s.db, runner, actor)
	if err == nil {
		s.notifyRunners()
	}
	return n, err
}

// notifyRunners 唤醒正在长轮询的 Runner，让新的指令尽快下发。
func (s *Service) notifyRunners() {
	if s.hub != nil {
		s.hub.All()
	}
}

// upgradeErr 把升级操作的错误转换成 RPC 错误。
func upgradeErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, upgradectl.ErrRunnerNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	}
	if _, ok := errors.AsType[*upgradectl.InvalidError](err); ok {
		return invalid(err)
	}
	return rpcError(connect.CodeFailedPrecondition, err)
}

func (a *rpcAPI) GetRunnerUpgrade(ctx context.Context, _ *connect.Request[apiv1.GetRunnerUpgradeRequest]) (*connect.Response[apiv1.GetRunnerUpgradeResponse], error) {
	resp, err := a.s.GetRunnerUpgrade(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (a *rpcAPI) SetRunnerUpgradeRegistry(ctx context.Context, req *connect.Request[apiv1.SetRunnerUpgradeRegistryRequest]) (*connect.Response[apiv1.SetRunnerUpgradeRegistryResponse], error) {
	if err := a.s.SetRunnerUpgradeRegistry(ctx, req.Msg.Registry, rpcActor(ctx)); err != nil {
		return nil, upgradeErr(err)
	}
	return connect.NewResponse(&apiv1.SetRunnerUpgradeRegistryResponse{}), nil
}

func (a *rpcAPI) RequestRunnerUpgrade(ctx context.Context, req *connect.Request[apiv1.RequestRunnerUpgradeRequest]) (*connect.Response[apiv1.RequestRunnerUpgradeResponse], error) {
	r, created, err := a.s.RequestRunnerUpgrade(ctx, req.Msg.Runner, req.Msg.Version, rpcActor(ctx))
	if err != nil {
		return nil, upgradeErr(err)
	}
	return connect.NewResponse(&apiv1.RequestRunnerUpgradeResponse{Request: r, Created: created}), nil
}

func (a *rpcAPI) CancelRunnerUpgrade(ctx context.Context, req *connect.Request[apiv1.CancelRunnerUpgradeRequest]) (*connect.Response[apiv1.CancelRunnerUpgradeResponse], error) {
	r, err := a.s.CancelRunnerUpgrade(ctx, req.Msg.Runner, rpcActor(ctx))
	if err != nil {
		return nil, upgradeErr(err)
	}
	return connect.NewResponse(&apiv1.CancelRunnerUpgradeResponse{Request: r}), nil
}

func (a *rpcAPI) CheckRunnerUpgrade(ctx context.Context, req *connect.Request[apiv1.CheckRunnerUpgradeRequest]) (*connect.Response[apiv1.CheckRunnerUpgradeResponse], error) {
	n, err := a.s.CheckRunnerUpgrade(ctx, req.Msg.Runner, rpcActor(ctx))
	if err != nil {
		return nil, upgradeErr(err)
	}
	return connect.NewResponse(&apiv1.CheckRunnerUpgradeResponse{Count: clampInt32(n)}), nil
}
