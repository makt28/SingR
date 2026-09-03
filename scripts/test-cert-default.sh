#!/usr/bin/env bash
# 默认证书路径 + 证书状态列的回归测试。
#
# 为什么需要它：certificate_path/key_path 为空时代入默认路径这条规则同时存在于
# Go（cmd/sing-box/cmd_run.go 的 applyDefaultCertificatePaths，由
# cmd/sing-box/cmd_run_test.go 守着）和 shell（SingR.sh / SingR-docker.sh 的
# SYNC BLOCK）。两份实现漂移的后果很难查：singr list 会给一个根本起不来的配置报
# OK。本脚本守 shell 那一侧。
#
# 直接从 SingR.sh 里抠出 SYNC BLOCK 来跑 —— 不 source 整个脚本（它末尾会进菜单），
# 也不复制一份函数（复制就等于又多一处会漂移的实现）。
#
#   bash scripts/test-cert-default.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

FAILED=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; echo "         want: [$2]"; echo "         got:  [$3]"; FAILED=1; }
check() { [[ "$2" == "$3" ]] && pass "$1" || fail "$1" "$2" "$3"; }

# ---- 抽出 SYNC BLOCK ----
extract_block() {
    local file="$1" start end
    start="$(grep -n '^# >>>>>>>>>>>>>>>> SYNC BLOCK' "${file}" | head -1 | cut -d: -f1)"
    end="$(grep -n '^# <<<<<<<<<<<<<<<< SYNC BLOCK' "${file}" | head -1 | cut -d: -f1)"
    [[ -n "${start}" && -n "${end}" ]] || { echo "找不到 SYNC BLOCK 标记：${file}" >&2; exit 1; }
    sed -n "${start},${end}p" "${file}"
}

CERT_DIR="${TMP}/certs"
mkdir -p "${CERT_DIR}"
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo "CERT_DIR=\"${CERT_DIR}\""
    echo "SERVER_CONFIG=\"${TMP}/server.json\""
    echo "PANEL_CONFIG=\"${TMP}/panel.json\""
    extract_block "${REPO_DIR}/SingR.sh"
} > "${TMP}/block.sh"

run() { bash -c "source '${TMP}/block.sh'; $1" 2>/dev/null; }

echo "== SYNC BLOCK 在两个脚本里逐字相同 =="
if diff <(extract_block "${REPO_DIR}/SingR.sh") <(extract_block "${REPO_DIR}/SingR-docker.sh") >/dev/null; then
    pass "SingR.sh 与 SingR-docker.sh 的 SYNC BLOCK 一致"
else
    fail "SingR.sh 与 SingR-docker.sh 的 SYNC BLOCK 一致" "identical" "differs"
fi

echo "== 默认路径探测顺序（须与 Go 侧 defaultCertificateNames 一致）=="
check "都不存在时落到 default.pem" "${CERT_DIR}/default.pem" "$(run 'node_default_cert_path')"
: > "${CERT_DIR}/default.crt"; echo x > "${CERT_DIR}/default.crt"
check "只有 default.crt 时用 .crt"  "${CERT_DIR}/default.crt" "$(run 'node_default_cert_path')"
echo x > "${CERT_DIR}/default.pem"
check "两个都在时 .pem 优先"        "${CERT_DIR}/default.pem" "$(run 'node_default_cert_path')"
rm -f "${CERT_DIR}/default.crt"
check "私钥名固定 default.key"      "${CERT_DIR}/default.key" "$(run 'node_default_key_path')"

echo "== node_effective_cert =="
cat > "${TMP}/server.json" <<JSON
{ "inbounds": [
  { "type":"anytls",   "tag":"empty-in", "tls":{"enabled":true,"certificate_path":"","key_path":""} },
  { "type":"anytls",   "tag":"explicit-in","tls":{"enabled":true,"certificate_path":"/x/a.pem","key_path":"/x/a.key"} },
  { "type":"anytls",   "tag":"half-cert-in","tls":{"enabled":true,"certificate_path":"/x/a.pem","key_path":""} },
  { "type":"anytls",   "tag":"half-key-in", "tls":{"enabled":true,"certificate_path":"","key_path":"/x/a.key"} }
] }
JSON
check "两个都空 -> 代入默认" \
    "${CERT_DIR}/default.pem|${CERT_DIR}/default.key" "$(run 'node_effective_cert empty-in')"
