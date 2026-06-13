#!/usr/bin/env bash

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

SERVICE_NAME="singr"
APP_NAME="SingR"
INSTALL_DIR="/usr/local/SingR"
BIN_PATH="${INSTALL_DIR}/singr"
CONFIG_DIR="/etc/singr"
PANEL_CONFIG="${CONFIG_DIR}/panel.json"
SERVER_CONFIG="${CONFIG_DIR}/server.json"
LOG_FILE="/var/log/singr.log"
RELEASE_REPO="${SINGR_RELEASE_REPO:-makt28/SingR}"
RELEASE_BRANCH="${SINGR_RELEASE_BRANCH:-main}"
INSTALL_URL="https://raw.githubusercontent.com/${RELEASE_REPO}/${RELEASE_BRANCH}/install.sh"
SCRIPT_URL="https://raw.githubusercontent.com/${RELEASE_REPO}/${RELEASE_BRANCH}/SingR.sh"

# Hysteria2 端口跳跃（iptables NAT REDIRECT）相关
PORTHOP_RULES="${CONFIG_DIR}/porthop.rules"
PORTHOP_COMMENT="singr-porthop"
PORTHOP_SERVICE="singr-porthop"
PORTHOP_SERVICE_FILE="/etc/systemd/system/${PORTHOP_SERVICE}.service"
SELF_CMD="/usr/bin/SingR"

[[ ${EUID} -ne 0 ]] && echo -e "${red}错误：${plain} 必须使用 root 用户运行此脚本！\n" && exit 1

confirm() {
    local prompt="$1"
    local default="${2:-}"
    local answer
    if [[ -n "${default}" ]]; then
        read -r -p "${prompt} [默认${default}]: " answer
        [[ -z "${answer}" ]] && answer="${default}"
    else
        read -r -p "${prompt} [y/n]: " answer
    fi
    [[ "${answer}" == "y" || "${answer}" == "Y" ]]
}

before_show_menu() {
    echo
    read -r -p "按回车返回主菜单: " _
    show_menu
}

check_status() {
    if [[ ! -f "/etc/systemd/system/${SERVICE_NAME}.service" ]]; then
        return 2
    fi
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        return 0
    fi
    return 1
}

check_enabled() {
    systemctl is-enabled --quiet "${SERVICE_NAME}" >/dev/null 2>&1
}

