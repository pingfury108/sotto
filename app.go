package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"sotto/internal/asr"
	"sotto/internal/ble"
	"sotto/internal/config"
	"sotto/internal/inject"
	"sotto/internal/proto"
	"sotto/internal/session"
)

// 连接状态。
const (
	stateIdle       = "idle"
	stateConnecting = "connecting"
	stateConnected  = "connected"
	stateError      = "error"
)

// app 是应用状态。字段分两类：
//   - 表单/视图字段：只在主线程（视图构建与主线程回调）读写；
//   - 运行状态字段：受 mu 保护，后台 goroutine 与视图都能读。
type app struct {
	cfg *config.Store
	log *logBuf

	win   *mygo.Window
	float *mygo.Window
	tray  *mygo.Tray

	// —— 表单（主线程）——
	apiKey     string
	wsURL      string
	resource   string
	model      string
	nonstream  bool
	deviceName string
	autoConn   bool
	injectMode int // 0 auto / 1 unicode / 2 clipboard
	injectText string
	showFloat  bool
	page       string

	// —— 运行状态（受 mu）——
	mu         sync.Mutex
	connState  string
	connDetail string
	asrTesting bool
	asrOK      bool
	asrResult  string
	floatText  string
	floatHint  string
	quitting   bool

	bleCached ble.Client       // 复用同一个 CoreBluetooth 管理器（每次新建会泄漏）
	sess      *session.Manager // 连接级会话管理者（断连时置空）
	events    chan []byte

	// 连接意图与重连状态（受 mu）：wantConnected 表示"用户希望保持连接"。
	connecting    bool
	wantConnected bool
	reconnAttempt int
	reconnStop    chan struct{}
	timeSyncStop  chan struct{}
	floatH        int

	// showFloating 的原子镜像：表单字段只在主线程写，后台回调读这个。
	showFloatFlag atomic.Bool

	// ASR/注入的当前快照：会话 goroutine 读它，主线程在 saveConfig 时更新。
	asrCfg        asr.Config
	injectModeStr string

	// 候选字合并：120ms 内只刷最新一帧，首帧立即显示。
	candText      string
	candScheduled bool

	themeApplied bool
	themeDark    bool
	toasts       []ui.Toast

	// 重绘去抖
	repaintMu      sync.Mutex
	repaintPending bool
}

func newApp() *app {
	a := &app{
		cfg:       config.Load(config.Dir()),
		connState: stateIdle,
		events:    make(chan []byte, 64),
		page:      "main",
	}
	a.log = newLogBuf(400, a.requestRepaint)
	a.applyConfig(a.cfg.Get())
	return a
}

// ---- 配置 ----

func (a *app) applyConfig(c config.Config) {
	a.showFloatFlag.Store(c.ShowFloating)
	a.apiKey, a.wsURL = c.VolcanoAPIKey, c.VolcanoWSURL
	a.resource, a.model = c.VolcanoResourceID, c.VolcanoModel
	a.nonstream = c.VolcanoNonstream
	a.deviceName, a.autoConn = c.DeviceName, c.AutoConnect
	a.injectText = c.InjectSample
	a.showFloat = c.ShowFloating
	a.mu.Lock()
	a.asrCfg, a.injectModeStr = c.ASR(), c.InjectMode
	a.mu.Unlock()
	switch c.InjectMode {
	case config.InjectUnicode:
		a.injectMode = 1
	case config.InjectClipboard:
		a.injectMode = 2
	default:
		a.injectMode = 0
	}
}

// saveConfig 把表单写回磁盘。输入框 Changed 时调用，文件很小，直接同步落盘。
func (a *app) saveConfig() {
	modes := []string{config.InjectAuto, config.InjectUnicode, config.InjectClipboard}
	err := a.cfg.Update(func(c *config.Config) {
		c.VolcanoAPIKey = a.apiKey
		c.VolcanoWSURL = a.wsURL
		c.VolcanoResourceID = a.resource
		c.VolcanoModel = a.model
		c.VolcanoNonstream = a.nonstream
		c.DeviceName = a.deviceName
		c.AutoConnect = a.autoConn
		c.InjectMode = modes[a.injectMode]
		c.InjectSample = a.injectText
		c.ShowFloating = a.showFloat
	})
	if err != nil {
		a.logf("保存配置失败：%v", err)
	}
	a.showFloatFlag.Store(a.showFloat)
	a.mu.Lock()
	a.asrCfg = a.formASRNoLock()
	a.injectModeStr = modes[a.injectMode]
	a.mu.Unlock()
}

