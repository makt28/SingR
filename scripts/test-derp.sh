#!/usr/bin/env bash
# DERP 中继（SingR.sh / SingR-docker.sh 的 SYNC BLOCK「DERP 中继」）的回归测试。
#
# 重启与校验是桩：node_backend_verify 读一个开关文件决定「起没起来」，用来覆盖回滚路径。
# 其余全是真实代码 —— 直接从 SingR.sh 抠出 SYNC BLOCK 来跑。两个脚本里的块必须逐字
# 相同，开头先比对。
#
# 设了 SINGR_BIN（本机能跑的 singr 二进制，需带 with_gvisor,with_tailscale）时，再用
# `singr check` 校验生成的 server.json 能被内核接受。
#
#   bash scripts/test-derp.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

command -v jq >/dev/null 2>&1 || { echo "skip - 没有 jq"; exit 0; }
command -v openssl >/dev/null 2>&1 || { echo "skip - 没有 openssl"; exit 0; }

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

echo "== 两个脚本的块逐字相同 =="
if diff <(extract_block "${REPO_DIR}/SingR.sh" "DERP 中继") \
    <(extract_block "${REPO_DIR}/SingR-docker.sh" "DERP 中继") >/dev/null; then
    pass "SingR.sh 与 SingR-docker.sh 一致"
else
    fail "SingR.sh 与 SingR-docker.sh 一致" "identical" "different"
fi

CFG="${TMP}/cfg"
STATE="${TMP}/state"
mkdir -p "${CFG}" "${STATE}"
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo 'APP_NAME="SingR"'
    echo "CONFIG_DIR=\"${CFG}\""
    echo "CERT_DIR=\"${CFG}/certs\""
    echo "SERVER_CONFIG=\"${CFG}/server.json\""
    echo "PANEL_CONFIG=\"${CFG}/panel.json\""
    echo 'confirm() { return 1; }'
    extract_block "${REPO_DIR}/SingR.sh" 节点管理
    extract_block "${REPO_DIR}/SingR.sh" 默认证书更新源
    extract_block "${REPO_DIR}/SingR.sh" "DERP 中继"
    # ---- 桩 ----
    echo "node_backend_restart() { echo x >> '${STATE}/restarts'; }"
    echo "node_backend_verify() { [[ ! -f '${STATE}/broken' ]]; }"
} >"${TMP}/block.sh"

run() { bash -c "source '${TMP}/block.sh'; $1" </dev/null 2>&1; }
q() { jq -c "$1" "${CFG}/server.json"; }
restarts() { [[ -f "${STATE}/restarts" ]] && wc -l <"${STATE}/restarts" | tr -d ' ' || echo 0; }

cat >"${CFG}/panel.json" <<'EOF'
{"nodes":[{"paneltype":"SSpanel","intag":"anytls-in","outtag":"anytls-out","apiconfig":{"apihost":"https://p.example","apikey":"k","nodeid":1,"nodetype":"V2ray"}}]}
EOF
cat >"${CFG}/server.json" <<'EOF'
{
  "log": {"level": "info"},
  "inbounds": [{"type": "anytls", "tag": "anytls-in", "listen": "::", "listen_port": 0, "users": [],
    "tls": {"enabled": true, "server_name": "", "certificate_path": "", "key_path": ""}}],
  "outbounds": [{"type": "direct", "tag": "anytls-out"}, {"type": "direct", "tag": "direct"}],
  "route": {"rules": [{"inbound": "anytls-in", "outbound": "anytls-out"}], "final": "direct"}
}
EOF
ORIGINAL="$(q .)"
PSK_A="$(printf 'a%.0s' {1..64})"
PSK_B="$(printf 'b%.0s' {1..64})"

