package main

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"

	"sotto/internal/asr"
	"sotto/internal/config"
)

// view 是主窗根视图：安装主题、排队 toast，再按 page 渲染。
func (a *app) view(c *ui.Context) {
	if t := c.Theme(); !a.themeApplied || a.themeDark != t.Dark {
		c.SetTheme(appTheme(t.Dark))
		a.themeApplied, a.themeDark = true, t.Dark
	}
	for _, tt := range a.toasts {
		c.AddToast(tt)
	}
	a.toasts = nil

	switch a.page {
	case "settings":
		a.viewSettings(c)
	case "diag":
		a.viewDiag(c)
	default:
		a.viewMain(c)
	}
}

// ─── 通用组件 ─────────────────────────────────────────────────────
//
// 设计约定（CRAP + 中心点）：
//   - 一个屏幕只有一个视觉主角：主界面的连接状态块；其余元素一律更小、更灰；
//   - 卡片只有一种样式（Surface 底 + rCard 圆角 + 16 内距），用重复建立品牌；
//   - 所有元素左对齐到同一条 16pt 槽线，标签列固定 76pt；
//   - 只有主操作是 PrimaryButton，其余都是 Button（避免互相争夺注意力）。

func card(c *ui.Context, body func()) {
	t := c.Theme()
	ui.Box(c).FillWidth().Padding(sp4).Radius(rCard).
		Background(t.Surface).Border(1, cardLine(t)).Children(body)
}

func sectionTitle(c *ui.Context, s string) {
	ui.Text(c, s).FontSize(fsCaption).Bold().
		TextColor(c.Theme().TextMuted).LetterSpacing(0.6)
}

// field 是「左标签 + 右控件」的横向表单行。
func field(c *ui.Context, label string, control func()) {
	t := c.Theme()
	ui.Row(c).FillWidth().Gap(sp3).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).FontSize(fsBody).TextColor(t.TextMuted).Width(76).Shrink(0)
		ui.Box(c).Grow(1).Children(control)
	})
}

// settingRow 是「左标签+描述 + 右开关」。
func settingRow(c *ui.Context, label, desc string, control func()) {
	t := c.Theme()
	ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(sp3).Children(func() {
		ui.Column(c).Grow(1).Gap(1).Children(func() {
			ui.Text(c, label).FontSize(fsBody)
			if desc != "" {
				ui.Text(c, desc).FontSize(fsCaption).TextColor(t.TextMuted).Wrap()
			}
		})
		control()
	})
}

func kvRow(c *ui.Context, key, val string) {
	t := c.Theme()
	ui.Row(c).FillWidth().Gap(sp3).AlignItems(ui.Start).Children(func() {
		ui.Text(c, key).FontSize(fsBody).TextColor(t.TextMuted).Width(96).Shrink(0)
		ui.Text(c, val).FontSize(fsBody).Grow(1).Wrap()
	})
}

func hint(c *ui.Context, s string) {
	ui.Text(c, s).FontSize(fsCaption).TextColor(c.Theme().TextMuted).Wrap()
}

// statusPill 是状态条：小圆点 + 一行说明。tone 取 "ok" / "warn" / "error"。
func statusPill(c *ui.Context, tone, text string) {
	t := c.Theme()
	var bg, dot ui.Color
	switch tone {
	case "ok":
		bg, dot = okSoft(t), statusColor(t, stateConnected)
	case "warn":
		bg, dot = warnSoft(t), statusColor(t, stateConnecting)
	default:
		bg, dot = dangerSoft(t), statusColor(t, stateError)
	}
	ui.Row(c).FillWidth().Gap(sp2).AlignItems(ui.Center).
		Padding(sp2, sp3).Radius(rCtl).Background(bg).Children(func() {
		ui.Box(c).Width(6).Height(6).Radius(3).Shrink(0).Background(dot)
		ui.Text(c, text).FontSize(fsCaption).Grow(1).Wrap()
	})
}

func statusLabel(state string) string {
	switch state {
	case stateConnected:
		return "已连接"
	case stateConnecting:
		return "连接中"
	case stateError:
		return "异常"
	}
	return "未连接"
}

func injectModeName(i int) string {
	switch i {
	case 1:
		return config.InjectUnicode
	case 2:
		return config.InjectClipboard
	}
	return config.InjectAuto
}

// pageHeader 是二级页的返回栏。
func pageHeader(c *ui.Context, title string, back func()) {
	ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(sp2).Children(func() {
		ui.Button(c, "←").Tooltip("返回").OnClick(back)
		ui.Text(c, title).FontSize(fsEmphasis).Bold().Grow(1)
	})
}

// ─── 主界面 ───────────────────────────────────────────────────────

