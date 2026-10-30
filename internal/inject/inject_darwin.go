//go:build darwin

// Package inject 把文本注入当前获得键盘焦点的输入框（macOS）。
//
// 两条路径，与官方 companion/inject.py 一致：
//   - 键盘事件：CGEvent 逐字符送 unicode，完全不碰剪贴板；
//   - 剪贴板：pbcopy 写入，发 Cmd+V，再恢复原剪贴板文本。
//
// 三个真机踩出来的细节，一条都不能省：
//  1. 按下和抬起都要挂同一串 unicode —— 只给按下挂，抬起就是一个裸 keycode 0
//     （=a 键松开），Qt/Chromium 系应用会把整个字符丢掉；
//  2. 每个新事件都 CGEventSetFlags(ev, 0) —— 新建事件会继承"当前修饰键状态"，
//     刚发过 Cmd+A 之后不清零，接下来每个字符都变成快捷键被吃掉；
//  3. 每字符间隔 3ms —— 0 间隔时事件会在 1–2ms 内全部排出，忙渲染的应用整批丢弃。
//
// 辅助功能授权：无授权时 CGEventPost 是**静默成功**的（事件根本不送达），
// 所以注入前必须问 AXIsProcessTrusted。
//
// 落地校验：注入前后各读一次聚焦元素字数（见 ax_darwin.go），字数没动就慢速
// 重打一次，仍不动才判失败并回退剪贴板 —— 这条是"看起来成功其实没进字"的唯一解药。
package inject

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/ebitengine/purego"
)

// 注入方式（与 config 的常量对应；这里不 import config，避免循环依赖）。
const (
	ModeAuto      = "auto"
	ModeUnicode   = "unicode"
	ModeClipboard = "clipboard"
)

// 事件与按键常量。
const (
	tapHID      = 0 // kCGHIDEventTap
	flagMaskCmd = 1 << 20
	keyReturn   = 36
	keyDelete   = 51
	keyA        = 0
	keyV        = 9

	typeGap     = 3 * time.Millisecond
	typeGapSlow = 15 * time.Millisecond

	guide = "请在 系统设置 → 隐私与安全性 → 辅助功能 中授权本程序，然后重试。"
)

var (
	libAS, libCF uintptr

	cgEventCreateKeyboardEvent      func(source uintptr, virtualKey uint16, keyDown bool) uintptr
	cgEventKeyboardSetUnicodeString func(event uintptr, length uintptr, s *uint16)
	cgEventSetFlags                 func(event uintptr, flags uint64)
	cgEventPost                     func(tap uint32, event uintptr)
	axIsProcessTrusted              func() bool
	cfRelease                       func(obj uintptr)

	loadErr error

	// ErrNoTrust 是未授权时返回的错误。
	ErrNoTrust = errors.New("辅助功能未授权：" + guide)
	// ErrNotLanded 是"键盘事件一个字都没进去"（聚焦元素字数没变）。
	ErrNotLanded = errors.New("键盘事件没有落到输入框（聚焦元素字数无变化），目标应用可能不吃合成事件")
	// ErrPasteNotLanded 是剪贴板粘贴同样没落地。
	ErrPasteNotLanded = errors.New("粘贴没有落到输入框（字数无变化）：焦点可能不在输入框上")
	errNoAppKit       = errors.New("inject: 加载 AppKit 失败")
)

func load() error {
	if loadErr != nil {
		return loadErr
	}
	var err error
	libAS, err = purego.Dlopen("/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices",
		purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		loadErr = fmt.Errorf("inject: 加载 ApplicationServices 失败: %w", err)
		return loadErr
	}
	purego.RegisterLibFunc(&cgEventCreateKeyboardEvent, libAS, "CGEventCreateKeyboardEvent")
	purego.RegisterLibFunc(&cgEventKeyboardSetUnicodeString, libAS, "CGEventKeyboardSetUnicodeString")
	purego.RegisterLibFunc(&cgEventSetFlags, libAS, "CGEventSetFlags")
	purego.RegisterLibFunc(&cgEventPost, libAS, "CGEventPost")
	purego.RegisterLibFunc(&axIsProcessTrusted, libAS, "AXIsProcessTrusted")

	libCF, err = purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation",
		purego.RTLD_GLOBAL|purego.RTLD_NOW)
	if err != nil {
		loadErr = fmt.Errorf("inject: 加载 CoreFoundation 失败: %w", err)
		return loadErr
	}
	purego.RegisterLibFunc(&cfRelease, libCF, "CFRelease")
	return nil
}

// Trusted 报告本程序是否已获得「辅助功能」授权。
func Trusted() bool {
	if err := load(); err != nil {
		return false
	}
	return axIsProcessTrusted()
}

// Type 逐字符发送 unicode 键盘事件（不做落地校验）。
func Type(text string) error { return typeUnicode(text, typeGap) }

