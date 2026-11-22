package workflow

import (
	"slices"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"

	"github.com/hewenyu/kilnflow.dev/internal/expr"
	"github.com/hewenyu/kilnflow.dev/internal/oci"
	"github.com/hewenyu/kilnflow.dev/internal/release"
	"github.com/hewenyu/kilnflow.dev/internal/upload"
)

func (d *decoder) parseRelease(v *yaml.Node) *Release {
	r := &Release{}
	d.fields(v, "release", handlers{
		"verify-reproducible": func(_, v *yaml.Node) { r.VerifyReproducible = d.boolv(v) },
		"assets":              func(_, v *yaml.Node) { r.Assets = parseSeq(d, v, "release.assets", d.parseAsset) },
		"checksums":           func(_, v *yaml.Node) { r.Checksums = d.str(v) },
		"github":              func(_, v *yaml.Node) { r.GitHub = d.parseReleaseGitHub(v) },
		"uploads":             func(_, v *yaml.Node) { r.Uploads = parseSeq(d, v, "release.uploads", d.parseUploadTarget) },
		"npm":                 func(_, v *yaml.Node) { r.Npm = parseSeq(d, v, "release.npm", d.parseNpmTarget) },
		"images":              func(_, v *yaml.Node) { r.Images = parseSeq(d, v, "release.images", d.parseImageTarget) },
		"runtimes":            func(_, v *yaml.Node) { r.Runtimes = parseSeq(d, v, "release.runtimes", d.parseRuntimeTarget) },
	}, nil)
	d.normalizeAssets(v, r)
	return r
}

// normalizeAssets 把旧写法 release.github.assets / checksums 移到 release.assets / checksums（两种写法等价，
// 只能写一处），并检查 asset 和文件类发布目标（github、uploads）是否配套（PLAN #13.4）。
func (d *decoder) normalizeAssets(v *yaml.Node, r *Release) {
	if g := r.GitHub; g != nil {
		switch {
		case len(g.Assets) == 0:
		case len(r.Assets) > 0:
			d.errAt(v, "release.assets 和 release.github.assets 只能写一处（推荐 release.assets，供 github、uploads 共用）")
		default:
			r.Assets, g.Assets = g.Assets, nil
		}
		switch {
		case g.Checksums == "":
		case r.Checksums != "":
			d.errAt(v, "release.checksums 和 release.github.checksums 只能写一处")
		default:
			r.Checksums, g.Checksums = g.Checksums, ""
		}
	}
	switch {
	case len(r.Assets) > 0 && !r.HasFileTarget():
		d.errAt(v, "release.assets 没有使用者：还需要 github 或 uploads")
	case len(r.Uploads) > 0 && len(r.Assets) == 0:
		d.errAt(v, "release.uploads 需要 release.assets（要上传的文件）")
	}
	seen := map[string]bool{}
	for _, u := range r.Uploads {
		if u.URL != "" && seen[u.URL] {
			d.diags = append(d.diags, Diag{File: d.file, Line: u.Pos.Line, Col: u.Pos.Col, Msg: "release.uploads 中 url " + u.URL + " 重复"})
		}
		seen[u.URL] = true
	}
}

func (d *decoder) parseUploadTarget(c *yaml.Node) UploadTarget {
	t := UploadTarget{Pos: pos(c)}
	d.fields(c, "uploads[]", handlers{
		"name":       func(_, v *yaml.Node) { t.Name = d.str(v) },
		"url":        func(_, v *yaml.Node) { t.URL = d.str(v) },
		"token":      func(_, v *yaml.Node) { t.Token = d.str(v) },
		"latest":     func(_, v *yaml.Node) { t.Latest = d.str(v) },
		"prerelease": func(_, v *yaml.Node) { t.Prerelease = d.str(v) },
	}, nil)
	for _, f := range [][2]string{{"url", t.URL}, {"token", t.Token}} {
		if f[1] == "" {
			d.errAt(c, "uploads[] 缺少 %s", f[0])
		}
	}
	// 含表达式的在规划时求值后检查
	if t.URL != "" && !expr.HasExpr(t.URL) {
		if err := upload.CheckURL(t.URL); err != nil {
			d.errAt(c, "uploads[].url：%v", err)
		}
	}
	for _, f := range [][2]string{{"latest", t.Latest}, {"prerelease", t.Prerelease}} {
		switch f[1] {
		case "", ChoiceAuto, ChoiceTrue, ChoiceFalse:
		default:
			d.errAt(c, "uploads[].%s 只能是 auto、true、false", f[0])
		}
	}
	return t
}