func (a *app) viewMain(c *ui.Context) {
	ui.Column(c).Fill().Children(func() {
		a.viewHeader(c)
		// 中段可滚：三块内容，顺序就是使用顺序（先连上、再配对 Key、最后看输出）。
		ui.Scroll(c).Fill().Children(func() {
			ui.Column(c).FillWidth().Padding(sp4).Gap(sp4).Children(func() {
				a.cardStatus(c)
				a.cardASR(c)
				a.cardOutput(c)
			})
		})
		a.panelLog(c)
	})
}

func (a *app) viewHeader(c *ui.Context) {
	t := c.Theme()
	state, _ := a.connSnapshot()
	ui.Column(c).FillWidth().Padding(sp4, sp4, sp2).Gap(sp1).Children(func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(sp2).Children(func() {
			ui.Text(c, "sotto").FontSize(fsTitle).Bold().Grow(1)
			ui.Box(c).Width(6).Height(6).Radius(3).Background(statusColor(t, state))
			ui.Text(c, statusLabel(state)).FontSize(fsCaption).TextColor(t.TextMuted)
			ui.Button(c, "设置").FontSize(fsCaption).OnClick(func() { a.page = "settings" })
		})
		ui.Text(c, "按住设备说话，松手即注入当前输入框").FontSize(fsCaption).TextColor(t.TextMuted)
		ui.Box(c).FillWidth().Height(1).Background(t.Border).Opacity(0.5)
	})
}

// cardStatus 是页面的视觉主角：设备是谁、现在什么状态、下一步做什么。
func (a *app) cardStatus(c *ui.Context) {
	t := c.Theme()
	state, detail := a.connSnapshot()
	connected := state == stateConnected
	busy := state == stateConnecting

	title := "未连接设备"
	sub := "点右侧按钮，扫到 " + a.deviceName + " 后自动配对"
	switch {
	case connected:
		title = a.deviceName
		if detail == "" {
			sub = "已连接，可以按住设备说话"
		} else {
			sub = detail
		}
	case busy:
		title = "连接中…"
		sub = detail
	case state == stateError:
		title = "连接失败"
		sub = detail
	}

	card(c, func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(sp3).Children(func() {
			ui.Box(c).Width(10).Height(10).Radius(5).Shrink(0).
				Background(statusColor(t, state))
			ui.Column(c).Grow(1).Gap(2).Children(func() {
				ui.Text(c, title).FontSize(fsTitle).Bold().SingleLine()
				ui.Text(c, sub).FontSize(fsCaption).TextColor(t.TextMuted).Wrap()
			})
			if connected || busy {
				ui.Button(c, "断开").OnClick(func() { a.disconnectDevice() })
			} else {
				ui.PrimaryButton(c, "连接").OnClick(func() { a.connectDevice() })
			}
		})
		if busy {
			ui.Row(c).FillWidth().Gap(sp2).AlignItems(ui.Center).Padding(sp2, 0, 0, 0).Children(func() {
				ui.Spinner(c)
				hint(c, "正在与设备握手，首次会弹蓝牙授权")
			})
		}
	})
}

func (a *app) cardASR(c *ui.Context) {
	_, ok, result := a.asrSnapshot()
	card(c, func() {
		ui.Column(c).FillWidth().Gap(sp3).Children(func() {
			sectionTitle(c, "语音识别")

			// 后端：云端（有候选字）或局域网（整段识别，零出站）。
			if ui.Segmented(c, &a.backend, "云端（火山）", "局域网（空音）").Changed() {
				a.asrReset()
				a.saveConfig()
			}

			if a.backend == backendAirvoice {
				a.fieldsAirvoice(c)
			} else {
				a.fieldsVolcano(c)
			}

			switch {
			case result != "" && ok:
				statusPill(c, "ok", result)
			case result != "":
				statusPill(c, "error", result)
			case a.backend == backendAirvoice && a.avURL == "":
				statusPill(c, "warn", "还没有填局域网地址：手机 App 通知栏里显示的那个 host:port")
			case a.backend == backendVolcano && a.apiKey == "":
				statusPill(c, "warn", "还没有填 API Key：填好后点「测试」验证")
			}
		})
	})
}

// fieldsVolcano 是云端（火山流式）那一组字段。
func (a *app) fieldsVolcano(c *ui.Context) {
	t := c.Theme()
	testing, _, _ := a.asrSnapshot()
	idx := resourceIndex(a.resource)

	ui.Row(c).FillWidth().Gap(sp2).AlignItems(ui.Center).Children(func() {
		if ui.TextInput(c, &a.apiKey).Placeholder("粘贴火山 API Key").Password().Grow(1).Changed() {
			a.saveConfig()
		}
		cfg := a.formASR()
		ui.Button(c, "测试").OnClick(func() { a.testASR(cfg) })
		if testing {
			ui.Spinner(c)
		}
	})

	ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
		ui.Text(c, "资源").FontSize(fsBody).TextColor(t.TextMuted).Width(76).Shrink(0)
		if ui.Segmented(c, &idx, resourceChoices[0].label, resourceChoices[1].label).Changed() {
			a.resource = resourceChoices[idx].id
			a.saveConfig()
		}
		ui.Box(c).Grow(1)
	})
	hint(c, resourceChoices[idx].price+" · "+resourceChoices[idx].id)
}

