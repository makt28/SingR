#!/usr/bin/env bash
# 删掉最后一个节点（零节点状态）的回归测试。
#
# 没有节点时二进制以 'no Node with InTag found' 退出，开着开机自启 / --restart 的话
# 会被无限拉起。所以删最后一个节点时不走重启校验，而是 node_backend_idle：停掉后端，
# 开着自启就暂停并留标记；有节点了再由 node_backend_resume 按标记恢复。
#
# 第一部分从 SingR.sh 抠出 SYNC BLOCK「节点管理」，后端钩子换成只记调用的桩，验证
# del / add / 启动守卫在零节点前后各调了什么。第二部分单独抠出两个后端的 idle/resume
# 实现，systemctl / docker 换成改状态文件的桩，验证"只恢复自己暂停的那份自启"。
#
#   bash scripts/test-node-del.sh
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
contains() { [[ "$3" == *"$2"* ]] && pass "$1" || fail "$1" "*$2*" "$3"; }

extract_block() {
    local file="$1" name="$2" start end
    start="$(grep -n "^# >>>>>>>>>>>>>>>> SYNC BLOCK: ${name} " "${file}" | head -1 | cut -d: -f1)"
    end="$(grep -n "^# <<<<<<<<<<<<<<<< SYNC BLOCK: ${name} " "${file}" | head -1 | cut -d: -f1)"
    [[ -n "${start}" && -n "${end}" ]] || { echo "找不到 SYNC BLOCK「${name}」：${file}" >&2; exit 1; }
    sed -n "${start},${end}p" "${file}"
}

# 按名字抠出顶层函数（从 `name() {` 到第一个顶格的 `}`）。
extract_fn() {
    awk -v n="$2" '$0 ~ "^"n"\\(\\) *\\{" {p=1} p {print} p && /^}/ {exit}' "$1"
}

CFG="${TMP}/cfg"
STATE="${TMP}/state"
mkdir -p "${CFG}/certs" "${STATE}"
echo x > "${CFG}/certs/default.pem"
echo x > "${CFG}/certs/default.key"
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo 'APP_NAME="SingR"'
    echo "CONFIG_DIR=\"${CFG}\""
    echo "CERT_DIR=\"${CFG}/certs\""
    echo "SERVER_CONFIG=\"${CFG}/server.json\""
    echo "PANEL_CONFIG=\"${CFG}/panel.json\""
    echo "CERT_SOURCE=\"${CFG}/cert-source.json\""
    echo 'confirm() { return 0; }'
    extract_block "${REPO_DIR}/SingR.sh" 节点管理
    # ---- 后端桩：只记调用顺序 ----
    echo "node_backend_restart() { echo restart >> '${STATE}/calls'; }"
    echo "node_backend_verify() { echo verify >> '${STATE}/calls'; [[ ! -f '${STATE}/verify-fail' ]]; }"
    echo "node_backend_idle() { echo idle >> '${STATE}/calls'; }"
    echo "node_backend_resume() { echo resume >> '${STATE}/calls'; }"
    echo 'node_cert_renew_hint() { :; }'
    # 面板请求桩：固定回一个 anytls 节点
    echo "node_panel_fetch() { jq -cn '{ret:1,data:{server:\"a.example.com;443;0;ws;tls;path=/anytls\"}}' > \"\$5\"; echo 200; }"
} > "${TMP}/block.sh"

