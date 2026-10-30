package main

import (
	"log"

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

		a.win = mygo.NewWindow(mygo.WindowOptions{
			Title:     "sotto",
			Width:     540,
			Height:    820,
			MinWidth:  480,
			MinHeight: 600,
			Content:   ui.View(a.view),
		})
		// 关窗只是收起：托盘还在，设备也还连着。
		a.win.OnClose(func(e *mygo.CloseEvent) {
			if a.quitting {
				return
			}
			e.PreventDefault()
			a.win.Hide()
		})
		mygo.App.OnActivate(func(hasVisibleWindows bool) {
			if !hasVisibleWindows {
				a.showMain()
			}
		})

		a.float = a.newFloatingWindow()
		a.setupTray()

		a.logf("sotto 启动，配置：%s", a.cfg.Path())
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
		a.quitting = true
		a.disconnectDevice()
	})

	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// showMain 把主窗带到前台。
func (a *app) showMain() {
	if a.win == nil {
		return
	}
	a.win.Show()
	a.win.Focus()
}
