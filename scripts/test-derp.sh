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
    # 假 headscale：curl 换成函数，按 STATE 里的文件决定应答，并把每次请求记进 calls。
    cat <<EOF
STATE='${STATE}'
EOF
    cat <<'EOF'
curl() {
    local out="" method="GET" header="" body="" url=""
    while (($# > 0)); do
        case "$1" in
            -o) out="$2"; shift 2 ;;
            -X) method="$2"; shift 2 ;;
            -H) [[ "$2" == @* ]] && header="${2#@}"; shift 2 ;;
            --data) body="$2"; shift 2 ;;
            -w | --proto | --noproxy | --connect-timeout | --max-time) shift 2 ;;
            -*) shift ;;
            *) url="$1"; shift ;;
        esac
    done
    printf '%s %s %s %s\n' "${method}" "${url}" "$(cat "${header}")" "${body}" >>"${STATE}/calls"
    [[ -f "${STATE}/unreachable" ]] && return 7
    if [[ -f "${STATE}/code" ]]; then
        echo denied >"${out}"
        cat "${STATE}/code"
        return 0
    fi
    case "${method} ${url}" in
        "GET "*/linbei/derp/mesh) cat "${STATE}/mesh" >"${out}"; echo 200 ;;
        "DELETE "*) : >"${out}"; echo 204 ;;
        *) echo '{}' >"${out}"; echo 200 ;;
    esac
}
EOF
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
CONTROL="https://hs.example.com"
TOKEN="$(printf 't%.0s' {1..40})"
PSK="$(printf 'a%.0s' {1..64})"
printf '{"psk":"%s","nodes":[{"host":"203.0.113.5:8443","pin":"x"},{"host":"203.0.113.6:8443","pin":"y"}]}' \
    "${PSK}" >"${STATE}/mesh"
calls() { [[ -f "${STATE}/calls" ]] && cat "${STATE}/calls"; }
ncalls() { [[ -f "${STATE}/calls" ]] && wc -l <"${STATE}/calls" | tr -d ' ' || echo 0; }
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

echo "== 开启前的参数检查 =="
check "没有 headscale 地址被拒" 'rejected' "$(run "derp_cmd enable --token ${TOKEN}" >/dev/null && echo accepted || echo rejected)"
check "http 地址被拒" 'rejected' "$(run "derp_cmd enable --control http://hs.example.com --token ${TOKEN}" >/dev/null && echo accepted || echo rejected)"
check "没有 token 被拒" 'rejected' "$(run "derp_cmd enable --control ${CONTROL}" >/dev/null && echo accepted || echo rejected)"
check "token 太短被拒" 'rejected' "$(run "derp_cmd enable --control ${CONTROL} --token short" >/dev/null && echo accepted || echo rejected)"
check "对外地址带协议被拒" 'rejected' "$(run "derp_cmd enable --control ${CONTROL} --token ${TOKEN} --advertise https://x" >/dev/null && echo accepted || echo rejected)"
check "参数错误时没碰 headscale 也没改配置" "0 ${ORIGINAL}" "$(ncalls) $(q .)"

echo "== headscale 拒绝或连不上时不开启 =="
echo 401 >"${STATE}/code"
OUT="$(run "derp_cmd enable --control ${CONTROL} --token ${TOKEN}")"
check "401 时不开启" "${ORIGINAL}" "$(q .)"
check "401 给出 token 不对的提示" 'yes' "$(grep -q '节点 token' <<<"${OUT}" && echo yes || echo no)"
echo 404 >"${STATE}/code"
OUT="$(run "derp_cmd enable --control ${CONTROL} --token ${TOKEN}")"
check "404 提示不是林北的 fork" 'yes' "$(grep -q 'fork' <<<"${OUT}" && echo yes || echo no)"
rm -f "${STATE}/code"
touch "${STATE}/unreachable"
run "derp_cmd enable --control ${CONTROL} --token ${TOKEN}" >/dev/null
check "连不上时不开启" "${ORIGINAL}" "$(q .)"
rm -f "${STATE}/unreachable"
check "没开启就没有 token 文件" 'no' "$([[ -e "${CFG}/derp/control.token" ]] && echo yes || echo no)"
: >"${STATE}/calls"

echo "== 开启 =="
run "derp_cmd enable --control ${CONTROL}/ --token ${TOKEN}" >/dev/null
check "生成一个 derp 服务" '1' "$(q '[.services[] | select(.tag=="derp")] | length')"
check "默认端口 8443" '8443' "$(q '.services[0].listen_port')"
check "只放行回环" 'true' "$(q '.services[0].verify_client_loopback')"
check "探测口是 DERP 端口 +1" '8444' "$(q '.services[0].probe_listen_port')"
check "headscale 地址去掉末尾斜杠" "\"${CONTROL}\"" "$(q '.services[0].control_url')"
check "token 存成文件、不进 server.json" "\"${CFG}/derp/control.token\" 0" \
    "$(q '.services[0].control_token_path') $(grep -c "${TOKEN}" "${CFG}/server.json")"
