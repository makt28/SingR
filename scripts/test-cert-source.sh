#!/usr/bin/env bash
# 默认证书更新源（SingR.sh / SingR-docker.sh 的 SYNC BLOCK「默认证书更新源」）的回归测试。
#
# 下载换成桩：cert_source_fetch 从本地的"远端目录"拷文件，systemctl 是空函数。
# node_backend_restart 也是桩，只记次数 —— 更新默认证书必须只换文件、从不重启
# （同进程里用别的证书的节点不该被牵连），用例里断言它一次都没被调用。
# 其余全是真实代码 —— 直接从 SingR.sh 抠出两个 SYNC BLOCK 来跑。
#
#   bash scripts/test-cert-source.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

command -v openssl >/dev/null 2>&1 || { echo "skip - 没有 openssl"; exit 0; }
command -v jq >/dev/null 2>&1 || { echo "skip - 没有 jq"; exit 0; }

FAILED=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; echo "         want: [$2]"; echo "         got:  [$3]"; FAILED=1; }
check() { [[ "$2" == "$3" ]] && pass "$1" || fail "$1" "$2" "$3"; }

extract_block() {
    local file="$1" name="$2" start end
    start="$(grep -n "^# >>>>>>>>>>>>>>>> SYNC BLOCK: ${name} " "${file}" | head -1 | cut -d: -f1)"
    end="$(grep -n "^# <<<<<<<<<<<<<<<< SYNC BLOCK: ${name} " "${file}" | head -1 | cut -d: -f1)"
    [[ -n "${start}" && -n "${end}" ]] || { echo "找不到 SYNC BLOCK「${name}」：${file}" >&2; exit 1; }
    sed -n "${start},${end}p" "${file}"
}

CFG="${TMP}/cfg"
REMOTE="${TMP}/remote"
STATE="${TMP}/state"
mkdir -p "${CFG}/certs" "${REMOTE}" "${STATE}" "${TMP}/systemd"
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo 'APP_NAME="SingR"; SELF_CMD="/usr/bin/SingR"'
    echo "CONFIG_DIR=\"${CFG}\""
    echo "CERT_DIR=\"${CFG}/certs\""
    echo "SERVER_CONFIG=\"${CFG}/server.json\""
    echo "PANEL_CONFIG=\"${CFG}/panel.json\""
    echo "CERT_TIMER_DIR=\"${TMP}/systemd\""
    echo 'confirm() { return 1; }'
    extract_block "${REPO_DIR}/SingR.sh" 节点管理
    extract_block "${REPO_DIR}/SingR.sh" 默认证书更新源
    # ---- 桩 ----
    # https://remote/<名字> -> ${REMOTE}/<名字>；文件不存在就是下载失败。
    echo "cert_source_fetch() { local f=\"${REMOTE}/\${1##*/}\"; [[ -s \"\${f}\" ]] && cp \"\${f}\" \"\$2\"; }"
    echo "node_backend_restart() { echo x >> '${STATE}/restarts'; }"
    echo 'systemctl() { :; }'
} > "${TMP}/block.sh"

run() { bash -c "source '${TMP}/block.sh'; $1" </dev/null 2>&1; }

