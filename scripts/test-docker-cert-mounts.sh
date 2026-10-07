#!/usr/bin/env bash
# docker 版证书原地引用的回归测试：cert_mount_dirs（容器要挂哪些目录）和
# certs_migrate（旧版复制模型的一次性迁移）。
#
# 函数直接从 SingR-docker.sh 里按名字抠出来跑，docker 相关的几个函数换成桩，
# 不需要 docker。
#
#   bash scripts/test-docker-cert-mounts.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
# pwd -P：macOS 的 /var 是 /private/var 的软链，不取规范路径的话 readlink -f 算出来的
# 目录就不在 CONFIG_DIR 前缀下了（生产环境的 /etc/singr-docker 本来就是规范路径）。
TMP="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "${TMP}"' EXIT

command -v jq >/dev/null 2>&1 || { echo "skip - 没有 jq"; exit 0; }

FAILED=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; echo "         want: [$2]"; echo "         got:  [$3]"; FAILED=1; }
check() { [[ "$2" == "$3" ]] && pass "$1" || fail "$1" "$2" "$3"; }

DOCKER_SH="${REPO_DIR}/SingR-docker.sh"
extract_block() {
    local start end
    start="$(grep -n "^# >>>>>>>>>>>>>>>> SYNC BLOCK: $1 " "${DOCKER_SH}" | head -1 | cut -d: -f1)"
    end="$(grep -n "^# <<<<<<<<<<<<<<<< SYNC BLOCK: $1 " "${DOCKER_SH}" | head -1 | cut -d: -f1)"
    sed -n "${start},${end}p" "${DOCKER_SH}"
}
extract_func() {
    awk -v name="$1" '$0 ~ "^"name"\\(\\) \\{" {p=1} p {print} p && /^}/ {exit}' "${DOCKER_SH}"
}

CFG="${TMP}/etc/singr-docker"
H="${TMP}/host"          # 宿主机上 CONFIG_DIR 以外的地方
STATE="${TMP}/state"
mkdir -p "${CFG}/certs" "${H}" "${STATE}"
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo "CONFIG_DIR=\"${CFG}\""
    echo "CERT_DIR=\"${CFG}/certs\""
    echo "SERVER_CONFIG=\"${CFG}/server.json\""
    echo "PANEL_CONFIG=\"${CFG}/panel.json\""
    echo "CERTS_MAP=\"${CFG}/certs.json\""
    echo "CERT_MIGRATION_NOTICE=\"${CFG}/cert-migration-notice.txt\""
    echo 'RUN_FLAGS=()'
    extract_block 节点管理
    for f in cert_mount_problem cert_dirs_for cert_path_mountable cert_mount_dirs certs_migrate; do
        extract_func "${f}"
    done
    echo 'docker() { :; }'
    echo "container_running() { [[ -f '${STATE}/running' ]]; }"
    echo "container_recreate() { echo x >> '${STATE}/recreates'; }"
    echo "verify_running() { [[ ! -f '${STATE}/verify_fail' ]]; }"
} > "${TMP}/block.sh"

run() { bash -c "source '${TMP}/block.sh'; $1" </dev/null; }
run_err() { bash -c "source '${TMP}/block.sh'; $1" </dev/null 2>&1 >/dev/null; }
lines() { printf '%s\n' "$@"; }

for f in cert_mount_problem cert_dirs_for cert_path_mountable cert_mount_dirs certs_migrate; do
    grep -q "^${f}() {" "${TMP}/block.sh" || { echo "FAIL - 没从 SingR-docker.sh 抠出 ${f}"; exit 1; }
done

echo "== cert_mount_problem：哪些目录不能同路径挂 =="
for d in / /etc /usr /usr/local/certs /tmp /var /opt /opt/singr /proc/x /run/certs; do
    run "cert_mount_problem '${d}'" >/dev/null && pass "拒绝 ${d}" || fail "拒绝 ${d}" "有问题" "放行"
done
for d in /etc/letsencrypt/live/a.example.com /etc/ssl/private /root /root/certs /opt/certs /home/u/c /var/lib/certs; do
    run "cert_mount_problem '${d}'" >/dev/null && fail "放行 ${d}" "放行" "拒绝" || pass "放行 ${d}"
done
run "cert_mount_problem '/root/a,b'" >/dev/null && pass "拒绝含逗号的路径" || fail "拒绝含逗号的路径" "有问题" "放行"
run "cert_mount_problem '/root/a:b'" >/dev/null && pass "拒绝含冒号的路径" || fail "拒绝含冒号的路径" "有问题" "放行"

