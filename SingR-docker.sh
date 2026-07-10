#!/usr/bin/env bash
#
# SingR management script — DOCKER backend.
#
# Mirrors SingR.sh (the bare-metal/systemd script) verb-for-verb, but drives a
# Docker container instead of a systemd unit. A host runs either the bare-metal
# install OR the docker install, never both, so this installs as the same
# `singr` / `SingR` command. Config is isolated to /etc/singr-docker so it can
# never collide with the bare-metal /etc/singr paths.
#
# Backend detection: this script is only installed by install-docker.sh, which
# also writes ${CONFIG_DIR}/docker.conf. Its presence == docker mode.

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

APP_NAME="SingR"
CONFIG_DIR="${SINGR_CONFIG_DIR:-/etc/singr-docker}"
DOCKER_CONF="${CONFIG_DIR}/docker.conf"
SELF_CMD="/usr/bin/SingR"

RELEASE_REPO="${SINGR_RELEASE_REPO:-makt28/SingR}"
RELEASE_BRANCH="${SINGR_RELEASE_BRANCH:-main}"
INSTALL_URL="https://raw.githubusercontent.com/${RELEASE_REPO}/${RELEASE_BRANCH}/install-docker.sh"
SCRIPT_URL="https://raw.githubusercontent.com/${RELEASE_REPO}/${RELEASE_BRANCH}/SingR-docker.sh"

# Hysteria2 端口跳跃（宿主机 iptables NAT REDIRECT）——与裸机版一致，host 网络下
# 容器与宿主机共享网络栈，所以这段完全复用，不涉及 docker。
PORTHOP_RULES="${CONFIG_DIR}/porthop.rules"
PORTHOP_COMMENT="singr-porthop"
PORTHOP_SERVICE="singr-porthop"
PORTHOP_SERVICE_FILE="/etc/systemd/system/${PORTHOP_SERVICE}.service"

[[ ${EUID} -ne 0 ]] && echo -e "${red}错误：${plain} 必须使用 root 用户运行此脚本！\n" && exit 1

# ---- 容器运行参数默认值（docker.conf 覆盖）--------------------------------
IMAGE="ghcr.io/makt28/singr:latest"
CONTAINER="singr"
RESTART="always"
LOG_MAX_SIZE="10m"
LOG_MAX_FILE="5"
RUN_FLAGS=()

load_conf() {
    if [[ -f "${DOCKER_CONF}" ]]; then
        # shellcheck source=/dev/null
        source "${DOCKER_CONF}"
    fi
}
load_conf

require_docker() {
    command -v docker >/dev/null 2>&1 || {
        echo -e "${red}未检测到 docker，请先运行 install-docker.sh 安装${plain}"
        exit 1
    }
}

