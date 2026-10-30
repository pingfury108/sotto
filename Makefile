# mygo CLI 在 CGO_ENABLED 未设置时会强制传 CGO_ENABLED=0，
# 而 BLE 走 cbgo（cgo）——没有 cgo 的话 cbgo 的类型定义全部丢失，
# 报错会是一堆 "undefined: CentralManager"，与真实原因无关。
# 所以这里统一在命令行前缀上带 CGO_ENABLED=1。
CGO := CGO_ENABLED=1

.PHONY: dev build test probe tidy

# 开发运行（首次会弹蓝牙授权）
dev:
	$(CGO) go tool mygo dev

# 打包 .app / .dmg
build:
	$(CGO) go tool mygo build

# 纯逻辑包单测（不依赖 cgo）
test:
	go test ./internal/...

# 命令行闸门（M0/M1）
probe:
	$(CGO) go run ./cmd/probe

# 只验火山 ASR 的 Key / 资源 ID（不需要设备，不消耗识别时长）
asr:
	go run ./cmd/asrcheck

# 手工验注入链路（授权 → 聚焦元素 → 注入 → 落地校验）
inject:
	go run ./cmd/injectcheck

tidy:
	go mod tidy
