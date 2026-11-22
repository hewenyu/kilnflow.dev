package workflow

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/hewenyu/kilnflow.dev/internal/release"
)

// goldenSpecJSON 是 testSpec 序列化后的 JSON（为便于阅读做了缩进，比较前压缩）。
// jobs.spec 中已经保存了这种格式的数据，键名改了旧数据就读不回来；Pos 只用于诊断，不保存。
const goldenSpecJSON = `{
  "workflow": "release",
  "job_id": "publish",
  "platform": "server",
  "executor": "server",
  "inputs": [],
  "inputs_default": false,
  "network": false,
  "reproducible": false,
  "cache": false,
  "effects": false,
  "cpu": 0,
  "memory_bytes": 0,
  "env": {},
  "timeout_minutes": 0,
  "fail_fast": false,
  "release": {
    "VerifyReproducible": true,
    "GitHub": {
      "Repository": "o/public",
      "Target": "main",
      "AllowSourceName": true,
      "Assets": [
        {
          "From": "build.bin",
          "Archive": "tar.gz",
          "Name": "app-${{ matrix.target }}"
        }
      ],
      "Checksums": "SHA256SUMS",
      "Sign": "cosign",
      "Provenance": "slsa",
      "SBOM": false,
      "NotesFrom": "CHANGELOG.md",
      "Prerelease": "auto",
      "Latest": "true"
    },
    "Npm": [
      {
        "From": "build.bin",
        "Name": "app-${{ matrix.os }}",
        "Files": [
          "app",
          "lib/**"
        ],
        "Package": {
          "os": [
            "${{ matrix.os }}"
          ]
        },
        "Access": "public",
        "DistTag": "next",
        "AllowSourceName": true
      }
    ],
    "Images": [
      {
        "From": "image.image",
        "Name": "ghcr.io/o/app",
        "Username": "u",
        "Password": "${{ secrets.TOKEN }}",
        "Sign": "none",
        "Tags": [
          "v1.0.0"
        ],
        "FloatingTags": [
          "latest"
        ]
      }
    ],
    "Runtimes": [
      {
        "From": "rt.dist",
        "Name": "app",
        "Version": "1.0.0",
        "Platform": "linux/arm64",
        "Env": {
          "PATH": "bin"
        }
      }
    ],
    "Assets": [
      {
        "From": "build.dmg",
        "Archive": "none",
        "Name": "app-${{ github.ref_name }}"
      }
    ],
    "Checksums": "SUMS",
    "Uploads": [
      {
        "Name": "site",
        "URL": "https://example.com/api/releases",
        "Token": "${{ secrets.SITE_TOKEN }}",
        "Latest": "auto",
        "Prerelease": "false"
      }
    ]
  },
  "image": {
    "Base": "docker.io/library/debian@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "Platforms": [
      "linux/arm64",
      "linux/amd64"
    ],
    "Files": [
      {
        "From": "build.bin",
        "Select": {
          "goarch": "arm64"
        },
        "To": "/usr/bin/"
      }
    ],
    "Entrypoint": [
      "/usr/bin/app"
    ],
    "Cmd": [
      "serve"
    ],
    "Env": {
      "Keys": [
        "B",
        "A"
      ],
      "Values": {
        "A": "1",
        "B": "2"
      }
    },
    "Workdir": "/srv",
    "User": "nobody",
    "Labels": {
      "Keys": [
        "org"
      ],
      "Values": {
        "org": "o"
      }
    }
  }
}`

