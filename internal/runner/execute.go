package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"time"

	runnerv1 "github.com/hewenyu/kilnflow.dev/gen/go/kiln/runner/v1"
	"github.com/hewenyu/kilnflow.dev/internal/bytesize"
	"github.com/hewenyu/kilnflow.dev/internal/cas"
	"github.com/hewenyu/kilnflow.dev/internal/runnerkit"
	"github.com/hewenyu/kilnflow.dev/internal/shorten"
	"github.com/hewenyu/kilnflow.dev/internal/toolchain"
	"github.com/hewenyu/kilnflow.dev/internal/workflow"
)

// execution 是一次执行过程中各阶段共享的状态。
type execution struct {
	j       *job
	dirs    *attemptDirs
	ex      executor
	secrets map[string]string // job 引用的 secret
	env     map[string]string // 全部 step 共用的环境变量
	kc      *keychain         // signing 的临时 keychain（没有 signing 时为 nil）
	git     *gitChannel       // step 中 git 的凭据通道（gitchannel.go）
}

// execute 执行 job 的全部阶段，返回结果。
func (j *job) execute(ctx context.Context) (res *jobResult) {
	dirs, err := newAttemptDirs(j.r.workDir(j.spec.Ref.AttemptId))
	if err != nil {
		return resultFromErr(infra(classPrepare, err))
	}
	writeAttemptMeta(dirs.root, attemptMeta{RepoID: j.spec.GetSource().GetRepoId(), JobID: j.spec.JobId})
	e := &execution{j: j, dirs: dirs}
	defer func() { e.cleanup(ctx, res) }()

	sp := j.spec
	j.log.Printf("%s · %s · run #%d · job #%d · attempt #%d · Runner %s", sp.Workflow, sp.DisplayName, sp.RunId, sp.JobId, sp.Ref.AttemptId, j.r.state.Name)
	execDesc := sp.GetExecutor().GetKind()
	if img := sp.GetExecutor().GetImage(); img != "" {
		execDesc += " · " + img
	}
	j.log.Printf("平台 %s · executor %s", sp.GetExecutor().GetPlatform(), execDesc)

	manifest, err := e.run(ctx)
	if err != nil {
		return resultFromErr(err)
	}
	return &jobResult{outcome: runnerv1.Outcome_OUTCOME_SUCCEEDED, manifest: manifest}
}

// run 依次执行各阶段，返回结果 manifest 的 digest。
func (e *execution) run(ctx context.Context) (string, error) {
	if err := e.fetchSecrets(ctx); err != nil {
		return "", err
	}
	if err := e.fetchSource(ctx); err != nil {
		return "", err
	}
	if err := e.setupExecutor(ctx); err != nil {
		return "", err
	}
	mounted, err := e.installRuntimes(ctx)
	if err != nil {
		return "", err
	}
	if err := e.placeFetches(ctx); err != nil {
		return "", err
	}
	if err := e.buildEnv(mounted); err != nil {
		return "", err
	}
	if err := e.setupGit(ctx); err != nil {
		return "", err
	}
	if err := e.setupSigning(ctx); err != nil {
		return "", err
	}
	e.j.prog.set(runnerv1.Phase_PHASE_PREPARE, "启动执行环境")
	if err := e.ex.prepare(ctx, e.j.log); err != nil {
		return "", infra(classPrepare, err)
	}
	if err := e.runSteps(ctx); err != nil {
		return "", err
	}
	return e.collectOutputs(ctx)
}

