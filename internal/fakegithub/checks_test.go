package fakegithub

import (
	"encoding/json"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/github"
)

// Check Run 的创建、部分更新和按名称、external_id 查找。
func TestCheckRuns(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	build, err := f.inst.CreateCheckRun(ctx, demo, &github.CheckRun{Name: "ci / build", HeadSHA: "sha1", ExternalID: "job-1", DetailsURL: "https://kiln/1"})
	if err != nil {
		t.Fatal(err)
	}
	if build.ID == 0 || build.Status != "queued" {
		t.Errorf("没有指定 status 时应为 queued：%+v", build)
	}
	test, err := f.inst.CreateCheckRun(ctx, demo, &github.CheckRun{Name: "ci / test", HeadSHA: "sha1", ExternalID: "job-2", Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.inst.CreateCheckRun(ctx, demo, &github.CheckRun{Name: "ci / build", HeadSHA: "sha2", ExternalID: "job-3"}); err != nil {
		t.Fatal(err)
	}
	if test.ID <= build.ID {
		t.Errorf("ID 应递增：%d %d", build.ID, test.ID)
	}

	// 只更新给出的字段；output 原样保存
	out := &github.CheckRunOutput{Title: "失败", Summary: "1 个错误"}
	if err := f.inst.UpdateCheckRun(ctx, demo, build.ID, &github.CheckRun{Status: "completed", Conclusion: "failure", Output: out}); err != nil {
		t.Fatal(err)
	}
	if err := f.inst.UpdateCheckRun(ctx, demo, build.ID, &github.CheckRun{Name: "ci / build (renamed)"}); err != nil {
		t.Fatal(err)
	}
	runs := f.s.CheckRuns("sha1")
	if len(runs) != 2 || runs[0].ID != build.ID || runs[1].ID != test.ID {
		t.Fatalf("CheckRuns(sha1) 应按 ID 排序返回 2 个：%+v", runs)
	}
	got := runs[0]
	var gotOut github.CheckRunOutput
	if err := json.Unmarshal(got.Output, &gotOut); err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || got.Conclusion != "failure" || got.Name != "ci / build (renamed)" || got.UpdateCount != 2 ||
		got.ExternalID != "job-1" || got.DetailsURL != "https://kiln/1" || got.Repo != demo || gotOut.Title != "失败" {
		t.Errorf("更新后的 Check Run：%+v output=%+v", got, gotOut)
	}
	if err := f.inst.UpdateCheckRun(ctx, demo, 999999, &github.CheckRun{Status: "completed"}); !github.IsNotFound(err) {
		t.Errorf("不存在的 Check Run 应返回 404：%v", err)
	}

	// 按名称过滤后再按 external_id 匹配
	found, err := f.inst.FindCheckRun(ctx, demo, "sha1", "ci / test", "job-2", f.s.AppID)
	if err != nil || found == nil || found.ID != test.ID {
		t.Errorf("FindCheckRun = %+v %v", found, err)
	}
	if found, err := f.inst.FindCheckRun(ctx, demo, "sha1", "ci / build", "job-2", f.s.AppID); err != nil || found != nil {
		t.Errorf("名称不同时不应找到：%+v %v", found, err)
	}
	if found, err := f.inst.FindCheckRun(ctx, demo, "sha3", "ci / build", "job-1", 0); err != nil || found != nil {
		t.Errorf("其他 sha 上不应找到：%+v %v", found, err)
	}
	if runs := f.s.CheckRuns("none"); len(runs) != 0 {
		t.Errorf("没有 Check Run 的 sha：%+v", runs)
	}
}
