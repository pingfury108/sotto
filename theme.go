package main

import (
	"strings"

	"github.com/egoist/mygo/ui"
)

// ─── 设计 token（全站唯一定义处）──────────────────────────────────
//
// 配色沿用设备端的设计规范（固件 README：暖白 #FAF8F4 / off-black #1D1D1F /
// 琥珀棕 #7C6853，夜间 #1A1A1A / #E8E6E1 / #B8A188）：桌面端和手里的设备
// 看着是同一个产品，而不是"又一个用默认配色的工具窗口"。
const (
	lightBg      = "#FAF8F4"
	lightSurface = "#FFFFFF"
	lightLine    = "#E8E1D7"
	lightText    = "#1D1D1F"
	lightMuted   = "#7A736B"
	lightAccent  = "#7C6853" // 琥珀棕
	lightHover   = "#6A5744"
	lightPressed = "#58483A"

	darkBg      = "#1A1A1A"
	darkSurface = "#242422"
	darkLine    = "#37332E"
	darkText    = "#E8E6E1"
	darkMuted   = "#A39A8F"
	darkAccent  = "#B8A188"
	darkHover   = "#CBB69E"
	darkPressed = "#9C8870"

	successColor = "#3F8F5A"
	warningColor = "#B4762A"
	dangerColor  = "#B4443A"
)

// 间距：4pt 网格；圆角：控件 < 行 < 卡片；字号：1.25 比例。
const (
	sp1 float32 = 4
	sp2 float32 = 8
	sp3 float32 = 12
	sp4 float32 = 16
	sp6 float32 = 24
)

const (
	rCtl  float32 = 6
	rRow  float32 = 10
	rCard float32 = 14
)

const (
	fsCaption  float32 = 11
	fsBody     float32 = 13
	fsEmphasis float32 = 15
	fsTitle    float32 = 21
	fsMono     float32 = 11.5
)

// fsMonoFamily 是系统等宽字体族名。
const fsMonoFamily = "monospace"

// appTheme 在框架主题上换掉整套颜色，保留深浅色跟随。
func appTheme(dark bool) *ui.Theme {
	t := ui.LightTheme()
	if dark {
		t = ui.DarkTheme()
		t.Background = ui.Hex(darkBg)
		t.Surface = ui.Hex(darkSurface)
		t.SurfaceHover = ui.Hex("#2D2D2A")
		t.SurfacePressed = ui.Hex("#33332F")
		t.Border = ui.Hex(darkLine)
		t.Text = ui.Hex(darkText)
		t.TextMuted = ui.Hex(darkMuted)
		t.Accent = ui.Hex(darkAccent)
		t.AccentHover = ui.Hex(darkHover)
		t.AccentPressed = ui.Hex(darkPressed)
		t.AccentText = ui.Hex(darkBg)
		t.Danger = ui.Hex("#D9614F")
		t.Warning = ui.Hex("#D19A4B")
		t.Success = ui.Hex("#5FA874")
		t.Selection = ui.RGBA(184, 161, 136, 0.28)
		t.Focus = ui.RGBA(184, 161, 136, 0.55)
	} else {
		t.Background = ui.Hex(lightBg)
		t.Surface = ui.Hex(lightSurface)
		t.SurfaceHover = ui.Hex("#F5F1EA")
		t.SurfacePressed = ui.Hex("#EDE7DC")
		t.Border = ui.Hex(lightLine)
		t.Text = ui.Hex(lightText)
		t.TextMuted = ui.Hex(lightMuted)
		t.Accent = ui.Hex(lightAccent)
		t.AccentHover = ui.Hex(lightHover)
		t.AccentPressed = ui.Hex(lightPressed)
		t.AccentText = ui.Hex("#FFFFFF")
		t.Danger = ui.Hex(dangerColor)
		t.Warning = ui.Hex(warningColor)
		t.Success = ui.Hex(successColor)
		t.Selection = ui.RGBA(124, 104, 83, 0.20)
		t.Focus = ui.RGBA(124, 104, 83, 0.50)
		t.Scrollbar = ui.RGBA(29, 29, 31, 0.22)
	}
	t.Spacing = sp1
	t.Radius = rCtl
	return t
}

// cardLine 是卡片描边：暖色浅线，让白卡在暖白底上有清晰边界。
func cardLine(t *ui.Theme) ui.Color {
	return t.Border
}

// okSoft / warnSoft / dangerSoft 是状态条底色：只声明状态，不参与文字语义。
func okSoft(t *ui.Theme) ui.Color {
	if t.Dark {
		return ui.RGBA(95, 168, 116, 0.18)
	}
	return ui.RGBA(63, 143, 90, 0.12)
}

func warnSoft(t *ui.Theme) ui.Color {
	if t.Dark {
		return ui.RGBA(209, 154, 75, 0.18)
	}
	return ui.RGBA(180, 118, 42, 0.12)
}

func dangerSoft(t *ui.Theme) ui.Color {
	if t.Dark {
		return ui.RGBA(217, 97, 79, 0.18)
	}
	return ui.RGBA(180, 68, 58, 0.10)
}

// statusColor 把连接状态映射成状态点颜色。
func statusColor(t *ui.Theme, state string) ui.Color {
	switch state {
	case "connected":
		return t.Success
	case "connecting":
		return t.Warning
	case "error":
		return t.Danger
	}
	return t.TextMuted
}

// logLineColor 按关键字给日志行着色，只做级别区分，不做语法高亮。
func logLineColor(t *ui.Theme, line string) ui.Color {
	switch {
	case containsAny(line, "错误", "失败", "error", "ERRO"):
		return t.Danger
	case containsAny(line, "警告", "warn"):
		return t.Warning
	case containsAny(line, "已连接", "成功", "定稿", "OK"):
		return t.Success
	}
	return t.TextMuted
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