echo "== 开启 =="
run 'derp_cmd enable' >/dev/null
check "生成一个 derp 服务" '1' "$(q '[.services[] | select(.tag=="derp")] | length')"
check "默认端口 8443" '8443' "$(q '.services[0].listen_port')"
check "只放行回环" 'true' "$(q '.services[0].verify_client_loopback')"
check "用本机固定自签证书" "{\"enabled\":true,\"certificate_path\":\"${CFG}/derp/derp-tls.crt\",\"key_path\":\"${CFG}/derp/derp-tls.key\"}" "$(q '.services[0].tls')"
check "证书已生成" 'yes yes' "$([[ -s "${CFG}/derp/derp-tls.crt" ]] && echo yes || echo no) $([[ -s "${CFG}/derp/derp-tls.key" ]] && echo yes || echo no)"
check "私钥 0600" '600' "$(stat -c %a "${CFG}/derp/derp-tls.key" 2>/dev/null || stat -f %Lp "${CFG}/derp/derp-tls.key")"
PIN="$(run 'derp_pin')"
check "指纹是 base64 SHA-256" 'yes' "$([[ "${PIN}" =~ ^[A-Za-z0-9+/]{43}=$ ]] && echo yes || echo no)"
# 与 Go 侧同一算法：SHA-256(公钥 DER)。这里换一条独立的命令链算一遍对照。
check "指纹与公钥 DER 的 SHA-256 一致" \
    "$(openssl pkey -in "${CFG}/derp/derp-tls.key" -pubout -outform DER 2>/dev/null | openssl dgst -sha256 -binary | base64)" "${PIN}"
# 先收下整段输出再 grep：pipefail 下 grep -q 提前退出会让上游吃 SIGPIPE，整条管道判失败。
SHOW="$(run 'derp_show')"
check "状态页给出可直接粘贴的列表项" 'yes' "$(grep -qF "\"pin\": \"${PIN}\"" <<<"${SHOW}" && echo yes || echo no)"
check "密钥路径在配置目录下" "\"${CFG}/derp/derp.key\"" "$(q '.services[0].config_path')"
check "没设列表时不带 mesh 字段" 'null null' "$(q '.services[0].mesh_with_url') $(q '.services[0].mesh_psk_file')"
check "改写规则排在第一条" \
    '{"domain":["derp.linbei.internal"],"action":"route-options","override_address":"127.0.0.1","override_port":8443}' \
    "$(q '.route.rules[0]')"
check "原有规则保留在后面" '{"inbound":"anytls-in","outbound":"anytls-out"}' "$(q '.route.rules[1]')"
check "重启过一次" '1' "$(restarts)"
check "没有留下备份" 'no' "$([[ -e "${CFG}/server.json.bak" ]] && echo yes || echo no)"

run 'derp_cmd enable' >/dev/null
check "重复开启不换证书（指纹不变）" "${PIN}" "$(run 'derp_pin')"
check "重复开启不会叠加服务" '1' "$(q '[.services[] | select(.tag=="derp")] | length')"
check "重复开启不会叠加规则" '1' "$(q '[.route.rules[] | select(.action=="route-options")] | length')"

echo "== 端口 =="
check "非法端口被拒" 'rejected' "$(run 'derp_cmd port 70000' >/dev/null && echo accepted || echo rejected)"
check "带前导零的端口不按八进制算" 'accepted' "$(run 'derp_valid_port 08443' >/dev/null && echo accepted || echo rejected)"
run 'derp_cmd port 9443' >/dev/null
check "改端口：服务与改写规则一起变" '9443 9443' "$(q '.services[0].listen_port') $(q '.route.rules[0].override_port')"

