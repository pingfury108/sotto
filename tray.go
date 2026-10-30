package main

import (
	"bytes"
	"image"
	"image/png"
	"math"

	"github.com/egoist/mygo"
)

// setupTray 创建菜单栏图标。图标用模板图案（仅 alpha），跟随系统深浅色。
func (a *app) setupTray() {
	tray, err := mygo.NewTray(mygo.TrayOptions{
		ID:             "main",
		Icon:           trayPNG(),
		IconIsTemplate: true,
		ToolTip:        "sotto",
	})
	if err != nil {
		a.logf("托盘创建失败：%v", err)
		return
	}
	a.tray = tray
	a.refreshTray()
}

// refreshTray 同步托盘菜单与悬停提示。
func (a *app) refreshTray() {
	if a.tray == nil {
		return
	}
	state, _ := a.connSnapshot()
	a.tray.SetToolTip("sotto · " + statusLabel(state))

	connected := state == stateConnected || state == stateConnecting
	connItem := &mygo.MenuItem{Label: "连接设备", Click: func(*mygo.MenuItem, *mygo.Window) {
		a.connectDevice()
	}}
	if connected {
		connItem = &mygo.MenuItem{Label: "断开设备", Click: func(*mygo.MenuItem, *mygo.Window) {
			a.disconnectDevice()
		}}
	}

	a.tray.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
		{Label: "sotto · " + statusLabel(state), Disabled: true},
		mygo.Separator(),
		connItem,
		{Label: "预览悬浮窗", Click: func(*mygo.MenuItem, *mygo.Window) {
			a.showFloating("这是候选字的预览效果，按住设备说话时会实时更新。", "预览")
		}},
		{Label: "测试注入", Click: func(*mygo.MenuItem, *mygo.Window) {
			a.testInject(a.injectText, injectModeName(a.injectMode))
		}},
		mygo.Separator(),
		{Label: "打开 sotto", Click: func(*mygo.MenuItem, *mygo.Window) { a.showMain() }},
		{Role: mygo.RoleQuit},
	}))
}

// trayPNG 画一个 32×32 的波形图标（三条圆头竖柱，中间最高）。
// 4× 超采样后按覆盖率取 alpha，边缘才干净。
func trayPNG() []byte {
	const (
		size = 32
		ss   = 4
	)
	type bar struct{ cx, halfW, top, bottom float64 }
	bars := []bar{
		{10.5, 1.9, 12, 20},
		{16.0, 1.9, 7, 25},
		{21.5, 1.9, 11, 21},
	}
	inBar := func(b bar, x, y float64) bool {
		// 圆头竖柱 = 两个半圆 + 一个矩形
		if y >= b.top+b.halfW && y <= b.bottom-b.halfW {
			return math.Abs(x-b.cx) <= b.halfW
		}
		cy := b.top + b.halfW
		if y < cy {
			cy = b.top + b.halfW
		} else {
			cy = b.bottom - b.halfW
		}
		dx, dy := x-b.cx, y-cy
		return dx*dx+dy*dy <= b.halfW*b.halfW
	}

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			cover := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := float64(px) + (float64(sx)+0.5)/ss
					y := float64(py) + (float64(sy)+0.5)/ss
					for _, b := range bars {
						if inBar(b, x, y) {
							cover++
							break
						}
					}
				}
			}
			if cover == 0 {
				continue
			}
			a := uint8(255 * cover / (ss * ss))
			i := img.PixOffset(px, py)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 0, 0, 0, a
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}