mkcert() { # <名字> <天数> [CN]
    openssl req -x509 -newkey rsa:2048 -nodes -days "$2" -subj "/CN=${3:-t.example.com}" \
        -keyout "${TMP}/$1.key" -out "${TMP}/$1.pem" 2>/dev/null
}
fp() { openssl x509 -noout -fingerprint -sha256 -in "$1" 2>/dev/null; }
put_remote() { cp "${TMP}/$1.pem" "${REMOTE}/cert.pem"; cp "${TMP}/$1.key" "${REMOTE}/key.pem"; }
put_local() { cp "${TMP}/$1.pem" "${CFG}/certs/default.pem"; cp "${TMP}/$1.key" "${CFG}/certs/default.key"; }
restarts() { [[ -f "${STATE}/restarts" ]] && wc -l <"${STATE}/restarts" | tr -d ' ' || echo 0; }
reset() {
    rm -rf "${CFG}/certs" "${STATE}"/* "${REMOTE}"/* "${CFG}/cert-source.json" "${TMP}/systemd"/*
    mkdir -p "${CFG}/certs"
}
URLS="https://remote/cert.pem https://remote/key.pem"

mkcert long 90
mkcert long2 90 other.example.com
mkcert soon 3
mkcert sooner 2
openssl req -x509 -newkey rsa:2048 -nodes -days 90 -subj "/CN=x" -keyout "${TMP}/stray.key" -out /dev/null 2>/dev/null

echo "== cert_url_mask =="
check "藏掉 query"      "https://h.example/p/c.pem?***"  "$(run 'cert_url_mask "https://h.example/p/c.pem?token=abc"')"
check "藏掉 userinfo"   "https://***@h.example/c.pem"   "$(run 'cert_url_mask "https://u:pw@h.example/c.pem"')"
check "普通地址原样"    "https://h.example/c.pem"       "$(run 'cert_url_mask "https://h.example/c.pem"')"

echo "== cert_check_pair =="
check "成对的证书和私钥通过" "" "$(run "cert_check_pair '${TMP}/long.pem' '${TMP}/long.key'")"
check "私钥不匹配被拒"  "证书与私钥不匹配" "$(run "cert_check_pair '${TMP}/long.pem' '${TMP}/stray.key'")"
echo junk > "${TMP}/junk"
check "证书不是 PEM 被拒" "证书不是有效的 PEM 证书" "$(run "cert_check_pair '${TMP}/junk' '${TMP}/long.key'")"
check "私钥无效被拒"    "私钥无效（或带口令加密）" "$(run "cert_check_pair '${TMP}/long.pem' '${TMP}/junk'")"

echo "== cert_source_set =="
reset
out="$(run 'cert_source_set "http://remote/cert.pem" "https://remote/key.pem"')"
[[ "${out}" == *"必须是 https://"* && ! -e "${CFG}/cert-source.json" ]] && pass "拒绝 http 地址" || fail "拒绝 http 地址" "https 报错且不写配置" "${out}"

reset; put_remote long
run "cert_source_set ${URLS}" >/dev/null
check "首次配置：下载到默认证书" "$(fp "${TMP}/long.pem")" "$(fp "${CFG}/certs/default.pem")"
check "首次配置：写入更新源" "https://remote/cert.pem" "$(jq -r .cert_url "${CFG}/cert-source.json")"
check "更新源文件权限 600" "600" "$(stat -f %Lp "${CFG}/cert-source.json" 2>/dev/null || stat -c %a "${CFG}/cert-source.json")"
check "私钥权限 600" "600" "$(stat -f %Lp "${CFG}/certs/default.key" 2>/dev/null || stat -c %a "${CFG}/certs/default.key")"
[[ -f "${TMP}/systemd/singr-cert-update.timer" ]] && pass "装上每日 timer" || fail "装上每日 timer" "timer 文件" "无"
check "首次配置：不重启" "0" "$(restarts)"

reset; put_remote long; rm -f "${REMOTE}/key.pem"
run "cert_source_set ${URLS}" >/dev/null
[[ ! -e "${CFG}/cert-source.json" && ! -e "${CFG}/certs/default.pem" ]] && pass "下载失败：什么都不写" \
    || fail "下载失败：什么都不写" "无配置无证书" "$(ls "${CFG}" "${CFG}/certs")"

reset; put_remote long; put_local soon
cat > "${CFG}/server.json" <<'JSON'
{ "inbounds": [ { "type":"anytls", "tag":"a-in", "tls":{"enabled":true,"server_name":"t.example.com","certificate_path":"","key_path":""} } ] }
JSON
echo '{ "nodes": [ { "intag": "a-in" } ] }' > "${CFG}/panel.json"
out="$(run "cert_source_set ${URLS}")"
check "默认证书正被节点使用 + 非交互：拒绝替换" "$(fp "${TMP}/soon.pem")" "$(fp "${CFG}/certs/default.pem")"
[[ "${out}" == *"a-in"* ]] && pass "列出受影响的节点" || fail "列出受影响的节点" "a-in" "${out}"
rm -f "${CFG}/server.json" "${CFG}/panel.json"

echo "== cert-update（定时任务）=="
setup_source() { jq -n '{cert_url:"https://remote/cert.pem", key_url:"https://remote/key.pem"}' > "${CFG}/cert-source.json"; }

reset; setup_source; put_local long; put_remote long2
run 'cert_update_cmd' >/dev/null
check "剩余 90 天：不下载不替换" "$(fp "${TMP}/long.pem")" "$(fp "${CFG}/certs/default.pem")"

reset; setup_source; put_local soon; put_remote soon
out="$(run 'cert_update_cmd')"; rc=$?
[[ "${rc}" != 0 && "${out}" == *"远端尚未续期"* ]] && pass "临期但远端还是同一张：报错不替换" \
    || fail "临期但远端还是同一张：报错不替换" "rc!=0 且提示远端尚未续期" "rc=${rc} ${out}"

reset; setup_source; put_local soon; put_remote long
run 'cert_update_cmd' >/dev/null
check "临期 + 远端更新：替换证书" "$(fp "${TMP}/long.pem")" "$(fp "${CFG}/certs/default.pem")"
check "临期 + 远端更新：替换私钥" "$(openssl pkey -in "${TMP}/long.key" -pubout)" "$(openssl pkey -in "${CFG}/certs/default.key" -pubout)"
check "临期 + 远端更新：只换文件，不重启" "0" "$(restarts)"
[[ ! -e "${CFG}/certs/.default-prev.cert" ]] && pass "成功后清掉备份" || fail "成功后清掉备份" "无备份" "还在"

reset; setup_source; put_local soon; put_remote sooner
run 'cert_update_cmd' >/dev/null
check "远端比本地旧：不替换" "$(fp "${TMP}/soon.pem")" "$(fp "${CFG}/certs/default.pem")"

reset; setup_source; put_local soon; put_remote long; cp "${TMP}/stray.key" "${REMOTE}/key.pem"
run 'cert_update_cmd' >/dev/null
check "远端私钥不匹配：不替换" "$(fp "${TMP}/soon.pem")" "$(fp "${CFG}/certs/default.pem")"
[[ -z "$(ls -A "${CFG}/certs" | grep download)" ]] && pass "不留下载临时文件" || fail "不留下载临时文件" "无" "$(ls -A "${CFG}/certs")"

reset; setup_source; put_remote long
cp "${TMP}/soon.pem" "${CFG}/certs/default.crt"; cp "${TMP}/soon.key" "${CFG}/certs/default.key"
run 'cert_update_cmd' >/dev/null
check "正在用 default.crt：更新写回 .crt" "$(fp "${TMP}/long.pem")" "$(fp "${CFG}/certs/default.crt")"
[[ ! -e "${CFG}/certs/default.pem" ]] && pass "不另写 default.pem（进程盯着的是 .crt）" \
    || fail "不另写 default.pem" "无 default.pem" "有"

reset; setup_source; put_local long; put_remote long2
run 'cert_update_cmd --force' >/dev/null
check "--force：不看剩余天数直接替换" "$(fp "${TMP}/long2.pem")" "$(fp "${CFG}/certs/default.pem")"

reset
out="$(run 'cert_update_cmd')"; rc=$?
[[ "${rc}" == 0 && "${out}" == *"未配置"* ]] && pass "没配更新源：无事可做" || fail "没配更新源：无事可做" "rc=0 未配置" "rc=${rc} ${out}"

echo
if [[ "${FAILED}" == 0 ]]; then
    echo "PASS: 默认证书更新源 测试通过"
else
    echo "FAIL: 有用例未通过"
fi
exit "${FAILED}"