// resourceChoices 是资源 ID 的快捷切换项（主窗分段控件用）。
var resourceChoices = []struct {
	id    string
	label string
	price string
}{
	{asr.ResourceID20, "2.0 代", "推荐 · " + asr.ResourceID20 + "（小时版）；并发版用 " + asr.ResourceID20Concurrent},
	{asr.ResourceID10, "1.0 代", "历史版本 · " + asr.ResourceID10 + "（小时版）；并发版用 " + asr.ResourceID10Concurrent},
}

// resourceIndex 由当前资源 ID 反推分段选中项（0 = 2.0，1 = 1.0）。
func resourceIndex(id string) int {
	switch id {
	case asr.ResourceID10, asr.ResourceID10Concurrent:
		return 1
	}
	return 0
}

// sessionConfig 给会话管理者用：取快照，避免后台 goroutine 读主线程字段。
func (a *app) sessionConfig() session.Config {
	a.mu.Lock()
	cfg, mode := a.asrCfg, a.injectModeStr
	a.mu.Unlock()
	return session.Config{
		ASR:       cfg,
		Inject:    func(text string) (string, error) { return inject.Paste(text, mode) },
		WaitFinal: 6 * time.Second,
	}
}

// formASRNoLock 与 formASR 相同，但调用方已持有 a.mu。
func (a *app) formASRNoLock() asr.Config {
	return asr.Config{
		APIKey:          a.apiKey,
		WSURL:           a.wsURL,
		ResourceID:      a.resource,
		Model:           a.model,
		EnableNonStream: a.nonstream,
	}
}

// formASR 用当前表单值构造 ASR 参数（主线程调用）。
func (a *app) formASR() asr.Config {
	return asr.Config{
		APIKey:          a.apiKey,
		WSURL:           a.wsURL,
		ResourceID:      a.resource,
		Model:           a.model,
		EnableNonStream: a.nonstream,
	}
}

// ---- 日志与重绘 ----

func (a *app) logf(format string, args ...any) { a.log.Printf(format, args...) }

// requestRepaint 合并高频刷新：80ms 内最多重绘一次。
func (a *app) requestRepaint() {
	a.repaintMu.Lock()
	if a.repaintPending {
		a.repaintMu.Unlock()
		return
	}
	a.repaintPending = true
	a.repaintMu.Unlock()

	go func() {
		time.Sleep(80 * time.Millisecond)
		a.repaintMu.Lock()
		a.repaintPending = false
		a.repaintMu.Unlock()
		a.refresh()
	}()
}

// refresh 让窗口与托盘按当前状态重绘。任意 goroutine 可调。
func (a *app) refresh() {
	if a.win != nil {
		a.win.Update(func() {})
	}
	if a.float != nil && a.isFloatingVisible() {
		a.float.Update(func() {})
	}
	a.refreshTray()
}

// toast 在窗口上弹一条提示（视图构建时展示）。
func (a *app) toast(title, typ string) {
	if a.win == nil {
		return
	}
	a.win.Update(func() {
		a.toasts = append(a.toasts, ui.Toast{Title: title, Type: typ})
	})
}

// ---- 连接状态 ----