// cleanup 销毁执行环境并删除工作区（失败时按配置保留现场）。
func (e *execution) cleanup(ctx context.Context, res *jobResult) {
	j := e.j
	j.prog.set(runnerv1.Phase_PHASE_CLEANUP, "清理执行环境和工作区")
	// 先关闭 git 凭据通道：不再接受新的传输，吊销全部未关闭的操作（step 留下的后台 git 之后拿不到凭据）
	if e.git != nil {
		e.git.close()
	}
	if e.ex != nil {
		e.ex.destroy(ctx)
	}
	if o, ok := e.ex.(*ociExec); ok {
		j.r.recordImages(o.imagesUsed())
	}
	// 临时 keychain 总是删除，保留失败现场时也不保留
	if e.kc != nil {
		cctx, cancel := cleanupContext(ctx)
		e.kc.cleanup(cctx)
		cancel()
	}
	failed := res == nil || res.outcome != runnerv1.Outcome_OUTCOME_SUCCEEDED
	if (j.spec.KeepOnFailure && failed) || j.r.cfg.KeepWorkspaces {
		j.log.Printf("保留工作区：%s（%s）", e.dirs.root, j.r.state.Name)
		return
	}
	err := runnerkit.RemoveTree(e.dirs.root)
	if err != nil {
		// 容器中以 root 身份写入的文件，宿主机用户可能删不掉：先用 job 的镜像（已经在本地）删除，
		// 不行再用清理容器（remove.go）
		if rc, ok := e.ex.(rootCleaner); ok {
			rc.cleanupRoot(ctx)
			err = runnerkit.RemoveTree(e.dirs.root)
		}
		if err != nil {
			cctx, cancel := cleanupContext(ctx)
			rm := newRemover(j.r.cfg, j.r.docker(), j.r.state.RunnerID)
			_, err = rm.remove(cctx, e.dirs.root)
			cancel()
			if rm.used {
				j.r.recordImages([]string{rm.image})
			}
		}
	}
	if err != nil {
		j.r.log.Warn("删除工作区失败", "dir", e.dirs.root, "err", err)
	}
}

// fetchSecrets 获取 job 引用的 secret，并登记为需要脱敏的值（阶段 1）。这是执行阶段的第一个 RPC，也是版本探测：旧 Server
// 不认识它（Unimplemented）时 job 以 infra 失败，不退回到 FetchCredentials（PLAN #8.4）。
// 源码 token 不在这里：按操作签发（gitchannel.go）。
func (e *execution) fetchSecrets(ctx context.Context) error {
	j := e.j
	j.prog.set(runnerv1.Phase_PHASE_PREPARE, "获取 secret")
	resp, err := runnerkit.Retry(ctx, rpcAttempts, func() (*runnerv1.FetchSecretsResponse, error) {
		return runnerkit.Call(ctx, j.r.c, j.r.c.RPC().FetchSecrets, &runnerv1.FetchSecretsRequest{Ref: j.spec.Ref})
	})
	if err != nil {
		return infra(classCredentials, oldServerError(err))
	}
	for _, v := range resp.Secrets {
		j.log.AddSecret(v)
	}
	e.secrets = resp.Secrets
	return nil
}

// fetchSource 拉取源码并检出到工作区（阶段 2）。
func (e *execution) fetchSource(ctx context.Context) error {
	src := e.j.spec.Source
	if src == nil || src.Sha == "" {
		return nil
	}
	e.j.prog.set(runnerv1.Phase_PHASE_PREPARE, "拉取源码 "+src.Repository+"@"+shorten.Hash(src.Sha))
	t := time.Now()
	if err := e.j.r.fetchSource(ctx, src, e.j, e.dirs.ws, e.j.log); err != nil {
		return infra(classFetch, err)
	}
	e.j.log.Printf("源码 %s@%s 已检出（%s）", src.Repository, shorten.Hash(src.Sha), time.Since(t).Round(time.Millisecond))
	return nil
}

// setupExecutor 创建 executor；oci 时同时准备镜像（阶段 3）。
func (e *execution) setupExecutor(ctx context.Context) error {
	sp := e.j.spec
	cfg := e.j.r.cfg
	e.j.prog.set(runnerv1.Phase_PHASE_PREPARE, "准备执行环境（"+sp.GetExecutor().GetKind()+"）")
	switch kind := workflow.ExecutorKind(sp.GetExecutor().GetKind()); kind {
	case workflow.ExecutorHost:
		e.ex = &hostExec{d: e.dirs, extraPath: cfg.ExtraPath, network: sp.Network, proxy: cfg.Proxy.Host}
		e.addProxySecrets(cfg.Proxy.Host)
	case workflow.ExecutorOCI:
		o := newOCIExec(cfg.Docker, e.dirs, sp, e.j.r.state.RunnerID)
		o.proxy = cfg.Proxy.OCI
		e.addProxySecrets(cfg.Proxy.OCI)
		// 先登记，镜像准备失败时 destroy 也能清理
		e.ex = o
		if err := o.prepareImage(ctx, e.j.log); err != nil {
			return infra(classPrepare, err)
		}
		if bk := sp.Executor.GetBuildkitImage(); bk != "" {
			if !cfg.Buildkit {
				return infra(classPrepare, errors.New("job 需要 buildkit，但这个 Runner 没有开启（config.toml 中 buildkit = true）"))
			}
			if err := o.prepareBuildkitClient(ctx, bk, cfg.path(buildkitDirName), e.j.log); err != nil {
				return infra(classPrepare, err)
			}
		}
		// 拉取后立即记录：Runner 在 job 执行中被强行结束时，镜像仍然有记录，之后会被清理
		e.j.r.recordImages(o.imagesUsed())
	default:
		return infra(classPrepare, fmt.Errorf("不支持的 executor %q", kind))
	}
	return nil
}

