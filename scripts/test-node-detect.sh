#!/usr/bin/env bash
# singr add 按面板判断协议（SingR.sh / SingR-docker.sh 的 SYNC BLOCK「节点管理」里的
# node_panel_parse_server / node_panel_detect）的回归测试。
#
# 协议判断规则在 Go（poet/api/sspanel 的 ParseV2rayNodeResponse，运行时用）和 shell
# （add 时挑 inbound 类型用）各有一份。两边读同一个样例文件
# poet/api/sspanel/testdata/v2ray_alias_servers.tsv —— Go 那侧由 TestV2rayAliasFixtures
# 守着，这里守 shell 这侧。改规则时两边和样例一起改。
#
# 其余部分：curl 换成桩，检查请求怎么发、各种失败怎么报；有 python3 时再起一个本地
# HTTP 服务，用真 curl 验证 -K 配置的写法；最后用桩掉的后端把 node_add 走一遍。
#
#   bash scripts/test-node-detect.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
FIXTURES="${REPO_DIR}/poet/api/sspanel/testdata/v2ray_alias_servers.tsv"
TMP="$(mktemp -d)"
SERVER_PID=""
trap '[[ -n "${SERVER_PID}" ]] && kill "${SERVER_PID}" 2>/dev/null; rm -rf "${TMP}"' EXIT

command -v jq >/dev/null 2>&1 || { echo "skip - 没有 jq"; exit 0; }

FAILED=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; echo "         want: [$2]"; echo "         got:  [$3]"; FAILED=1; }
check() { [[ "$2" == "$3" ]] && pass "$1" || fail "$1" "$2" "$3"; }
contains() { [[ "$3" == *"$2"* ]] && pass "$1" || fail "$1" "*$2*" "$3"; }
lacks() { [[ "$3" != *"$2"* ]] && pass "$1" || fail "$1" "不含 $2" "$3"; }

extract_block() {
    local file="$1" name="$2" start end
    start="$(grep -n "^# >>>>>>>>>>>>>>>> SYNC BLOCK: ${name} " "${file}" | head -1 | cut -d: -f1)"
    end="$(grep -n "^# <<<<<<<<<<<<<<<< SYNC BLOCK: ${name} " "${file}" | head -1 | cut -d: -f1)"
    [[ -n "${start}" && -n "${end}" ]] || { echo "找不到 SYNC BLOCK「${name}」：${file}" >&2; exit 1; }
    sed -n "${start},${end}p" "${file}"
}

CFG="${TMP}/cfg"
STATE="${TMP}/state"
mkdir -p "${CFG}/certs" "${STATE}"
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo 'APP_NAME="SingR"'
    echo "CONFIG_DIR=\"${CFG}\""
    echo "CERT_DIR=\"${CFG}/certs\""
    echo "SERVER_CONFIG=\"${CFG}/server.json\""
    echo "PANEL_CONFIG=\"${CFG}/panel.json\""
    echo "CERT_SOURCE=\"${CFG}/cert-source.json\""
    extract_block "${REPO_DIR}/SingR.sh" 节点管理
    # ---- 后端桩 ----
    echo "node_backend_restart() { echo x >> '${STATE}/restarts'; }"
    echo 'node_backend_verify() { return 0; }'
    echo 'node_cert_renew_hint() { :; }'
} > "${TMP}/block.sh"

