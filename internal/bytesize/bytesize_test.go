package bytesize

import "testing"

func TestFormat(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB",
		5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB", 2 << 40: "2.0 TiB", 3 << 50: "3.0 PiB", 4096 << 50: "4096.0 PiB",
		// 保留一位小数会进位成 1024.0 时改用更大的单位（以前是 "1024.0 KiB"）
		1<<20 - 1: "1.0 MiB", 1<<30 - 1: "1.0 GiB", 1023*1024 + 972: "1023.9 KiB",
	} {
		if got := Format(n); got != want {
			t.Errorf("Format(%d) = %q，应为 %q", n, got, want)
		}
	}
}
