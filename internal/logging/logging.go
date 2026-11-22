// Package logging 构造 kiln-server 和 kiln-runner 共用的 slog.Logger。
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// 控制日志输出的环境变量。
const (
	// EnvDebug 设置为任意非空值时输出调试日志。
	EnvDebug = "KILN_DEBUG"
	// EnvFormat 选择日志格式：text（默认）或 json。
	EnvFormat = "KILN_LOG_FORMAT"
)

// 支持的日志格式。
const (
	FormatText = "text"
	FormatJSON = "json"
)

// New 返回写到 w 的 Logger。debug 为 true 时级别为 Debug，否则为 Info；
// format 为 "text"、"json"，空字符串等同于 "text"，其他值返回错误。
func New(w io.Writer, debug bool, format string) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if debug {
		opts.Level = slog.LevelDebug
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", FormatText:
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case FormatJSON:
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("%s=%q 无效：只支持 %s 或 %s", EnvFormat, format, FormatText, FormatJSON)
	}
}

// FromEnv 按 KILN_DEBUG 和 KILN_LOG_FORMAT 调用 New。
func FromEnv(w io.Writer) (*slog.Logger, error) {
	return New(w, os.Getenv(EnvDebug) != "", os.Getenv(EnvFormat))
}