echo "== cert_mount_dirs =="
# certbot 布局：live/ 下是指向 ../../archive/ 的软链。
mkdir -p "${H}/le/live/a" "${H}/le/archive/a" "${H}/plain/sub" "${H}/other" "${H}/a,b"
echo c > "${H}/le/archive/a/fullchain1.pem"; echo k > "${H}/le/archive/a/privkey1.pem"
ln -s ../../archive/a/fullchain1.pem "${H}/le/live/a/fullchain.pem"
ln -s ../../archive/a/privkey1.pem "${H}/le/live/a/privkey.pem"
echo c > "${H}/plain/c.pem"; echo k > "${H}/plain/sub/c.key"
echo c > "${H}/a,b/c.pem"; echo k > "${H}/a,b/c.key"
echo c > "${CFG}/certs/x.crt"; echo k > "${CFG}/certs/x.key"

inbound() { printf '{"type":"anytls","tag":"%s","tls":{"enabled":true,"certificate_path":"%s","key_path":"%s"}}' "$1" "$2" "$3"; }
server() { local IFS=,; printf '{"inbounds":[%s]}' "$*" > "${CFG}/server.json"; }

echo c > "${CFG}/certs/default.pem"; echo k > "${CFG}/certs/default.key"
server "$(inbound d-in "" "")" "$(inbound x-in "${CFG}/certs/x.crt" "${CFG}/certs/x.key")"
check "默认证书 + CONFIG_DIR 内的证书：不额外挂载" "" "$(run cert_mount_dirs 2>/dev/null)"

server "$(inbound le-in "${H}/le/live/a/fullchain.pem" "${H}/le/live/a/privkey.pem")"
check "certbot 软链：live 和 archive 都挂" "$(lines "${H}/le/archive/a" "${H}/le/live/a")" "$(run cert_mount_dirs 2>/dev/null)"

server "$(inbound p-in "${H}/plain/c.pem" "${H}/plain/sub/c.key")"
check "子目录被父目录包含：只挂父目录" "${H}/plain" "$(run cert_mount_dirs 2>/dev/null)"

rm -f "${CFG}/certs/default.pem"; ln -s "${H}/other/real.pem" "${CFG}/certs/default.pem"; echo c > "${H}/other/real.pem"
server "$(inbound d-in "" "")"
check "default.pem 是指向外面的软链：挂链接目标的目录" "${H}/other" "$(run cert_mount_dirs 2>/dev/null)"
rm -f "${CFG}/certs/default.pem"; echo c > "${CFG}/certs/default.pem"

server "$(inbound g-in "${H}/gone/c.pem" "${H}/gone/c.key")"
check "目录不存在：不挂（交给 entrypoint 报缺证书）" "" "$(run cert_mount_dirs 2>/dev/null)"

server "$(inbound comma-in "${H}/a,b/c.pem" "${H}/a,b/c.key")"
check "含逗号的目录：不挂" "" "$(run cert_mount_dirs 2>/dev/null)"
[[ "$(run_err cert_mount_dirs)" == *"跳过挂载"* ]] && pass "含逗号的目录：给出警告" || fail "含逗号的目录：给出警告" "跳过挂载" "$(run_err cert_mount_dirs)"

server "$(inbound rel-in "certs/c.pem" "certs/c.key")"
check "相对路径：不挂" "" "$(run cert_mount_dirs 2>/dev/null)"

printf '{"inbounds":[{"type":"anytls","tag":"off-in","tls":{"enabled":false,"certificate_path":"%s","key_path":"%s"}}]}' \
    "${H}/plain/c.pem" "${H}/plain/sub/c.key" > "${CFG}/server.json"
check "TLS 没开的 inbound：不挂" "" "$(run cert_mount_dirs 2>/dev/null)"

rm -f "${CFG}/server.json"
check "还没有 server.json：取 RUN_FLAGS（首次安装）" "${H}/plain" \
    "$(run "RUN_FLAGS=(--api-url x --cert-path '${H}/plain/c.pem' --key-path '${H}/plain/sub/c.key' --sni s); cert_mount_dirs" 2>/dev/null)"
server "$(inbound d-in "" "")"
check "有 server.json 时不看 RUN_FLAGS（不取并集）" "" \
    "$(run "RUN_FLAGS=(--cert-path '${H}/plain/c.pem' --key-path '${H}/plain/sub/c.key'); cert_mount_dirs" 2>/dev/null)"

echo "== cert_path_mountable（add / 安装时的预检）=="
run "cert_path_mountable '${H}/le/live/a/fullchain.pem'" 2>/dev/null && pass "certbot 路径可挂" || fail "certbot 路径可挂" 0 1
run "cert_path_mountable 'rel/c.pem'" 2>/dev/null && fail "相对路径拒绝" 1 0 || pass "相对路径拒绝"
run "cert_path_mountable '/etc/c.pem'" 2>/dev/null && fail "/etc 下的证书拒绝" 1 0 || pass "/etc 下的证书拒绝"