// fieldsAirvoice 是局域网（空音）那一组字段：只需要 host:port 与可选 Token。
func (a *app) fieldsAirvoice(c *ui.Context) {
	testing, _, _ := a.asrSnapshot()

	ui.Row(c).FillWidth().Gap(sp2).AlignItems(ui.Center).Children(func() {
		if ui.TextInput(c, &a.avURL).Placeholder("192.168.32.145:8787").Grow(1).Changed() {
			a.saveConfig()
		}
		url, token := a.avURL, a.avToken
		ui.Button(c, "测试").OnClick(func() { a.testAirvoice(url, token) })
		if testing {
			ui.Spinner(c)
		}
	})
	field(c, "Token", func() {
		if ui.TextInput(c, &a.avToken).Placeholder("留空 = 手机端未开鉴权").Password().Changed() {
			a.saveConfig()
		}
	})
	hint(c, "手机与电脑同一 WiFi，App 里打开「局域网识别 API」。只填 host:port，不加 http:// 与路径。"+
		"整段识别：没有实时候选字，松手后一次性出文本；音频不出内网、免费。")
}

func (a *app) cardOutput(c *ui.Context) {
	card(c, func() {
		ui.Column(c).FillWidth().Gap(sp3).Children(func() {
			sectionTitle(c, "输出")

			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, "注入方式").FontSize(fsBody).TextColor(c.Theme().TextMuted).Width(76).Shrink(0)
				if ui.Segmented(c, &a.injectMode, "自动", "键盘事件", "剪贴板").Changed() {
					a.saveConfig()
				}
				ui.Box(c).Grow(1)
			})

			settingRow(c, "候选字悬浮窗", "屏幕底部居中，始终不抢焦点", func() {
				if ui.Switch(c, &a.showFloat).Changed() {
					a.saveConfig()
					if !a.showFloat {
						a.hideFloating()
					}
				}
			})

			ui.Row(c).FillWidth().Gap(sp2).AlignItems(ui.Center).Children(func() {
				text, mode := a.injectText, injectModeName(a.injectMode)
				ui.Button(c, "试注入").OnClick(func() { a.testInject(text, mode) })
				ui.Button(c, "检查授权").OnClick(func() { a.logTrust() })
				ui.Button(c, "预览悬浮窗").OnClick(func() {
					a.showFloating("这是候选字的预览效果，按住设备说话时会实时更新。", "预览")
				})
				ui.Box(c).Grow(1)
			})
			hint(c, "「键盘事件」用 CGEvent 逐字符送 unicode，不碰剪贴板；自动模式失败时回退剪贴板。")
		})
	})
}

// panelLog 是底部固定日志面板：小字、等宽、低对比，永远不抢上面三块的戏。
func (a *app) panelLog(c *ui.Context) {
	t := c.Theme()
	lines := a.log.Tail(80)
	ui.Column(c).FillWidth().Padding(0, sp4, sp4).Children(func() {
		ui.Box(c).FillWidth().Height(150).Radius(rCard).Background(t.Surface).
			Border(1, cardLine(t)).Padding(sp2, sp3).Children(func() {
			ui.Column(c).Fill().Gap(sp1).Children(func() {
				ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(sp2).Children(func() {
					sectionTitle(c, "日志")
					ui.Box(c).Grow(1)
					ui.Button(c, "复制").FontSize(fsCaption).OnClick(func() {
						c.Services().WriteClipboard(a.log.Text())
					})
					ui.Button(c, "清空").FontSize(fsCaption).OnClick(func() { a.log.Clear() })
				})
				ui.Scroll(c).Fill().Children(func() {
					ui.Column(c).FillWidth().Children(func() {
						if len(lines) == 0 {
							hint(c, "暂无日志")
							return
						}
						for _, line := range lines {
							ui.Text(c, line).Font(fsMonoFamily).FontSize(fsMono).
								TextColor(logLineColor(t, line)).SingleLine()
						}
					})
				})
			})
		})
	})
}

// ─── 设置页 ───────────────────────────────────────────────────────

