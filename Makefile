# mygo CLI 在 CGO_ENABLED 未设置时会强制传 CGO_ENABLED=0，
# 而 BLE 走 cbgo（cgo）——没有 cgo 的话 cbgo 的类型定义全部丢失，
# 报错会是一堆 "undefined: CentralManager"，与真实原因无关。
# 所以这里统一在命令行前缀上带 CGO_ENABLED=1。
CGO := CGO_ENABLED=1

# ─── 代码签名身份 ────────────────────────────────────────────────
#
# 为什么必须固定：ad-hoc 签名的「指定要求」就是二进制的 cdhash
# （codesign -d -r- 可以自己看），每次重建都变，macOS 的 TCC（蓝牙 /
# 辅助功能）便认为换了个程序，已授权限全部失效。换成固定证书后
# 「指定要求」变成 identifier + certificate leaf，重建不再影响授权。
#
# 证书与私钥当仓库资产管理（scripts/signing/sotto-dev.{key,cert}.pem、.p12），
# 构建时导入项目内临时钥匙串 .signing/build.keychain-db（gitignore），不碰登录钥匙串：
#   make cert          # 一次性生成 + 导入钥匙串（会弹一次窗）
#
# 之后 make dev / make build 会在构建前自动确保身份可用（scripts/ensure-sign.sh），
# 没有证书文件时回退 ad-hoc 并提示。
# 证书名由 ./scripts/signing-cert.sh 写进 .signing-identity（已 gitignore）；
# 也可以用 make dev SIGN=xxx 显式覆盖，或 SIGN= 强制 ad-hoc。
SIGN ?= sotto-dev
SIGN_PRESENT := $(if $(strip $(SIGN)),$(shell test -f "scripts/signing/$(SIGN).p12" && echo 1),0)
ifeq ($(strip $(SIGN)),)
  SIGNFLAG :=
  $(info [签名] 已指定空身份：按 ad-hoc 签名（权限会在每次重建后失效）)
else ifeq ($(SIGN_PRESENT),0)
  SIGNFLAG :=
  $(info [签名] 未找到身份 "$(SIGN)"：回退 ad-hoc，权限会在每次重建后失效。跑一次 `make cert` 可根治。)
else
  SIGNFLAG := -sign "$(SIGN)"
  $(info [签名] 使用固定身份 "$(SIGN)"（重建不会丢权限）)
endif

.PHONY: dev build test probe asr inject tidy cert ensure-sign

# 证书在项目里，缺了自动导入钥匙串（无证书时是空操作）
ensure-sign:
	@./scripts/ensure-sign.sh "$(SIGN)"

# 开发运行（首次会弹蓝牙授权）
dev: ensure-sign
	$(CGO) go tool mygo dev $(SIGNFLAG)

# 打包 .app / .dmg
build: ensure-sign
	$(CGO) go tool mygo build $(SIGNFLAG)

# 只跑纯逻辑包的单测（不依赖 cgo / 真机）
test:
	go test ./...

# 命令行闸门（M0/M1：BLE + 音频 + ASR）
probe:
	$(CGO) go run ./cmd/probe

# 只验火山 ASR 的 Key / 资源 ID（不需要设备，不消耗识别时长）
asr:
	go run ./cmd/asrcheck

# 手工验注入链路（授权 → 聚焦元素 → 注入 → 落地校验）
inject:
	go run ./cmd/injectcheck

# 一次性创建固定的代码签名证书（之后 dev/build 自动使用）
cert:
	./scripts/signing-cert.sh $(SIGN)

tidy:
	go mod tidy
