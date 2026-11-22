package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		debug     bool
		format    string
		wantJSON  bool
		wantDebug bool
	}{
		{name: "默认文本", format: "", wantJSON: false},
		{name: "text", format: "text", wantJSON: false},
		{name: "json", format: "json", wantJSON: true},
		{name: "大小写和空白", format: " JSON ", wantJSON: true},
		{name: "debug", debug: true, format: "text", wantDebug: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			log, err := New(&buf, tt.debug, tt.format)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			log.Debug("调试", "k", "v")
			log.Info("信息", "k", "v")
			out := buf.String()
			if got := strings.Contains(out, "调试"); got != tt.wantDebug {
				t.Errorf("包含 debug 日志 = %v，期望 %v；输出：%s", got, tt.wantDebug, out)
			}
			first, _, _ := strings.Cut(out, "\n")
			if got := json.Valid([]byte(first)); got != tt.wantJSON {
				t.Errorf("JSON 格式 = %v，期望 %v；输出：%s", got, tt.wantJSON, out)
			}
		})
	}
}

func TestNewInvalidFormat(t *testing.T) {
	t.Parallel()
	if _, err := New(&bytes.Buffer{}, false, "yaml"); err == nil {
		t.Fatal("New(format=yaml) 应该返回错误")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(EnvDebug, "1")
	t.Setenv(EnvFormat, "json")
	var buf bytes.Buffer
	log, err := FromEnv(&buf)
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	log.Debug("调试")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("输出不是 JSON：%v；%s", err, buf.String())
	}
	if rec["level"] != "DEBUG" {
		t.Errorf("level = %v，期望 DEBUG", rec["level"])
	}
}
