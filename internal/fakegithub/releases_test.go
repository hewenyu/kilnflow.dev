package fakegithub

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/github"
)

// Release 的创建、读取、更新和列出。
func TestReleases(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	yes, no := true, false

	rel, err := f.inst.CreateRelease(ctx, demo, &github.Release{
		TagName: "v1", TargetCommitish: "main", Name: "v1", Body: "notes", Draft: &yes, Prerelease: &no, GenerateNotes: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rel.ID == 0 || rel.Draft == nil || !*rel.Draft || rel.HTMLURL != "https://github.com/tester/demo/releases/tag/v1" ||
		!strings.HasPrefix(rel.Body, "notes\n\n**Full Changelog**") {
		t.Errorf("CreateRelease = %+v", rel)
	}
	other, err := f.inst.CreateRelease(ctx, "tester/other", &github.Release{TagName: "v9"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Draft == nil || *other.Draft {
		t.Error("没有指定 draft 时应为 false")
	}

	// 更新：发布 draft、设置 make_latest 和 target，其他字段不变
	updated, err := f.inst.UpdateRelease(ctx, demo, rel.ID, &github.Release{Draft: &no, MakeLatest: "true", TargetCommitish: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if *updated.Draft || updated.MakeLatest != "true" || updated.TargetCommitish != "abc" || updated.Name != "v1" {
		t.Errorf("UpdateRelease = %+v", updated)
	}
	if _, err := f.inst.UpdateRelease(ctx, demo, 999999, &github.Release{Draft: &no}); !github.IsNotFound(err) {
		t.Errorf("更新不存在的 Release 应返回 404：%v", err)
	}
	got, err := f.inst.GetRelease(ctx, demo, rel.ID)
	if err != nil || *got.Draft || got.MakeLatest != "true" {
		t.Errorf("GetRelease = %+v %v", got, err)
	}
	if _, err := f.inst.GetRelease(ctx, demo, 999999); !github.IsNotFound(err) {
		t.Errorf("不存在的 Release 应返回 404：%v", err)
	}

	// 列表按仓库区分（不区分大小写）
	list, err := f.inst.ListReleases(ctx, demo)
	if err != nil || len(list) != 1 || list[0].ID != rel.ID {
		t.Errorf("ListReleases = %+v %v", list, err)
	}
	if rels := f.s.Releases("Tester/Other"); len(rels) != 1 || rels[0].ID != other.ID {
		t.Errorf("Releases(Tester/Other) = %+v", rels)
	}
}

// asset 的上传、列出、读取内容和删除。
func TestReleaseAssets(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	rel, err := f.inst.CreateRelease(ctx, demo, &github.Release{TagName: "v1"})
	if err != nil {
		t.Fatal(err)
	}

	// 上传两个 asset，digest 是内容的 sha256
	for _, a := range []struct{ name, data string }{{"a.tgz", "aaa"}, {"b.tgz", "bbbb"}} {
		got, err := f.inst.UploadReleaseAsset(ctx, demo, rel.ID, a.name, strings.NewReader(a.data), int64(len(a.data)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(a.data))
		if got.Name != a.name || got.Size != int64(len(a.data)) || got.Digest != "sha256:"+hex.EncodeToString(sum[:]) || got.State != "uploaded" {
			t.Errorf("上传 %s：%+v", a.name, got)
		}
	}
	if _, err := f.inst.UploadReleaseAsset(ctx, demo, 999999, "x", strings.NewReader("x"), 1); !github.IsNotFound(err) {
		t.Errorf("上传到不存在的 Release 应返回 404：%v", err)
	}
	assets, err := f.inst.ListReleaseAssets(ctx, demo, rel.ID)
	if err != nil || len(assets) != 2 || assets[0].Name != "a.tgz" {
		t.Fatalf("ListReleaseAssets = %+v %v", assets, err)
	}
	if string(f.s.AssetData(rel.ID, "b.tgz")) != "bbbb" || f.s.AssetData(rel.ID, "c.tgz") != nil || f.s.AssetData(999999, "a.tgz") != nil {
		t.Error("AssetData 返回的内容不对")
	}
	if _, err := f.inst.ListReleaseAssets(ctx, demo, 999999); !github.IsNotFound(err) {
		t.Errorf("不存在的 Release 应返回 404：%v", err)
	}

	if got, err := f.inst.GetRelease(ctx, demo, rel.ID); err != nil || len(got.Assets) != 2 {
		t.Errorf("GetRelease 应带上 asset：%+v %v", got, err)
	}

	// 删除 asset
	if err := f.inst.DeleteReleaseAsset(ctx, demo, assets[0].ID); err != nil {
		t.Fatal(err)
	}
	if rels := f.s.Releases(demo); len(rels[0].Assets) != 1 || rels[0].Assets[0].Name != "b.tgz" {
		t.Errorf("删除后的 asset：%+v", rels[0].Assets)
	}
}

// Releases 返回的是副本：修改它不影响模拟 GitHub 的状态。
func TestReleasesReturnsCopy(t *testing.T) {
	f := newFixture(t)
	rel, err := f.inst.CreateRelease(t.Context(), demo, &github.Release{TagName: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.inst.UploadReleaseAsset(t.Context(), demo, rel.ID, "a", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	rels := f.s.Releases(demo)
	rels[0].Name = "changed"
	rels[0].Assets[0].Name = "changed"
	if again := f.s.Releases(demo); again[0].Name == "changed" || again[0].Assets[0].Name != "a" {
		t.Errorf("修改副本影响了状态：%+v", again[0])
	}
	data := f.s.AssetData(rel.ID, "a")
	data[0] = 'y'
	if string(f.s.AssetData(rel.ID, "a")) != "x" {
		t.Error("修改 AssetData 的结果影响了状态")
	}
}