// typeUnicode 是逐字符注入的实现；gap 由调用方决定（正常 3ms，重打 15ms）。
func typeUnicode(text string, gap time.Duration) error {
	if err := load(); err != nil {
		return err
	}
	if !axIsProcessTrusted() {
		return ErrNoTrust
	}
	for _, r := range text {
		units := utf16.Encode([]rune{r})
		// 抬键也挂同一串 unicode（见包注释第 1 条）。
		for _, down := range []bool{true, false} {
			ev := cgEventCreateKeyboardEvent(0, 0, down)
			if ev == 0 {
				return errors.New("inject: CGEventCreateKeyboardEvent 返回空")
			}
			cgEventKeyboardSetUnicodeString(ev, uintptr(len(units)), &units[0])
			cgEventSetFlags(ev, 0) // 见包注释第 2 条
			cgEventPost(tapHID, ev)
			cfRelease(ev)
		}
		time.Sleep(gap)
	}
	return nil
}

// typeAndVerify 注入并尽力校验落地。
//
// 返回 nil 表示"已确认落地"或"该元素无法校验"（保持旧行为放行）；
// 返回 ErrNotLanded 表示两次都没落地，调用方可回退剪贴板或报错。
// 只在"字数一点没动"时重打一次：部分落地绝不重打，否则会叠字。
func typeAndVerify(text string) error {
	before, canVerify := probeVerifiable()
	if err := typeUnicode(text, typeGap); err != nil {
		return err
	}
	if !canVerify {
		return nil // 终端/非文本元素：字数不可信，放行
	}
	if got := settledLen(before); got < 0 || got != before {
		return nil
	}
	if err := typeUnicode(text, typeGapSlow); err != nil {
		return err
	}
	if got := settledLen(before); got < 0 || got != before {
		return nil
	}
	return ErrNotLanded
}

// probeVerifiable 读一次聚焦元素字数；第二个返回值表示字数是否可信。
func probeVerifiable() (int, bool) {
	if err := loadAX(); err != nil {
		return 0, false
	}
	p := ProbeAX()
	if !p.Verifiable() {
		return 0, false
	}
	return p.Chars, true
}

// Key 执行设备按键动作：enter 回车，clear 清空输入框（Cmd+A 后 Delete）。
func Key(action string) error {
	if err := load(); err != nil {
		return err
	}
	if !axIsProcessTrusted() {
		return ErrNoTrust
	}
	switch action {
	case "enter":
		return postKey(keyReturn, 0)
	case "clear":
		if err := postKey(keyA, flagMaskCmd); err != nil {
			return err
		}
		time.Sleep(30 * time.Millisecond)
		return postKey(keyDelete, 0)
	default:
		return fmt.Errorf("inject: 未知按键动作 %q", action)
	}
}

func postKey(code uint16, flags uint64) error {
	for _, down := range []bool{true, false} {
		ev := cgEventCreateKeyboardEvent(0, code, down)
		if ev == 0 {
			return errors.New("inject: CGEventCreateKeyboardEvent 返回空")
		}
		cgEventSetFlags(ev, flags)
		cgEventPost(tapHID, ev)
		cfRelease(ev)
	}
	return nil
}

// Paste 按方式注入文本，返回实际走通的路径（"unicode" 或 "clipboard"）。
func Paste(text string, mode string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("inject: 文本为空")
	}
	switch mode {
	case ModeUnicode:
		return "unicode", typeAndVerify(text)
	case ModeClipboard:
		return "clipboard", pasteClipboard(text)
	}
	// auto：先键盘事件，确认没落地再回退剪贴板。
	err := typeAndVerify(text)
	if err == nil {
		return "unicode", nil
	}
	if cerr := pasteClipboard(text); cerr != nil {
		return "", fmt.Errorf("键盘事件失败（%v），剪贴板也失败（%v）", err, cerr)
	}
	return "clipboard", nil
}

// pasteClipboard 备份剪贴板 → 写入文本 → Cmd+V → 校验 → 恢复旧文本。
func pasteClipboard(text string) error {
	old, hadOld := readClipboard()
	verifyBefore, canVerify := probeVerifiable()

	if err := writeClipboard(text); err != nil {
		return err
	}
	time.Sleep(30 * time.Millisecond)
	if err := pasteKeystroke(); err != nil {
		return err
	}
	// 走到这条路多半是键盘事件已经确认没落地，若粘贴也没落地就必须报错，
	// 不能再打一句"已粘贴 N 字符"骗人。settledLen 顺带充当"等应用消费粘贴"的间隔。
	if canVerify {
		if got := settledLen(verifyBefore); got >= 0 && got == verifyBefore {
			return ErrPasteNotLanded
		}
	}
	if hadOld {
		_ = writeClipboard(old) // 尽力恢复；失败不影响注入结果
	}
	return nil
}

// pasteKeystroke 发 Cmd+V：优先 CGEvent，未授权时退到 System Events。
func pasteKeystroke() error {
	if Trusted() {
		if err := postKey(keyV, flagMaskCmd); err == nil {
			return nil
		}
	}
	return osascript(`tell application "System Events" to keystroke "v" using command down`)
}

func osascript(script string) error {
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript 失败（%v）：%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func readClipboard() (string, bool) {
	out, err := exec.Command("pbpaste").Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

func writeClipboard(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pbcopy 失败: %w", err)
	}
	return nil
}

// timeSleep 供 ax_darwin.go 复用，避免两个文件各写一遍。
func timeSleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