echo "== certs_migrate =="
setup_migrate() {
    rm -rf "${CFG}"/* "${STATE}"/*; mkdir -p "${CFG}/certs"
    echo c > "${CFG}/certs/default.pem"; echo k > "${CFG}/certs/default.key"
    echo c > "${CFG}/certs/cp-in.crt"; echo k > "${CFG}/certs/cp-in.key"
    echo c > "${CFG}/certs/lost-in.crt"; echo k > "${CFG}/certs/lost-in.key"
    server "$(inbound cp-in "${CFG}/certs/cp-in.crt" "${CFG}/certs/cp-in.key")" \
        "$(inbound lost-in "${CFG}/certs/lost-in.crt" "${CFG}/certs/lost-in.key")" \
        "$(inbound def-in "" "")"
    jq -n --arg le "${H}/le/live/a" --arg gone "${H}/gone" '{
        "cp-in":   {cert: ($le + "/fullchain.pem"), key: ($le + "/privkey.pem")},
        "lost-in": {cert: ($gone + "/c.pem"),       key: ($gone + "/c.key")},
        "def-in":  {cert: ($le + "/fullchain.pem"), key: ($le + "/privkey.pem")},
        "ghost-in":{cert: ($le + "/fullchain.pem"), key: ($le + "/privkey.pem")}
    }' > "${CFG}/certs.json"
}
tls_of() { jq -r --arg t "$1" '.inbounds[] | select(.tag==$t) | .tls.certificate_path + "|" + .tls.key_path' "${CFG}/server.json"; }

setup_migrate; touch "${STATE}/running"
run certs_migrate >/dev/null
check "副本节点改写回源路径" "${H}/le/live/a/fullchain.pem|${H}/le/live/a/privkey.pem" "$(tls_of cp-in)"
check "源已不存在：保留副本路径" "${CFG}/certs/lost-in.crt|${CFG}/certs/lost-in.key" "$(tls_of lost-in)"
check "默认证书节点：仍为空（不改写）" "|" "$(tls_of def-in)"
[[ ! -e "${CFG}/certs.json" ]] && ls "${CFG}"/certs.json.migrated-* >/dev/null 2>&1 \
    && pass "certs.json 改名，迁移只跑一次" || fail "certs.json 改名" "certs.json.migrated-*" "$(ls "${CFG}")"
[[ ! -e "${CFG}/certs/cp-in.crt" ]] && ls "${CFG}"/certs/.migrated-*/cp-in.crt >/dev/null 2>&1 \
    && pass "不再引用的副本收进 .migrated-*" || fail "不再引用的副本收进 .migrated-*" "移走" "$(ls -A "${CFG}/certs")"
[[ -e "${CFG}/certs/lost-in.crt" ]] && pass "仍被引用的副本留在原处" || fail "仍被引用的副本留在原处" "在" "不在"
notice="$(cat "${CFG}/cert-migration-notice.txt" 2>/dev/null)"
[[ "${notice}" == *"def-in"* && "${notice}" == *"lost-in"* && "${notice}" != *"cp-in"* && "${notice}" != *"ghost-in"* ]] \
    && pass "提示文件只记无法自动续期的节点（def-in、lost-in）" || fail "提示文件内容" "def-in + lost-in" "${notice}"
check "容器在跑：重建一次" "1" "$(wc -l <"${STATE}/recreates" | tr -d ' ')"
echo '{"nodes":[{"intag":"cp-in","apiconfig":{"apihost":"https://p.example.com","nodeid":1}}]}' > "${CFG}/panel.json"
[[ "$(run 'node_list' 2>/dev/null)" == *"cert-migration-notice.txt"* ]] && pass "singr list 显示迁移提示" \
    || fail "singr list 显示迁移提示" "提示行" "无"
run certs_migrate >/dev/null
check "第二次执行什么都不做" "1" "$(wc -l <"${STATE}/recreates" | tr -d ' ')"

setup_migrate
run certs_migrate >/dev/null
check "容器没在跑：只改配置不重建" "0" "$( [[ -f "${STATE}/recreates" ]] && wc -l <"${STATE}/recreates" | tr -d ' ' || echo 0)"
check "容器没在跑：配置照样改写" "${H}/le/live/a/fullchain.pem|${H}/le/live/a/privkey.pem" "$(tls_of cp-in)"

setup_migrate; touch "${STATE}/running" "${STATE}/verify_fail"
run certs_migrate >/dev/null
check "迁移后起不来：还原 server.json" "${CFG}/certs/cp-in.crt|${CFG}/certs/cp-in.key" "$(tls_of cp-in)"
[[ -e "${CFG}/certs.json" ]] && pass "迁移后起不来：保留 certs.json 下次重试" || fail "保留 certs.json" "在" "不在"
[[ -e "${CFG}/certs/cp-in.crt" ]] && pass "迁移后起不来：副本不动" || fail "副本不动" "在" "不在"

echo
if [[ "${FAILED}" == 0 ]]; then
    echo "PASS: docker 证书挂载 / 迁移 测试通过"
else
    echo "FAIL: 有用例未通过"
fi
exit "${FAILED}"
