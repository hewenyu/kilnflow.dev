package workflow

import (
	"regexp"
	"slices"
	"strings"
)

// EnvNameRe 是 shell 和容器都能接受的环境变量名。Runner 生成 export 脚本、OCI 挂载配置时共用这一条规则。
var EnvNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// toolCacheEnvBlocked 是 toolcache 不允许覆写的环境变量：Runner 自身的运行环境（PATH、HOME、临时目录、动态库
// 预加载等），以及 Kiln 和 git 保留的前缀。缓存目录的内容由 job 写入，不该通过这类变量影响 Runner 或后续任务。
var toolCacheEnvBlocked = []string{
	"PATH", "HOME", "TMPDIR", "SHELL", "IFS", "ENV", "BASH_ENV", "LD_PRELOAD", "LD_LIBRARY_PATH",
}

// toolCacheEnvBlockedPrefixes 是禁止覆写的前缀。
var toolCacheEnvBlockedPrefixes = []string{"DYLD_", "GIT_", "KILN_", "LD_"}

// ValidToolCacheName 判断 toolcache 的 name 是否是安全的单段标识符。name 是宿主机缓存目录的一段路径，也被拼进
// OCI 容器中的挂载路径，因此只允许安全的单段标识符：禁止空值、"."、".."、路径分隔符、冒号和空白。
func ValidToolCacheName(name string) bool {
	return validSegment(name)
}

// ValidToolCacheEnv 判断 toolcache 的 Env 是否可以作为环境变量名，且不覆写 Runner 的危险变量。
func ValidToolCacheEnv(env string) bool {
	if !EnvNameRe.MatchString(env) {
		return false
	}
	if strings.HasPrefix(env, "_") {
		// 双下划线开头的变量由 shell 和运行库保留。
		return !strings.HasPrefix(env, "__")
	}
	if slices.Contains(toolCacheEnvBlocked, env) {
		return false
	}
	for _, prefix := range toolCacheEnvBlockedPrefixes {
		if strings.HasPrefix(env, prefix) {
			return false
		}
	}
	return true
}
