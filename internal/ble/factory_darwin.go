//go:build darwin

package ble

// newPlatform 在 macOS 上用 cbgo 直连 CoreBluetooth（错误信息最完整）。
func newPlatform(h Handler) Client { return newCB(h) }
