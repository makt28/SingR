#!/usr/bin/env bash
# 节点独立证书（SYNC BLOCK「节点独立证书」）+ 节点 SNI 显示 + 管理脚本跟着 update 升级
# 的回归测试。
#
# 和 test-cert-source.sh 同样的做法：从 SingR.sh 抠出 SYNC BLOCK 来跑，下载
# （cert_source_fetch）、面板请求（node_panel_fetch）、后端重启 / 校验、systemctl 换成桩。
# 重点守三件事：
#   · 节点证书和默认证书互不影响（谁更新都碰不到另一边的文件和登记）；
#   · 生命周期跟着节点走（add 失败不留东西，del 连证书一起删，删空卸 timer）；
#   · 多个节点各自更新，一个失败不耽误其他的。
#
#   bash scripts/test-node-cert.sh
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
contains() { [[ "$3" == *"$2"* ]] && pass "$1" || fail "$1" "包含 $2" "$3"; }
lacks() { [[ "$3" != *"$2"* ]] && pass "$1" || fail "$1" "不含 $2" "$3"; }

extract_block() {
    local file="$1" name="$2" start end
    start="$(grep -n "^# >>>>>>>>>>>>>>>> SYNC BLOCK: ${name} " "${file}" | head -1 | cut -d: -f1)"
    end="$(grep -n "^# <<<<<<<<<<<<<<<< SYNC BLOCK: ${name} " "${file}" | head -1 | cut -d: -f1)"
    [[ -n "${start}" && -n "${end}" ]] || { echo "找不到 SYNC BLOCK「${name}」：${file}" >&2; exit 1; }
    sed -n "${start},${end}p" "${file}"
}

echo "== SYNC BLOCK 在两个脚本里逐字相同 =="
for name in 节点独立证书 通用函数; do
    if diff <(extract_block "${REPO_DIR}/SingR.sh" "${name}") <(extract_block "${REPO_DIR}/SingR-docker.sh" "${name}") >/dev/null; then
        pass "「${name}」一致"
    else
        fail "「${name}」一致" "相同" "不同"
    fi
done

CFG="${TMP}/cfg"
REMOTE="${TMP}/remote"
STATE="${TMP}/state"
SYSD="${TMP}/systemd"
mkdir -p "${CFG}/certs" "${REMOTE}" "${STATE}" "${SYSD}"
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo 'APP_NAME="SingR"; SELF_CMD="/usr/bin/SingR"'
    echo "CONFIG_DIR=\"${CFG}\""
    echo "CERT_DIR=\"${CFG}/certs\""
    echo "SERVER_CONFIG=\"${CFG}/server.json\""
    echo "PANEL_CONFIG=\"${CFG}/panel.json\""
    echo "CERT_TIMER_DIR=\"${SYSD}\""
    echo 'confirm() { return 0; }'
    extract_block "${REPO_DIR}/SingR.sh" 节点管理
    extract_block "${REPO_DIR}/SingR.sh" 默认证书更新源
    extract_block "${REPO_DIR}/SingR.sh" 节点独立证书
    # ---- 桩 ----
    # https://remote/<名字> -> ${REMOTE}/<名字>；文件不存在就是下载失败。
    echo "cert_source_fetch() { local f=\"${REMOTE}/\${1##*/}\"; [[ -s \"\${f}\" ]] && cp \"\${f}\" \"\$2\"; }"
    echo "node_backend_restart() { echo x >> '${STATE}/restarts'; }"
    echo "node_backend_verify() { [[ ! -f '${STATE}/verify-fail' ]]; }"
    echo 'node_backend_idle() { :; }; node_backend_resume() { :; }; node_cert_renew_hint() { :; }'
    echo 'systemctl() { :; }'
    # 面板：${STATE}/panel-<id> 里放 server 字符串；没有这个文件 = 面板连不上。
    echo "node_panel_fetch() { local f='${STATE}/panel-'\"\$3\"; [[ -f \"\${f}\" ]] || return 7; jq -cn --arg s \"\$(cat \"\${f}\")\" '{ret:1,data:{server:\$s}}' > \"\$5\"; echo 200; }"
} > "${TMP}/block.sh"

