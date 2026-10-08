#!/usr/bin/env bash
# WARP 出口（SingR.sh / SingR-docker.sh 的 SYNC BLOCK「WARP 出口」）的回归测试。
#
# 二进制换成桩：warp_backend_exec 的 register / test 返回预置结果，endpoint 默认用 jq
# 仿造；设了 SINGR_BIN（带 with_gvisor,with_wireguard 编出来的 singr）时 endpoint 走真
# 二进制，并额外用它 `check` 一遍写出来的 server.json —— 规则形状和 endpoint 字段能不能
# 被 sing-box 接受，以它为准。node_backend_restart / verify 也是桩。
# 其余全是真实代码 —— 直接从 SingR.sh 抠出 SYNC BLOCK 来跑。
#
#   bash scripts/test-warp.sh
#   SINGR_BIN=/path/to/singr bash scripts/test-warp.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

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

echo "SYNC BLOCK 两边一致"
if diff <(extract_block "${REPO_DIR}/SingR.sh" "WARP 出口") \
    <(extract_block "${REPO_DIR}/SingR-docker.sh" "WARP 出口") >/dev/null; then
    pass "SingR.sh 与 SingR-docker.sh 的「WARP 出口」逐字相同"
else
    fail "SingR.sh 与 SingR-docker.sh 的「WARP 出口」逐字相同" "identical" "differs"
fi

CFG="${TMP}/cfg"
STATE="${TMP}/state"
mkdir -p "${CFG}" "${STATE}"
SINGR_BIN="${SINGR_BIN:-}"
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo 'APP_NAME="SingR"; SELF_CMD="/usr/bin/SingR"'
    echo "CONFIG_DIR=\"${CFG}\""
    echo "CERT_DIR=\"${CFG}/certs\""
    echo "SERVER_CONFIG=\"${CFG}/server.json\""
    echo "PANEL_CONFIG=\"${CFG}/panel.json\""
    echo "STATE=\"${STATE}\"; SINGR_BIN=\"${SINGR_BIN}\""
    extract_block "${REPO_DIR}/SingR.sh" 节点管理
    extract_block "${REPO_DIR}/SingR.sh" "WARP 出口"
    cat <<'STUB'
# ---- 桩 ----
# test 的结果：${STATE}/test.json 存在就原样输出，否则按 --via 生成一个"通"的结果
# （auto 时选 ${STATE}/auto_via，默认 v4）。
warp_backend_exec() {
    local sub="$2" via=""
    [[ "$1" == warp ]] || return 2
    shift 2
    while [[ $# -gt 0 ]]; do
        case "$1" in --via) via="$2"; shift 2 ;; *) shift ;; esac
    done
    echo "${sub} ${via}" >> "${STATE}/calls"
    case "${sub}" in
        register)
            [[ -f "${STATE}/register_fail" ]] && { echo "register: HTTP 429" >&2; return 1; }
            cat <<'JSON'
{"id":"dev","token":"tok","private_key":"yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=",
 "peer_public_key":"bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=","reserved":[1,2,3],
 "address":["172.16.0.2/32","2606:4700:110:8a36::1/128"],
 "endpoint_v4":"162.159.192.1","endpoint_v6":"2606:4700:d0::a29f:c001","port":2408}
JSON
            ;;
        test)
            cat >/dev/null
            if [[ -f "${STATE}/test.json" ]]; then
                cat "${STATE}/test.json"
                jq -e '.ok' "${STATE}/test.json" >/dev/null
                return
            fi
            [[ "${via}" == auto ]] && via="$(cat "${STATE}/auto_via" 2>/dev/null || echo v4)"
            jq -n --arg v "${via}" '{ok: true, via: $v, results: [{via: $v, ok: true,
                ipv4: {ok: true, ip: "104.28.0.1", warp: "on", colo: "SIN"},
                ipv6: {ok: true, ip: "2a09:bac1::1", warp: "on", colo: "SIN"}}]}'
            ;;
        endpoint)
            if [[ -n "${SINGR_BIN}" ]]; then
                "${SINGR_BIN}" warp endpoint --via "${via}"
                return
            fi
            jq --arg v "${via}" '{type: "wireguard", tag: "warp", mtu: 1280, address, private_key,
                peers: [{address: (if $v == "v6" then .endpoint_v6 else .endpoint_v4 end), port,
                         public_key: .peer_public_key, allowed_ips: ["0.0.0.0/0", "::/0"],
                         persistent_keepalive_interval: 25, reserved}]}'
            ;;
    esac
}
node_backend_restart() { echo x >> "${STATE}/restarts"; }
node_backend_verify() { [[ ! -f "${STATE}/verify_fail" ]]; }
STUB
} > "${TMP}/block.sh"