func testSpec(withPos bool) *Spec {
	sbom := false
	sp := &Spec{
		Workflow: "release",
		JobID:    "publish",
		Platform: PlatformServer,
		Executor: ExecutorServer,
		Inputs:   []string{},
		Env:      map[string]string{},
		Release: &Release{
			VerifyReproducible: true,
			GitHub: &ReleaseGitHub{
				Repository:      "o/public",
				Target:          "main",
				AllowSourceName: true,
				Assets: []Asset{
					{From: "build.bin", Archive: release.ArchiveTarGz, Name: "app-${{ matrix.target }}"},
				},
				Checksums:  "SHA256SUMS",
				Sign:       SignCosign,
				Provenance: "slsa",
				SBOM:       &sbom,
				NotesFrom:  "CHANGELOG.md",
				Prerelease: "auto",
				Latest:     "true",
			},
			Npm: []NpmTarget{{
				From: "build.bin", Name: "app-${{ matrix.os }}", Files: []string{"app", "lib/**"},
				Package: map[string]any{"os": []any{"${{ matrix.os }}"}}, Access: NpmAccessPublic, DistTag: "next", AllowSourceName: true,
			}},
			Images: []ImageTarget{{
				From: "image.image", Name: "ghcr.io/o/app", Username: "u",
				Password: "${{ secrets.TOKEN }}", Sign: SignNone,
				Tags: []string{"v1.0.0"}, FloatingTags: []string{"latest"},
			}},
			Runtimes: []RuntimeTarget{{
				From: "rt.dist", Name: "app", Version: "1.0.0", Platform: "linux/arm64",
				Env: map[string]any{"PATH": "bin"},
			}},
			Assets:    []Asset{{From: "build.dmg", Archive: release.ArchiveNone, Name: "app-${{ github.ref_name }}"}},
			Checksums: "SUMS",
			Uploads: []UploadTarget{{
				Name: "site", URL: "https://example.com/api/releases", Token: "${{ secrets.SITE_TOKEN }}",
				Latest: ChoiceAuto, Prerelease: ChoiceFalse,
			}},
		},
		Image: &ImageJob{
			Base:       "docker.io/library/debian@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Platforms:  []string{"linux/arm64", "linux/amd64"},
			Files:      []ImageFile{{From: "build.bin", Select: map[string]string{"goarch": "arm64"}, To: "/usr/bin/"}},
			Entrypoint: []string{"/usr/bin/app"},
			Cmd:        []string{"serve"},
			Env:        &OrderedMap{Keys: []string{"B", "A"}, Values: map[string]string{"A": "1", "B": "2"}},
			Workdir:    "/srv",
			User:       "nobody",
			Labels:     &OrderedMap{Keys: []string{"org"}, Values: map[string]string{"org": "o"}},
		},
	}
	if withPos {
		sp.Release.GitHub.Assets[0].Pos = Pos{Line: 3, Col: 7}
		sp.Release.Images[0].Pos = Pos{Line: 9, Col: 5}
		sp.Release.Assets[0].Pos = Pos{Line: 12, Col: 7}
		sp.Release.Uploads[0].Pos = Pos{Line: 15, Col: 7}
		sp.Release.Npm[0].Pos = Pos{Line: 18, Col: 7}
	}
	return sp
}

func TestSpecJSONGolden(t *testing.T) {
	var want bytes.Buffer
	if err := json.Compact(&want, []byte(goldenSpecJSON)); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(testSpec(true))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("Spec 的 JSON 变了（jobs.spec 中的旧数据会读不回来）：\n got  %s\n want %s", got, want.Bytes())
	}
	var back Spec
	if err := json.Unmarshal(want.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if want := testSpec(false); !reflect.DeepEqual(&back, want) {
		t.Fatalf("反序列化结果不一致：\n got  %+v\n want %+v", back, *want)
	}
}

func TestRuntimePlatforms(t *testing.T) {
	job := func(platform string, runtimes ...string) *PlannedJob {
		return &PlannedJob{Spec: &Spec{Platform: platform, Runtimes: runtimes}}
	}
	plans := []*Plan{
		{Jobs: []*PlannedJob{
			job("linux/arm64", "go", "node"),
			job("darwin/arm64", "go"),
			job("linux/arm64", "go"),
		}},
		{Jobs: []*PlannedJob{
			job("linux/amd64", "go"),
			job("${{ matrix.os }}", "bun"), // 平台不合法：runtime 出现，但没有平台
			job(PlatformServer),
			{JobID: "no-spec"},
		}},
	}
	got := RuntimePlatforms(plans)
	want := map[string][]string{
		"go":   {"darwin/arm64", "linux/amd64", "linux/arm64"},
		"node": {"linux/arm64"},
		"bun":  {},
	}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := RuntimePlatforms(nil); len(got) != 0 {
		t.Fatalf("没有 plan 时应为空，实际 %v", got)
	}
}
