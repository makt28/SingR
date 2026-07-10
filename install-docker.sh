#!/usr/bin/env bash
#
# SingR installer — DOCKER backend.
#
# Installs Docker (if missing), pulls the SingR node image, writes the run
# config to /etc/singr-docker/docker.conf, creates the container, and installs
# the docker-backed `singr` / `SingR` management command (SingR-docker.sh).
#
# This is a standalone parallel to install.sh (the bare-metal/systemd
# installer): it does NOT touch install.sh, SingR.sh, /etc/singr, the systemd
# unit, or /usr/local/SingR. A host uses one installer or the other.
#
# Usage:
#   bash install-docker.sh --api-url URL --api-key KEY --node-id N --protocol anytls|hysteria2 [opts]
#
# App parameters (written into panel.json on first container start):
#   --api-url URL              (required)   panel apihost
#   --api-key KEY              (required)   panel apikey
#   --node-id N                (required)   panel node id
#   --protocol anytls|hysteria2 (required)
#   --panel-type NAME          (default SSpanel)
#   --node-type NAME           (default V2ray)
#   --timeout N                (default 20)
#   --speed-limit N            (default 0)
#   --device-limit N           (default 0)
#   --enable-device-limit BOOL (default false)
#   --update-periodic N        (default 60)
#   --sni HOST                 inbound server_name
#   --cert-path PATH           TLS cert path inside the container/volume
#   --key-path PATH            TLS key path
#   --log-level LEVEL          (default info)
# Container options:
#   --image REF                (default ghcr.io/makt28/singr:latest)
#   --container-name NAME      (default singr)
#   --restart POLICY           (default always)
#   --log-max-size SIZE        (default 10m)
#   --log-max-file N           (default 5)
#
set -euo pipefail

red='\033[0;31m'; green='\033[0;32m'; yellow='\033[0;33m'; plain='\033[0m'
log_info() { echo -e "${green}$*${plain}"; }
log_warn() { echo -e "${yellow}$*${plain}"; }
die()      { echo -e "${red}$*${plain}" >&2; exit 1; }

CONFIG_DIR="/etc/singr-docker"
CERT_DIR="${CONFIG_DIR}/certs"
DOCKER_CONF="${CONFIG_DIR}/docker.conf"

RELEASE_REPO="${SINGR_RELEASE_REPO:-makt28/SingR}"
RELEASE_BRANCH="${SINGR_RELEASE_BRANCH:-main}"
SCRIPT_URL="https://raw.githubusercontent.com/${RELEASE_REPO}/${RELEASE_BRANCH}/SingR-docker.sh"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- container defaults -----------------------------------------------------
IMAGE="ghcr.io/makt28/singr:latest"
CONTAINER="singr"
RESTART="always"
LOG_MAX_SIZE="10m"
LOG_MAX_FILE="5"

# --- app flags (forwarded verbatim to the container entrypoint) -------------
API_URL=""; API_KEY=""; NODE_ID=""; PROTOCOL=""
declare -a APP_FLAGS=()

check_root() { [[ "${EUID}" -eq 0 ]] || die "错误：必须使用 root 用户运行此脚本。"; }

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --api-url)      API_URL="$2"; APP_FLAGS+=(--api-url "$2"); shift 2;;
            --api-key)      API_KEY="$2"; APP_FLAGS+=(--api-key "$2"); shift 2;;
            --node-id)      NODE_ID="$2"; APP_FLAGS+=(--node-id "$2"); shift 2;;
            --protocol)     PROTOCOL="$2"; APP_FLAGS+=(--protocol "$2"); shift 2;;
            --panel-type)   APP_FLAGS+=(--panel-type "$2"); shift 2;;
            --node-type)    APP_FLAGS+=(--node-type "$2"); shift 2;;
            --timeout)      APP_FLAGS+=(--timeout "$2"); shift 2;;
            --speed-limit)  APP_FLAGS+=(--speed-limit "$2"); shift 2;;
            --device-limit) APP_FLAGS+=(--device-limit "$2"); shift 2;;
            --enable-device-limit) APP_FLAGS+=(--enable-device-limit "$2"); shift 2;;
            --update-periodic)     APP_FLAGS+=(--update-periodic "$2"); shift 2;;
            --sni)          APP_FLAGS+=(--sni "$2"); shift 2;;
            --cert-path)    APP_FLAGS+=(--cert-path "$2"); shift 2;;
            --key-path)     APP_FLAGS+=(--key-path "$2"); shift 2;;
            --log-level)    APP_FLAGS+=(--log-level "$2"); shift 2;;
            --image)          IMAGE="$2"; shift 2;;
            --container-name) CONTAINER="$2"; shift 2;;
            --restart)        RESTART="$2"; shift 2;;
            --log-max-size)   LOG_MAX_SIZE="$2"; shift 2;;
            --log-max-file)   LOG_MAX_FILE="$2"; shift 2;;
            -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
            *) die "未知参数：$1（用 -h 查看用法）";;
        esac
    done
}