check "显式路径原样返回" "/x/a.pem|/x/a.key" "$(run 'node_effective_cert explicit-in')"
# 半配置绝不能各自补默认：Go 侧 certificateUnset 要求两个都空才替换，
# 只填一个时二进制会以 missing certificate / missing key 拒绝启动。
check "只有 cert -> key 保持空（不补默认）" "/x/a.pem|" "$(run 'node_effective_cert half-cert-in')"
check "只有 key  -> cert 保持空（不补默认）" "|/x/a.key" "$(run 'node_effective_cert half-key-in')"

echo "== node_inbound_exists 与'证书为空'是两回事 =="
run 'node_inbound_exists empty-in' && pass "存在的 tag 返回 0" || fail "存在的 tag 返回 0" 0 1
run 'node_inbound_exists ghost-in' && fail "不存在的 tag 返回非 0" "non-zero" 0 || pass "不存在的 tag 返回非 0"

echo "== node_cert_status =="
if command -v openssl >/dev/null 2>&1; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 90 \
        -keyout "${CERT_DIR}/default.key" -out "${CERT_DIR}/default.pem" -subj "/CN=t" 2>/dev/null
    openssl req -x509 -newkey rsa:2048 -nodes -days 10 \
        -keyout "${TMP}/soon.key" -out "${TMP}/soon.pem" -subj "/CN=s" 2>/dev/null
    long_status="$(run "node_cert_status ${CERT_DIR}/default.pem")"
    soon_status="$(run "node_cert_status ${TMP}/soon.pem")"
    case "${long_status}" in
        "OK 剩 89 天"|"OK 剩 90 天"|OK) pass "长期证书显示剩余天数（${long_status}）" ;;
        *) fail "长期证书显示剩余天数" "OK 剩 89/90 天" "${long_status}" ;;
    esac
    case "${soon_status}" in
        "OK 剩 9 天"|"OK 剩 10 天"|OK) pass "临期证书显示剩余天数（${soon_status}）" ;;
        *) fail "临期证书显示剩余天数" "OK 剩 9/10 天" "${soon_status}" ;;
    esac
    echo "not a certificate" > "${TMP}/junk.pem"
    check "读不出的文件退回纯 OK（不能报成已过期）" "OK" "$(run "node_cert_status ${TMP}/junk.pem")"
else
    echo "  skip - 没有 openssl，跳过证书状态用例"
fi

echo "== node_list 的 CERT 列 =="
cat > "${TMP}/panel.json" <<'JSON'
{ "name":"t", "nodes":[
  {"intag":"empty-in",     "apiconfig":{"apihost":"https://a.example.com","nodeid":1}},
  {"intag":"half-cert-in", "apiconfig":{"apihost":"https://a.example.com","nodeid":2}},
  {"intag":"explicit-in",  "apiconfig":{"apihost":"https://a.example.com","nodeid":3}},
  {"intag":"ghost-in",     "apiconfig":{"apihost":"https://a.example.com","nodeid":4}}
] }
JSON
listing="$(run 'node_list')"
grep -q "empty-in .*OK"        <<<"${listing}" && pass "默认路径就位的节点显示 OK" \
    || fail "默认路径就位的节点显示 OK" "OK" "$(grep empty-in <<<"${listing}")"
grep -q "half-cert-in .*配置不全" <<<"${listing}" && pass "半配置节点显示 配置不全" \
    || fail "半配置节点显示 配置不全" "配置不全" "$(grep half-cert-in <<<"${listing}")"
grep -q "explicit-in .*缺失"    <<<"${listing}" && pass "显式路径文件不存在显示 缺失" \
    || fail "显式路径文件不存在显示 缺失" "缺失" "$(grep explicit-in <<<"${listing}")"
grep -q "ghost-in .*无 inbound" <<<"${listing}" && pass "server.json 里没有的 tag 显示 无 inbound" \
    || fail "server.json 里没有的 tag 显示 无 inbound" "无 inbound" "$(grep ghost-in <<<"${listing}")"

echo
if [[ "${FAILED}" == 0 ]]; then
    echo "PASS: 默认证书路径 / 证书状态 测试通过"
else
    echo "FAIL: 有用例未通过"
fi
exit "${FAILED}"