confirm() {
    local prompt="$1" default="${2:-}" answer
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

container_exists()  { docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "${CONTAINER}"; }
container_running() { docker ps    --format '{{.Names}}' 2>/dev/null | grep -qx "${CONTAINER}"; }

# 删旧建新（update / 首次 bootstrap 共用）。RUN_FLAGS 是首启引导参数；因为
# entrypoint 只在 panel.json 不存在时生成，重建不会覆盖已有配置。
container_recreate() {
    require_docker
    docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
    docker run -d \
        --name "${CONTAINER}" \
        --network host \
        --restart "${RESTART}" \
        --log-opt max-size="${LOG_MAX_SIZE}" \
        --log-opt max-file="${LOG_MAX_FILE}" \
        -v "${CONFIG_DIR}:${CONFIG_DIR}" \
        "${IMAGE}" "${RUN_FLAGS[@]}"
}

# docker run -d 只要容器被创建就返回 0，即使入口随即退出（例如缺证书）。
# 停一下再确认容器仍在运行，供 update 等破坏性路径判断真正成功与否。
verify_running() {
    sleep 2
    container_running
}

# 从镜像引用里取出 repo（去掉 tag 和 @digest）。正确区分 registry 端口冒号与
# tag 冒号：tag 冒号只会出现在最后一个 / 之后的段里，所以只有当最后一段含冒号
# 时才去掉 tag。否则 ${IMAGE%:*} 会把 localhost:5000/img 误截成 localhost。
image_repo() {
    local ref="${1%@*}"        # 去掉 @sha256:... digest
    local last="${ref##*/}"    # 最后一段（name[:tag]）
    if [[ "${last}" == *:* ]]; then
        ref="${ref%:*}"        # 最后一段带 tag → 去掉（此时末尾冒号即 tag 冒号）
    fi
    printf '%s' "${ref}"
}

check_status() {
    require_docker
    container_exists || return 2
    container_running || return 1
    return 0
}

start() {
    require_docker
    local rc=0
    if container_running; then
        echo -e "${green}${APP_NAME} 已运行，无需再次启动${plain}"
    elif container_exists; then
        if docker start "${CONTAINER}" >/dev/null && verify_running; then
            echo -e "${green}${APP_NAME} 启动成功${plain}"
        else
            echo -e "${red}${APP_NAME} 启动失败（容器未在运行），请用 singr log 查看日志${plain}"; rc=1
        fi
    else
        if container_recreate && verify_running; then
            echo -e "${green}${APP_NAME} 已创建并启动${plain}"
        else
            echo -e "${red}${APP_NAME} 创建后未能运行（可能缺证书），请用 singr log 查看日志${plain}"; rc=1
        fi
    fi
    if [[ $# == 0 ]]; then before_show_menu; fi
    return ${rc}
}

stop() {
    require_docker
    local rc=0
    if docker stop "${CONTAINER}" >/dev/null 2>&1; then
        echo -e "${green}${APP_NAME} 停止成功${plain}"
    else
        echo -e "${red}${APP_NAME} 停止失败${plain}"; rc=1
    fi
    if [[ $# == 0 ]]; then before_show_menu; fi
    return ${rc}
}

restart() {
    require_docker
    local rc=0
    if container_exists; then
        if docker restart "${CONTAINER}" >/dev/null && verify_running; then
            echo -e "${green}${APP_NAME} 重启成功${plain}"
        else
            echo -e "${red}${APP_NAME} 重启失败（容器未在运行），请用 singr log 查看日志${plain}"; rc=1
        fi
    else
        if container_recreate && verify_running; then
            echo -e "${green}${APP_NAME} 已创建并启动${plain}"
        else
            echo -e "${red}${APP_NAME} 创建后未能运行（可能缺证书），请用 singr log 查看日志${plain}"; rc=1
        fi
    fi
    if [[ $# == 0 ]]; then before_show_menu; fi
    return ${rc}
}

status() {
    require_docker
    docker ps -a --filter "name=^/${CONTAINER}$" \
        --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}' 2>/dev/null || true
    if [[ $# == 0 ]]; then before_show_menu; fi
}

enable() {
    require_docker
    docker update --restart=always "${CONTAINER}" >/dev/null 2>&1 || true
    RESTART="always"; save_restart
    echo -e "${green}${APP_NAME} 已设置开机自启（--restart=always）${plain}"
    if [[ $# == 0 ]]; then before_show_menu; fi
}

disable() {
    require_docker
    docker update --restart=no "${CONTAINER}" >/dev/null 2>&1 || true
    RESTART="no"; save_restart
    echo -e "${green}${APP_NAME} 已取消开机自启（--restart=no）${plain}"
    if [[ $# == 0 ]]; then before_show_menu; fi
}

save_restart() {
    [[ -f "${DOCKER_CONF}" ]] || return 0
    if grep -q '^RESTART=' "${DOCKER_CONF}"; then
        sed -i "s/^RESTART=.*/RESTART=\"${RESTART}\"/" "${DOCKER_CONF}"
    else
        echo "RESTART=\"${RESTART}\"" >> "${DOCKER_CONF}"
    fi
}

# 用 # 作分隔符，IMAGE 里的 / 不会破坏 sed。
save_image() {
    [[ -f "${DOCKER_CONF}" ]] || return 0
    if grep -q '^IMAGE=' "${DOCKER_CONF}"; then
        sed -i "s#^IMAGE=.*#IMAGE=\"${IMAGE}\"#" "${DOCKER_CONF}"
    else
        echo "IMAGE=\"${IMAGE}\"" >> "${DOCKER_CONF}"
    fi
}

show_log() {
    require_docker
    docker logs -f --tail 200 "${CONTAINER}"
    if [[ $# == 0 ]]; then before_show_menu; fi
}

update_singr() {
    require_docker
    local version="${2:-}"
    if [[ $# == 0 ]]; then
        read -r -p "输入指定版本（如 v0.5.0 或 0.5.0），留空为最新版: " version
    fi

    # 无版本 → :latest 频道；指定版本 → 换 tag。docker tag 同时提供 v 前缀与纯
    # semver 两种，用户给哪种都行，不做前缀改写。
    local repo image
    repo="$(image_repo "${IMAGE}")"
    if [[ -n "${version}" ]]; then
        image="${repo}:${version}"
    else
        image="${repo}:latest"
    fi

    echo -e "${green}拉取镜像：${image}${plain}"
    if ! docker pull "${image}"; then
        echo -e "${red}镜像拉取失败，未做变更${plain}"
        if [[ $# == 0 ]]; then before_show_menu; fi
        return 1
    fi

    IMAGE="${image}"
    save_image
    local rc=0
    if container_recreate && verify_running; then
        show_version 0
        echo -e "${green}更新完成${plain}"
    else
        echo -e "${red}容器未能正常启动（可能证书缺失或新镜像有问题），请用 singr log 查看日志。旧容器已被替换。${plain}"; rc=1
    fi
    if [[ $# == 0 ]]; then before_show_menu; fi
    return ${rc}
}

uninstall_singr() {
    confirm "确定要卸载 ${APP_NAME}（docker）吗？配置目录 ${CONFIG_DIR} 也会被删除" "n" || {
        [[ $# == 0 ]] && show_menu
        return 0
    }
    require_docker
    docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
    docker rmi "${IMAGE}" >/dev/null 2>&1 || true

    # 清理 Hysteria2 端口跳跃：flush 活动 NAT 规则 + 停用并删除 singr-porthop.service，
    # 否则 UDP 重定向会残留，且服务会引用即将被删除的 /usr/bin/SingR。
    porthop_flush_all
    if [[ -f "${PORTHOP_SERVICE_FILE}" ]]; then
        systemctl disable --now "${PORTHOP_SERVICE}" >/dev/null 2>&1 || true
        rm -f "${PORTHOP_SERVICE_FILE}"
        systemctl daemon-reload 2>/dev/null || true
    fi

    rm -rf "${CONFIG_DIR}"
    echo -e "${green}卸载成功${plain}"
    echo "如需删除管理脚本，请运行：rm -f /usr/bin/SingR /usr/bin/singr"
    if [[ $# == 0 ]]; then before_show_menu; fi
}

config() {
    local editor="${EDITOR:-vi}"
    echo "1. 修改面板配置：${CONFIG_DIR}/panel.json"
    echo "2. 修改 sing-box 配置：${CONFIG_DIR}/server.json"
    read -r -p "请选择 [1-2，默认 1]: " num
    case "${num}" in
        2) "${editor}" "${CONFIG_DIR}/server.json" ;;
        *) "${editor}" "${CONFIG_DIR}/panel.json" ;;
    esac
    confirm "是否重启 ${APP_NAME}" "y" && restart 0
    if [[ $# == 0 ]]; then before_show_menu; fi
}

show_version() {
    require_docker
    if container_running; then
        docker exec "${CONTAINER}" singr version 2>/dev/null || echo -e "${red}无法执行 version${plain}"
    else
        docker run --rm "${IMAGE}" version 2>/dev/null || echo -e "${red}无法执行 version${plain}"
    fi
    if [[ $# == 0 ]]; then before_show_menu; fi
}

update_shell() {
    curl -fsSL "${SCRIPT_URL}" -o /usr/bin/SingR
    chmod +x /usr/bin/SingR
    ln -sf /usr/bin/SingR /usr/bin/singr
    echo -e "${green}管理脚本升级成功，请重新运行 SingR${plain}"
    exit 0
}

show_status() {
    check_status
    case $? in
        0) echo -e "${APP_NAME} 状态：${green}已运行${plain}" ;;
        1) echo -e "${APP_NAME} 状态：${yellow}未运行（容器已创建）${plain}" ;;
        2) echo -e "${APP_NAME} 状态：${red}未安装（无容器）${plain}" ;;
    esac
}

# ---------------- Hysteria2 端口跳跃管理（宿主机 iptables，与裸机版一致）------
porthop_check_tools() {
    local mode="${1:-}"
    if ! command -v iptables >/dev/null 2>&1 || ! command -v ip6tables >/dev/null 2>&1; then
        [[ "${mode}" == "silent" ]] || echo -e "${red}未检测到 iptables/ip6tables，请先安装${plain}"
        return 1
    fi
}
porthop_valid_port() { [[ "$1" =~ ^[0-9]+$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }
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
porthop_flush_table() {
    local ipt="$1" line del
    "${ipt}" -t nat -S PREROUTING 2>/dev/null | grep -- "--comment ${PORTHOP_COMMENT}" | while read -r line; do
        del="-D ${line#-A }"
        # shellcheck disable=SC2086
        "${ipt}" -t nat ${del} 2>/dev/null || true
    done
}
porthop_flush_all() { porthop_flush_table iptables; porthop_flush_table ip6tables; }
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
        systemctl daemon-reload 2>/dev/null || true
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
        echo -e "${red}端口必须是 1-65535 的整数${plain}"; return
    fi
    if [ "${start}" -gt "${end}" ]; then echo -e "${red}起始端口不能大于结束端口${plain}"; return; fi
    if [ "${target}" -ge "${start}" ] && [ "${target}" -le "${end}" ]; then
        echo -e "${red}目标端口不能落在跳跃区间 ${start}-${end} 内（会造成环路）${plain}"; return
    fi
    mkdir -p "${CONFIG_DIR}"
    line="${start}:${end}:${target}"
    if [[ -f "${PORTHOP_RULES}" ]] && grep -qxF "${line}" "${PORTHOP_RULES}"; then
        echo -e "${yellow}该规则已存在${plain}"; return
    fi
    echo "${line}" >> "${PORTHOP_RULES}"
    porthop_apply_pair iptables "${start}" "${end}" "${target}"
    porthop_apply_pair ip6tables "${start}" "${end}" "${target}"
    porthop_ensure_service
    echo -e "${green}已添加并生效：${start}-${end} -> ${target}/udp (v4+v6)，已设置开机自动重放${plain}"
}
porthop_del() {
    porthop_check_tools || return
    if [[ ! -s "${PORTHOP_RULES}" ]]; then echo -e "${yellow}当前没有任何端口跳跃规则${plain}"; return; fi
    local -a rules=()
    local start end target sel chosen i
    while IFS=: read -r start end target; do
        [[ -z "${start}" || "${start}" == \#* ]] && continue
        rules+=("${start}:${end}:${target}")
    done < "${PORTHOP_RULES}"
    if [[ ${#rules[@]} -eq 0 ]]; then echo -e "${yellow}当前没有任何端口跳跃规则${plain}"; return; fi
    for i in "${!rules[@]}"; do
        IFS=: read -r start end target <<<"${rules[$i]}"
        echo "  $((i + 1))) ${start}-${end} -> ${target}/udp"
    done
    read -r -p "输入要删除的编号 (回车取消): " sel
    [[ -z "${sel}" ]] && return
    if ! [[ "${sel}" =~ ^[0-9]+$ ]] || [ "${sel}" -lt 1 ] || [ "${sel}" -gt "${#rules[@]}" ]; then
        echo -e "${red}编号无效${plain}"; return
    fi
    chosen="${rules[$((sel - 1))]}"
    IFS=: read -r start end target <<<"${chosen}"
    porthop_delete_pair iptables "${start}" "${end}" "${target}"
    porthop_delete_pair ip6tables "${start}" "${end}" "${target}"
    grep -vxF "${chosen}" "${PORTHOP_RULES}" > "${PORTHOP_RULES}.tmp" 2>/dev/null || true
    mv -f "${PORTHOP_RULES}.tmp" "${PORTHOP_RULES}"
    if [[ ! -s "${PORTHOP_RULES}" ]]; then systemctl disable "${PORTHOP_SERVICE}" >/dev/null 2>&1 || true; fi
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
    echo "${APP_NAME} 管理脚本使用方法（Docker 版）："
    echo "------------------------------------------"
    echo "SingR 或 singr              显示管理菜单"
    echo "singr start                 启动 SingR 容器"
    echo "singr stop                  停止容器"
    echo "singr restart               重启容器"
    echo "singr status                查看容器状态"
    echo "singr enable                设置开机自启 (--restart=always)"
    echo "singr disable               取消开机自启 (--restart=no)"
    echo "singr log                   查看容器日志"
    echo "singr update [版本]         拉取镜像并重建（留空=最新，或指定 v0.5.0）"
    echo "singr config                修改配置并重启"
    echo "singr version               查看版本"
    echo "singr uninstall             卸载（删容器与配置）"
    echo "singr update_shell          更新管理脚本"
    echo "singr porthop               Hysteria2 端口跳跃管理"
    echo "------------------------------------------"
}

show_menu() {
    echo -e "
  ${green}${APP_NAME} 后端管理脚本（Docker）${plain}
  0. 修改配置
  2. 更新 SingR（拉镜像重建）
  3. 卸载 SingR
  4. 启动 SingR
  5. 停止 SingR
  6. 重启 SingR
  7. 查看 SingR 状态
  8. 查看 SingR 日志
  9. 设置开机自启
 10. 取消开机自启
 11. 查看 SingR 版本
 12. 更新管理脚本
 13. Hysteria2 端口跳跃管理
"
    show_status
    echo
    read -r -p "请输入选择 [0-13]: " num
    case "${num}" in
        0) config ;;
        2) update_singr ;;
        3) uninstall_singr ;;
        4) start ;;
        5) stop ;;
        6) restart ;;
        7) status ;;
        8) show_log ;;
        9) enable ;;
        10) disable ;;
        11) show_version ;;
        12) update_shell ;;
        13) porthop_menu && before_show_menu ;;
        *) echo -e "${red}请输入正确的数字 [0-13]${plain}" && before_show_menu ;;
    esac
}

if [[ $# -gt 0 ]]; then
    case "$1" in
        start) start 0 ;;
        stop) stop 0 ;;
        restart) restart 0 ;;
        status) status 0 ;;
        enable) enable 0 ;;
        disable) disable 0 ;;
        log) show_log 0 ;;
        update) update_singr 0 "${2:-}" ;;
        config) config 0 ;;
        uninstall) uninstall_singr 0 ;;
        version) show_version 0 ;;
        update_shell) update_shell ;;
        porthop) porthop_menu ;;
        porthop-apply) porthop_reapply_all ;;
        porthop-flush) porthop_flush_all ;;
        _bootstrap) container_recreate ;;   # 供 install-docker.sh 首次创建调用
        *) show_usage ;;
    esac
else
    show_menu
fi