run() { bash -c "source '${TMP}/block.sh'; $1" </dev/null 2>&1; }

mkcert() { # <名字> <天数> [CN]
    openssl req -x509 -newkey rsa:2048 -nodes -days "$2" -subj "/CN=${3:-t.example.com}" \
        -keyout "${TMP}/$1.key" -out "${TMP}/$1.pem" 2>/dev/null
}
fp() { openssl x509 -noout -fingerprint -sha256 -in "$1" 2>/dev/null; }
put_remote() { cp "${TMP}/$1.pem" "${REMOTE}/$2.pem"; cp "${TMP}/$1.key" "${REMOTE}/$2.key"; }
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
restarts() { [[ -f "${STATE}/restarts" ]] && wc -l <"${STATE}/restarts" | tr -d ' ' || echo 0; }
panel() { printf '%s' "$2" > "${STATE}/panel-$1"; }
reset() {
    rm -rf "${CFG}" "${STATE}"/* "${REMOTE}"/* "${SYSD}"/*
    mkdir -p "${CFG}/certs"
    cp "${REPO_DIR}/release/poet/server.json" "${CFG}/server.json"
    echo '{"nodes":[]}' > "${CFG}/panel.json"
    cp "${TMP}/def.pem" "${CFG}/certs/default.pem"; cp "${TMP}/def.key" "${CFG}/certs/default.key"
}
add() { run "node_add --api-url https://p.example.com --api-key k --node-id $1 ${2:-}"; }
NC="${CFG}/certs/node"

mkcert def 90 default.example.com
mkcert a 90 a.example.com
mkcert a2 90 a.example.com
mkcert asoon 3 a.example.com
mkcert along 60 a.example.com
mkcert b 90 b.example.com
mkcert bsoon 3 b.example.com
mkcert bnew 80 b.example.com

echo "== add --node-cert-url =="
reset; put_remote a a
panel 1 'x.example.com;443;0;ws;tls;path=/anytls|host=a.example.com'
out="$(add 1 '--node-cert-url https://remote/a.pem --node-key-url https://remote/a.key')"; rc=$?
check "添加成功" "0" "${rc}"
lacks "没有调到未定义的函数" "command not found" "${out}"
check "证书下载到 certs/node/<intag>.pem" "$(fp "${TMP}/a.pem")" "$(fp "${NC}/anytls-in.pem")"
check "server.json 写显式路径（证书）" "${NC}/anytls-in.pem" "$(jq -r '.inbounds[]|select(.tag=="anytls-in")|.tls.certificate_path' "${CFG}/server.json")"
check "server.json 写显式路径（私钥）" "${NC}/anytls-in.key" "$(jq -r '.inbounds[]|select(.tag=="anytls-in")|.tls.key_path' "${CFG}/server.json")"
check "登记了来源" "https://remote/a.pem" "$(jq -r '."anytls-in".cert_url' "${CFG}/node-certs.json")"
check "登记文件权限 600" "600" "$(mode "${CFG}/node-certs.json")"
check "私钥权限 600" "600" "$(mode "${NC}/anytls-in.key")"
[[ -f "${SYSD}/singr-node-cert-update.timer" ]] && pass "装上节点证书 timer" || fail "装上节点证书 timer" "有" "无"
contains "timer 调的是 node-cert-update" "ExecStart=/usr/bin/SingR node-cert-update" "$(cat "${SYSD}/singr-node-cert-update.service" 2>/dev/null)"
[[ ! -e "${SYSD}/singr-cert-update.timer" ]] && pass "不碰默认证书的 timer" || fail "不碰默认证书的 timer" "无" "有"
[[ ! -e "${CFG}/cert-source.json" ]] && pass "不碰默认证书更新源" || fail "不碰默认证书更新源" "无" "有"
check "默认证书原封不动" "$(fp "${TMP}/def.pem")" "$(fp "${CFG}/certs/default.pem")"
check "记下了面板 host" "a.example.com" "$(jq -r '."anytls-in".panel_host' "${CFG}/node-meta.json")"
check "只重启一次" "1" "$(restarts)"

echo "== add 失败不留东西 =="
reset
panel 1 'x.example.com;443;0;ws;tls;path=/anytls'
before="$(cat "${CFG}/server.json" "${CFG}/panel.json")"
out="$(add 1 '--node-cert-url https://remote/a.pem --node-key-url https://remote/a.key')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "下载失败：拒绝" || fail "下载失败：拒绝" "非 0" "${rc}"
check "下载失败：配置没动" "${before}" "$(cat "${CFG}/server.json" "${CFG}/panel.json")"
[[ ! -e "${NC}/anytls-in.pem" && ! -e "${CFG}/node-certs.json" ]] && pass "下载失败：不留证书和登记" || fail "下载失败：不留证书和登记" "无" "$(ls -A "${NC}" 2>/dev/null)"
check "下载失败：不重启" "0" "$(restarts)"

reset; put_remote a a; touch "${STATE}/verify-fail"
panel 1 'x.example.com;443;0;ws;tls;path=/anytls'
before="$(cat "${CFG}/server.json" "${CFG}/panel.json")"
out="$(add 1 '--node-cert-url https://remote/a.pem --node-key-url https://remote/a.key')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "起不来：失败" || fail "起不来：失败" "非 0" "${rc}"
check "起不来：配置回滚" "${before}" "$(cat "${CFG}/server.json" "${CFG}/panel.json")"
[[ ! -e "${NC}/anytls-in.pem" && ! -e "${CFG}/node-certs.json" && ! -e "${SYSD}/singr-node-cert-update.timer" ]] \
    && pass "起不来：证书、登记、timer 都清掉" || fail "起不来：证书、登记、timer 都清掉" "无" "$(ls -A "${NC}" "${SYSD}" 2>/dev/null)"

echo "== 参数检查 =="
reset
panel 1 'x.example.com;443;0;ws;tls;path=/anytls'
out="$(add 1 '--node-cert-url https://remote/a.pem')"
contains "只给一半被拒" "必须同时给出" "${out}"
out="$(add 1 '--node-cert-url https://remote/a.pem --node-key-url https://remote/a.key --cert-url https://r/c --key-url https://r/k')"
contains "和 --cert-url 互斥" "不能和 --cert-path / --cert-url 一起用" "${out}"
out="$(add 1 '--node-cert-url https://remote/a.pem --node-key-url https://remote/a.key --cert-path /x --key-path /y')"
contains "和 --cert-path 互斥" "不能和 --cert-path / --cert-url 一起用" "${out}"
out="$(add 1 '--node-cert-url http://remote/a.pem --node-key-url https://remote/a.key')"
contains "拒绝 http" "必须是 https://" "${out}"
check "参数错误都不重启" "0" "$(restarts)"

echo "== 两个节点各自更新 =="
reset; put_remote asoon a; put_remote b b
panel 1 'x.example.com;443;0;ws;tls;path=/anytls|host=a.example.com'
panel 2 'x.example.com;8443;0;ws;tls;path=/anytls|host=b.example.com'
add 1 '--node-cert-url https://remote/a.pem --node-key-url https://remote/a.key' >/dev/null
add 2 '--node-cert-url https://remote/b.pem --node-key-url https://remote/b.key' >/dev/null
B="anytls-in-2"
check "第二个节点也登记了" "https://remote/b.pem" "$(jq -r --arg t "${B}" '.[$t].cert_url' "${CFG}/node-certs.json")"
put_remote along a; put_remote bnew b
r0="$(restarts)"
out="$(run 'node_cert_update_all')"; rc=$?
check "临期的 a 被替换" "$(fp "${TMP}/along.pem")" "$(fp "${NC}/anytls-in.pem")"
check "没到期的 b 不动" "$(fp "${TMP}/b.pem")" "$(fp "${NC}/${B}.pem")"
check "全部成功返回 0" "0" "${rc}"
check "只换文件不重启" "${r0}" "$(restarts)"
contains "提示里带 SNI（a）" "节点 anytls-in（SNI a.example.com）" "${out}"
contains "提示里带 SNI（b）" "节点 ${B}（SNI b.example.com）的证书剩余" "${out}"

# a 的远端下线 + b 临期且远端有新证书：a 失败不耽误 b，整体非零。
cp "${TMP}/bsoon.pem" "${NC}/${B}.pem"; cp "${TMP}/bsoon.key" "${NC}/${B}.key"
cp "${TMP}/asoon.pem" "${NC}/anytls-in.pem"; cp "${TMP}/asoon.key" "${NC}/anytls-in.key"
rm -f "${REMOTE}/a.pem"
out="$(run 'node_cert_update_all')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "有失败就非零" || fail "有失败就非零" "非 0" "${rc}"
check "a 失败：保留原证书" "$(fp "${TMP}/asoon.pem")" "$(fp "${NC}/anytls-in.pem")"
check "a 失败不耽误 b" "$(fp "${TMP}/bnew.pem")" "$(fp "${NC}/${B}.pem")"

echo "== 和默认证书互不影响 =="
check "默认证书的使用者里没有节点证书节点" "" "$(run 'cert_default_inbounds')"
mkcert def2 90 default.example.com
cp "${TMP}/def2.pem" "${REMOTE}/cert.pem"; cp "${TMP}/def2.key" "${REMOTE}/key.pem"
before="$(fp "${NC}/anytls-in.pem")$(fp "${NC}/${B}.pem")$(cat "${CFG}/node-certs.json")"
run 'cert_source_set https://remote/cert.pem https://remote/key.pem' >/dev/null
check "配默认更新源：换的是默认证书" "$(fp "${TMP}/def2.pem")" "$(fp "${CFG}/certs/default.pem")"
check "配默认更新源：节点证书和登记不动" "${before}" "$(fp "${NC}/anytls-in.pem")$(fp "${NC}/${B}.pem")$(cat "${CFG}/node-certs.json")"
[[ -f "${SYSD}/singr-cert-update.timer" && -f "${SYSD}/singr-node-cert-update.timer" ]] \
    && pass "两个 timer 各自存在" || fail "两个 timer 各自存在" "都有" "$(ls "${SYSD}")"
before="$(fp "${CFG}/certs/default.pem")$(cat "${CFG}/cert-source.json")"
run 'node_cert_update_all --force' >/dev/null
check "节点证书更新：默认证书和更新源不动" "${before}" "$(fp "${CFG}/certs/default.pem")$(cat "${CFG}/cert-source.json")"

echo "== node-cert 命令 =="
mkcert b3 85 b.example.com
put_remote b3 b3
out="$(run "node_cert_cmd ${B} --cert-url https://remote/b3.pem --key-url https://remote/b3.key")"
check "更换来源：立即下载" "$(fp "${TMP}/b3.pem")" "$(fp "${NC}/${B}.pem")"
check "更换来源：登记更新" "https://remote/b3.pem" "$(jq -r --arg t "${B}" '.[$t].cert_url' "${CFG}/node-certs.json")"
out="$(run "node_cert_cmd ${B}")"
contains "查看单个：带 SNI" "${B}（SNI b.example.com）" "${out}"
contains "查看单个：地址打码前缀" "https://remote/b3.pem" "${out}"
out="$(run 'node_cert_cmd')"
contains "查看全部：列出 a" "anytls-in（SNI a.example.com）" "${out}"
# 用默认证书的节点不能只换来源
panel 3 'x.example.com;9443;0;ws;tls;path=/anytls|host=d.example.com'
add 3 >/dev/null
out="$(run 'node_cert_cmd anytls-in-3 --cert-url https://remote/b3.pem --key-url https://remote/b3.key')"; rc=$?
[[ ${rc} -ne 0 ]] && pass "默认证书节点：拒绝只换来源" || fail "默认证书节点：拒绝只换来源" "非 0" "${rc}"
check "默认证书节点：默认证书不动" "$(fp "${TMP}/def2.pem")" "$(fp "${CFG}/certs/default.pem")"
out="$(run 'cert_source_show')"
contains "默认证书查看：列出使用节点和 SNI" "anytls-in-3（SNI d.example.com）" "${out}"
lacks "默认证书查看：不含节点证书节点" "anytls-in（SNI a.example.com）" "${out}"

echo "== singr list 的 SNI 列 =="
out="$(run 'node_list')"
grep -q "anytls-in .*a.example.com .*OK" <<<"${out}" && pass "列表显示面板 host 作为 SNI" || fail "列表显示面板 host 作为 SNI" "a.example.com" "$(grep 'anytls-in ' <<<"${out}")"
# 老节点（没有记录）：不带 --refresh 显示 server_name 或 -，带 --refresh 补查面板。
jq 'del(."anytls-in-3")' "${CFG}/node-meta.json" > "${TMP}/m" && mv "${TMP}/m" "${CFG}/node-meta.json"
out="$(run 'node_list')"
grep -q "anytls-in-3 .* - .*OK" <<<"${out}" && pass "没记录且没 server_name：显示 -" || fail "没记录且没 server_name：显示 -" "-" "$(grep 'anytls-in-3' <<<"${out}")"
out="$(run 'node_list --refresh')"
grep -q "anytls-in-3 .*d.example.com" <<<"${out}" && pass "--refresh 向面板补查 host" || fail "--refresh 向面板补查 host" "d.example.com" "$(grep 'anytls-in-3' <<<"${out}")"
check "补查结果记下来" "d.example.com" "$(jq -r '."anytls-in-3".panel_host' "${CFG}/node-meta.json")"
jq 'del(."anytls-in-3")' "${CFG}/node-meta.json" > "${TMP}/m" && mv "${TMP}/m" "${CFG}/node-meta.json"
rm -f "${STATE}/panel-3"
out="$(run 'node_list --refresh')"
grep -q "anytls-in-3 .*OK" <<<"${out}" && pass "面板连不上：列表照常输出" || fail "面板连不上：列表照常输出" "有该行" "${out}"
run 'node_meta_has anytls-in-3' >/dev/null && fail "面板连不上：不记空值" "无记录" "有" || pass "面板连不上：不记空值，下次再查"

echo "== del 跟着清理 =="
out="$(run "node_del ${B}")"
lacks "del 没有调到未定义的函数" "command not found" "${out}"
[[ ! -e "${NC}/${B}.pem" && ! -e "${NC}/${B}.key" ]] && pass "删节点：证书一起删" || fail "删节点：证书一起删" "无" "$(ls -A "${NC}")"
check "删节点：登记删掉" "false" "$(jq --arg t "${B}" 'has($t)' "${CFG}/node-certs.json")"
check "删节点：SNI 记录删掉" "false" "$(jq --arg t "${B}" 'has($t)' "${CFG}/node-meta.json")"
[[ -f "${SYSD}/singr-node-cert-update.timer" ]] && pass "还有节点证书：timer 保留" || fail "还有节点证书：timer 保留" "有" "无"
run 'node_del anytls-in-3' >/dev/null
check "删默认证书节点：默认证书不动" "$(fp "${TMP}/def2.pem")" "$(fp "${CFG}/certs/default.pem")"
run 'node_del anytls-in' >/dev/null
[[ ! -e "${SYSD}/singr-node-cert-update.timer" && ! -e "${CFG}/node-certs.json" ]] \
    && pass "最后一个节点证书删掉：卸 timer、删登记" || fail "最后一个节点证书删掉：卸 timer、删登记" "无" "$(ls "${SYSD}" "${CFG}")"
[[ -f "${SYSD}/singr-cert-update.timer" ]] && pass "默认证书 timer 不受影响" || fail "默认证书 timer 不受影响" "有" "无"

echo "== --clear =="
reset; put_remote a a
panel 1 'x.example.com;443;0;ws;tls;path=/anytls|host=a.example.com'
add 1 '--node-cert-url https://remote/a.pem --node-key-url https://remote/a.key' >/dev/null
run 'node_cert_cmd anytls-in --clear' >/dev/null
[[ -s "${NC}/anytls-in.pem" ]] && pass "--clear：节点还在用，证书保留" || fail "--clear：节点还在用，证书保留" "有" "无"
[[ ! -e "${CFG}/node-certs.json" && ! -e "${SYSD}/singr-node-cert-update.timer" ]] && pass "--clear：登记和 timer 去掉" || fail "--clear：登记和 timer 去掉" "无" "$(ls "${SYSD}")"
out="$(run 'node_cert_update_all')"; rc=$?
[[ ${rc} == 0 && "${out}" == *"无事可做"* ]] && pass "没有登记：定时任务无事可做" || fail "没有登记：定时任务无事可做" "rc=0 无事可做" "rc=${rc} ${out}"

echo "== update_shell_install（docker 的 update 也调它）=="
SH="${TMP}/sh"
mkdir -p "${SH}/bin"
# 目标故意不叫 SingR：macOS 默认文件系统不分大小写，SingR 和软链 singr 会是同一个文件。
T="${SH}/bin/SingR-main"
echo 'old' > "${T}"
{
    echo 'red=""; green=""; yellow=""; plain=""'
    echo "SCRIPT_URL=https://raw.example/SingR-docker.sh; SHELL_TARGET='${T}'"
    extract_block "${REPO_DIR}/SingR-docker.sh" 通用函数
    echo "curl() { local o=''; while [[ \$# -gt 0 ]]; do [[ \"\$1\" == -o ]] && { o=\"\$2\"; shift; }; shift; done; [[ -f '${SH}/remote' ]] || return 22; cp '${SH}/remote' \"\${o}\"; }"
} > "${TMP}/sh.sh"
runsh() { bash -c "source '${TMP}/sh.sh'; $1" </dev/null 2>&1; }
runsh 'update_shell_install' >/dev/null; rc=$?
[[ ${rc} -ne 0 ]] && pass "下载失败：返回非零" || fail "下载失败：返回非零" "非 0" "${rc}"
check "下载失败：脚本不动" "old" "$(cat "${T}")"
printf 'show_menu() {\n' > "${SH}/remote"
runsh 'update_shell_install' >/dev/null
check "语法坏的：脚本不动" "old" "$(cat "${T}")"
printf '#!/usr/bin/env bash\nshow_menu() { :; }\n' > "${SH}/remote"
out="$(runsh 'update_shell_install; echo after')"
contains "成功后不退出（调用方接着走）" "after" "${out}"
check "成功：脚本被替换" "$(cat "${SH}/remote")" "$(cat "${T}")"
[[ -L "${SH}/bin/singr" ]] && pass "成功：小写软链" || fail "成功：小写软链" "软链" "无"
out="$(runsh 'update_shell; echo after')"
lacks "update_shell 本身仍然成功即退出" "after" "${out}"

echo "== docker 的 update 拉完镜像后更新管理脚本 =="
fn="$(awk '/^update_singr\(\) \{/{f=1} f{print} f&&/^\}/{exit}' "${REPO_DIR}/SingR-docker.sh")"
{
    echo 'red=""; green=""; yellow=""; plain=""; IMAGE=ghcr.io/x/singr:latest'
    echo "${fn}"
    echo "require_docker() { :; }; image_repo() { echo ghcr.io/x/singr; }; save_image() { :; }"
    echo "docker() { echo \"docker \$*\" >> '${STATE}/dk'; [[ ! -f '${STATE}/pull-fail' ]]; }"
    echo "update_shell_install() { echo shell >> '${STATE}/dk'; }"
    echo "node_none_configured() { [[ -f '${STATE}/none' ]]; }"
    echo "container_recreate() { echo recreate >> '${STATE}/dk'; }; verify_running() { :; }; show_version() { :; }"
} > "${TMP}/dk.sh"
rundk() { bash -c "source '${TMP}/dk.sh'; $1" </dev/null 2>&1; }
rm -f "${STATE}"/*
rundk 'update_singr 0' >/dev/null
check "拉镜像 → 更新脚本 → 重建容器" "docker pull ghcr.io/x/singr:latest shell recreate" "$(tr '\n' ' ' <"${STATE}/dk" | sed 's/ $//')"
rm -f "${STATE}"/*; touch "${STATE}/none"
rundk 'update_singr 0' >/dev/null
contains "没有节点时也更新脚本" "shell" "$(cat "${STATE}/dk")"
rm -f "${STATE}"/*; touch "${STATE}/pull-fail"
rundk 'update_singr 0' >/dev/null
lacks "拉镜像失败：不动脚本" "shell" "$(cat "${STATE}/dk")"

echo
if [[ "${FAILED}" == 0 ]]; then
    echo "PASS: 节点独立证书 测试通过"
else
    echo "FAIL: 有用例未通过"
fi
exit "${FAILED}"