func (d *decoder) parseReleaseGitHub(v *yaml.Node) *ReleaseGitHub {
	g := &ReleaseGitHub{}
	d.fields(v, "release.github", handlers{
		"repository":        func(_, v *yaml.Node) { g.Repository = d.str(v) },
		"target":            func(_, v *yaml.Node) { g.Target = d.str(v) },
		"allow-source-name": func(_, v *yaml.Node) { g.AllowSourceName = d.boolv(v) },
		"checksums":         func(_, v *yaml.Node) { g.Checksums = d.str(v) },
		"sign":              func(_, v *yaml.Node) { g.Sign = d.str(v) },
		"provenance":        func(_, v *yaml.Node) { g.Provenance = d.str(v) },
		"sbom":              func(_, v *yaml.Node) { g.SBOM = d.boolp(v) },
		"prerelease":        func(_, v *yaml.Node) { g.Prerelease = d.str(v) },
		"latest":            func(_, v *yaml.Node) { g.Latest = d.str(v) },
		"notes": func(_, v *yaml.Node) {
			d.fields(v, "release.github.notes", handlers{
				"from": func(_, v *yaml.Node) { g.NotesFrom = d.str(v) },
			}, nil)
		},
		"assets": func(_, v *yaml.Node) { g.Assets = parseSeq(d, v, "release.github.assets", d.parseAsset) },
	}, nil)
	return g
}

func (d *decoder) parseAsset(c *yaml.Node) Asset {
	a := Asset{Archive: release.ArchiveTarGz, Pos: pos(c)}
	d.fields(c, "assets[]", handlers{
		"from":    func(_, v *yaml.Node) { a.From = d.str(v) },
		"archive": func(_, v *yaml.Node) { a.Archive = d.str(v) },
		"name":    func(_, v *yaml.Node) { a.Name = d.str(v) },
	}, nil)
	switch a.Archive {
	case release.ArchiveTarGz, release.ArchiveZip, release.ArchiveNone:
	default:
		d.errAt(c, "archive 只能是 tar.gz、zip、none")
	}
	if a.From == "" {
		d.errAt(c, "asset 缺少 from")
	}
	return a
}

func (d *decoder) parseNpmTarget(c *yaml.Node) NpmTarget {
	t := NpmTarget{Pos: pos(c)}
	credential := func(k, _ *yaml.Node) {
		d.errAt(k, "npm[] 不能写 %s：npm 的 registry、scope 和 token 在 Kiln 中按账号配置（kiln npm set）", k.Value)
	}
	d.fields(c, "npm[]", handlers{
		"from":  func(_, v *yaml.Node) { t.From = d.str(v) },
		"name":  func(_, v *yaml.Node) { t.Name = d.str(v) },
		"files": func(_, v *yaml.Node) { t.Files = d.strList(v) },
		"package": func(_, v *yaml.Node) {
			m, ok := toValue(v).(map[string]any)
			if !ok {
				d.errAt(v, "npm[].package 应为映射（合并进 package.json 的字段）")
				return
			}
			t.Package = m
		},
		"access":            func(_, v *yaml.Node) { t.Access = d.str(v) },
		"dist-tag":          func(_, v *yaml.Node) { t.DistTag = d.str(v) },
		"allow-source-name": func(_, v *yaml.Node) { t.AllowSourceName = d.boolv(v) },
		"registry":          credential,
		"token":             credential,
	}, nil)
	for _, f := range [][2]string{{"from", t.From}, {"name", t.Name}, {"access", t.Access}} {
		if f[1] == "" {
			d.errAt(c, "npm[] 缺少 %s", f[0])
		}
	}
	// 含表达式的在发布时按上游变体求值后检查
	if t.Name != "" && !expr.HasExpr(t.Name) {
		if err := CheckNpmName(t.Name); err != nil {
			d.errAt(c, "npm[].name：%v", err)
		}
	}
	switch t.Access {
	case "", NpmAccessPublic, NpmAccessRestricted:
	default:
		d.errAt(c, "npm[].access 只能是 public 或 restricted")
	}
	if t.DistTag != "" && t.DistTag != NpmDistTagAuto && !expr.HasExpr(t.DistTag) {
		if err := CheckNpmDistTag(t.DistTag); err != nil {
			d.errAt(c, "npm[].dist-tag：%v", err)
		}
	}
	for _, f := range t.Files {
		if !doublestar.ValidatePattern(f) {
			d.errAt(c, "npm[].files 中的 %q 不是合法的 glob", f)
		}
	}
	for _, k := range []string{"name", "version"} {
		if _, ok := t.Package[k]; ok {
			d.errAt(c, "npm[].package 不能写 %s（由 Kiln 按 npm[].name、tag 写入）", k)
		}
	}
	return t
}