// scheduleReconnect 按指数退避安排一次重连（1s→2s→4s…上限 30s）。
func (a *app) scheduleReconnect() {
	a.mu.Lock()
	if !a.wantConnected || a.reconnStop != nil {
		a.mu.Unlock()
		return
	}
	a.reconnAttempt++
	n := a.reconnAttempt
	stop := make(chan struct{})
	a.reconnStop = stop
	a.mu.Unlock()

	delay := time.Second << uint(minInt(n-1, 5))
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	secs := int(delay / time.Second)
	a.setConn(stateConnecting, fmt.Sprintf("%d 秒后自动重连（第 %d 次）", secs, n))
	a.logf("将在 %d 秒后自动重连（第 %d 次）", secs, n)

	go func() {
		select {
		case <-time.After(delay):
		case <-stop:
			return
		}
		a.mu.Lock()
		a.reconnStop = nil
		a.mu.Unlock()
		a.connectDevice()
	}()
}

// cancelReconnect 取消待执行的重连（不动重连计数）。
func (a *app) cancelReconnect() {
	a.mu.Lock()
	if a.reconnStop != nil {
		close(a.reconnStop)
		a.reconnStop = nil
	}
	a.mu.Unlock()
}

// startTimeSync 连接建立后每小时校时一次（首次已在连接流程里做过）。
func (a *app) startTimeSync(client ble.Client) {
	a.stopTimeSync()
	stop := make(chan struct{})
	a.mu.Lock()
	a.timeSyncStop = stop
	a.mu.Unlock()

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				line, err := proto.TimeSet(time.Now().Unix())
				if err != nil {
					continue
				}
				if err := client.WriteCTRL(line); err != nil {
					a.logf("周期校时失败：%v", err)
					continue
				}
				a.logf("周期校时已完成（每 1 小时）")
			}
		}
	}()
}

func (a *app) stopTimeSync() {
	a.mu.Lock()
	if a.timeSyncStop != nil {
		close(a.timeSyncStop)
		a.timeSyncStop = nil
	}
	a.mu.Unlock()
}

// fitFloatingHeight 按文本实测高度调整悬浮窗：底部锚定，向上生长。
// 参数是上一帧文本元素的高度，所以两帧内收敛；差值 ≤2px 不再动，避免抖动。
func (a *app) fitFloatingHeight(textH float32) {
	if a.float == nil {
		return
	}
	// 文本 + 上下内距(2×sp3) + 脚注行(~20) + 行间距(sp2) + 外距(2×sp2)
	target := int(textH) + int(2*sp3) + 20 + int(sp2) + int(2*sp2)
	if target < 96 {
		target = 96
	}
	if target > 360 {
		target = 360
	}
	b := a.float.Bounds()
	if b.Height == 0 || absInt(target-b.Height) <= 2 {
		return
	}
	a.mu.Lock()
	a.floatH = target
	a.mu.Unlock()

	// 底边固定：高度变化时 Y 反向移动同样多。
	y := b.Y + b.Height - target
	a.float.SetBounds(mygo.Rectangle{X: b.X, Y: y, Width: b.Width, Height: target})
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// session 返回当前会话管理者（可能为 nil）。
func (a *app) session() *session.Manager {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess
}

// pushCandidate 收到候选字：120ms 合并只刷最新一帧，首帧立即显示。
func (a *app) pushCandidate(text string) {
	a.mu.Lock()
	a.candText = text
	enabled := a.showFloatFlag.Load()
	if a.candScheduled || !enabled {
		a.mu.Unlock()
		return
	}
	a.candScheduled = true
	a.mu.Unlock()

	// 窗口还没出来时立刻显示，赶在第一帧候选字上。
	if !a.isFloatingVisible() {
		a.showFloating(text, "识别中…")
	}

	go func() {
		time.Sleep(120 * time.Millisecond)
		a.mu.Lock()
		latest := a.candText
		a.candScheduled = false
		enabled := a.showFloatFlag.Load()
		a.mu.Unlock()
		if enabled {
			a.showFloating(latest, "识别中…")
		}
	}()
}

// finishCandidate 会话结束：收起悬浮窗（注入已由会话层完成）。
func (a *app) finishCandidate(text string) {
	if !a.showFloatFlag.Load() {
		return
	}
	a.hideFloating()
	a.mu.Lock()
	a.candText = ""
	a.mu.Unlock()
}

func (a *app) setConn(state, detail string) {
	a.mu.Lock()
	a.connState, a.connDetail = state, detail
	a.mu.Unlock()
	a.refresh()
}

func (a *app) connSnapshot() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connState, a.connDetail
}

