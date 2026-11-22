package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrTagConflict 表示不可变 tag 已经指向其他 digest（PLAN #13.8）。PendingTags 返回的是 *TagConflictError。
var ErrTagConflict = errors.New("不可变 tag 已指向其他 digest")

// TagConflictError 是不可变 tag 已经指向其他 digest 的错误，errors.Is(err, ErrTagConflict) 为 true。
type TagConflictError struct {
	// Tag 是冲突的 tag，Have 是它已经指向的 digest，Want 是本次构建的 digest。
	Tag, Have, Want string
}

func (e *TagConflictError) Error() string {
	return fmt.Sprintf("tag %s 已指向 %s，本次构建是 %s：%v", e.Tag, e.Have, e.Want, ErrTagConflict)
}

// Is 让 errors.Is(err, ErrTagConflict) 成立。
func (e *TagConflictError) Is(target error) bool { return target == ErrTagConflict }

// RootManifest 返回 Root 对应的 manifest（推送 tag 时上传它）。
func (img *Image) RootManifest() Manifest {
	for _, m := range img.Manifests {
		if m.Digest == img.Root.Digest {
			return m
		}
	}
	return Manifest{Descriptor: img.Root}
}

// PendingTags 是阶段 A 对不可变 tag 的检查：不存在 → 需要设置；已指向 root → 已完成，跳过；
// 已指向其他 digest → 返回 ErrTagConflict。
func PendingTags(ctx context.Context, c *Client, tags []string, root string) ([]string, error) {
	var pending []string
	for _, t := range tags {
		d, found, err := c.ManifestDigest(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("查询 tag %s：%w", t, err)
		}
		switch {
		case !found:
			pending = append(pending, t)
		case d != root:
			return nil, &TagConflictError{Tag: t, Have: d, Want: root}
		}
	}
	return pending, nil
}

// PushStats 是一次推送的统计。
type PushStats struct {
	Uploaded, Skipped int
	Bytes             int64
}

// Push 按 digest 推送镜像的全部内容，不打任何 tag（PLAN #13.4 阶段 A）：
// 已存在的 blob 跳过，缺失的用 open 读出后上传；然后按顺序推送 manifest 和 index（Root 最后）。
func Push(ctx context.Context, c *Client, img *Image, open Opener) (PushStats, error) {
	var st PushStats
	for _, b := range img.Blobs {
		ok, err := c.BlobExists(ctx, b.Digest)
		if err != nil {
			return st, fmt.Errorf("查询 blob %s：%w", b.Digest, err)
		}
		if ok {
			st.Skipped++
			continue
		}
		openBlob := func(ctx context.Context) (io.ReadCloser, error) { return open(ctx, b.Digest) }
		if err := c.PushBlob(ctx, b, openBlob); err != nil {
			return st, fmt.Errorf("上传 blob %s：%w", b.Digest, err)
		}
		st.Uploaded++
		st.Bytes += b.Size
	}
	for _, m := range img.Manifests {
		if err := c.PushManifest(ctx, m, m.Digest); err != nil {
			return st, fmt.Errorf("推送 manifest %s：%w", m.Digest, err)
		}
	}
	return st, nil
}

// SetTag 把 tag 指向镜像的 Root（PLAN #13.4 阶段 B）。重复设置是幂等的。
func SetTag(ctx context.Context, c *Client, img *Image, tag string) error {
	if err := c.PushManifest(ctx, img.RootManifest(), tag); err != nil {
		return fmt.Errorf("设置 tag %s：%w", tag, err)
	}
	return nil
}
