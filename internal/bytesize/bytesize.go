// Package bytesize 把字节数格式化为便于阅读的形式（日志和 Dashboard 共用）。
package bytesize

import "fmt"

// unit 是相邻单位之间的倍数（二进制前缀）。
const unit = 1024

// units 是 KiB 及以上各级单位的首字母。
const units = "KMGTP"

// Format 把字节数格式化为 "512 B"、"1.5 KiB"、"2.0 MiB" 等，最大单位为 PiB。
func Format(n int64) string {
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < len(units)-1; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	// 保留一位小数后会进位成 1024.0 时（例如 1048575 字节），改用更大的单位
	if v >= unit-roundingStep && exp < len(units)-1 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", v, units[exp])
}

// roundingStep 是保留一位小数时会进位到下一个整数的差值。
const roundingStep = 0.05