// startEventPump 把 BLE 事件流按行拆开后记录到日志（任意 goroutine 写入）。
func (a *app) startEventPump() {
	go func() {
		lines := proto.NewLineReassembler(0)
		for chunk := range a.events {
			for _, line := range lines.Feed(chunk) {
				ev, err := proto.ParseEvent(line)
				if err != nil {
					a.logf("畸形事件行：%q", string(line))
					continue
				}
				a.handleEvent(ev)
			}
		}
	}()
}

func (a *app) handleEvent(ev proto.Event) {
	switch ev.Kind {
	case proto.EventDeviceHello:
		a.logf("设备 hello，proto=%d", ev.Proto)
	case proto.EventVoiceStart:
		if mgr := a.session(); mgr != nil {
			mgr.OnVoiceStart(ev.AudioFormat())
		} else {
			a.logf("voice.start（%s）但设备未就绪", ev.AudioFormat())
		}
	case proto.EventVoiceEnd:
		if mgr := a.session(); mgr != nil {
			mgr.OnVoiceEnd()
		}
	case proto.EventKeyAction:
		a.logf("设备按键：%s", ev.Action)
		action := ev.Action
		go func() {
			if err := inject.Key(action); err != nil {
				a.logf("按键注入失败（%s）：%v", action, err)
				return
			}
			a.logf("已注入按键：%s", action)
		}()
	case proto.EventAgentAction:
		a.logf("审批结果：%s → %s", ev.TaskID, ev.Action)
	case proto.EventStatus:
		if mgr := a.session(); mgr != nil {
			mgr.OnStatus(ev.Drop)
		} else {
			a.logf("设备掉帧 %d", ev.Drop)
		}
	default:
		a.logf("未知事件：%s", ev.Kind)
	}
}

// connectDevice 扫描 → 连接 → 发现 → 订阅 → 校时（后台执行）。
func (a *app) connectDevice() {
	a.cancelReconnect()
	a.mu.Lock()
	if a.connecting || a.connState == stateConnected {
		a.mu.Unlock()
		return
	}
	a.connecting, a.wantConnected = true, true
	a.connState, a.connDetail = stateConnecting, "启用蓝牙…"
	a.mu.Unlock()
	a.refresh()

	name := a.deviceName
	go func() {
		// 复用同一个 CoreBluetooth 管理器：CBClient 每 new 一个都会建一个
		// CBCentralManager（cbgo 按设计不回收 ObjC 对象），重连多次就会越积越多。
		a.mu.Lock()
		client := a.bleCached
		a.mu.Unlock()
		if client == nil {
			var err error
			client, err = ble.New(ble.Handler{
				OnLog: a.logf,
				OnEvent: func(chunk []byte) {
					select {
					case a.events <- chunk:
					default: // 事件极少，队列满说明异常；丢了也不阻塞回调线程
					}
				},
				OnAudio: func(chunk []byte) {
					if mgr := a.session(); mgr != nil {
						mgr.OnAudio(chunk)
					}
				},
				OnDisconnect: a.handleDisconnect,
			}, "cb")
			if err != nil {
				a.failConnect(err)
				return
			}
			a.mu.Lock()
			a.bleCached = client
			a.mu.Unlock()
		}

		a.logf("启用蓝牙…")
		if err := client.Enable(); err != nil {
			a.failConnect(err)
			return
		}

		a.setConn(stateConnecting, "扫描 "+name+"…")
		target, err := client.Scan(name, 15000)
		if err != nil {
			a.failConnect(err)
			return
		}

		a.setConn(stateConnecting, "连接 "+name+"…")
		a.logf("连接 %s（%s）…", name, target.Addr)
		if err := client.Connect(target); err != nil {
			a.failConnect(err)
			return
		}

		a.setConn(stateConnecting, "读取服务…")
		gatt, err := client.Discover()
		if err != nil {
			client.Close()
			a.failConnect(err)
			return
		}
		a.logf("服务 %s，特征 %d 个，写值上限 %d B", gatt.Service, len(gatt.Chars), gatt.WriteLimit)

		if err := client.Subscribe(); err != nil {
			client.Close()
			a.failConnect(err)
			return
		}

		// 校时同时验证加密链路：CTRL 带 WRITE_ENC，未配对时确认写会失败。
		synced := false
		if line, err := proto.TimeSet(time.Now().Unix()); err == nil {
			if err := client.WriteCTRLConfirmed(line); err != nil {
				a.logf("校时/配对探测失败：%v", err)
			} else {
				a.logf("校时成功 → 链路已加密，配对 OK")
				synced = true
			}
		}

		detail := "已连接，可以按住设备说话（配对未确认）"
		if synced {
			detail = "已连接，可以按住设备说话"
		}
		mgr := session.NewManager(client, a.sessionConfig, session.Handler{
			OnPartial: a.pushCandidate,
			OnFinal:   a.finishCandidate,
			OnLog:     a.logf,
		})
		a.mu.Lock()
		a.sess = mgr
		a.connecting = false
		a.reconnAttempt = 0
		a.connState, a.connDetail = stateConnected, detail
		a.mu.Unlock()
		a.logf("已连接设备 %s", name)
		a.startTimeSync(client)
		a.refresh()
	}()
}

