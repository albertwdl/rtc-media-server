// Package log 提供项目统一的格式化日志入口。
package log

import stdlog "log"

// Debugf 输出调试日志。
func Debugf(format string, args ...any) {
	stdlog.Printf("[DEBUG] "+format, args...)
}

// Infof 输出普通信息日志。
func Infof(format string, args ...any) {
	stdlog.Printf("[INFO] "+format, args...)
}

// Warnf 输出警告日志。
func Warnf(format string, args ...any) {
	stdlog.Printf("[WARN] "+format, args...)
}

// Errorf 输出错误日志。
func Errorf(format string, args ...any) {
	stdlog.Printf("[ERROR] "+format, args...)
}
