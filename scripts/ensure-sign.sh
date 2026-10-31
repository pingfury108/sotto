#!/usr/bin/env bash
# 构建前准备签名身份。
#
# 证书与私钥是**仓库里的资产**（scripts/signing/），不导入登录钥匙串；
# 每次构建用一个项目内的临时钥匙串，签完不污染系统：
#
#   .signing/build.keychain-db     ← 由 scripts/signing/<name>.p12 导入生成（gitignore）
#
# codesign 只认钥匙串里的身份，所以这里做三件事（都幂等）：
#   1. 没有临时钥匙串就建一个（密码固定在脚本里，纯本地文件）；
#   2. 把 p12 导进去，并授权 codesign 直接使用私钥（免得每次弹窗）；
#   3. 把这个钥匙串放进当前用户的搜索列表（否则 codesign 找不到）。
#
# 没有证书文件时什么都不做，构建回退 ad-hoc。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NAME="${1:-sotto-dev}"
P12="${ROOT}/scripts/signing/${NAME}.p12"
PASS="${SIGN_P12_PASS:-sotto}"
KC="${ROOT}/.signing/build.keychain-db"
KC_PASS="${SIGN_KEYCHAIN_PASS:-sotto}"

[[ ! -f "$P12" ]] && exit 0

mkdir -p "${ROOT}/.signing"

if [[ ! -f "$KC" ]]; then
  echo "[签名] 创建项目内临时钥匙串：.signing/build.keychain-db"
  security create-keychain -p "$KC_PASS" "$KC" >/dev/null
  security set-keychain-settings -lut 21600 "$KC" >/dev/null   # 6 小时后自动锁
  security unlock-keychain -p "$KC_PASS" "$KC" >/dev/null
  security import "$P12" -k "$KC" -P "$PASS" \
    -T /usr/bin/codesign -T /usr/bin/security >/dev/null
  security set-key-partition-list -S apple-tool:,apple:,codesign: \
    -s -k "$KC_PASS" "$KC" >/dev/null
else
  security unlock-keychain -p "$KC_PASS" "$KC" >/dev/null
fi

# 放进搜索列表（幂等）。只在列表里没有时追加，不动其它钥匙串。
if ! security list-keychains -d user | grep -Fq "build.keychain-db"; then
  # shellcheck disable=SC2046
  security list-keychains -d user -s $(security list-keychains -d user | sed 's/^[[:space:]]*//;s/"//g') "$KC" >/dev/null
fi

# 自签名证书不在 find-identity -v（"valid identity"）里，所以不做那道校验；
# 身份解析失败时 codesign 会直接给出 "no identity found"。
exit 0