echo "== PSK 与对端列表 =="
check "没有 PSK 不能设列表" 'rejected' "$(run 'derp_cmd url https://list.example/derp.json' >/dev/null && echo accepted || echo rejected)"
check "非法 PSK 被拒" 'rejected' "$(run 'derp_cmd psk 1234' >/dev/null && echo accepted || echo rejected)"
run "derp_cmd psk ${PSK_A}" >/dev/null
check "PSK 文件内容不带换行" "${PSK_A}" "$(cat "${CFG}/derp/mesh.psk")"
check "PSK 文件大小正好 64 字节" '64' "$(wc -c <"${CFG}/derp/mesh.psk" | tr -d ' ')"
check "PSK 文件 0600" '600' "$(stat -c %a "${CFG}/derp/mesh.psk" 2>/dev/null || stat -f %Lp "${CFG}/derp/mesh.psk")"
check "服务引用 PSK 文件" "\"${CFG}/derp/mesh.psk\"" "$(q '.services[0].mesh_psk_file')"
check "列表必须 https" 'rejected' "$(run 'derp_cmd url http://list.example/derp.json' >/dev/null && echo accepted || echo rejected)"
run 'derp_cmd url https://list.example/derp.json' >/dev/null
check "设了列表" '"https://list.example/derp.json"' "$(q '.services[0].mesh_with_url')"
run 'derp_cmd port 8443' >/dev/null
check "改端口保留列表与 PSK" '"https://list.example/derp.json" true' \
    "$(q '.services[0].mesh_with_url') $(q '.services[0].mesh_psk_file != null')"
run 'derp_cmd url --clear' >/dev/null
check "清除列表" 'null' "$(q '.services[0].mesh_with_url')"
run 'derp_cmd url https://list.example/derp.json' >/dev/null

GEN="$(run 'derp_cmd psk gen')"
check "psk gen 打印给其它节点用的命令" 'yes' "$(grep -qE 'singr derp psk [0-9a-f]{64}$' <<<"${GEN}" && echo yes || echo no)"
check "psk gen 写入的就是打印的那个" "$(grep -oE '[0-9a-f]{64}$' <<<"${GEN}")" "$(cat "${CFG}/derp/mesh.psk")"

echo "== 起不来就回滚 =="
run "derp_cmd psk ${PSK_A}" >/dev/null
BEFORE="$(q .)"
touch "${STATE}/broken"
run "derp_cmd psk ${PSK_B}" >/dev/null
check "PSK 回滚到旧值" "${PSK_A}" "$(cat "${CFG}/derp/mesh.psk")"
check "server.json 回滚" "${BEFORE}" "$(q .)"
run 'derp_cmd port 7443' >/dev/null
check "改端口失败也回滚" '8443' "$(q '.services[0].listen_port')"
rm -f "${STATE}/broken"

echo "== 未开启时设 PSK =="
run 'derp_cmd disable' >/dev/null
check "关闭后去掉服务" 'null' "$(q '.services')"
check "关闭后去掉改写规则、保留其它规则" '[{"inbound":"anytls-in","outbound":"anytls-out"}]' "$(q '.route.rules')"
check "关闭后与开启前完全一致" "${ORIGINAL}" "$(q .)"
check "关闭不删证书（再开指纹不变）" 'yes' "$([[ -s "${CFG}/derp/derp-tls.crt" ]] && echo yes || echo no)"
N="$(restarts)"
run "derp_cmd psk ${PSK_B}" >/dev/null
check "未开启时只保存 PSK、不重启" "${N} ${PSK_B}" "$(restarts) $(cat "${CFG}/derp/mesh.psk")"
check "未开启时不能改端口" 'rejected' "$(run 'derp_cmd port 9443' >/dev/null && echo accepted || echo rejected)"
check "未开启时不改 server.json" "${ORIGINAL}" "$(q .)"

if [[ -n "${SINGR_BIN:-}" ]]; then
    echo "== singr check =="
    run 'derp_cmd enable' >/dev/null
    run 'derp_cmd url https://list.example/derp.json' >/dev/null
    # 夹具里的 anytls 没有证书（实机靠 -p 补默认证书路径），check 时让它自签，只看 DERP 部分。
    jq '.inbounds[].tls.insecure = true' "${CFG}/server.json" >"${TMP}/check.json"
    if out="$("${SINGR_BIN}" check -c "${TMP}/check.json" 2>&1)"; then
        pass "内核接受生成的配置"
    else
        fail "内核接受生成的配置" "ok" "${out}"
    fi
fi

if [[ "${FAILED}" != 0 ]]; then
    echo "FAILED"
    exit 1
fi
echo "PASS"
