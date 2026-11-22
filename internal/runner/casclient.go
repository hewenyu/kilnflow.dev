package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/actionkey"
	"github.com/hewenyu/kilnflow.dev/internal/bytesize"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
)

// 与 Server 的 CAS 之间的对象传输（PLAN #10.2）。Runner 的本地缓存与 Server 的 v0 存储格式相同，
// 都是 cas.FSStore：写入时校验 digest 并原子地改名。

const (
	// transferBatchSize 是一次 PrepareUpload / PrepareDownload 请求中的对象数上限。
	transferBatchSize = 500
	// transferAttempts 是上传或下载单个对象的重试次数。
	transferAttempts = 4
)

// casStore 返回 Runner 的本地 CAS 缓存（<dir>/cas）。
func (r *Runner) casStore() *cas.FSStore {
	return &cas.FSStore{Root: r.cfg.path(casDirName)}
}

// uploadBlobs 上传 Server 缺失的对象（不记录进度，用于 manifest 等小对象）。local 是 digest → 本地文件。
func (j *job) uploadBlobs(ctx context.Context, local map[string]string) error {
	return j.upload(ctx, local, nil)
}

// uploadOutputs 上传产物：记录进度（心跳上报），并按节流间隔写进度日志。
// labels 是 digest → 日志中显示的名称（输出名称/工作区中的相对路径，不含本地绝对路径）。
func (j *job) uploadOutputs(ctx context.Context, local, labels map[string]string) error {
	return j.upload(ctx, local, labels)
}

// upload 上传 Server 缺失的对象；labels 不为 nil 时记录进度和写进度日志。
func (j *job) upload(ctx context.Context, local, labels map[string]string) error {
	if len(local) == 0 {
		return nil
	}
	track := labels != nil
	blobs := make([]*runnerv1.BlobRef, 0, len(local))
	size := map[string]int64{}
	for d, p := range local {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		blobs = append(blobs, &runnerv1.BlobRef{Digest: d, Size: st.Size()})
		size[d] = st.Size()
	}
	slices.SortFunc(blobs, func(a, b *runnerv1.BlobRef) int { return strings.Compare(a.Digest, b.Digest) })
	uploaded := 0
	var stopLog func()
	defer func() {
		if stopLog != nil {
			stopLog()
		}
	}()
	for batch := range slices.Chunk(blobs, transferBatchSize) {
		resp, err := runnerkit.Retry(ctx, rpcAttempts, func() (*runnerv1.PrepareUploadResponse, error) {
			return runnerkit.Call(ctx, j.r.c, j.r.c.RPC().PrepareUpload, &runnerv1.PrepareUploadRequest{Ref: j.spec.Ref, Blobs: batch})
		})
		if err != nil {
			return err
		}
		for _, u := range resp.Uploads {
			if _, ok := local[u.Digest]; !ok {
				return fmt.Errorf("收到未请求的对象 %s 的上传地址", u.Digest)
			}
		}
		if track {
			j.planUpload(batch, resp.Uploads, size)
			if len(resp.Uploads) > 0 && stopLog == nil {
				stopLog = j.logUploadProgress()
			}
		}
		for _, u := range resp.Uploads {
			if err := j.putObject(ctx, u, local[u.Digest], size[u.Digest], labels); err != nil {
				if track && ctx.Err() != nil {
					j.log.Printf("产物上传已中止（job 已取消或超时）")
				}
				return err
			}
			uploaded++
		}
	}
	if track && uploaded > 0 {
		stopLog()
		stopLog = nil
		j.log.Printf("%s", j.prog.uploadDone())
	}
	return nil
}

// planUpload 登记一批对象中需要上传的部分，并写日志说明要上传多少、Server 已有多少。
func (j *job) planUpload(batch []*runnerv1.BlobRef, uploads []*runnerv1.UploadTarget, size map[string]int64) {
	var all, need int64
	for _, b := range batch {
		all += b.Size
	}
	for _, u := range uploads {
		need += size[u.Digest]
	}
	skipped := len(batch) - len(uploads)
	j.prog.planUpload(len(batch), all, skipped, all-need)
	switch {
	case len(uploads) == 0:
		j.log.Printf("Server 已有全部 %d 个文件（%s），无需上传", len(batch), bytesize.Format(all))
	case skipped > 0:
		j.log.Printf("上传产物到 Kiln Server：%d 个文件，%s（另有 %d 个文件 Server 已有，跳过）", len(uploads), bytesize.Format(need), skipped)
	default:
		j.log.Printf("上传产物到 Kiln Server：%d 个文件，%s", len(uploads), bytesize.Format(need))
	}
}

