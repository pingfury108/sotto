//go:build !darwin

// Package inject 在本平台不可用（只做 macOS）。
package inject

import "errors"

// 与 darwin 实现保持一致的模式常量。
const (
	ModeAuto      = "auto"
	ModeUnicode   = "unicode"
	ModeClipboard = "clipboard"
)

// ErrNoTrust 占位：非 darwin 平台直接不可用。
var ErrNoTrust = errors.New("inject: 仅支持 macOS")

// Trusted 报告授权状态（非 macOS 恒为 false）。
func Trusted() bool { return false }

// Type 逐字符注入（非 macOS 不支持）。
func Type(string) error { return ErrNoTrust }

// Key 执行按键动作（非 macOS 不支持）。
func Key(string) error { return ErrNoTrust }

// Paste 注入文本（非 macOS 不支持）。
func Paste(string, string) (string, error) { return "", ErrNoTrust }

// AXProbe 是非 darwin 平台的占位：没有辅助功能 API，永远"无法校验"。
type AXProbe struct {
	App      string
	Role     string
	Chars    int
	HasChars bool
}

// Verifiable 在非 darwin 平台恒为 false。
func (p AXProbe) Verifiable() bool { return false }

// ProbeAX 非 darwin 平台返回零值。
func ProbeAX() AXProbe { return AXProbe{} }
