package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	gh "github.com/hewenyu/kilnflow.dev/internal/github"
)

// 接收 webhook（保存到 events 表）和事件处理循环。

// maxWebhookBody 是 webhook 请求体的上限（GitHub 的 payload 最大 25 MiB）。
const maxWebhookBody = 25 << 20

// 事件处理的重试策略：第 n 次失败后等待 eventBackoffBase·2^n（5·2^n 秒），失败 eventMaxTries 次后放弃。
// GitHub 限流不算失败：推迟到恢复时间，不计入次数（deferRateLimited）。
const (
	eventMaxTries       = 8
	eventBatch          = 20
	eventProcessTimeout = 5 * time.Minute
	eventBackoffBase    = 5 * time.Second
	// eventStateTimeout 是记录事件结果的时限：不随处理循环退出取消，但必须有期限。
	eventStateTimeout = 15 * time.Second
	// eventPollInterval 是兜底轮询的间隔（保存 webhook 后由 Hub 立即唤醒）。
	eventPollInterval = 5 * time.Second
)

// HandleWebhook 校验并保存 webhook（POST /webhook），由 ingest 循环异步处理。
func (i *Ingester) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if i.gh == nil {
		http.Error(w, errNoGitHubApp.Error(), http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		// 超过上限时明确返回 413：截断的请求体签名也校验不过，报 401 会让发送方以为是凭据问题
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			http.Error(w, "请求体超过上限", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "读取请求失败", http.StatusBadRequest)
		return
	}
	if !gh.VerifySignature([]byte(i.cfg.WebhookSecret), body, r.Header.Get(gh.HeaderSignature)) {
		http.Error(w, "签名无效", http.StatusUnauthorized)
		return
	}
	delivery := r.Header.Get(gh.HeaderDelivery)
	event := r.Header.Get(gh.HeaderEvent)
	if delivery == "" || event == "" {
		http.Error(w, "缺少 "+gh.HeaderDelivery+" 或 "+gh.HeaderEvent, http.StatusBadRequest)
		return
	}
	var head struct {
		Action       string `json:"action"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	// head 只用于填写 events 的索引列；结构不符的 JSON 照样保存，由 processEvent 丢弃（不是 JSON 的请求体写不进 jsonb 列，返回 500；GitHub 的 webhook 总是 JSON）
	_ = json.Unmarshal(body, &head)
	_, err = i.db.Exec(r.Context(), `INSERT INTO events (delivery_id, event_type, action, installation_id, payload) VALUES ($1,$2,nullif($3,''),$4,$5)
		ON CONFLICT (delivery_id) DO NOTHING`, delivery, event, head.Action, head.Installation.ID, body)
	if err != nil {
		i.log.Error("保存 webhook", "err", err)
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}
	i.hub.KickIngest()
	w.WriteHeader(http.StatusAccepted)
}

// Run 循环处理 events 表中待处理的事件（保存 webhook 后立即唤醒，另有 eventPollInterval 的兜底轮询），直到 ctx 结束。
func (i *Ingester) Run(ctx context.Context) {
	t := time.NewTicker(eventPollInterval)
	defer t.Stop()
	for {
		for i.ingestBatch(ctx) > 0 {
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-i.hub.Ingest():
		}
	}
}

// webhookEvent 是 events 表中待处理的一条记录。
type webhookEvent struct {
	ID, Type, Action string
	Payload          []byte
	Attempts         int
}

func (i *Ingester) ingestBatch(ctx context.Context) int {
	rows, err := i.db.Query(ctx, `SELECT delivery_id, event_type, coalesce(action,''), payload, attempts FROM events
		WHERE processed_at IS NULL AND next_attempt_at <= now() ORDER BY received_at LIMIT $1`, eventBatch)
	if err != nil {
		if ctx.Err() == nil {
			i.log.Error("读取 events", "err", err)
		}
		return 0
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[webhookEvent])
	if err != nil {
		if ctx.Err() == nil {
			i.log.Error("读取 events", "err", err)
		}
		return 0
	}
	for _, e := range list {
		if ctx.Err() != nil {
			return 0
		}
		i.ingestOne(ctx, e)
	}
	return len(list)
}

// ingestOne 处理一个事件并记录结果；失败时按退避策略安排重试。记录结果的写入用不随投递循环退出取消的 context：
// 事件已经有结果时必须落盘，否则下一轮启动时状态与实际不一致。
func (i *Ingester) ingestOne(ctx context.Context, e webhookEvent) {
	pctx, cancel := context.WithTimeout(ctx, eventProcessTimeout)
	result, err := i.processEvent(pctx, e.ID, e.Type, e.Action, e.Payload)
	cancel()
	// 结果写入的 context 在处理返回之后才创建：处理本身最多 eventProcessTimeout，若提前创建，
	// 慢处理会把 eventStateTimeout 耗光，结果写不进去，下一轮只能重放。
	sctx, cancelState := context.WithTimeout(context.WithoutCancel(ctx), i.stateTimeout)
	defer cancelState()
	if err == nil {
		if _, err := i.db.Exec(sctx, `UPDATE events SET processed_at=now(), result=$2, error=NULL WHERE delivery_id=$1`, e.ID, result); err != nil {
			i.log.Warn("记录事件结果", "delivery", e.ID, "err", err)
		}
		if result != "" {
			i.log.Info("事件已处理", "delivery", e.ID, "event", e.Type, "action", e.Action, "result", result)
		}
		return
	}
	if ctx.Err() != nil {
		// 服务退出、调用方取消：处理可能只做了一半。不计入重试次数（不是事件处理失败），按当前次数退避后重试。
		i.recordInterrupted(sctx, e, err)
		return
	}
	if rl, ok := gh.RateLimited(err); ok {
		i.deferRateLimited(sctx, e, rl)
		return
	}
	tries := e.Attempts + 1
	i.log.Warn("处理事件失败", "delivery", e.ID, "event", e.Type, "tries", tries, "err", err)
	var uerr error
	if tries >= eventMaxTries {
		_, uerr = i.db.Exec(sctx, `UPDATE events SET processed_at=now(), attempts=$2, error=$3, result='failed' WHERE delivery_id=$1`,
			e.ID, tries, err.Error())
	} else {
		backoff := eventBackoffBase << tries
		_, uerr = i.db.Exec(sctx, `UPDATE events SET attempts=$2, error=$3, next_attempt_at=now()+make_interval(secs => $4) WHERE delivery_id=$1`,
			e.ID, tries, err.Error(), backoff.Seconds())
	}
	if uerr != nil {
		i.log.Warn("记录事件失败", "delivery", e.ID, "err", uerr)
	}
}

// recordInterrupted 记录一次因服务退出 / 取消而中断的处理：不计入重试次数，按当前次数退避后重试。
func (i *Ingester) recordInterrupted(ctx context.Context, e webhookEvent, err error) {
	backoff := eventBackoffBase << e.Attempts
	if _, uerr := i.db.Exec(ctx, `UPDATE events SET error=$2, next_attempt_at=now()+make_interval(secs => $3) WHERE delivery_id=$1`,
		e.ID, "处理被中断："+err.Error(), backoff.Seconds()); uerr != nil {
		i.log.Warn("记录事件失败", "delivery", e.ID, "err", uerr)
	}
}

// deferRateLimited 把因 GitHub 限流没有处理完的事件推迟到恢复时间：不计入重试次数（限流再久也不会放弃事件），
// 恢复后照常处理，由 dedupe_key 保证只生成一次 run，重跑请求按 delivery 去重（planner.RerunRequest.DeliveryID）。原因记录在 events.error 中。
// 限流中的 installation 不会再发出请求（gh.Client 直接返回 *gh.RateLimitError），同一 installation 的其他事件也随之推迟。
func (i *Ingester) deferRateLimited(ctx context.Context, e webhookEvent, rl *gh.RateLimitError) {
	if rl.API != nil {
		i.log.Warn("处理事件被 GitHub 限流，等待恢复后重试", "delivery", e.ID, "event", e.Type, "installation", rl.Installation,
			"kind", rl.Kind, "until", rl.Until, "err", rl.API)
	} else {
		i.log.Info("事件推迟到 GitHub 限流恢复", "delivery", e.ID, "event", e.Type, "installation", rl.Installation, "until", rl.Until)
	}
	if _, err := i.db.Exec(ctx, `UPDATE events SET error=$2, next_attempt_at=greatest($3::timestamptz, now()) WHERE delivery_id=$1`,
		e.ID, "等待 GitHub 限流恢复："+rl.Error(), rl.Until); err != nil {
		i.log.Warn("记录事件失败", "delivery", e.ID, "err", err)
	}
}