# curl 桩：记下 argv 和 stdin（-K - 的配置），按 ${STATE}/body、code 应答；
# ${STATE}/curl-fail 存在时模拟连不上。
cat > "${TMP}/curl-stub.sh" <<EOF
curl() {
    printf '%s\n' "\$@" > '${STATE}/argv'
    cat > '${STATE}/stdin'
    echo x >> '${STATE}/calls'
    local out=""
    while [[ \$# -gt 0 ]]; do [[ "\$1" == "-o" ]] && { out="\$2"; shift; }; shift; done
    [[ -f '${STATE}/curl-fail' ]] && return 7
    cp '${STATE}/body' "\${out}"
    cat '${STATE}/code'
}
EOF

run() { bash -c "source '${TMP}/block.sh'; source '${TMP}/curl-stub.sh'; $1" </dev/null 2>&1; }
run_real() { bash -c "source '${TMP}/block.sh'; $1" </dev/null 2>&1; }
respond() { printf '%s' "$2" > "${STATE}/body"; printf '%s' "$1" > "${STATE}/code"; }
node_json() { jq -cn --arg s "$1" '{ret: 1, data: {server: $s}}'; }
calls() { [[ -f "${STATE}/calls" ]] && wc -l <"${STATE}/calls" | tr -d ' ' || echo 0; }
reset_state() { rm -f "${STATE}"/*; }

echo "== SYNC BLOCK 在两个脚本里逐字相同 =="
if diff <(extract_block "${REPO_DIR}/SingR.sh" 节点管理) <(extract_block "${REPO_DIR}/SingR-docker.sh" 节点管理) >/dev/null; then
    pass "SingR.sh 与 SingR-docker.sh 的「节点管理」一致"
else
    fail "SingR.sh 与 SingR-docker.sh 的「节点管理」一致" "identical" "differs"
fi

echo "== node_panel_parse_server：与 Go 共用的样例 =="
rows=0
while IFS=$'\t' read -r _go want port host server; do
    [[ -z "${_go}" || "${_go}" == \#* ]] && continue
    rows=$((rows + 1))
    printf '%s' "${server}" > "${TMP}/server"
    got="$(bash -c "source '${TMP}/block.sh'; node_panel_parse_server \"\$(cat '${TMP}/server')\"" 2>/dev/null)"
    rc=$?
    if [[ "${want}" == "-" ]]; then
        [[ ${rc} -ne 0 && -z "${got}" ]] && pass "拒绝 ${server}" || fail "拒绝 ${server}" "非 0 退出、无输出" "rc=${rc} ${got}"
    else
        [[ "${host}" == "-" ]] && host=""
        check "${server}" "${want}"$'\t'"${port}"$'\t'"${host}" "${got}"
    fi
done < "${FIXTURES}"
[[ ${rows} -gt 10 ]] && pass "读到 ${rows} 条样例" || fail "读到样例" ">10" "${rows}"

echo "== node_panel_detect：请求与失败 =="
reset_state
respond 200 "$(node_json 'a.example.com;443;0;ws;tls;path=/hy2|host=h.example.com')"
got="$(run 'node_panel_detect "https://panel.example.com/" "se\"c\\ret key" 7 V2ray 20')"
check "识别出 hysteria2" "hysteria2"$'\t'"443"$'\t'"h.example.com" "${got}"
argv="$(cat "${STATE}/argv")"
stdin="$(cat "${STATE}/stdin")"
lacks "api-key 不在 curl 命令行里" 'c\ret key' "${argv}"
contains "从 stdin 读配置" "-K" "${argv}"
contains "去掉面板地址结尾的 / 再拼路径" 'url = "https://panel.example.com/mod_mu/nodes/7/info"' "${stdin}"
contains "key 参数（引号与反斜杠已转义）" 'data-urlencode = "key=se\"c\\ret key"' "${stdin}"
contains "muKey 参数" 'data-urlencode = "muKey=se\"c\\ret key"' "${stdin}"
contains "用 --get 把参数放进 query" $'\nget\n' "${stdin}"
contains "超时用 --timeout 的值" $'--max-time\n20' "${argv}"

reset_state
respond 200 '{"ret":0,"msg":"token is invalid"}'
got="$(run 'node_panel_detect https://p 1 7 V2ray 20')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "ret≠1 时失败" || fail "ret≠1 时失败" "非 0" "${rc}"
contains "报出面板的 msg" "token is invalid" "${got}"

reset_state
respond 502 '<html>Bad Gateway</html>'
got="$(run 'node_panel_detect https://p 1 7 V2ray 20')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "非 JSON 时失败" || fail "非 JSON 时失败" "非 0" "${rc}"
contains "报出状态码和响应开头" "HTTP 502）：<html>Bad Gateway" "${got}"

reset_state
respond 200 '{"ret":1,"data":{"server":""}}'
got="$(run 'node_panel_detect https://p 1 7 V2ray 20')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "data.server 为空时失败" || fail "data.server 为空时失败" "非 0" "${rc}"
contains "说明 data.server 为空" "data.server 为空" "${got}"

reset_state
respond 200 "$(node_json 'a.example.com;443;0;ws;tls;path=/ws')"
got="$(run 'node_panel_detect https://p 1 7 V2ray 20')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "普通 ws 节点被拒绝" || fail "普通 ws 节点被拒绝" "非 0" "${rc}"
contains "说明只认这三种 path" "/anytls、/hy2、/naive" "${got}"

reset_state
touch "${STATE}/curl-fail"
got="$(run 'node_panel_detect https://p 1 7 V2ray 20')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "连不上时失败" || fail "连不上时失败" "非 0" "${rc}"
contains "带上 curl 退出码" "curl 退出码 7" "${got}"

reset_state
got="$(run 'node_panel_detect https://p 1 7 trojan 20')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "--node-type trojan 被拒绝" || fail "--node-type trojan 被拒绝" "非 0" "${rc}"
check "不合适的 node-type 根本不发请求" "0" "$(calls)"

echo "== 真 curl 读 -K 配置（本地 HTTP 服务）=="
if command -v python3 >/dev/null 2>&1 && command -v curl >/dev/null 2>&1; then
    cat > "${TMP}/panel.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, parse_qs
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        u = urlparse(self.path); q = parse_qs(u.query)
        ok = u.path == "/sub/mod_mu/nodes/7/info" and q.get("key") == ['k"\\ &=x'] and q.get("muKey") == ['k"\\ &=x']
        body = {"ret": 1, "data": {"server": "a.example.com;8443;0;ws;tls;path=/naive|host=n.example.com"}} if ok \
            else {"ret": 0, "msg": "bad request " + self.path}
        b = json.dumps(body).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
s = HTTPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(s.server_address[1]))
s.serve_forever()
PY
    python3 -I "${TMP}/panel.py" "${TMP}/port" &
    SERVER_PID=$!
    disown "${SERVER_PID}" 2>/dev/null   # 退出时 kill 它，别让 bash 打印 Terminated
    for _ in $(seq 50); do [[ -s "${TMP}/port" ]] && break; sleep 0.1; done
    port="$(cat "${TMP}/port" 2>/dev/null)"
    got="$(run_real "node_panel_detect 'http://127.0.0.1:${port}/sub/' 'k\"\\ &=x' 7 V2ray 10")"
    check "key 原样（URL 编码后）到达面板，路径正确" "naive"$'\t'"8443"$'\t'"n.example.com" "${got}"
else
    echo "  skip - 没有 python3 或 curl"
fi

echo "== node_add 端到端（后端桩掉）=="
cp "${REPO_DIR}/release/poet/server.json" "${CFG}/server.json"
echo '{"nodes":[]}' > "${CFG}/panel.json"
echo x > "${CFG}/certs/default.pem"
echo x > "${CFG}/certs/default.key"
snapshot() { cat "${CFG}/server.json" "${CFG}/panel.json" | shasum | cut -d' ' -f1; }

reset_state
respond 200 "$(node_json 'a.example.com;443;0;ws;tls;path=/hy2|host=h.example.com')"
got="$(run 'node_add --api-url https://p.example.com --api-key k --node-id 9')"; rc=$?
check "不给 --protocol 也能添加" "0" "${rc}"
check "panel.json 指向 hysteria2 inbound" "hysteria2-in" "$(jq -r '.nodes[0].intag' "${CFG}/panel.json")"
check "该 inbound 类型是 hysteria2" "hysteria2" "$(jq -r '.inbounds[] | select(.tag=="hysteria2-in") | .type' "${CFG}/server.json")"
contains "提示面板给的协议" "协议 hysteria2，端口 443，SNI h.example.com" "${got}"
lacks "面板给了 host= 时不再提示 obfs" "obfs 会静默关闭" "${got}"
check "重启了一次" "1" "$(wc -l <"${STATE}/restarts" | tr -d ' ')"

reset_state
before="$(snapshot)"
respond 200 "$(node_json 'a.example.com;8443;0;ws;tls;path=/naive')"
got="$(run 'node_add --api-url https://p.example.com --api-key k --node-id 10 --protocol anytls')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "--protocol 与面板不一致时拒绝" || fail "--protocol 与面板不一致时拒绝" "非 0" "${rc}"
contains "说明面板是什么协议" "面板节点 #10 是 naive" "${got}"
check "拒绝时配置没动" "${before}" "$(snapshot)"
check "拒绝时没有重启" "no" "$([[ -f "${STATE}/restarts" ]] && echo yes || echo no)"

reset_state
respond 200 "$(node_json 'a.example.com;8443;0;ws;tls;path=/naive')"
got="$(run 'node_add --api-url https://p.example.com --api-key k --node-id 10 --protocol NAIVE')"; rc=$?
check "--protocol 与面板一致时照常添加" "0" "${rc}"
check "naive inbound 已写入" "naive" "$(jq -r '.inbounds[] | select(.tag=="naive-in") | .type' "${CFG}/server.json")"

reset_state
before="$(snapshot)"
respond 200 "$(node_json 'a.example.com;443;0;ws;tls;path=/ws')"
got="$(run 'node_add --api-url https://p.example.com --api-key k --node-id 11')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "认不出协议时拒绝" || fail "认不出协议时拒绝" "非 0" "${rc}"
check "认不出协议时配置没动" "${before}" "$(snapshot)"

reset_state
before="$(snapshot)"
touch "${STATE}/curl-fail"
got="$(run 'node_add --api-url https://p.example.com --api-key k --node-id 12')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "面板连不上时拒绝" || fail "面板连不上时拒绝" "非 0" "${rc}"
check "面板连不上时配置没动" "${before}" "$(snapshot)"

reset_state
respond 200 "$(node_json 'a.example.com;443;0;ws;tls;path=/hy2')"
got="$(run 'node_add --api-url https://p.example.com --api-key k --node-id 13')"; rc=$?
check "hy2 无 SNI 也能添加" "0" "${rc}"
contains "面板没 host= 又没 --sni 时提示 obfs" "obfs 会静默关闭" "${got}"
check "第二个 hysteria2 节点另分配 tag" "hysteria2-in-13" "$(jq -r '.nodes[-1].intag' "${CFG}/panel.json")"

echo
if [[ ${FAILED} -eq 0 ]]; then
    echo "全部通过"
else
    echo "有失败"
    exit 1
fi
