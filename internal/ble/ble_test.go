package ble

import "testing"

func TestUUID16(t *testing.T) {
	cases := map[string]string{
		// 常规 128 位基址派生
		"0000A2B0-0000-1000-8000-00805F9B34FB": "A2B0",
		"0000a2b1-0000-1000-8000-00805f9b34fb": "A2B1",
		// CoreBluetooth 有时收回成 16 位
		"A2B0": "A2B0",
		// 真机取证：NimBLE 的小端声明 + cbgo 整体逆序 → 16 位部分跑到末尾
		"FB349B5F-8000-8000-1000-00000000A2B0": "A2B0",
		"FB349B5F-8000-8000-1000-00000000A2B1": "A2B1",
		"FB349B5F-8000-8000-1000-00000000A2B3": "A2B3",
		// 32 位写法
		"0000A2B3": "A2B3",
		// 非基址 UUID：原样返回（16 位部分无意义）
		"6E400001-B5A3-F393-E0A9-E50E24DCCA9E": "6E400001B5A3F393E0A9E50E24DCCA9E",
	}
	for in, want := range cases {
		if got := uuid16(in); got != want {
			t.Fatalf("uuid16(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasCTRL(t *testing.T) {
	if !hasCTRL([]string{"FB349B5F-8000-8000-1000-00000000A2B1"}) {
		t.Fatal("反序 UUID 也应识别为 CTRL")
	}
	if hasCTRL([]string{"0000A2B2-0000-1000-8000-00805F9B34FB"}) {
		t.Fatal("EVENT 不应被当成 CTRL")
	}
}