check "token 文件内容" "${TOKEN}" "$(tr -d '\n' <"${CFG}/derp/control.token")"
check "token 文件 0600" '600' "$(mode "${CFG}/derp/control.token")"
check "没指定对外地址时不写这个字段" 'null' "$(q '.services[0].control_advertise_host')"
check "不再有 PSK 字段" 'null null' "$(q '.services[0].mesh_psk_file') $(q '.services[0].mesh_psk')"
check "用本机固定自签证书" "{\"enabled\":true,\"certificate_path\":\"${CFG}/derp/derp-tls.crt\",\"key_path\":\"${CFG}/derp/derp-tls.key\"}" "$(q '.services[0].tls')"
check "私钥 0600" '600' "$(mode "${CFG}/derp/derp-tls.key")"
PIN="$(run 'derp_pin')"
check "指纹是 base64 SHA-256" 'yes' "$([[ "${PIN}" =~ ^[A-Za-z0-9+/]{43}=$ ]] && echo yes || echo no)"
# 与 Go 侧同一算法：SHA-256(公钥 DER)。这里换一条独立的命令链算一遍对照。
check "指纹与公钥 DER 的 SHA-256 一致" \
    "$(openssl pkey -in "${CFG}/derp/derp-tls.key" -pubout -outform DER 2>/dev/null | openssl dgst -sha256 -binary | base64)" "${PIN}"
check "报到带端口和指纹、token 走请求头" \
    "POST ${CONTROL}/linbei/derp/register Authorization: Bearer ${TOKEN} {\"port\":8443,\"pin\":\"${PIN}\"}" \
    "$(sed -n 1p "${STATE}/calls")"
check "随后拉名单" "GET ${CONTROL}/linbei/derp/mesh" "$(sed -n 2p "${STATE}/calls" | cut -d' ' -f1-2)"
check "名单写进进程的缓存" "$(cat "${STATE}/mesh")" "$(cat "${CFG}/derp/mesh-control.cache.json")"
check "缓存 0600（里面有 PSK）" '600' "$(mode "${CFG}/derp/mesh-control.cache.json")"
check "443 改写到 DERP 端口" \
    '{"domain":["derp.linbei.internal"],"port":[443],"action":"route-options","override_address":"127.0.0.1","override_port":8443}' \
    "$(q '.route.rules[0]')"
check "80 改写到探测口" \
    '{"domain":["derp.linbei.internal"],"port":[80],"action":"route-options","override_address":"127.0.0.1","override_port":8444}' \
    "$(q '.route.rules[1]')"
check "原有规则保留在后面" '{"inbound":"anytls-in","outbound":"anytls-out"}' "$(q '.route.rules[2]')"
check "重启过一次" '1' "$(restarts)"
check "没有留下备份" 'no no' "$([[ -e "${CFG}/server.json.bak" ]] && echo yes || echo no) $([[ -e "${CFG}/derp/control.token.bak" ]] && echo yes || echo no)"
SHOW="$(run 'derp_show')"
check "状态页显示名单数量" 'yes' "$(grep -q '2 个节点' <<<"${SHOW}" && echo yes || echo no)"
check "状态页不显示 token" 'no' "$(grep -q "${TOKEN}" <<<"${SHOW}" && echo yes || echo no)"

run 'derp_cmd enable' >/dev/null
check "不带参数重复开启沿用原配置" "\"${CONTROL}\" 8443" "$(q '.services[0].control_url') $(q '.services[0].listen_port')"
check "重复开启不换证书（指纹不变）" "${PIN}" "$(run 'derp_pin')"
check "重复开启不会叠加服务" '1' "$(q '[.services[] | select(.tag=="derp")] | length')"
check "重复开启不会叠加规则" '2' "$(q '[.route.rules[] | select(.action=="route-options")] | length')"

echo "== 端口与对外地址 =="
check "非法端口被拒" 'rejected' "$(run 'derp_cmd port 70000' >/dev/null && echo accepted || echo rejected)"
check "带前导零的端口不按八进制算" 'accepted' "$(run 'derp_valid_port 08443' >/dev/null && echo accepted || echo rejected)"
check "65535 的探测口取下一个之前的" '65534' "$(run 'derp_probe_port 65535')"
: >"${STATE}/calls"
run 'derp_cmd port 9443' >/dev/null
check "改端口：服务与两条改写规则一起变" '9443 9443 9444 9444' \
    "$(q '.services[0].listen_port') $(q '.route.rules[0].override_port') $(q '.services[0].probe_listen_port') $(q '.route.rules[1].override_port')"