// addProxySecrets 让日志隐藏 job 使用的代理凭证（只有 network: true 的 job 会注入代理）。
func (e *execution) addProxySecrets(p ProxySettings) {
	if !e.j.spec.Network {
		return
	}
	for _, v := range p.secrets() {
		e.j.log.AddSecret(v)
	}
}

// installRuntimes 安装 job 使用的 runtime，返回挂载信息（阶段 4）。
func (e *execution) installRuntimes(ctx context.Context) ([]toolchain.Mounted, error) {
	mounted := make([]toolchain.Mounted, 0, len(e.j.spec.Runtimes))
	for _, rt := range e.j.spec.Runtimes {
		e.j.prog.set(runnerv1.Phase_PHASE_PREPARE, "准备 runtime "+rt.Name+" "+rt.Version)
		t := time.Now()
		ref := toolchain.Ref{Name: rt.Name, Version: rt.Version, Platform: rt.Platform, Digest: rt.Digest}
		in, err := toolchain.Install(ctx, e.j.r.cfg.path(rtDirName), ref)
		if err != nil {
			return nil, infra(classRuntime, err)
		}
		runnerkit.Touch(in.Dir) // 记录最后使用的时间（gc.go）
		mounted = append(mounted, toolchain.Mounted{Mount: e.ex.rtMount(rt.Name, in.Dir), Meta: in.Meta})
		e.j.log.Printf("runtime %s %s（%s）：%s", rt.Name, rt.Version, rt.Platform, toolchain.SourceText(in.Reused, time.Since(t)))
	}
	return mounted, nil
}

// placeFetches 把上游 job 的输出放到工作区（阶段 5）。
func (e *execution) placeFetches(ctx context.Context) error {
	for _, f := range e.j.spec.Fetch {
		e.j.prog.set(runnerv1.Phase_PHASE_PREPARE, "下载上游输出 "+f.From)
		n, err := e.j.placeFetch(ctx, f, e.dirs.ws)
		if err != nil {
			return infra(classFetch, fmt.Errorf("fetch %s：%w", f.From, err))
		}
		e.j.log.Printf("fetch %s → %s（%d 个文件）", f.From, f.To, n)
	}
	return nil
}

