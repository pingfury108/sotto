//go:build !darwin && !windows

package ble

// 其它平台没有 BLE 实现：编译通过，运行时给出明确错误，
// 让 UI 显示"当前平台暂不支持"而不是编译失败。
type unsupported struct{ h Handler }

func newPlatform(h Handler) Client { return &unsupported{h: h} }

func (u *unsupported) Enable() error                    { return ErrUnsupported }
func (u *unsupported) Scan(string, int) (Target, error) { return Target{}, ErrUnsupported }
func (u *unsupported) Connect(Target) error             { return ErrUnsupported }
func (u *unsupported) Discover() (GATT, error)          { return GATT{}, ErrUnsupported }
func (u *unsupported) Subscribe() error                 { return ErrUnsupported }
func (u *unsupported) WriteCTRL([]byte) error           { return ErrUnsupported }
func (u *unsupported) WriteCTRLConfirmed([]byte) error  { return ErrUnsupported }
func (u *unsupported) Connected() bool                  { return false }
func (u *unsupported) Close() error                     { return nil }
