// Package ble 是 AI Passport 的 BLE 中央端：扫描、连接、发现、订阅、写 CTRL。
//
// 契约（固件 main/ble_audio.c）：
//
//	服务 0xA2B0
//	  ├─ 0xA2B1 CTRL  WRITE|WRITE_ENC|WRITE_NO_RSP  Mac→设备 JSON 行 ≤2048B
//	  ├─ 0xA2B2 EVENT NOTIFY                        设备→Mac JSON 行 ≤512B，跨通知分片
//	  └─ 0xA2B3 AUDIO NOTIFY                        设备→Mac 音频分片（2B 片头）
//
// 两套实现，同一接口：
//
//	"cb"  （默认）直接用 tinygo-org/cbgo，delegate 回调里的 NSError 原样透出；
//	"lib"        用 tinygo.org/x/bluetooth，只作对照——它在 darwin 上会把服务发现
//	             的错误吞成一句 service not found，排障时完全看不见原因。
//
// 线程事实：CoreBluetooth 的 delegate 回调跑在 cbgo 自建的 dispatch queue
// （bt_queue）线程上，不占主线程，与 MyGo 的 AppKit 主循环无冲突。因此 Handler
// 的回调必须立即返回，只做有界入队，解码与网络交给调用方 goroutine。
//
// 配对：CTRL 带 WRITE_ENC，写之前链路必须已加密。macOS 的 CoreBluetooth 会在首次
// 访问受保护特征时自动发起 Just Works 配对，Go 侧不存密钥；用 WriteCTRLConfirmed
// 在连接后探一次，失败即可给出"删除设备重新配对"的指引。
//
// 分片大小的事实：设备发送缓冲只有 256B（main/ble_audio.c 的 s_audio_tx），
// 所以即使协商到 MTU 517，单片载荷也 ≤256B（数据 254B）。一块 804B 因此约 4 片、
// 约 40 条通知/秒——不要按 mtu-3-2 推算片数。
package ble

import (
	"errors"
	"strings"
)

// GATT 标识（固件常量）。
const (
	ServiceUUID = "0000A2B0-0000-1000-8000-00805F9B34FB"
	CtrlUUID    = "0000A2B1-0000-1000-8000-00805F9B34FB"
	EventUUID   = "0000A2B2-0000-1000-8000-00805F9B34FB"
	AudioUUID   = "0000A2B3-0000-1000-8000-00805F9B34FB"

	// DeviceName 是设备广播名。
	DeviceName = "AI Passport"
)

// 常见错误。
var (
	ErrNotFound     = errors.New("ble: 设备未找到")
	ErrScanTimeout  = errors.New("ble: 扫描超时")
	ErrNotConnected = errors.New("ble: 未连接")
	ErrNoService    = errors.New("ble: 未发现服务 0xA2B0")
	ErrNoChar       = errors.New("ble: 缺失必需特征（CTRL/EVENT/AUDIO）")
)

// Handler 收设备侧数据。回调发生在 CoreBluetooth 的队列线程上：
// 实现必须立即返回（只做有界入队），禁止阻塞。
type Handler struct {
	// OnAudio 收到一段 AUDIO 通知载荷（2B 片头 + 块数据，可能已被系统合并多条）。
	OnAudio func(chunk []byte)
	// OnEvent 收到一段 EVENT 通知载荷（JSON 行，可能跨片、可能多行合并）。
	OnEvent func(chunk []byte)
	// OnDisconnect 链路断开（设备关机/走远/系统蓝牙关闭）。err 可能为 nil。
	OnDisconnect func(err error)
	// OnLog 诊断日志。
	OnLog func(format string, args ...any)
}

func (h Handler) log(format string, args ...any) {
	if h.OnLog != nil {
		h.OnLog(format, args...)
	}
}

// Target 是扫描命中的设备。
type Target struct {
	Addr string   // macOS 上是 CoreBluetooth UUID
	RSSI int16    // 广播 RSSI
	Adv  []string // 广播包里的服务 UUID（诊断用）

	inner any // 实现私有句柄（cbgo.Peripheral / bluetooth.Address）
}