// handleDisconnect 处理链路意外断开：收束会话、把状态收敛成可重连的形态。
func (a *app) handleDisconnect(err error) {
	a.mu.Lock()
	mgr := a.sess
	a.sess = nil
	detail := "设备已断开，点「连接」重连"
	if err != nil {
		detail = "断开：" + err.Error()
	}
	if a.connState == stateConnected || a.connState == stateConnecting {
		a.connState, a.connDetail = stateError, detail
	}
	a.mu.Unlock()

	a.stopTimeSync()
	if mgr != nil {
		mgr.Close() // 进行中的会话尽力补发 done
	}
	a.logf("链路已断开：%s", detail)
	a.refresh()

	a.mu.Lock()
	want := a.wantConnected
	a.mu.Unlock()
	if want {
		a.scheduleReconnect()
	}
}

func (a *app) failConnect(err error) {
	a.logf("连接失败：%v", err)
	a.mu.Lock()
	a.connecting = false
	a.connState, a.connDetail = stateError, err.Error()
	want := a.wantConnected
	a.mu.Unlock()
	a.refresh()
	if want {
		a.scheduleReconnect()
	}
}

// disconnectDevice 断开连接。
func (a *app) disconnectDevice() {
	a.mu.Lock()
	client := a.bleCached
	mgr := a.sess
	a.sess = nil
	a.connecting = false
	a.wantConnected = false
	a.reconnAttempt = 0
	a.connState, a.connDetail = stateIdle, ""
	a.mu.Unlock()
	a.cancelReconnect()
	a.stopTimeSync()
	if mgr != nil {
		mgr.Close() // 收束进行中的会话（会尽力补发 done）
	}
	if client != nil && client.Connected() {
		if err := client.Close(); err != nil {
			a.logf("断开失败：%v", err)
		} else {
			a.logf("已断开连接")
		}
	}
	a.refresh()
}

// ---- ASR 测试 ----

// testASR 用当前表单里的 Key/端点做一次零音频握手。
func (a *app) testASR(cfg asr.Config) {
	a.mu.Lock()
	if a.asrTesting {
		a.mu.Unlock()
		return
	}
	a.asrTesting, a.asrResult, a.asrOK = true, "", false
	a.mu.Unlock()
	a.refresh()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := asr.TestConnection(ctx, cfg)
		a.mu.Lock()
		a.asrTesting = false
		if err != nil {
			a.asrOK, a.asrResult = false, err.Error()
		} else {
			a.asrOK, a.asrResult = true, "连接成功，Key 与资源 ID 有效"
		}
		a.mu.Unlock()
		if err != nil {
			a.logf("ASR 测试失败：%v", err)
		} else {
			a.logf("ASR 测试成功（%s）", cfg.ResourceID)
		}
		a.refresh()
	}()
}