func (a *app) viewSettings(c *ui.Context) {
	ui.Column(c).Fill().Padding(sp4).Gap(sp3).Children(func() {
		pageHeader(c, "设置", func() { a.page = "main" })
		ui.Scroll(c).Fill().Children(func() {
			ui.Column(c).FillWidth().Gap(sp3).Children(func() {
				card(c, func() {
					ui.Column(c).FillWidth().Gap(sp3).Children(func() {
						sectionTitle(c, "设备")
						field(c, "广播名", func() {
							if ui.TextInput(c, &a.deviceName).Placeholder("AI Passport").Changed() {
								a.saveConfig()
							}
						})
						settingRow(c, "启动时自动连接", "", func() {
							if ui.Switch(c, &a.autoConn).Changed() {
								a.saveConfig()
							}
						})
					})
				})

				card(c, func() {
					ui.Column(c).FillWidth().Gap(sp3).Children(func() {
						sectionTitle(c, "云端识别（高级）")
						field(c, "资源 ID", func() {
							if ui.TextInput(c, &a.resource).Placeholder(asr.DefaultResourceID).Changed() {
								a.saveConfig()
							}
						})
						field(c, "端点", func() {
							if ui.TextInput(c, &a.wsURL).Placeholder(asr.DefaultWSURL).Changed() {
								a.saveConfig()
							}
						})
						field(c, "模型", func() {
							if ui.TextInput(c, &a.model).Placeholder(asr.DefaultModel).Changed() {
								a.saveConfig()
							}
						})
						settingRow(c, "二遍识别定稿", "enable_nonstream：最终文本更准，多等一次", func() {
							if ui.Switch(c, &a.nonstream).Changed() {
								a.saveConfig()
							}
						})
					})
				})

				card(c, func() {
					ui.Column(c).FillWidth().Gap(sp3).Children(func() {
						sectionTitle(c, "云端识别调优")
						field(c, "热词", func() {
							if ui.TextInput(c, &a.hotwords).Placeholder("伏羲, sotto, deploy.sh").Changed() {
								a.saveConfig()
							}
						})
						hint(c, "逗号分隔。用于人名、项目名、命令这类容易听错的词；词太多会互相干扰，建议 10 个以内。")
						field(c, "上下文", func() {
							if ui.TextInput(c, &a.contexts).Placeholder("上一句对话，可留空").Changed() {
								a.saveConfig()
							}
						})
						hint(c, "竖线分隔多条，按 user / bot 交替作为对话历史，帮助模型对齐专有名词。")
						ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
							ui.Text(c, "静音判停").FontSize(fsBody).TextColor(c.Theme().TextMuted).Width(76).Shrink(0)
							idx := endWindowIndex(a.endWindow)
							if ui.Segmented(c, &idx, "快 0.5s", "默认 0.8s", "耐等 1.5s").Changed() {
								a.endWindow = endWindowPresets[idx]
								a.saveConfig()
							}
							ui.Box(c).Grow(1)
						})
						hint(c, "连续静音达到该时长就判定一句话说完，并触发二遍识别重写这句。")
						settingRow(c, "语义顺滑", "enable_ddc：删掉停顿词、语气词、语义重复词", func() {
							if ui.Switch(c, &a.ddc).Changed() {
								a.saveConfig()
							}
						})
					})
				})

				card(c, func() {
					ui.Column(c).FillWidth().Gap(sp3).Children(func() {
						sectionTitle(c, "调试")
						field(c, "测试文本", func() {
							if ui.TextInput(c, &a.injectText).Placeholder("要注入的文字").Changed() {
								a.saveConfig()
							}
						})
						ui.Row(c).FillWidth().Gap(sp2).Children(func() {
							ui.Button(c, "诊断信息").OnClick(func() { a.page = "diag" })
							ui.Button(c, "复制配置路径").OnClick(func() {
								c.Services().WriteClipboard(a.cfg.Path())
							})
							ui.Box(c).Grow(1)
						})
					})
				})
			})
		})
	})
}

// ─── 诊断页 ───────────────────────────────────────────────────────

func (a *app) viewDiag(c *ui.Context) {
	ui.Column(c).Fill().Padding(sp4).Gap(sp3).Children(func() {
		pageHeader(c, "诊断信息", func() { a.page = "settings" })
		ui.Scroll(c).Fill().Children(func() {
			ui.Column(c).FillWidth().Gap(sp3).Children(func() {
				card(c, func() {
					ui.Column(c).FillWidth().Gap(sp2).Children(func() {
						for _, kv := range a.diag() {
							kvRow(c, kv[0], kv[1])
						}
					})
				})
				ui.Row(c).FillWidth().Gap(sp2).Children(func() {
					ui.Button(c, "复制日志").OnClick(func() {
						c.Services().WriteClipboard(a.log.Text())
					})
					ui.Button(c, "复制诊断").OnClick(func() {
						var b strings.Builder
						for _, kv := range a.diag() {
							fmt.Fprintf(&b, "%s：%s\n", kv[0], kv[1])
						}
						c.Services().WriteClipboard(b.String())
					})
					ui.Box(c).Grow(1)
				})
			})
		})
	})
}
