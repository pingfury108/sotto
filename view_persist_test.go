package main

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"sotto/internal/config"
)

// 界面里改的设置必须立刻落盘：这是"填了地址却没保存"的回归测试。
func TestAirvoiceFieldsPersist(t *testing.T) {
	dir := t.TempDir()
	a := newAppAt(dir)
	a.backend = backendAirvoice
	a.saveConfig() // 先写一次，模拟用户点过分段控件

	tt := ui.NewTester(a.view, 540, 820)
	if err := tt.Click("局域网地址"); err != nil {
		t.Fatalf("找不到地址输入框: %v", err)
	}
	tt.Type("192.168.32.145:8787")

	if got := a.avURL; got != "192.168.32.145:8787" {
		t.Fatalf("输入没进绑定值: %q", got)
	}
	if got := config.Load(dir).Get().AirvoiceURL; got != "192.168.32.145:8787" {
		t.Fatalf("地址没有落盘，磁盘上是 %q", got)
	}

	if err := tt.Click("局域网 Token"); err != nil {
		t.Fatalf("找不到 Token 输入框: %v", err)
	}
	tt.Type("s3cret")
	if got := config.Load(dir).Get().AirvoiceToken; got != "s3cret" {
		t.Fatalf("Token 没有落盘，磁盘上是 %q", got)
	}
}

// 火山的 Key 同理（历史上只有切换别的控件时才会顺带保存）。
func TestVolcanoKeyPersists(t *testing.T) {
	dir := t.TempDir()
	a := newAppAt(dir)
	a.backend = backendVolcano

	tt := ui.NewTester(a.view, 540, 820)
	if err := tt.Click("API Key"); err != nil {
		t.Fatalf("找不到 Key 输入框: %v", err)
	}
	tt.Type("my-volcano-key")
	if got := config.Load(dir).Get().VolcanoAPIKey; got != "my-volcano-key" {
		t.Fatalf("Key 没有落盘，磁盘上是 %q", got)
	}
}

// 每个设置项都必须能存下来再读回去（漏一个字段就会"改了没记住"，
// 更糟的是重启后表单是空的，下一次保存会把磁盘上的正确值覆盖成空串）。
func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()

	a1 := newAppAt(dir)
	a1.backend = backendAirvoice
	a1.avURL = "192.168.1.9:8787"
	a1.avToken = "tok"
	a1.apiKey = "volcano-key"
	a1.wsURL = "wss://example/ws"
	a1.resource = "volc.seedasr.sauc.concurrent"
	a1.model = "bigmodel"
	a1.nonstream = false
	a1.hotwords = "伏羲, sotto"
	a1.contexts = "上一句"
	a1.endWindow = 1500
	a1.ddc = true
	a1.deviceName = "My Passport"
	a1.autoConn = false
	a1.injectMode = 2
	a1.injectText = "测试文本"
	a1.showFloat = false
	a1.saveConfig()

	a2 := newAppAt(dir)
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"后端", a2.backend, backendAirvoice},
		{"局域网地址", a2.avURL, "192.168.1.9:8787"},
		{"局域网 Token", a2.avToken, "tok"},
		{"火山 Key", a2.apiKey, "volcano-key"},
		{"端点", a2.wsURL, "wss://example/ws"},
		{"资源 ID", a2.resource, "volc.seedasr.sauc.concurrent"},
		{"模型", a2.model, "bigmodel"},
		{"二遍识别", a2.nonstream, false},
		{"热词", a2.hotwords, "伏羲, sotto"},
		{"上下文", a2.contexts, "上一句"},
		{"判停", a2.endWindow, 1500},
		{"语义顺滑", a2.ddc, true},
		{"设备名", a2.deviceName, "My Passport"},
		{"自动连接", a2.autoConn, false},
		{"注入方式", a2.injectMode, 2},
		{"注入示例", a2.injectText, "测试文本"},
		{"悬浮窗", a2.showFloat, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s 没读回来: %v（want %v）", c.name, c.got, c.want)
		}
	}

	// 引擎快照也要跟上（会话层读的是它，不是表单）
	a2.mu.Lock()
	backend, url, token := a2.engineBackend, a2.engineAVURL, a2.engineAVToken
	mode := a2.injectModeStr
	a2.mu.Unlock()
	if backend != backendAirvoice || url != "192.168.1.9:8787" || token != "tok" {
		t.Fatalf("引擎快照不对: %d / %q / %q", backend, url, token)
	}
	if mode != "clipboard" {
		t.Fatalf("注入方式快照不对: %q", mode)
	}
	if a2.showFloatFlag.Load() || a2.backendFlag.Load() != backendAirvoice {
		t.Fatal("原子镜像没跟上")
	}
}