func (d *decoder) parseImageTarget(c *yaml.Node) ImageTarget {
	t := ImageTarget{Pos: pos(c)}
	d.fields(c, "images[]", handlers{
		"from":          func(_, v *yaml.Node) { t.From = d.str(v) },
		"name":          func(_, v *yaml.Node) { t.Name = d.str(v) },
		"tags":          func(_, v *yaml.Node) { t.Tags = d.strList(v) },
		"floating-tags": func(_, v *yaml.Node) { t.FloatingTags = d.strList(v) },
		"username":      func(_, v *yaml.Node) { t.Username = d.str(v) },
		"password":      func(_, v *yaml.Node) { t.Password = d.str(v) },
		"sign":          func(_, v *yaml.Node) { t.Sign = d.str(v) },
	}, nil)
	for _, f := range [][2]string{{"from", t.From}, {"name", t.Name}, {"username", t.Username}, {"password", t.Password}} {
		if f[1] == "" {
			d.errAt(c, "images[] 缺少 %s", f[0])
		}
	}
	if len(t.Tags) == 0 {
		d.errAt(c, "images[] 缺少 tags（不可变 tag，例如 [\"${{ github.ref_name }}\"]）")
	}
	// 含表达式的在发布时校验
	if t.Name != "" && !expr.HasExpr(t.Name) {
		if _, err := oci.ParseName(t.Name); err != nil {
			d.errAt(c, "images[].name：%v", err)
		}
	}
	for _, tag := range append(slices.Clone(t.Tags), t.FloatingTags...) {
		if !expr.HasExpr(tag) {
			if err := oci.CheckTag(tag); err != nil {
				d.errAt(c, "images[]：%v", err)
			}
		}
	}
	switch t.Sign {
	case "", SignNone, SignCosign:
	default:
		d.errAt(c, "images[].sign 只能是 cosign 或 none")
	}
	return t
}

func (d *decoder) parseRuntimeTarget(c *yaml.Node) RuntimeTarget {
	var t RuntimeTarget
	d.fields(c, "runtimes[]", handlers{
		"from":     func(_, v *yaml.Node) { t.From = d.str(v) },
		"name":     func(_, v *yaml.Node) { t.Name = d.str(v) },
		"version":  func(_, v *yaml.Node) { t.Version = d.str(v) },
		"platform": func(_, v *yaml.Node) { t.Platform = d.str(v) },
		"env": func(_, v *yaml.Node) {
			if m, ok := toValue(v).(map[string]any); ok {
				t.Env = m
			}
		},
	}, nil)
	return t
}

func (d *decoder) parseImage(v *yaml.Node) *ImageJob {
	im := &ImageJob{Env: NewOrderedMap(), Labels: NewOrderedMap()}
	d.fields(v, "image", handlers{
		"base":       func(_, v *yaml.Node) { im.Base = d.str(v) },
		"platforms":  func(_, v *yaml.Node) { im.Platforms = d.strList(v) },
		"entrypoint": func(_, v *yaml.Node) { im.Entrypoint = d.strList(v) },
		"cmd":        func(_, v *yaml.Node) { im.Cmd = d.strList(v) },
		"env":        func(_, v *yaml.Node) { im.Env = d.strMap(v, "env") },
		"workdir":    func(_, v *yaml.Node) { im.Workdir = d.str(v) },
		"user":       func(_, v *yaml.Node) { im.User = d.str(v) },
		"labels":     func(_, v *yaml.Node) { im.Labels = d.strMap(v, "labels") },
		"files":      func(_, v *yaml.Node) { im.Files = parseSeq(d, v, "image.files", d.parseImageFile) },
	}, nil)
	return im
}

func (d *decoder) parseImageFile(c *yaml.Node) ImageFile {
	var f ImageFile
	d.fields(c, "files[]", handlers{
		"from":   func(_, v *yaml.Node) { f.From = d.str(v) },
		"to":     func(_, v *yaml.Node) { f.To = d.str(v) },
		"select": func(_, v *yaml.Node) { f.Select = d.strMap(v, "select").Values },
	}, nil)
	return f
}
