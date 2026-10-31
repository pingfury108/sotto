//go:build darwin

package inject

import (
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// 这一层的用途只有一个：让"注入成功"有可校验的判据。
//
// 官方实现的教训（companion/inject.py）："已粘贴 N 字符"只表示"调用没抛异常"，
// 和真落地无关。用户最常见的一次失败是——焦点其实不在输入框（前台窗口对了，
// 聚焦元素却是标题/消息之类静态文本），此时 CGEvent 全部扔进空处，日志仍然一片绿色。
//
// 所以注入前后各读一次聚焦元素的字数（AXNumberOfCharacters，退到 AXValue 长度）：
//   - 字数变了 → 确认落地；
//   - 字数一点没动 → 慢速重打一次，仍不动才判失败，回退剪贴板；
//   - 拿不到字数 / 聚焦元素不是文本类 / 前台是终端 → **无法校验，放行**。
//     终端除外这一条很关键：Terminal 插入一行后字数实测不变、Warp 恒报 0，
//     把它当判据会把成功的注入判成失败，于是重打一次再粘贴一次，终端里出现
//     两三份重复文本 —— 比原来的静默失败更糟。
const (
	kCFStringEncodingUTF8 = 0x08000100
	kCFNumberCFIndexType  = 14
)

var (
	libAppKit uintptr

	axUIElementCreateApplication   func(pid int32) uintptr
	axUIElementSetMessagingTimeout func(el uintptr, seconds float32)
	axUIElementCopyAttributeValue  func(el, attr uintptr, out *uintptr) int32

	cfStringCreateWithCString func(alloc uintptr, cstr *byte, encoding uint32) uintptr
	cfStringGetLength         func(s uintptr) int
	cfStringGetCString        func(s uintptr, buf *byte, size int64, encoding uint32) bool
	cfNumberGetValue          func(num uintptr, typ int32, out unsafe.Pointer) bool
	cfGetTypeID               func(o uintptr) uintptr
	cfStringGetTypeID         func() uintptr
	cfNumberGetTypeID         func() uintptr

	axLoaded bool
)

func loadAX() error {
	if err := load(); err != nil {
		return err
	}
	if axLoaded {
		return nil
	}
	// NSWorkspace 在 AppKit 里；probe/CLI 进程可能还没加载过它。
	if libAppKit, _ = purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit",
		purego.RTLD_GLOBAL|purego.RTLD_NOW); libAppKit == 0 {
		return errNoAppKit
	}
	purego.RegisterLibFunc(&axUIElementCreateApplication, libAS, "AXUIElementCreateApplication")
	purego.RegisterLibFunc(&axUIElementSetMessagingTimeout, libAS, "AXUIElementSetMessagingTimeout")
	purego.RegisterLibFunc(&axUIElementCopyAttributeValue, libAS, "AXUIElementCopyAttributeValue")

	purego.RegisterLibFunc(&cfStringCreateWithCString, libCF, "CFStringCreateWithCString")
	purego.RegisterLibFunc(&cfStringGetLength, libCF, "CFStringGetLength")
	purego.RegisterLibFunc(&cfStringGetCString, libCF, "CFStringGetCString")
	purego.RegisterLibFunc(&cfNumberGetValue, libCF, "CFNumberGetValue")
	purego.RegisterLibFunc(&cfGetTypeID, libCF, "CFGetTypeID")
	purego.RegisterLibFunc(&cfStringGetTypeID, libCF, "CFStringGetTypeID")
	purego.RegisterLibFunc(&cfNumberGetTypeID, libCF, "CFNumberGetTypeID")

	axLoaded = true
	return nil
}

// AXProbe 是聚焦元素探针的结果（取不到的项留零值）。
type AXProbe struct {
	App      string // 前台 app 的 bundle id（取不到则为本地化名称）
	Role     string // 聚焦元素的 AXRole
	Chars    int    // 聚焦文本的字数
	HasChars bool   // Chars 是否可信（取不到为 false，表示"无法校验"）
}

// textishRoles 是字数判据可信的 AX 角色。
var textishRoles = map[string]bool{
	"AXTextField":   true,
	"AXTextArea":    true,
	"AXComboBox":    true,
	"AXSearchField": true,
}

// terminalApps 是不报可信字数的终端类应用（bundle id）。
var terminalApps = map[string]bool{
	"com.apple.Terminal":     true,
	"com.googlecode.iterm2":  true,
	"dev.warp.Warp-Stable":   true,
	"dev.warp.Warp":          true,
	"com.mitchellh.ghostty":  true,
	"io.alacritty":           true,
	"net.kovidgoyal.kitty":   true,
	"com.github.wez.wezterm": true,
	"co.zeit.hyper":          true,
	"com.jetbrains.intellij": true,
}

// verifiable 判断"字数"这个判据在当前聚焦元素上是否可信。
func (p AXProbe) Verifiable() bool {
	if !p.HasChars || !textishRoles[p.Role] {
		return false
	}
	return !terminalApps[p.App]
}