run() { bash -c "source '${TMP}/block.sh'; $1" </dev/null 2>&1; }
calls() { [[ -f "${STATE}/calls" ]] && tr '\n' ' ' <"${STATE}/calls" | sed 's/ $//' || echo ""; }
reset_state() { rm -f "${STATE}"/*; }
nodes() { jq -r '[.nodes[].intag] | join(",")' "${CFG}/panel.json"; }

cp "${REPO_DIR}/release/poet/server.json" "${CFG}/server.json"
echo '{"nodes":[]}' > "${CFG}/panel.json"

echo "== 先加两个节点 =="
reset_state
run 'node_add --api-url https://p.example.com --api-key k --node-id 1' >/dev/null
run 'node_add --api-url https://p.example.com --api-key k --node-id 2' >/dev/null
check "两个节点" "anytls-in,anytls-in-2" "$(nodes)"

echo "== 还剩多个节点时删一个：照常重启校验 =="
reset_state
out="$(run 'node_del 2')"; rc=$?
check "删除成功" "0" "${rc}"
check "剩下一个" "anytls-in" "$(nodes)"
check "走重启校验，不进零节点状态" "restart verify" "$(calls)"

echo "== 删最后一个节点 =="
reset_state
out="$(run 'node_del 1')"; rc=$?
check "允许删除" "0" "${rc}"
check "panel.json 没有节点了" "" "$(nodes)"
check "对应 inbound 一并删掉" "0" "$(jq '[.inbounds[] | select(.tag=="anytls-in")] | length' "${CFG}/server.json")"
check "不重启，只让后端停下" "idle" "$(calls)"
check "备份文件已清理" "0" "$(find "${CFG}" -name '*.bak' | wc -l | tr -d ' ')"
contains "提示添加节点后会自动启动" "singr add 添加节点后会自动启动" "${out}"

echo "== 零节点时的启动守卫 =="
reset_state
check "node_none_configured 为真" "0" "$(run 'node_none_configured; echo $?' | tail -1)"
out="$(run 'node_guard_start')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "start/restart 被拦下" || fail "start/restart 被拦下" "非 0" "${rc}"
contains "提示先 singr add" "先用 singr add 添加节点" "${out}"
check "被拦下时不动自启" "" "$(calls)"

echo "== 从零节点添加，起不来 =="
reset_state
touch "${STATE}/verify-fail"
out="$(run 'node_add --api-url https://p.example.com --api-key k --node-id 3')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "添加失败" || fail "添加失败" "非 0" "${rc}"
check "回滚到零节点" "" "$(nodes)"
check "回滚后回到停止状态，不再重启、不恢复自启" "restart verify idle" "$(calls)"
contains "说明保持停止" "保持停止" "${out}"

echo "== 从零节点添加，起来了 =="
reset_state
out="$(run 'node_add --api-url https://p.example.com --api-key k --node-id 3')"; rc=$?
check "添加成功" "0" "${rc}"
check "重启校验通过后才恢复自启" "restart verify resume" "$(calls)"
reset_state
out="$(run 'node_guard_start')"; rc=$?
check "有节点时守卫放行" "0" "${rc}"
check "放行时顺带恢复自启（手写 panel.json 的情况）" "resume" "$(calls)"

echo "== panel.json 解析不了时不拦 =="
cp "${CFG}/panel.json" "${CFG}/panel.json.keep"
echo '{broken' > "${CFG}/panel.json"
run 'node_none_configured' >/dev/null && fail "解析失败不算零节点" "非 0" "0" || pass "解析失败不算零节点"
mv -f "${CFG}/panel.json.keep" "${CFG}/panel.json"

echo "== 裸机 idle/resume：只恢复自己暂停的自启 =="
# systemctl 桩：${STATE}/enabled 存在 = 开机自启开着。
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo 'SERVICE_NAME="singr"'
    echo "CONFIG_DIR=\"${CFG}\""
    echo "systemctl() {
        echo \"\$*\" >> '${STATE}/systemctl'
        case \"\$1\" in
            is-enabled) [[ -f '${STATE}/enabled' ]] ;;
            enable) touch '${STATE}/enabled' ;;
            disable) rm -f '${STATE}/enabled' ;;
        esac
    }"
    extract_fn "${REPO_DIR}/SingR.sh" check_enabled
    grep '^AUTOSTART_PAUSED=' "${REPO_DIR}/SingR.sh"
    extract_fn "${REPO_DIR}/SingR.sh" node_backend_idle
    extract_fn "${REPO_DIR}/SingR.sh" node_backend_resume
} > "${TMP}/bare.sh"
bare() { bash -c "source '${TMP}/bare.sh'; $1" </dev/null 2>&1; }
enabled() { [[ -f "${STATE}/enabled" ]] && echo on || echo off; }
paused() { [[ -f "${CFG}/autostart-paused" ]] && echo yes || echo no; }

reset_state; rm -f "${CFG}/autostart-paused"
touch "${STATE}/enabled"
bare 'node_backend_idle' >/dev/null
check "开着自启时 idle 关掉它" "off" "$(enabled)"
check "并留下标记" "yes" "$(paused)"
contains "并停掉服务" "stop singr" "$(cat "${STATE}/systemctl")"
bare 'node_backend_idle' >/dev/null
check "再 idle 一次标记仍在（回滚路径会这样调）" "yes" "$(paused)"
bare 'node_backend_resume' >/dev/null
check "resume 恢复自启" "on" "$(enabled)"
check "并清掉标记" "no" "$(paused)"

reset_state; rm -f "${CFG}/autostart-paused"
bare 'node_backend_idle' >/dev/null
check "用户自己关了自启：idle 不留标记" "no" "$(paused)"
bare 'node_backend_resume' >/dev/null
check "resume 不替用户打开自启" "off" "$(enabled)"

echo "== docker idle/resume =="
# docker 桩：${STATE}/policy 是容器当前的 restart 策略，${STATE}/exists 表示容器存在。
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo 'CONTAINER="singr"; RESTART="always"'
    echo "CONFIG_DIR=\"${CFG}\""
    echo "docker() {
        echo \"\$*\" >> '${STATE}/docker'
        case \"\$1\" in
            ps) [[ -f '${STATE}/exists' ]] && echo singr ;;
            inspect) [[ -f '${STATE}/exists' ]] && cat '${STATE}/policy' ;;
            update) [[ -f '${STATE}/exists' ]] || return 1; printf '%s' \"\${2#--restart=}\" > '${STATE}/policy' ;;
            stop) [[ -f '${STATE}/exists' ]] ;;
        esac
    }"
    extract_fn "${REPO_DIR}/SingR-docker.sh" container_exists
    extract_fn "${REPO_DIR}/SingR-docker.sh" container_restart_policy
    grep '^AUTOSTART_PAUSED=' "${REPO_DIR}/SingR-docker.sh"
    extract_fn "${REPO_DIR}/SingR-docker.sh" node_backend_idle
    extract_fn "${REPO_DIR}/SingR-docker.sh" node_backend_resume
} > "${TMP}/docker.sh"
dk() { bash -c "source '${TMP}/docker.sh'; $1" </dev/null 2>&1; }
policy() { cat "${STATE}/policy"; }

for pol in always unless-stopped on-failure:3; do
    reset_state; rm -f "${CFG}/autostart-paused"
    touch "${STATE}/exists"; printf '%s' "${pol}" > "${STATE}/policy"
    dk 'node_backend_idle' >/dev/null
    check "--restart=${pol}：idle 改成 no" "no" "$(policy)"
    check "--restart=${pol}：留下标记" "yes" "$(paused)"
done
contains "并停掉容器" "stop singr" "$(cat "${STATE}/docker")"
dk 'node_backend_resume' >/dev/null
check "resume 按 docker.conf 的 RESTART 恢复" "always" "$(policy)"
check "并清掉标记" "no" "$(paused)"

reset_state; rm -f "${CFG}/autostart-paused"
touch "${STATE}/exists"; printf 'no' > "${STATE}/policy"
dk 'node_backend_idle' >/dev/null
check "用户 singr disable 过：idle 不留标记" "no" "$(paused)"

reset_state
: > "${CFG}/autostart-paused"
out="$(dk 'node_backend_resume')"
check "容器已被删掉（update 时）：resume 只清标记" "no" "$(paused)"
reset_state
out="$(dk 'node_backend_idle')"; rc=$?
check "容器不存在时 idle 也不报错" "0" "${rc}"

echo
if [[ ${FAILED} -eq 0 ]]; then
    echo "全部通过"
else
    echo "有失败"
    exit 1
fi