// subst 替换工作区占位符和 secret 引用。用单遍 Replacer：替换进来的 secret 值不会被再次扫描
// （否则值中恰好含有另一个引用时会被二次替换），按 key 排序保证结果确定。
func (e *execution) subst(s string) string {
	if !strings.Contains(s, "${{") {
		return s
	}
	pairs := []string{workflow.WorkspacePlaceholder, e.ex.workspace()}
	for _, k := range slices.Sorted(maps.Keys(e.secrets)) {
		pairs = append(pairs, "${{ secrets."+k+" }}", e.secrets[k])
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// buildEnv 计算全部 step 共用的环境变量（阶段 6）。
func (e *execution) buildEnv(mounted []toolchain.Mounted) error {
	sp := e.j.spec
	ex := e.ex
	env := map[string]string{
		"HOME": ex.home(), "TMPDIR": ex.tmp(), "TZ": "UTC", "CI": "true", "KILN": "true",
		"KILN_WORKSPACE": ex.workspace(), "SOURCE_DATE_EPOCH": sourceDateEpoch, "PATH": ex.basePath(),
		"LANG": "C.UTF-8",
	}
	ex.jobEnv(env)
	if err := toolchain.ApplyEnv(env, mounted); err != nil {
		return infra(classRuntime, err)
	}
	for _, tc := range sp.Toolcache {
		// 纵深防御：Server 已经校验过 workflow，但 Runner 不应信任收到的任务描述，
		// 否则 name 中的 .. 可以逃出缓存根目录并挂载宿主机任意目录，env 可以覆写关键变量。
		if !workflow.ValidToolCacheName(tc.Name) {
			return userErrorf("toolcache 的 name %q 无效", tc.Name)
		}
		if !workflow.ValidToolCacheEnv(tc.Env) {
			return userErrorf("toolcache 的 env %q 无效或会覆盖保留的环境变量", tc.Env)
		}
		dir := e.j.r.cfg.path(toolcacheDirName, ex.cacheDomain(), strconv.FormatInt(sp.Source.GetRepoId(), 10), platformDir(sp.Executor.Platform), tc.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return infra(classPrepare, err)
		}
		runnerkit.Touch(dir) // 记录最后使用的时间（gc.go）
		env[tc.Env] = ex.toolcacheMount(tc.Name, dir)
	}
	for k, v := range sp.Env {
		env[k] = e.subst(v)
	}
	syncProxyCase(env, sp.Env)
	ex.lockEnv(env)
	e.env = env
	return nil
}

// setupGit 打开 job 的 git 凭据通道，让 executor 准备 step 中 git 的设置（阶段 6，gitchannel.go、gitenv.go）：step 中补取
// 本仓库的历史、拉取 repositories 中的私有依赖时，每次 git 传输按需签发这个仓库的只读 token，传输结束吊销。
func (e *execution) setupGit(ctx context.Context) error {
	sp := e.j.spec
	scope := newGitScope(sp)
	p := gitPlan{scope: scope.url(), exe: e.j.r.exe, required: len(sp.Repositories) > 0}
	ch, err := openGitChannel(ctx, e.j.r.gitSocketParent, scope, e.j, e.j.log)
	switch {
	case err != nil && p.required:
		return infra(classPrepare, fmt.Errorf("%w：%w", errGitUnavailable, err))
	case err != nil:
		// 没有声明依赖的 job 照常运行：git 要凭据时 helper 只说明原因，不退回到写文件
		p.unavailable = err.Error()
	default:
		e.git = ch
		p.sockDir, p.sock = ch.dir, ch.sock
	}
	if err := e.ex.useGit(ctx, p, e.j.log); err != nil {
		return infra(classPrepare, err)
	}
	return nil
}

// setupSigning 为写了 signing 的 job 建立临时 keychain（阶段 6，PLAN #8.10）。
func (e *execution) setupSigning(ctx context.Context) error {
	s := e.j.spec.Signing
	if s == nil {
		return nil
	}
	if _, ok := e.ex.(*hostExec); !ok {
		return userErrorf("signing 只能用于在 macOS 宿主机上执行的 job")
	}
	// Xcode 在用户真正的主目录中找 provisioning profile，trustd 读这里的搜索列表，都不认 HOME
	u, err := user.Current()
	if err != nil {
		return infra(classPrepare, err)
	}
	home := e.j.r.userHome
	if home == "" {
		home = u.HomeDir
	}
	e.j.prog.set(runnerv1.Phase_PHASE_PREPARE, "准备签名")
	t := time.Now()
	kc, err := setupSigning(ctx, signingParams{
		spec: s, subst: e.subst, dirs: e.dirs, profileDirs: userProfileDirs(home), userHome: u.HomeDir,
		developerDir: e.env["DEVELOPER_DIR"], log: e.j.log,
	})
	e.kc = kc
	if err != nil {
		return err
	}
	kc.addEnv(e.env)
	e.j.log.Printf("signing 准备完成（%s）", time.Since(t).Round(time.Millisecond))
	return nil
}

// runSteps 依次执行 step（阶段 8；阶段 7 是 executor.prepare）。返回 nil 表示全部成功
// （或失败的 step 设置了 continue-on-error）；step 失败返回 *userError，取消返回 errCancelled。
func (e *execution) runSteps(ctx context.Context) error {
	sp := e.j.spec
	log := e.j.log
	for i, st := range sp.Steps {
		if ctx.Err() != nil {
			return errCancelled
		}
		e.j.step.Store(runnerkit.ClampInt32(i))
		name := st.Name
		if name == "" {
			name = runnerkit.FirstLine(st.Run)
		}
		e.j.prog.set(runnerv1.Phase_PHASE_STEP, fmt.Sprintf("[%d/%d] %s", i+1, len(sp.Steps), name))
		fmt.Fprintf(log, "\x1b[1;34m▶ [%d/%d] %s\x1b[0m\n", i+1, len(sp.Steps), name)
		env := maps.Clone(e.env)
		for k, v := range st.Env {
			env[k] = e.subst(v)
		}
		// step 的 env 同样不能恢复 network: false 的 job 的外网访问
		syncProxyCase(env, st.Env)
		e.ex.lockEnv(env)
		// 外层 kiln-orca task exec / 凭据通道的私有变量不能继承到 step 中：不管这个执行环境有没有可用的
		// git 凭据通道，都先清掉；有通道时由 a.apply 重新注入本次合法的值
		clearGitWrapperEnv(env)
		// git 的设置在 workflow 的 env 之后：环境变量配置追加在 workflow 自己的之后，askpass 等环境变量以这里的为准（step 的
		// 脚本自己再改环境、用 git -c 的不管，这不是安全边界）
		if a := e.ex.gitAccess(ctx, env, st.WorkingDirectory, log); a != nil {
			a.apply(env)
		}
		t := time.Now()
		code, err := e.ex.run(ctx, stepSpec{index: i, script: e.subst(st.Run), shell: st.Shell, workDir: st.WorkingDirectory, env: env}, log)
		if err != nil {
			if ctx.Err() != nil {
				return errCancelled
			}
			if ue, ok := errors.AsType[*userError](err); ok {
				log.Printf("step %q：%s", name, ue.msg)
				return userErrorf("step %q：%s", name, ue.msg)
			}
			return infra(classExec, fmt.Errorf("step %q：%w", name, err))
		}
		if code != 0 {
			if st.ContinueOnError {
				log.Printf("step %q 退出码 %d（continue-on-error，继续）", name, code)
				continue
			}
			log.Printf("step %q 失败，退出码 %d（%s）", name, code, time.Since(t).Round(time.Millisecond))
			return &userError{msg: fmt.Sprintf("step %q 退出码 %d", name, code), exitCode: code}
		}
		log.Printf("用时 %s", time.Since(t).Round(time.Millisecond))
	}
	return nil
}

// collectOutputs 收集并上传各命名输出，返回结果 manifest 的 digest（阶段 9）。先收集全部输出，
// 再一起上传（同一内容只传一次），最后上传各输出的 manifest 和结果 manifest。
func (e *execution) collectOutputs(ctx context.Context) (string, error) {
	j := e.j
	result := cas.ResultManifest{Schema: cas.ResultSchema, Outputs: map[string]string{}}
	outs := slices.SortedFunc(slices.Values(j.spec.Outputs), func(a, b *runnerv1.OutputSpec) int {
		return strings.Compare(a.Name, b.Name)
	})
	if len(outs) > 0 {
		j.prog.set(runnerv1.Phase_PHASE_UPLOAD, "收集产物")
		j.log.Printf("构建完成，正在收集产物（%d 个输出），之后上传到 Kiln Server", len(outs))
	} else {
		j.prog.set(runnerv1.Phase_PHASE_UPLOAD, "上传结果清单")
	}
	manifests := make([]cas.OutputManifest, 0, len(outs))
	local := map[string]string{}
	labels := map[string]string{}
	for _, o := range outs {
		c, err := collectOutput(e.dirs.ws, o.Path)
		if err != nil {
			if ue, ok := errors.AsType[*userError](err); ok {
				return "", userErrorf("输出 %s：%s", o.Name, ue.msg)
			}
			return "", infra(classUpload, err)
		}
		var size int64
		for _, f := range c.Manifest.Files {
			size += f.Size
			if _, ok := labels[f.Digest]; !ok && f.Digest != "" {
				labels[f.Digest] = o.Name + "/" + f.Path
			}
		}
		maps.Copy(local, c.Local)
		manifests = append(manifests, c.Manifest)
		j.log.Printf("输出 %s：%d 个文件，%s", o.Name, len(c.Manifest.Files), bytesize.Format(size))
	}
	if len(local) > 0 {
		j.prog.set(runnerv1.Phase_PHASE_UPLOAD, "上传产物到 Kiln Server")
		if err := j.uploadOutputs(ctx, local, labels); err != nil {
			return "", infra(classUpload, err)
		}
	}
	for i, o := range outs {
		d, err := j.putJSON(ctx, manifests[i])
		if err != nil {
			return "", infra(classUpload, err)
		}
		result.Outputs[o.Name] = d
	}
	d, err := j.putJSON(ctx, result)
	if err != nil {
		return "", infra(classUpload, err)
	}
	return d, nil
}
