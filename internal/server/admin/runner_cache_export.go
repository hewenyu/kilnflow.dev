package admin

// CacheBytes 返回 Runner 上报的缓存清单（ci_runners.cache_report）中全部项的大小之和（Dashboard 用）。
func CacheBytes(raw []byte) int64 { return cacheBytes(raw) }