validate() {
    [[ -n "${API_URL}" ]]  || die "缺少 --api-url。"
    [[ -n "${API_KEY}" ]]  || die "缺少 --api-key。"
    [[ "${NODE_ID}" =~ ^[0-9]+$ ]] || die "缺少合法 --node-id（整数）。"
    case "$(echo "${PROTOCOL}" | tr '[:upper:]' '[:lower:]')" in
        anytls|hysteria2|hy2) ;;
        *) die "缺少或非法 --protocol（anytls / hysteria2）。";;
    esac
}

install_docker() {
    if command -v docker >/dev/null 2>&1; then
        log_info "检测到 docker：$(docker --version 2>/dev/null || true)"
    else
        log_info "未检测到 docker，开始安装..."
        curl -fsSL https://get.docker.com | sh || die "docker 安装失败，请手动安装后重试。"
    fi
    systemctl enable --now docker >/dev/null 2>&1 || true
    docker info >/dev/null 2>&1 || die "docker 守护进程未运行，请检查 systemctl status docker。"
}

write_conf() {
    mkdir -p "${CONFIG_DIR}" "${CERT_DIR}"
    # printf %q 产出可被 bash 重新解析的安全 token，供 docker.conf 里的数组字面量使用。
    local flags_literal="" f
    for f in "${APP_FLAGS[@]}"; do
        flags_literal+=" $(printf '%q' "${f}")"
    done
    cat > "${DOCKER_CONF}" <<EOF
# SingR docker 运行配置，由 install-docker.sh 生成。
# 容器层参数在此维护；面板/协议配置在 ${CONFIG_DIR}/panel.json（singr config 修改）。
IMAGE="${IMAGE}"
CONTAINER="${CONTAINER}"
RESTART="${RESTART}"
LOG_MAX_SIZE="${LOG_MAX_SIZE}"
LOG_MAX_FILE="${LOG_MAX_FILE}"
RUN_FLAGS=(${flags_literal# })
EOF
    log_info "已写入运行配置：${DOCKER_CONF}"
}

install_management_script() {
    local target="/usr/bin/SingR"
    if [[ -f "${SCRIPT_DIR}/SingR-docker.sh" ]]; then
        install -m 755 "${SCRIPT_DIR}/SingR-docker.sh" "${target}"
    else
        curl -fsSL "${SCRIPT_URL}" -o "${target}" || die "下载管理脚本失败：${SCRIPT_URL}"
        chmod +x "${target}"
    fi
    ln -sf "${target}" /usr/bin/singr
    log_info "已安装管理命令：SingR / singr"
}

print_usage() {
    # 与入口一致的协议归一化，避免 hysteria2/hy2 被截成 hysteria/hy 而提示错误路径。
    local cert_proto
    case "$(echo "${PROTOCOL}" | tr '[:upper:]' '[:lower:]')" in
        hysteria2|hy2) cert_proto="hysteria2" ;;
        *)             cert_proto="anytls" ;;
    esac
    echo ""
    log_info "SingR（Docker）安装完成。"
    echo ""
    echo -e "${yellow}⚠ 证书：没有证书容器不会启动（与裸机一致）。${plain}"
    echo    "  请把证书放到：${CERT_DIR}/${cert_proto}.crt 与 ${cert_proto}.key"
    echo    "  （默认路径见 ${CONFIG_DIR}/server.json，也可用 --cert-path/--key-path 指定）"
    echo    "  放好后执行：singr restart"
    echo ""
    echo "常用管理命令："
    echo "------------------------------------------"
    echo "singr start / stop / restart   启停容器"
    echo "singr status                   查看状态"
    echo "singr log                      查看日志"
    echo "singr update                   拉最新镜像并重建"
    echo "singr config                   修改配置并重启"
    echo "singr version                  查看版本"
    echo "singr porthop                  Hysteria2 端口跳跃"
    echo "------------------------------------------"
    echo ""
    echo "配置目录：${CONFIG_DIR}"
}

main() {
    check_root
    parse_args "$@"
    validate
    install_docker
    write_conf
    install_management_script

    log_info "拉取镜像：${IMAGE}"
    docker pull "${IMAGE}" || die "镜像拉取失败：${IMAGE}"

    # 首次创建容器（entrypoint 会用上面的 flag 生成 panel.json / server.json）。
    # 证书若尚未放置，容器会拒绝启动——这是预期行为。
    /usr/bin/singr _bootstrap || log_warn "容器已创建但可能未成功启动（多半是证书未就绪）。放好证书后执行：singr restart"

    print_usage
}

main "$@"
