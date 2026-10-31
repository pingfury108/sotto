#!/usr/bin/env bash
# 重新生成仓库里的签名身份（证书当代码管理，放在 scripts/signing/）。
#
# 平时**不需要跑**：证书已经提交在 scripts/signing/ 下，make dev / make build
# 会把它们导入项目内临时钥匙串（.signing/build.keychain-db），不碰登录钥匙串。
#
# 只有想换一张证书（例如旧的到期）时才用：
#   ./scripts/signing-cert.sh [名字]
# 然后把 scripts/signing/ 下生成的文件提交进仓库。
set -euo pipefail

NAME="${1:-sotto-dev}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="${ROOT}/scripts/signing"
PASS="${SIGN_P12_PASS:-sotto}"

mkdir -p "$DIR"
echo "==> 生成自签名代码签名证书：${NAME}"
openssl req -x509 -newkey rsa:2048 -sha256 -days 3650 -nodes \
  -keyout "${DIR}/${NAME}.key.pem" -out "${DIR}/${NAME}.cert.pem" \
  -subj "/CN=${NAME}" \
  -addext "basicConstraints=critical,CA:false" \
  -addext "keyUsage=critical,digitalSignature" \
  -addext "extendedKeyUsage=critical,codeSigning" >/dev/null 2>&1

# macOS 的 security import 只认传统 PKCS#12（-legacy），否则 MAC 校验失败
openssl pkcs12 -export -legacy \
  -inkey "${DIR}/${NAME}.key.pem" -in "${DIR}/${NAME}.cert.pem" \
  -name "${NAME}" -out "${DIR}/${NAME}.p12" -passout "pass:${PASS}" >/dev/null 2>&1

# 旧的项目钥匙串作废，下次构建会按新证书重建
rm -f "${ROOT}/.signing/build.keychain-db"

cat <<MSG

生成完成：scripts/signing/${NAME}.{key.pem,cert.pem,p12}
把它们提交进仓库（make dev / make build 会自动使用）。

提醒：私钥进仓库等于公开。这个自签名身份只用于本机/私人构建；
TCC 授权绑定的是本机这条记录，换台机器要重新授权一次。
MSG
