package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/api/v1"
	"github.com/hewenyu/kilnflow.dev/internal/docprovider"
	"github.com/hewenyu/kilnflow.dev/internal/server/store"
)

// 外部设计文档绑定：把仓库关联到第三方文档集（首期 Outline）。API token 与 secret / npm 一样用信封加密，
// 只写不读；AI 开发与审计在规划任务时由 docsync 冻结快照，执行节点经 kiln-orca task doc 读取，不需要外部文档凭据。

// maxDocToken 是 Outline API token 的长度上限。
const maxDocToken = 4096

func (a *rpcAPI) ListDocBindings(ctx context.Context, _ *connect.Request[apiv1.ListDocBindingsRequest]) (*connect.Response[apiv1.ListDocBindingsResponse], error) {
	list, err := a.s.ListDocBindings(ctx)
	if err != nil {
		return nil, rpcError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.ListDocBindingsResponse{Bindings: list}), nil
}

func (a *rpcAPI) BindDocCollection(ctx context.Context, req *connect.Request[apiv1.BindDocCollectionRequest]) (*connect.Response[apiv1.BindDocCollectionResponse], error) {
	m := req.Msg
	b, err := a.s.BindDocCollection(ctx, m.Repo, m.BaseUrl, m.CollectionId, m.CollectionName, m.Token, rpcActor(ctx))
	if err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.BindDocCollectionResponse{Binding: b}), nil
}

func (a *rpcAPI) UnbindDocCollection(ctx context.Context, req *connect.Request[apiv1.UnbindDocCollectionRequest]) (*connect.Response[apiv1.UnbindDocCollectionResponse], error) {
	if err := a.s.UnbindDocCollection(ctx, req.Msg.Repo, rpcActor(ctx)); err != nil {
		return nil, rpcError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.UnbindDocCollectionResponse{}), nil
}

// ListDocBindings 返回全部仓库的文档绑定（不含 token）。
func (s *Service) ListDocBindings(ctx context.Context) ([]*apiv1.DocBinding, error) {
	rows, err := s.db.Query(ctx, `SELECT r.owner||'/'||r.name, b.provider, b.base_url, b.collection_id, b.collection_name,
			b.updated_by, b.updated_at
		FROM repo_doc_bindings b JOIN repos r ON r.id=b.repo_id ORDER BY lower(r.owner), lower(r.name)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*apiv1.DocBinding, error) {
		var b apiv1.DocBinding
		var updatedBy *string
		var updatedAt time.Time
		if err := row.Scan(&b.Repo, &b.Provider, &b.BaseUrl, &b.CollectionId, &b.CollectionName, &updatedBy, &updatedAt); err != nil {
			return nil, err
		}
		b.UpdatedBy = deref(updatedBy)
		b.UpdatedAt = updatedAt.Unix()
		return &b, nil
	})
}

// BindDocCollection 加密保存（新建或覆盖）仓库的文档绑定。token 只写入密文，不进入审计日志和错误信息。
func (s *Service) BindDocCollection(ctx context.Context, fullName, baseURL, collectionID, collectionName, token, actor string) (*apiv1.DocBinding, error) {
	baseURL = store.NormalizeDocBaseURL(baseURL)
	if err := checkDocBaseURL(baseURL); err != nil {
		return nil, err
	}
	collectionID = strings.TrimSpace(collectionID)
	if collectionID == "" {
		return nil, errors.New("文档集 id 为空")
	}
	token = strings.TrimSpace(token)
	switch {
	case token == "":
		return nil, errors.New("outline API token 为空")
	case len(token) > maxDocToken:
		return nil, errors.New("outline API token 太长")
	}
	repo, err := store.FindRepo(ctx, s.db, fullName)
	if err != nil {
		return nil, err
	}
	var out *apiv1.DocBinding
	// 绑定变更按仓库串行化：同一仓库的 bind / unbind 不交错。
	err = s.db.WithAdvisoryLock(ctx, store.DocBindingLockKey(repo.ID), func(ctx context.Context, conn *pgxpool.Conn) error {
		return store.TxOn(ctx, conn, func(tx pgx.Tx) error {
			// 只读绑定与「待决策清单」不能落在同一个集合：一份允许 Kiln 写、一份不允许，混在一起无法分清。
			if dd, err := store.GetDecisionDoc(ctx, tx, repo.ID); err != nil {
				return err
			} else if dd != nil && dd.Provider == "outline" && store.NormalizeDocBaseURL(dd.BaseURL) == baseURL &&
				dd.CollectionID == collectionID {
				return errors.New("这个集合已被待决策清单使用，请为只读文档绑定选择独立的集合")
			}
			if err := store.PutDocBinding(ctx, tx, s.cfg.MasterKey, store.DocBinding{
				RepoID: repo.ID, Provider: "outline", BaseURL: baseURL,
				CollectionID: collectionID, CollectionName: strings.TrimSpace(collectionName),
				CreatedBy: actor, UpdatedBy: &actor,
			}, token); err != nil {
				return err
			}
			if err := store.RecordConfigChange(ctx, tx, repo.OwnerID, actor, "doc.bind", map[string]any{
				"repo": repo.FullName(), "base_url": baseURL, "collection_id": collectionID,
			}); err != nil {
				return err
			}
			b, err := store.GetDocBinding(ctx, tx, repo.ID)
			if err != nil {
				return err
			}
			out = docBindingMsg(repo.FullName(), b)
			return nil
		})
	})
	return out, err
}

// UnbindDocCollection 删除仓库的文档绑定（没有绑定时报错）。
func (s *Service) UnbindDocCollection(ctx context.Context, fullName, actor string) error {
	repo, err := store.FindRepo(ctx, s.db, fullName)
	if err != nil {
		return err
	}
	return s.db.WithAdvisoryLock(ctx, store.DocBindingLockKey(repo.ID), func(ctx context.Context, conn *pgxpool.Conn) error {
		return store.TxOn(ctx, conn, func(tx pgx.Tx) error {
			ok, err := store.DeleteDocBinding(ctx, tx, repo.ID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s 没有文档绑定", repo.FullName())
			}
			return store.RecordConfigChange(ctx, tx, repo.OwnerID, actor, "doc.unbind", map[string]any{"repo": repo.FullName()})
		})
	})
}

// checkDocBaseURL 校验文档系统地址，规则与 Outline 客户端一致（docprovider.ValidateBaseURL）。
func checkDocBaseURL(baseURL string) error {
	return docprovider.ValidateBaseURL(baseURL)
}

func docBindingMsg(fullName string, b *store.DocBinding) *apiv1.DocBinding {
	return &apiv1.DocBinding{
		Repo: fullName, Provider: b.Provider, BaseUrl: b.BaseURL, CollectionId: b.CollectionID,
		CollectionName: b.CollectionName, UpdatedBy: deref(b.UpdatedBy), UpdatedAt: b.UpdatedAt.Unix(),
	}
}
