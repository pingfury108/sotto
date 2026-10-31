package main

import (
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// newFloatingWindow 创建候选字悬浮窗。
//
// 关键：Panel + ShowInactive，绝不调 Show()。Panel 在 macOS 上映射成
// nonactivating NSPanel（框架内部用 styleNonactivatingPanel +
// setHidesOnDeactivate:0 + canJoinAllSpaces/fullScreenAuxiliary），所以它
// 浮在所有窗口之上、跟随所有 Space、全屏 app 上也能显示，且**不抢焦点**——
// 抢了焦点，注入的文字就打进悬浮窗而不是用户的输入框。
func (a *app) newFloatingWindow() *mygo.Window {
	w := mygo.NewWindow(mygo.WindowOptions{
		Title:             "候选字",
		Panel:             true,
		Hidden:            true,
		Frameless:         true,
		Transparent:       true,
		AlwaysOnTop:       true,
		SkipTaskbar:       true,
		DisableResize:     true,
		DisableMinimize:   true,
		DisableMaximize:   true,
		DisableFullScreen: true,
		CornerRadius:      int(rCard),
		Width:             560,
		Height:            132,
		Content:           ui.View(a.viewFloating),
	})
	// 关窗只收起：面板没有关闭语义，误按 Cmd+W 不该把功能弄丢。
	//
	// 但退出时必须放行：mygo 的退出流程会关掉所有窗口（含悬浮窗），任何
	// 一个窗口 PreventDefault 都会让整次退出被取消——主窗已被销毁、悬浮窗
	// 又被拦住，app 就卡在"没有主窗、也退不出去"的状态。
	w.OnClose(func(e *mygo.CloseEvent) {
		if a.quitting.Load() {
			return
		}
		e.PreventDefault()
		w.Hide()
	})
	return w
}

// viewFloating 是悬浮窗内容：一行大号候选字 + 一枚状态脚注。
//
// 排版上只给两件事位置：文字（主角）和脚注（弱）。没有边框、没有按钮——
// 它出现的时间只有一两秒，不该让人分神去找操作。
func (a *app) viewFloating(c *ui.Context) {
	t := c.Theme()
	text, hintText := a.floatingSnapshot()
	ui.Box(c).Fill().Padding(sp3).Children(func() {
		ui.Column(c).FillWidth().Gap(sp2).Padding(sp3, sp4).Radius(rCard).
			Background(t.Surface).Children(func() {
			txt := ui.Text(c, text).FontSize(19).LineHeight(1.35).Wrap()
			// Bounds 是上一帧的布局结果：用文本实际高度反推窗口高度，
			// 两帧内收敛（宽度不变 → 文本高度稳定，不会来回抖）。
			if h := txt.Bounds().H; h > 0 {
				go a.fitFloatingHeight(h) // 不要在构建过程中改窗口尺寸
			}
			ui.Row(c).Gap(sp2).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Width(5).Height(5).Radius(2.5).Shrink(0).
					Background(statusColor(t, stateConnected))
				ui.Text(c, hintText).FontSize(fsCaption).TextColor(t.TextMuted)
			})
		})
	})
}