func (a *app) asrSnapshot() (testing, ok bool, result string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asrTesting, a.asrOK, a.asrResult
}

// ---- 注入 ----

// testInject 把示例文本注入当前焦点输入框。
func (a *app) testInject(text, mode string) {
	go func() {
		used, err := inject.Paste(text, mode)
		if err != nil {
			a.logf("注入失败：%v", err)
			a.toast("注入失败："+err.Error(), "error")
			return
		}
		a.logf("注入成功（%s）：%s", used, text)
		a.toast("已注入（"+used+"）", "success")
	}()
}

// logTrust 报告辅助功能授权状态。
func (a *app) logTrust() {
	if inject.Trusted() {
		a.logf("辅助功能授权：已授权")
		a.toast("已授权辅助功能", "success")
		return
	}
	a.logf("辅助功能授权：未授权（系统设置 → 隐私与安全性 → 辅助功能）")
	a.toast("未授权辅助功能，注入会被系统静默丢弃", "error")
}

// ---- 悬浮窗 ----

func (a *app) showFloating(text, hint string) {
	a.mu.Lock()
	a.floatText, a.floatHint = text, hint
	a.mu.Unlock()
	if a.float == nil {
		return
	}
	a.positionFloating()
	a.float.ShowInactive() // 绝不 Show()：那会给面板键盘焦点，注入就打空
	a.float.Update(func() {})
}

func (a *app) isFloatingVisible() bool {
	if a.float == nil {
		return false
	}
	return a.float.IsVisible()
}

func (a *app) hideFloating() {
	if a.float != nil {
		a.float.Hide()
	}
}

// floatingSnapshot 返回悬浮窗要显示的内容。
func (a *app) floatingSnapshot() (text, hint string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.floatText == "" {
		return "你好，世界。", "预览"
	}
	return a.floatText, a.floatHint
}

// positionFloating 把悬浮窗放到主屏底部居中。
func (a *app) positionFloating() {
	const (
		w   = 560
		gap = 96
	)
	a.mu.Lock()
	h := a.floatH
	a.mu.Unlock()
	if h < 96 {
		h = 112
	}
	ds := mygo.Screen.Displays()
	if len(ds) == 0 {
		return
	}
	wa := ds[0].WorkArea
	x := wa.X + (wa.Width-w)/2
	y := wa.Y + wa.Height - h - gap
	if y < wa.Y {
		y = wa.Y
	}
	a.float.SetBounds(mygo.Rectangle{X: x, Y: y, Width: w, Height: h})
}

// ---- 诊断 ----

// diag 返回诊断页要显示的键值对。
func (a *app) diag() [][2]string {
	state, detail := a.connSnapshot()
	_, ok, result := a.asrSnapshot()
	injectMode := []string{config.InjectAuto, config.InjectUnicode, config.InjectClipboard}[a.injectMode]
	asrState := "未测试"
	switch {
	case result == "":
	case ok:
		asrState = "可用"
	default:
		asrState = "失败"
	}
	return [][2]string{
		{"配置目录", config.Dir()},
		{"配置文件", a.cfg.Path()},
		{"设备连接", state + " " + detail},
		{"辅助功能授权", boolLabel(inject.Trusted(), "已授权", "未授权")},
		{"注入方式", injectMode},
		{"ASR", asrState},
		{"BLE 实现", "cbgo（CoreBluetooth）"},
	}
}

func boolLabel(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

// sortedKeys 是给「资源 ID」下拉用的固定选项说明（顺序稳定）。
func resourceOptions() (options []string, hints []string) {
	return []string{
		asr.DefaultResourceID,
		asr.ResourceID20,
	}, []string{
		"1.0 代，约 4.4 元/小时",
		"2.0 代，约 1 元/小时（更便宜，先用测试连接验证）",
	}
}
