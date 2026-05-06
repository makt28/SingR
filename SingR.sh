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
"
    show_status
    echo
    read -r -p "请输入选择 [0-12]: " num
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
        *) echo -e "${red}请输入正确的数字 [0-12]${plain}" && before_show_menu ;;
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
        *) show_usage ;;
    esac
else
    show_menu
fi