// BundleID 返回当前进程所属 app 的 bundle identifier（未打包运行时为空）。
//
// TCC（蓝牙/辅助功能）是按 bundle id + 代码签名记账的，所以排查"授权了却仍报未授权"
// 时必须先看清自己到底是哪个 bundle：dev 构建（Sotto Dev.app / *.dev）与正式包是两个身份。
func BundleID() string {
	if err := loadAX(); err != nil {
		return ""
	}
	main := objc.ID(objc.GetClass("NSBundle")).Send(objc.RegisterName("mainBundle"))
	if main == 0 {
		return ""
	}
	return nsStringToGo(main.Send(objc.RegisterName("bundleIdentifier")))
}

// ProbeAX 读取前台 app、聚焦元素角色与字数。永不 panic；失败返回零值。
func ProbeAX() AXProbe {
	if err := loadAX(); err != nil {
		return AXProbe{}
	}
	var p AXProbe

	ws := objc.ID(objc.GetClass("NSWorkspace")).Send(objc.RegisterName("sharedWorkspace"))
	if ws == 0 {
		return p
	}
	app := ws.Send(objc.RegisterName("frontmostApplication"))
	if app == 0 {
		return p
	}
	ident := app.Send(objc.RegisterName("bundleIdentifier"))
	if ident == 0 {
		ident = app.Send(objc.RegisterName("localizedName"))
	}
	p.App = nsStringToGo(ident)
	pid := objc.Send[int32](app, objc.RegisterName("processIdentifier"))

	el := axUIElementCreateApplication(pid)
	if el == 0 {
		return p
	}
	defer cfRelease(el)
	axUIElementSetMessagingTimeout(el, 1.0) // 目标应用卡住时别拖住注入

	focused := axCopy(el, "AXFocusedUIElement")
	if focused == 0 {
		return p
	}
	defer cfRelease(focused)

	if role := axCopy(focused, "AXRole"); role != 0 {
		p.Role = cfStringToGo(role)
		cfRelease(role)
	}

	// 先试 AXNumberOfCharacters（CFNumber），退到 AXValue（CFString）。
	if num := axCopy(focused, "AXNumberOfCharacters"); num != 0 {
		if cfGetTypeID(num) == cfNumberGetTypeID() {
			var n int64
			if cfNumberGetValue(num, kCFNumberCFIndexType, unsafe.Pointer(&n)) {
				p.Chars, p.HasChars = int(n), true
			}
		}
		cfRelease(num)
	}
	if !p.HasChars {
		if val := axCopy(focused, "AXValue"); val != 0 {
			if cfGetTypeID(val) == cfStringGetTypeID() {
				p.Chars, p.HasChars = cfStringGetLength(val), true
			}
			cfRelease(val)
		}
	}
	return p
}

// axCopy 复制一个 AX 属性；失败返回 0。
func axCopy(el uintptr, attr string) uintptr {
	name := cfString(attr)
	if name == 0 {
		return 0
	}
	defer cfRelease(name)
	var out uintptr
	if rc := axUIElementCopyAttributeValue(el, name, &out); rc != 0 {
		return 0
	}
	return out
}

// cfString 用 UTF-8 构造一个 CFString（调用方负责 release）。
func cfString(s string) uintptr {
	b := append([]byte(s), 0)
	return cfStringCreateWithCString(0, &b[0], kCFStringEncodingUTF8)
}

// cfStringToGo 用 CFStringGetCString 拷进 Go 缓冲，避免逐字节读 C 指针。
func cfStringToGo(s uintptr) string {
	if n := cfStringGetLength(s); n <= 0 {
		return ""
	}
	var buf [1024]byte
	if !cfStringGetCString(s, &buf[0], int64(len(buf)), kCFStringEncodingUTF8) {
		return ""
	}
	return trimNUL(buf[:])
}

// nsStringToGo 用 lengthOfBytesUsingEncoding 拿长度后整段拷贝。
func nsStringToGo(id objc.ID) string {
	if id == 0 {
		return ""
	}
	p := objc.Send[uintptr](id, objc.RegisterName("UTF8String"))
	if p == 0 {
		return ""
	}
	n := objc.Send[int](id, objc.RegisterName("lengthOfBytesUsingEncoding:"), uintptr(4))
	if n <= 0 || n > 4096 {
		return ""
	}
	// 借用 mygo 的写法：对 uintptr 变量取地址再转指针，避开 unsafe.Pointer(uintptr)。
	return string(unsafe.Slice(*(**byte)(unsafe.Pointer(&p)), n))
}

func trimNUL(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// settledLen 等聚焦元素字数变化（应用更新 AX 有延迟），最多 3×120ms。
// 返回最后读到的字数；读到不可信的结果返回 -1（等于"无法校验"）。
func settledLen(before int) int {
	last := before
	for i := 0; i < 3; i++ {
		timeSleep(120)
		p := ProbeAX()
		if !p.HasChars {
			return -1
		}
		last = p.Chars
		if last != before {
			return last
		}
	}
	return last
}