check_install() {
    check_status
    if [[ $? == 2 ]]; then
        echo -e "${red}请先安装 ${APP_NAME}${plain}"
        [[ $# == 0 ]] && before_show_menu
        return 1
    fi
    return 0
}

check_uninstall() {
    check_status
    if [[ $? != 2 ]]; then
        echo -e "${red}${APP_NAME} 已安装，请不要重复安装${plain}"
        [[ $# == 0 ]] && before_show_menu
        return 1
    fi
    return 0
}

install_singr() {
    bash <(curl -fsSL "${INSTALL_URL}") "$1"
    if [[ $? == 0 ]]; then
        start 0
    fi
}

update_singr() {
    local version="${2:-}"
    if [[ $# == 0 ]]; then
        echo
        read -r -p "输入指定版本，留空为最新版: " version
    fi
    bash <(curl -fsSL "${INSTALL_URL}") "${version}"
    if [[ $? == 0 ]]; then
        restart 0
        show_version 0
        echo -e "${green}更新完成${plain}"
    fi
    [[ $# == 0 ]] && before_show_menu
}

uninstall_singr() {
    confirm "确定要卸载 ${APP_NAME} 吗？配置文件也会被删除" "n" || {
        [[ $# == 0 ]] && show_menu
        return 0
    }

    systemctl stop "${SERVICE_NAME}" >/dev/null 2>&1 || true
    systemctl disable "${SERVICE_NAME}" >/dev/null 2>&1 || true
    rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
    systemctl daemon-reload
    systemctl reset-failed >/dev/null 2>&1 || true
    rm -rf "${INSTALL_DIR}" "${CONFIG_DIR}"

    echo -e "${green}卸载成功${plain}"
    echo "如需删除管理脚本，请运行：rm -f /usr/bin/SingR /usr/bin/singr"
    [[ $# == 0 ]] && before_show_menu
}

start() {
    check_status
    if [[ $? == 0 ]]; then
        echo -e "${green}${APP_NAME} 已运行，无需再次启动${plain}"
    else
        systemctl start "${SERVICE_NAME}"
        sleep 2
        if systemctl is-active --quiet "${SERVICE_NAME}"; then
            echo -e "${green}${APP_NAME} 启动成功${plain}"
        else
            echo -e "${red}${APP_NAME} 可能启动失败，请使用 SingR log 查看日志${plain}"
        fi
    fi
    [[ $# == 0 ]] && before_show_menu
}

stop() {
    systemctl stop "${SERVICE_NAME}"
    sleep 2
    if ! systemctl is-active --quiet "${SERVICE_NAME}"; then
        echo -e "${green}${APP_NAME} 停止成功${plain}"
    else
        echo -e "${red}${APP_NAME} 停止失败，请稍后查看状态${plain}"
    fi
    [[ $# == 0 ]] && before_show_menu
}

restart() {
    systemctl restart "${SERVICE_NAME}"
    sleep 2
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        echo -e "${green}${APP_NAME} 重启成功${plain}"
    else
        echo -e "${red}${APP_NAME} 可能启动失败，请使用 SingR log 查看日志${plain}"
    fi
    [[ $# == 0 ]] && before_show_menu
}

status() {
    systemctl status "${SERVICE_NAME}" --no-pager -l
    [[ $# == 0 ]] && before_show_menu
}

enable() {
    systemctl enable "${SERVICE_NAME}"
    echo -e "${green}${APP_NAME} 已设置开机自启${plain}"
    [[ $# == 0 ]] && before_show_menu
}

disable() {
    systemctl disable "${SERVICE_NAME}"
    echo -e "${green}${APP_NAME} 已取消开机自启${plain}"
    [[ $# == 0 ]] && before_show_menu
}

show_log() {
    if [[ -f "${LOG_FILE}" ]]; then
        echo "===== systemd journal: ${SERVICE_NAME}.service ====="
        journalctl -u "${SERVICE_NAME}.service" -n 80 --no-pager
        echo "===== following ${LOG_FILE} ====="
        tail -n 200 -F "${LOG_FILE}"
    else
        echo "未找到 ${LOG_FILE}，改为查看 systemd journal。"
        journalctl -u "${SERVICE_NAME}.service" -e --no-pager -f
    fi
    [[ $# == 0 ]] && before_show_menu
}

config() {
    local editor="${EDITOR:-vi}"
    echo "1. 修改面板配置：${PANEL_CONFIG}"
    echo "2. 修改 sing-box 配置：${SERVER_CONFIG}"
    read -r -p "请选择 [1-2，默认 1]: " num
    case "${num}" in
        2)
            "${editor}" "${SERVER_CONFIG}"
            ;;
        *)
            "${editor}" "${PANEL_CONFIG}"
            ;;
    esac
    confirm "是否重启 ${APP_NAME}" "y" && restart 0
    [[ $# == 0 ]] && before_show_menu
}

update_shell() {
    curl -fsSL "${SCRIPT_URL}" -o /usr/bin/SingR
    chmod +x /usr/bin/SingR
    ln -sf /usr/bin/SingR /usr/bin/singr
    echo -e "${green}管理脚本升级成功，请重新运行 SingR${plain}"
}

show_version() {
    if [[ -x "${BIN_PATH}" ]]; then
        "${BIN_PATH}" version
    else
        echo -e "${red}未找到二进制：${BIN_PATH}${plain}"
    fi
    [[ $# == 0 ]] && before_show_menu
}

show_enable_status() {
    if check_enabled; then
        echo -e "是否开机自启：${green}是${plain}"
    else
        echo -e "是否开机自启：${red}否${plain}"
    fi
}

show_status() {
    check_status
    case $? in
        0)
            echo -e "${APP_NAME} 状态：${green}已运行${plain}"
            show_enable_status
            ;;
        1)
            echo -e "${APP_NAME} 状态：${yellow}未运行${plain}"
            show_enable_status
            ;;
        2)
            echo -e "${APP_NAME} 状态：${red}未安装${plain}"
            ;;
    esac
}

# ---------------- Hysteria2 端口跳跃管理 ----------------
# 在 OS 防火墙层把一段 UDP 端口区间 REDIRECT 到真实 Hysteria2 端口（v4+v6）。
# 规则持久化为 ${PORTHOP_RULES}，由 singr-porthop.service 开机重放，不依赖
# 发行版的 iptables-persistent / iptables-services。每条规则带 comment
# "${PORTHOP_COMMENT}" 以便精确列出/删除，不影响用户已有的其它防火墙规则。

porthop_check_tools() {
    local mode="${1:-}"
    if ! command -v iptables >/dev/null 2>&1 || ! command -v ip6tables >/dev/null 2>&1; then
        [[ "${mode}" == "silent" ]] || echo -e "${red}未检测到 iptables/ip6tables，请先安装（apt install iptables 或 yum install iptables）${plain}"
        return 1
    fi
}

porthop_valid_port() {
    [[ "$1" =~ ^[0-9]+$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

porthop_apply_pair() {
    local ipt="$1" start="$2" end="$3" target="$4"
    if ! "${ipt}" -t nat -C PREROUTING -p udp --dport "${start}:${end}" \
        -m comment --comment "${PORTHOP_COMMENT}" -j REDIRECT --to-ports "${target}" 2>/dev/null; then
        "${ipt}" -t nat -A PREROUTING -p udp --dport "${start}:${end}" \
            -m comment --comment "${PORTHOP_COMMENT}" -j REDIRECT --to-ports "${target}" 2>/dev/null || true
    fi
}

porthop_delete_pair() {
    local ipt="$1" start="$2" end="$3" target="$4"
    while "${ipt}" -t nat -C PREROUTING -p udp --dport "${start}:${end}" \
        -m comment --comment "${PORTHOP_COMMENT}" -j REDIRECT --to-ports "${target}" 2>/dev/null; do
        "${ipt}" -t nat -D PREROUTING -p udp --dport "${start}:${end}" \
            -m comment --comment "${PORTHOP_COMMENT}" -j REDIRECT --to-ports "${target}" 2>/dev/null || break
    done
}

# 删除指定表里所有带 SingR comment 的 PREROUTING 规则（comment 无空格，-S 不会加引号）
porthop_flush_table() {
    local ipt="$1" line del
    "${ipt}" -t nat -S PREROUTING 2>/dev/null | grep -- "--comment ${PORTHOP_COMMENT}" | while read -r line; do
        del="-D ${line#-A }"
        # shellcheck disable=SC2086
        "${ipt}" -t nat ${del} 2>/dev/null || true
    done
}

porthop_flush_all() {
    porthop_flush_table iptables
    porthop_flush_table ip6tables
}

# systemd ExecStart：清掉旧的 SingR 规则后，按规则文件重新下发（开机重放）
porthop_reapply_all() {
    porthop_check_tools silent || return 0
    porthop_flush_all
    [[ -f "${PORTHOP_RULES}" ]] || return 0
    local start end target
    while IFS=: read -r start end target; do
        [[ -z "${start}" || "${start}" == \#* ]] && continue
        porthop_apply_pair iptables "${start}" "${end}" "${target}"
        porthop_apply_pair ip6tables "${start}" "${end}" "${target}"
    done < "${PORTHOP_RULES}"
}

porthop_ensure_service() {
    if [[ ! -f "${PORTHOP_SERVICE_FILE}" ]]; then
        cat > "${PORTHOP_SERVICE_FILE}" <<EOF
[Unit]
Description=SingR Hysteria2 port hopping (iptables NAT REDIRECT)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=${SELF_CMD} porthop-apply
ExecStop=${SELF_CMD} porthop-flush

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
    fi
    systemctl enable "${PORTHOP_SERVICE}" >/dev/null 2>&1 || true
}

porthop_list() {
    echo -e "${green}已配置的端口跳跃规则（${PORTHOP_RULES}）：${plain}"
    local i=0 start end target
    if [[ -s "${PORTHOP_RULES}" ]]; then
        while IFS=: read -r start end target; do
            [[ -z "${start}" || "${start}" == \#* ]] && continue
            i=$((i + 1))
            echo "  ${i}) ${start}-${end}  ->  ${target}/udp (v4+v6)"
        done < "${PORTHOP_RULES}"
    fi
    [[ ${i} -eq 0 ]] && echo "  (无)"
    if porthop_check_tools silent; then
        echo
        echo -e "${green}当前生效的 iptables(v4) NAT 规则：${plain}"
        iptables -t nat -S PREROUTING 2>/dev/null | grep -- "--comment ${PORTHOP_COMMENT}" | sed 's/^/  /' || echo "  (无)"
        echo -e "${green}当前生效的 ip6tables(v6) NAT 规则：${plain}"
        ip6tables -t nat -S PREROUTING 2>/dev/null | grep -- "--comment ${PORTHOP_COMMENT}" | sed 's/^/  /' || echo "  (无)"
    fi
}

porthop_add() {
    porthop_check_tools || return
    local start end target line
    read -r -p "起始端口 (start): " start
    read -r -p "结束端口 (end): " end
    read -r -p "目标端口 (真实 Hysteria2 UDP 端口): " target
    if ! porthop_valid_port "${start}" || ! porthop_valid_port "${end}" || ! porthop_valid_port "${target}"; then
        echo -e "${red}端口必须是 1-65535 的整数${plain}"
        return
    fi
    if [ "${start}" -gt "${end}" ]; then
        echo -e "${red}起始端口不能大于结束端口${plain}"
        return
    fi
    if [ "${target}" -ge "${start}" ] && [ "${target}" -le "${end}" ]; then
        echo -e "${red}目标端口不能落在跳跃区间 ${start}-${end} 内（会造成环路）${plain}"
        return
    fi
    mkdir -p "${CONFIG_DIR}"
    line="${start}:${end}:${target}"
    if [[ -f "${PORTHOP_RULES}" ]] && grep -qxF "${line}" "${PORTHOP_RULES}"; then
        echo -e "${yellow}该规则已存在${plain}"
        return
    fi
    echo "${line}" >> "${PORTHOP_RULES}"
    porthop_apply_pair iptables "${start}" "${end}" "${target}"
    porthop_apply_pair ip6tables "${start}" "${end}" "${target}"
    porthop_ensure_service
    echo -e "${green}已添加并生效：${start}-${end} -> ${target}/udp (v4+v6)，已设置开机自动重放${plain}"
}

porthop_del() {
    porthop_check_tools || return
    if [[ ! -s "${PORTHOP_RULES}" ]]; then
        echo -e "${yellow}当前没有任何端口跳跃规则${plain}"
        return
    fi
    local -a rules=()
    local start end target sel chosen i
    while IFS=: read -r start end target; do
        [[ -z "${start}" || "${start}" == \#* ]] && continue
        rules+=("${start}:${end}:${target}")
    done < "${PORTHOP_RULES}"
    if [[ ${#rules[@]} -eq 0 ]]; then
        echo -e "${yellow}当前没有任何端口跳跃规则${plain}"
        return
    fi
    for i in "${!rules[@]}"; do
        IFS=: read -r start end target <<<"${rules[$i]}"
        echo "  $((i + 1))) ${start}-${end} -> ${target}/udp"
    done
    read -r -p "输入要删除的编号 (回车取消): " sel
    [[ -z "${sel}" ]] && return
    if ! [[ "${sel}" =~ ^[0-9]+$ ]] || [ "${sel}" -lt 1 ] || [ "${sel}" -gt "${#rules[@]}" ]; then
        echo -e "${red}编号无效${plain}"
        return
    fi
    chosen="${rules[$((sel - 1))]}"
    IFS=: read -r start end target <<<"${chosen}"
    porthop_delete_pair iptables "${start}" "${end}" "${target}"
    porthop_delete_pair ip6tables "${start}" "${end}" "${target}"
    grep -vxF "${chosen}" "${PORTHOP_RULES}" > "${PORTHOP_RULES}.tmp" 2>/dev/null || true
    mv -f "${PORTHOP_RULES}.tmp" "${PORTHOP_RULES}"
    if [[ ! -s "${PORTHOP_RULES}" ]]; then
        systemctl disable "${PORTHOP_SERVICE}" >/dev/null 2>&1 || true
    fi
    echo -e "${green}已删除：${start}-${end} -> ${target}/udp (v4+v6)${plain}"
}

porthop_menu() {
    while true; do
        echo -e "
  ${green}Hysteria2 端口跳跃管理${plain}
  1. 查看跳跃规则
  2. 添加跳跃规则
  3. 删除跳跃规则
  0. 返回主菜单
"
        read -r -p "请输入选择 [0-3]: " hp
        case "${hp}" in
            1) porthop_list ;;
            2) porthop_add ;;
            3) porthop_del ;;
            0) return ;;
            *) echo -e "${red}请输入正确的数字 [0-3]${plain}" ;;
        esac
    done
}

show_usage() {
    echo "${APP_NAME} 管理脚本使用方法："
    echo "------------------------------------------"
    echo "SingR 或 singr              显示管理菜单"
    echo "SingR start / singr start   启动 SingR"
    echo "SingR stop / singr stop     停止 SingR"
    echo "SingR restart               重启 SingR"
    echo "SingR status                查看 SingR 状态"
    echo "SingR enable                设置 SingR 开机自启"
    echo "SingR disable               取消 SingR 开机自启"
    echo "SingR log                   查看 SingR 日志"
    echo "SingR update                更新到最新版本"
    echo "SingR update x.x.x          更新到指定版本"
    echo "SingR config                修改配置"
    echo "SingR install               安装 SingR"
    echo "SingR uninstall             卸载 SingR"
    echo "SingR version               查看 SingR 版本"
    echo "SingR update_shell          更新管理脚本"
    echo "SingR porthop               Hysteria2 端口跳跃管理"
    echo "------------------------------------------"
}

show_menu() {
    echo -e "
  ${green}${APP_NAME} 后端管理脚本${plain}
  0. 修改配置
  1. 安装 SingR
  2. 更新 SingR
  3. 卸载 SingR
  4. 启动 SingR
  5. 停止 SingR
  6. 重启 SingR
  7. 查看 SingR 状态
  8. 查看 SingR 日志
  9. 设置 SingR 开机自启
 10. 取消 SingR 开机自启
 11. 查看 SingR 版本
 12. 更新管理脚本
 13. Hysteria2 端口跳跃管理
"
    show_status
    echo
    read -r -p "请输入选择 [0-13]: " num
    case "${num}" in
        0) check_install && config ;;
        1) check_uninstall && install_singr ;;
        2) check_install && update_singr ;;
        3) check_install && uninstall_singr ;;
        4) check_install && start ;;
        5) check_install && stop ;;
        6) check_install && restart ;;
        7) check_install && status ;;
        8) check_install && show_log ;;
        9) check_install && enable ;;
        10) check_install && disable ;;
        11) check_install && show_version ;;
        12) update_shell ;;
        13) porthop_menu && before_show_menu ;;
        *) echo -e "${red}请输入正确的数字 [0-13]${plain}" && before_show_menu ;;
    esac
}

if [[ $# -gt 0 ]]; then
    case "$1" in
        start) check_install 0 && start 0 ;;
        stop) check_install 0 && stop 0 ;;
        restart) check_install 0 && restart 0 ;;
        status) check_install 0 && status 0 ;;
        enable) check_install 0 && enable 0 ;;
        disable) check_install 0 && disable 0 ;;
        log) check_install 0 && show_log 0 ;;
        update) check_install 0 && update_singr 0 "${2:-}" ;;
        config) check_install 0 && config 0 ;;
        install) check_uninstall 0 && install_singr "${2:-}" ;;
        uninstall) check_install 0 && uninstall_singr 0 ;;
        version) check_install 0 && show_version 0 ;;
        update_shell) update_shell ;;
        porthop) porthop_menu ;;
        porthop-apply) porthop_reapply_all ;;
        porthop-flush) porthop_flush_all ;;
        *) show_usage ;;
    esac
else
    show_menu
fi
