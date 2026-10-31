package main

import (
	"log"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"sotto/internal/inject"
)

func main() {
	// 单实例：再次点图标时把已有实例的窗口叫出来，而不是静默退出。
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}

	a := newApp()
	mygo.App.OnSecondInstance(func([]string, string) { a.showMain() })

	mygo.App.WhenReady(func() {
		a.startEventPump()

		a.ensureMainWindow()
		// 点 Dock 图标时把窗口叫回来（macOS 走 applicationShouldHandleReopen）
		mygo.App.OnActivate(func(hasVisibleWindows bool) {
			if !hasVisibleWindows {
				a.showMain()
			}
		})

		a.float = a.newFloatingWindow()
		a.setupTray()

		a.logf("sotto 启动，配置：%s", a.cfg.Path())
		// 权限是按 bundle id + 签名记账的：dev 与正式包是两个身份，ad-hoc 重建还会变。
		a.logf("应用标识：%s", bundleIDForDisplay())
		if inject.Trusted() {
			a.logf("辅助功能：已授权，注入可用")
		} else {
			a.logf("辅助功能未授权：注入会被系统静默丢弃（系统设置 → 隐私与安全性 → 辅助功能）")
		}

		if a.cfg.Get().AutoConnect {
			go a.connectDevice()
		}
	})

	mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) {
		a.quitting.Store(true)
		a.saveConfig() // 退出前再存一次
		// 会话收尾（可能含注入/写 CTRL）不该挡住退出流程。
		go a.disconnectDevice()
		// 退出若被某个窗口的 OnClose 拒绝（PreventDefault），mygo 会放弃本次退出
		// 并继续运行；这里兜底复位标志，否则主窗从此变成"一关就销毁"。
		time.AfterFunc(500*time.Millisecond, func() {
			if !a.quitConfirmed.Load() {
				a.quitting.Store(false)
			}
		})
	})
	mygo.App.OnWillQuit(func(e *mygo.QuitEvent) { a.quitConfirmed.Store(true) })

	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// createMainWindow 创建主窗并绑定关闭语义：默认"关窗即收起"，退出时放行。
func (a *app) createMainWindow() *mygo.Window {
	w := mygo.NewWindow(mygo.WindowOptions{
		Title:     "sotto",
		Width:     540,
		Height:    820,
		MinWidth:  480,
		MinHeight: 600,
		Content:   ui.View(a.view),
	})
	w.OnClose(func(e *mygo.CloseEvent) {
		if a.quitting.Load() {
			return // 退出流程：放行，让 mygo 真正销毁窗口
		}
		e.PreventDefault()
		e.Window.Hide()
		a.saveConfig() // 兜底：万一边输入边关窗，别丢最后一次改动
		a.logf("主窗已收起（托盘与设备连接仍在）")
	})
	return w
}

// ensureMainWindow 保证 a.win 指向一个活着的窗口；已销毁则重建。
//
// 重建是必需的：一旦窗口被销毁（例如某次退出被取消、或用户强制关闭），
// 之后 Show()/Focus() 都是空操作，托盘与 Dock 图标就会"点了没反应"。
func (a *app) ensureMainWindow() *mygo.Window {
	if a.win != nil && !a.win.IsDestroyed() {
		return a.win
	}
	a.win = a.createMainWindow()
	return a.win
}

// showMain 把主窗带到前台（需要时重建）。
func (a *app) showMain() {
	w := a.ensureMainWindow()
	w.Show()
	w.Focus()
}