check "改端口：新端口报到、旧端口撤下" "POST 9443|DELETE 8443" \
    "$(grep -oE '^(POST|DELETE) .*"port":[0-9]+' "${STATE}/calls" | sed -E 's/^([A-Z]+) .*"port":([0-9]+)$/\1 \2/' | grep -v '^GET' | paste -sd '|' -)"
run 'derp_cmd advertise JP1.example.com' >/dev/null
check "指定对外地址" '"JP1.example.com"' "$(q '.services[0].control_advertise_host')"
check "报到带上对外地址" 'yes' "$(grep -q '"host":"JP1.example.com"' "${STATE}/calls" && echo yes || echo no)"
run 'derp_cmd advertise --clear' >/dev/null
check "清除对外地址" 'null' "$(q '.services[0].control_advertise_host')"
check "清除后撤下按对外地址报到的那条" 'yes' \
    "$(grep -q '^DELETE .*"host":"JP1.example.com"' "${STATE}/calls" && echo yes || echo no)"

echo "== 换 token =="
NEW_TOKEN="$(printf 'n%.0s' {1..40})"
run "derp_cmd enable --token ${NEW_TOKEN}" >/dev/null
check "换了 token" "${NEW_TOKEN}" "$(tr -d '\n' <"${CFG}/derp/control.token")"

echo "== 起不来就回滚 =="
BEFORE="$(q .)"
touch "${STATE}/broken"
: >"${STATE}/calls"
run "derp_cmd enable --token ${TOKEN}" >/dev/null
check "token 回滚到旧值" "${NEW_TOKEN}" "$(tr -d '\n' <"${CFG}/derp/control.token")"
check "server.json 回滚" "${BEFORE}" "$(q .)"
run 'derp_cmd port 7443' >/dev/null
check "改端口失败也回滚" '9443' "$(q '.services[0].listen_port')"
check "本来开着时失败不撤下自己" 'no' "$(grep -q '^DELETE .*"port":9443' "${STATE}/calls" && echo yes || echo no)"
rm -f "${STATE}/broken"

echo "== 关闭 =="
: >"${STATE}/calls"
run 'derp_cmd disable' >/dev/null
check "关闭时从名单撤下本机" "DELETE ${CONTROL}/linbei/derp/register Authorization: Bearer ${NEW_TOKEN} {\"port\":9443}" \
    "$(cat "${STATE}/calls")"
check "关闭后去掉服务" 'null' "$(q '.services')"
check "关闭后去掉改写规则、保留其它规则" '[{"inbound":"anytls-in","outbound":"anytls-out"}]' "$(q '.route.rules')"
check "关闭后与开启前完全一致" "${ORIGINAL}" "$(q .)"
check "关闭删掉缓存（里面有 PSK）" 'no' "$([[ -e "${CFG}/derp/mesh-control.cache.json" ]] && echo yes || echo no)"
check "关闭不删证书（再开指纹不变）" 'yes' "$([[ -s "${CFG}/derp/derp-tls.crt" ]] && echo yes || echo no)"
check "未开启时不能改端口" 'rejected' "$(run 'derp_cmd port 9443' >/dev/null && echo accepted || echo rejected)"
check "未开启时不改 server.json" "${ORIGINAL}" "$(q .)"
run 'derp_cmd enable' >/dev/null
check "关掉再开不用重新输入（地址、端口、token 都沿用）" "\"${CONTROL}\" 9443 ${NEW_TOKEN}" \
    "$(q '.services[0].control_url') $(q '.services[0].listen_port') $(tr -d '\n' <"${CFG}/derp/control.token")"
run 'derp_cmd disable' >/dev/null

echo "== 首次开启失败撤下刚报到的那条 =="
touch "${STATE}/broken"
: >"${STATE}/calls"
run "derp_cmd enable --control ${CONTROL} --token ${TOKEN}" >/dev/null
check "首次开启失败：报到后又撤下" 'POST|GET|DELETE' "$(cut -d' ' -f1 "${STATE}/calls" | paste -sd '|' -)"
check "首次开启失败不改 server.json" "${ORIGINAL}" "$(q .)"
rm -f "${STATE}/broken"

if [[ -n "${SINGR_BIN:-}" ]]; then
    echo "== singr check =="
    run "derp_cmd enable --control ${CONTROL} --token ${TOKEN}" >/dev/null
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
