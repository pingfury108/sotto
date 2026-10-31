//go:build windows

package ble

// newPlatform 在 Windows 上用 tinygo.org/x/bluetooth（WinRT 后端，纯 syscall）。
// 注意：这条路径尚未在真机上验证过。
func newPlatform(h Handler) Client { return newLib(h) }