// GATT 是发现结果，用于诊断页与 probe 打印。
//
// macOS 下 UUID 的写法不保证统一：固件注册的是 128-bit 基 UUID，CoreBluetooth
// 可能回报完整 128 位，也可能收回成 "A2B0"。匹配一律走 uuid16() 归一化。
type GATT struct {
	Service    string
	Services   []string
	Chars      []string
	WriteLimit int // CTRL 单次写值上限（CBPeripheral.maximumWriteValueLength）
}

// Client 是一次连接的生命周期：Enable → Scan → Connect → Discover → Subscribe。
type Client interface {
	Enable() error
	Scan(name string, timeoutMillis int) (Target, error)
	Connect(t Target) error
	Discover() (GATT, error)
	Subscribe() error
	WriteCTRL(line []byte) error
	WriteCTRLConfirmed(line []byte) error
	Connected() bool
	Close() error
}

// newLibImpl 由 lib.go（构建标签 blelib）注册。默认构建不链接
// tinygo.org/x/bluetooth，避免它的包级变量在程序启动时就创建一个多余的
// CBCentralManager。
var newLibImpl func(Handler) Client

// New 按实现名创建客户端："cb"（默认）或 "lib"（需 -tags blelib）。
func New(h Handler, impl string) (Client, error) {
	switch impl {
	case "", "cb":
		return newCB(h), nil
	case "lib":
		if newLibImpl == nil {
			return nil, errors.New("ble: lib 实现未编译进来（加 -tags blelib）")
		}
		return newLibImpl(h), nil
	}
	return nil, errors.New("ble: 未知实现 " + impl + "（可选 cb / lib）")
}

// uuid16 把 UUID 归一化成 16 位短形式；无法识别时原样返回。
func uuid16(s string) string {
	u := strings.ToUpper(strings.ReplaceAll(s, "-", ""))
	switch len(u) {
	case 4, 8:
		return u[len(u)-4:]
	case 32:
		if h := baseUUID16(u); h != "" {
			return h
		}
		if h := nimbleUUID16(u); h != "" {
			return h
		}
	}
	return u
}

// nimbleBasePrefix 是真机上 128 位基址 UUID 被 cbgo 打印出来的样子。
//
// 固件用 BLE_UUID128_INIT(0xB0,0xA2,…,0xFB) 声明服务，NimBLE 按 16 字节小端
// 保存；cbgo 的 UUID.String() 又把字节整体逆序，于是 0xA2B0 服务在日志里变成
// "FB349B5F-8000-8000-1000-00000000A2B0"（16 位部分跑到末尾）。
// 这个前缀就是基址 0000-1000-8000-00805F9B34FB 经同一变换后的定值，
// 真机日志：发现的服务：[FB349B5F-8000-8000-1000-00000000A2B0]。
//
// 因此服务匹配不能只靠常规写法；下面还有一条“哪个服务里有 CTRL 就用哪个”的兜底。
const nimbleBasePrefix = "FB349B5F80008000100000000000"

func nimbleUUID16(u string) string {
	if len(u) == 32 && strings.HasPrefix(u, nimbleBasePrefix) {
		return u[28:]
	}
	return ""
}

// baseUUID16 判断 u（32 位十六进制）是否是蓝牙基址派生 UUID，是则返回 16 位部分。
func baseUUID16(u string) string {
	const base = "00001000800000805F9B34FB"
	if len(u) != 32 || !strings.HasPrefix(u, "0000") || !strings.HasSuffix(u, base) {
		return ""
	}
	return u[4:8]
}

// hasCTRL 判断一组特征里是否有 CTRL。
func hasCTRL(uuids []string) bool {
	for _, u := range uuids {
		if uuid16(u) == uuid16(CtrlUUID) {
			return true
		}
	}
	return false
}