run() { bash -c "source '${TMP}/block.sh'; $1" </dev/null 2>&1; }
q() { jq -c "$1" "${CFG}/server.json"; }
restarts() { if [[ -f "${STATE}/restarts" ]]; then wc -l < "${STATE}/restarts" | tr -d ' '; else echo 0; fi; }
reset() {
    rm -f "${STATE}"/* "${CFG}/warp.json" "${CFG}"/*.bak
    cp "${REPO_DIR}/release/poet/server.json" "${CFG}/server.json"
    echo '{"Nodes":[]}' > "${CFG}/panel.json"
}
ORIG_RULES="$(jq -c '.route.rules' "${REPO_DIR}/release/poet/server.json")"

echo "warp_norm_takeover"
for pair in v4:v4 4:v4 IPv4:v4 v6:v6 6:v6 ipv6:v6 4,6:all v4,v6:all 46:all "4, 6:all" v6+v4:all all:all both:all; do
    check "'${pair%:*}' -> ${pair##*:}" "${pair##*:}" "$(run "warp_norm_takeover '${pair%:*}'")"
done
for bad in "" 5 v5 ipv "4,,6" foo; do
    run "warp_norm_takeover '${bad}'" >/dev/null && fail "'${bad}' 应被拒绝" "rc!=0" "rc=0" || pass "'${bad}' 被拒绝"
done

echo "开启：接管 v4，auto 测出走 v6"
reset
echo v6 > "${STATE}/auto_via"
out="$(run 'warp_on --takeover v4')"
check "已注册账户" "dev" "$(jq -r .id "${CFG}/warp.json" 2>/dev/null)"
check "账户文件 0600" "600" "$(stat -c %a "${CFG}/warp.json" 2>/dev/null || stat -f %Lp "${CFG}/warp.json")"
check "endpoint peer 走 v6" "2606:4700:d0::a29f:c001" "$(q '.endpoints[0].peers[0].address' | tr -d '"')"
check "warp_mode = v4" "v4" "$(run warp_mode)"
check "warp_via = v6" "v6" "$(run warp_via)"
check "规则插在最前面，原规则原样留在后面" \
    "$(jq -c --argjson o "${ORIG_RULES}" -n '[{network:["tcp","udp"],action:"resolve"},{ip_cidr:["::/0"],action:"resolve",strategy:"ipv6_only"},{ip_cidr:["::/0"],invert:true,outbound:"warp"}] + $o')" \
    "$(q '.route.rules')"
check "重启一次" "1" "$(restarts)"
check "备份已清理" "" "$(ls "${CFG}"/*.bak 2>/dev/null)"
[[ "${out}" == *"WARP 出口已开启"* ]] && pass "提示已开启" || fail "提示已开启" "WARP 出口已开启" "${out}"

if [[ -n "${SINGR_BIN}" ]]; then
    # 真二进制校验：去掉要证书的 inbound，只验 endpoint + 规则 + 出站。
    jq 'del(.inbounds) | .route.rules |= map(select(.inbound == null)) | del(.log.output)' \
        "${CFG}/server.json" > "${TMP}/check.json"
    "${SINGR_BIN}" check -c "${TMP}/check.json" >/dev/null 2>"${TMP}/check.err" &&
        pass "sing-box check 接受 v4 接管配置" || fail "sing-box check 接受 v4 接管配置" "ok" "$(cat "${TMP}/check.err")"
fi

echo "改成全部接管：不重复、不残留"
run 'warp_on --takeover 4,6 --via v4' >/dev/null
check "warp_mode = all" "all" "$(run warp_mode)"
check "只有一个 warp endpoint" "1" "$(q '[.endpoints[] | select(.tag=="warp")] | length')"
check "peer 换成 v4" "162.159.192.1" "$(q '.endpoints[0].peers[0].address' | tr -d '"')"
check "规则 = 1 条全接管 + 原规则" \
    "$(jq -c --argjson o "${ORIG_RULES}" -n '[{network:["tcp","udp"],outbound:"warp"}] + $o')" "$(q '.route.rules')"
check "账户复用，没有再注册" "1" "$(grep -c '^register' "${STATE}/calls")"

if [[ -n "${SINGR_BIN}" ]]; then
    for m in v6 all; do
        run "warp_on --takeover ${m} --via v4" >/dev/null
        jq 'del(.inbounds) | .route.rules |= map(select(.inbound == null)) | del(.log.output)' \
            "${CFG}/server.json" > "${TMP}/check.json"
        "${SINGR_BIN}" check -c "${TMP}/check.json" >/dev/null 2>"${TMP}/check.err" &&
            pass "sing-box check 接受 ${m} 接管配置" || fail "sing-box check 接受 ${m} 接管配置" "ok" "$(cat "${TMP}/check.err")"
    done
fi

echo "node_add 之后 warp 规则仍在最前"
run 'node_write_server node-x anytls a.example.com "" ""' >/dev/null
check "第 0 条仍是 warp 规则" "warp" "$(q '.route.rules[0].outbound' | tr -d '"')"
check "新节点规则追加在末尾" "node-x" "$(q '.route.rules[-1].inbound' | tr -d '"')"

echo "关闭：恢复原样"
reset
run 'warp_on --takeover v6' >/dev/null
run 'warp_off' >/dev/null
check "warp_mode = off" "off" "$(run warp_mode)"
check "server.json 与安装模板结构一致" \
    "$(jq -S -c . "${REPO_DIR}/release/poet/server.json")" "$(jq -S -c . "${CFG}/server.json")"
check "账户保留" "dev" "$(jq -r .id "${CFG}/warp.json")"
check "已关闭时 off 不重启" "2" "$(run 'warp_off' >/dev/null; restarts)"

echo "测试不通：不改配置、不重启"
reset
echo '{"ok":false,"via":"","results":[{"via":"v4","ok":false,"ipv4":{"ok":false,"error":"timeout"},"ipv6":{"ok":false,"error":"timeout"}}]}' \
    > "${STATE}/test.json"
before="$(jq -S -c . "${CFG}/server.json")"
out="$(run 'warp_on --takeover v4')"
check "server.json 未变" "${before}" "$(jq -S -c . "${CFG}/server.json")"
check "没有重启" "0" "$(restarts)"
[[ "${out}" == *"WARP 不通"* && "${out}" == *"timeout"* ]] && pass "提示不通并给出原因" || fail "提示不通并给出原因" "WARP 不通 + timeout" "${out}"

echo "注册失败：不改配置、不留半截账户"
reset
touch "${STATE}/register_fail"
run 'warp_on --takeover v4' >/dev/null
check "没有账户文件" "" "$(ls "${CFG}"/warp.json "${CFG}"/.warp.* 2>/dev/null)"
check "server.json 未变" "$(jq -S -c . "${REPO_DIR}/release/poet/server.json")" "$(jq -S -c . "${CFG}/server.json")"

echo "重启后起不来：回滚"
reset
touch "${STATE}/verify_fail"
run 'warp_on --takeover all' >/dev/null
check "回滚后 warp_mode = off" "off" "$(run warp_mode)"
check "server.json 回到原样" "$(jq -S -c . "${REPO_DIR}/release/poet/server.json")" "$(jq -S -c . "${CFG}/server.json")"

echo "手写规则指向 warp：off 拒绝"
reset
run 'warp_on --takeover v4' >/dev/null
jq '.route.rules = [{domain_suffix: ["openai.com"], outbound: "warp"}] + .route.rules' "${CFG}/server.json" > "${TMP}/s" &&
    mv "${TMP}/s" "${CFG}/server.json"
out="$(run 'warp_off')"
check "warp_mode 仍是 v4" "v4" "$(run warp_mode)"
[[ "${out}" == *"openai.com"* ]] && pass "列出了那条规则" || fail "列出了那条规则" "openai.com" "${out}"

echo "手写配置识别为 custom，且 on 时保留手写规则"
reset
jq '.endpoints = [{type:"wireguard",tag:"warp",peers:[{address:"162.159.192.1"}]}]
     | .route.rules = [{domain_suffix:["openai.com"],outbound:"warp"}] + .route.rules' \
    "${CFG}/server.json" > "${TMP}/s" && mv "${TMP}/s" "${CFG}/server.json"
check "warp_mode = custom" "custom" "$(run warp_mode)"
run 'warp_on --takeover v6' >/dev/null
check "接管后 warp_mode = v6" "v6" "$(run warp_mode)"
check "手写规则还在" "1" "$(q '[.route.rules[] | select(.domain_suffix == ["openai.com"])] | length')"
check "仍只有一个 warp endpoint" "1" "$(q '[.endpoints[] | select(.tag=="warp")] | length')"

echo "register --force"
reset
run 'warp_register_cmd' >/dev/null
check "首次注册" "dev" "$(jq -r .id "${CFG}/warp.json")"
out="$(run 'warp_register_cmd')"
[[ "${out}" == *"--force"* ]] && pass "已有账户时提示 --force" || fail "已有账户时提示 --force" "--force" "${out}"
run 'warp_on --takeover v4' >/dev/null
run 'warp_register_cmd --force' >/dev/null
check "使用中时 --force 拒绝（没有 .old）" "" "$(ls "${CFG}"/warp.json.old 2>/dev/null)"
run 'warp_off' >/dev/null
run 'warp_register_cmd --force' >/dev/null
check "关闭后 --force 换新，旧的留 .old" "dev" "$(jq -r .id "${CFG}/warp.json.old" 2>/dev/null)"

[[ "${FAILED}" == 0 ]] && echo "PASS" || { echo "FAILED"; exit 1; }