// putObject 上传一个对象（失败时重试）：收到 Server 的成功响应（已校验 digest）才算完成。
func (j *job) putObject(ctx context.Context, u *runnerv1.UploadTarget, file string, size int64, labels map[string]string) error {
	if labels == nil {
		_, err := runnerkit.Retry(ctx, transferAttempts, func() (struct{}, error) { return struct{}{}, casPut(ctx, j.r.c, u.Url, file, nil) })
		if err != nil {
			return fmt.Errorf("上传 %s：%w", u.Digest, err)
		}
		return nil
	}
	label := labels[u.Digest]
	if label == "" {
		label = shorten.Hash(u.Digest)
	}
	attempt := 0
	_, err := runnerkit.Retry(ctx, transferAttempts, func() (struct{}, error) {
		attempt++
		j.prog.startObject(label, size)
		err := casPut(ctx, j.r.c, u.Url, file, j.prog.sent)
		if err != nil && ctx.Err() == nil {
			msg := j.log.MaskString(err.Error())
			j.prog.failed(msg)
			j.log.Printf("上传 %s 失败（第 %d/%d 次）：%s", label, attempt, transferAttempts, msg)
		}
		return struct{}{}, err
	})
	if err != nil {
		return fmt.Errorf("上传 %s：%w", label, err)
	}
	j.prog.confirmed()
	return nil
}

// logUploadProgress 每 progressLogInterval 写一行上传进度，返回停止函数（等待写日志的 goroutine 退出）。
func (j *job) logUploadProgress() (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(progressLogInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				j.log.Printf("%s", j.prog.uploadLine())
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

// putJSON 把对象规范化为 JCS、写入本地缓存并上传，返回 digest。
func (j *job) putJSON(ctx context.Context, v any) (string, error) {
	b, err := actionkey.Canonical(v)
	if err != nil {
		return "", err
	}
	d := cas.Digest(b)
	store := j.r.casStore()
	if _, err := store.Put(ctx, d, bytes.NewReader(b)); err != nil {
		return "", err
	}
	p, err := store.Path(d)
	if err != nil {
		return "", err
	}
	return d, j.uploadBlobs(ctx, map[string]string{d: p})
}

// ensureLocal 确保这些对象已在本地 CAS 缓存中（下载时由 FSStore.Put 校验 sha256）。
func (j *job) ensureLocal(ctx context.Context, digests []string) error {
	store := j.r.casStore()
	var missing []string
	seen := map[string]bool{}
	for _, d := range digests {
		if seen[d] {
			continue
		}
		seen[d] = true
		has, err := store.Has(ctx, d)
		if err != nil {
			return err
		}
		if !has {
			missing = append(missing, d)
			continue
		}
		if p, err := store.Path(d); err == nil {
			runnerkit.Touch(p) // 记录最后使用的时间（gc.go）
		}
	}
	for batch := range slices.Chunk(missing, transferBatchSize) {
		resp, err := runnerkit.Retry(ctx, rpcAttempts, func() (*runnerv1.PrepareDownloadResponse, error) {
			return runnerkit.Call(ctx, j.r.c, j.r.c.RPC().PrepareDownload, &runnerv1.PrepareDownloadRequest{Ref: j.spec.Ref, Digests: batch})
		})
		if err != nil {
			return err
		}
		for _, dl := range resp.Downloads {
			if _, err := runnerkit.Retry(ctx, transferAttempts, func() (struct{}, error) { return struct{}{}, j.download(ctx, store, dl.Digest, dl.Url) }); err != nil {
				return err
			}
		}
	}
	return nil
}

func (j *job) download(ctx context.Context, store *cas.FSStore, digest, u string) error {
	body, err := casGet(ctx, j.r.c, u)
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = store.Put(ctx, digest, body)
	return err
}
