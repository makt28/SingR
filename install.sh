#!/usr/bin/env bash

set -euo pipefail

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

APP_NAME="SingR"
SERVICE_NAME="singr"
BIN_NAME="singr"
DEFAULT_RELEASE_REPO="makt28/SingR"
DEFAULT_RELEASE_BRANCH="main"

INSTALL_DIR="/usr/local/SingR"
CONFIG_DIR="/etc/singr"
CERT_DIR="${CONFIG_DIR}/certs"
SYSTEMD_SERVICE="/etc/systemd/system/${SERVICE_NAME}.service"
BIN_PATH="${INSTALL_DIR}/${BIN_NAME}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CUR_DIR="$(pwd)"

# Optional install sources:
#   SINGR_BINARY=/path/to/sing-box-or-singr
#   SINGR_RELEASE_URL=https://example.com/SingR-linux-amd64.tar.gz
#   SINGR_RELEASE_REPO=owner/repo [version]
SINGR_BINARY="${SINGR_BINARY:-}"
SINGR_RELEASE_URL="${SINGR_RELEASE_URL:-}"
SINGR_RELEASE_REPO="${SINGR_RELEASE_REPO:-$DEFAULT_RELEASE_REPO}"
SINGR_RELEASE_BRANCH="${SINGR_RELEASE_BRANCH:-$DEFAULT_RELEASE_BRANCH}"
SINGR_VERSION="${1:-}"

log_info() {
    echo -e "${green}$*${plain}"
}

log_warn() {
    echo -e "${yellow}$*${plain}"
}

log_error() {
    echo -e "${red}$*${plain}"
}

die() {
    log_error "$*"
    exit 1
}

check_root() {
    if [[ "${EUID}" -ne 0 ]]; then
        die "错误：必须使用 root 用户运行此脚本。"
    fi
}

detect_os() {
    if [[ -f /etc/redhat-release ]]; then
        release="centos"
    elif grep -Eqi "debian" /etc/issue 2>/dev/null || grep -Eqi "debian" /proc/version 2>/dev/null; then
        release="debian"
    elif grep -Eqi "ubuntu" /etc/issue 2>/dev/null || grep -Eqi "ubuntu" /proc/version 2>/dev/null; then
        release="ubuntu"
    elif grep -Eqi "centos|red hat|redhat" /etc/issue 2>/dev/null || grep -Eqi "centos|red hat|redhat" /proc/version 2>/dev/null; then
        release="centos"
    else
        die "未检测到支持的系统版本。"
    fi

    os_version=""
    if [[ -f /etc/os-release ]]; then
        os_version="$(awk -F'[= ."]' '/VERSION_ID/{print $3}' /etc/os-release)"
    fi
    if [[ -z "${os_version}" && -f /etc/lsb-release ]]; then
        os_version="$(awk -F'[= ."]+' '/DISTRIB_RELEASE/{print $2}' /etc/lsb-release)"
    fi

    if [[ "${release}" == "centos" && -n "${os_version}" && "${os_version}" -le 6 ]]; then
        die "请使用 CentOS 7 或更高版本。"
    elif [[ "${release}" == "ubuntu" && -n "${os_version}" && "${os_version}" -lt 16 ]]; then
        die "请使用 Ubuntu 16 或更高版本。"
    elif [[ "${release}" == "debian" && -n "${os_version}" && "${os_version}" -lt 8 ]]; then
        die "请使用 Debian 8 或更高版本。"
    fi
}

detect_arch() {
    local raw_arch
    raw_arch="$(uname -m)"
    case "${raw_arch}" in
        x86_64 | x64 | amd64)
            arch="amd64"
            ;;
        aarch64 | arm64)
            arch="arm64"
            ;;
        armv7l | armv7)
            arch="armv7"
            ;;
        s390x)
            arch="s390x"
            ;;
        *)
            die "不支持的 CPU 架构：${raw_arch}"
            ;;
    esac

    if [[ "$(getconf WORD_BIT)" == "32" && "$(getconf LONG_BIT)" != "64" ]]; then
        die "不支持 32 位系统，请使用 64 位系统。"
    fi

    echo "架构：${arch}"
}

install_base() {
    if [[ "${release}" == "centos" ]]; then
        yum install -y epel-release || true
        yum install -y wget curl unzip tar ca-certificates jq
    else
        apt update -y
        apt install -y wget curl unzip tar ca-certificates jq
    fi
}

check_systemd() {
    command -v systemctl >/dev/null 2>&1 || die "未检测到 systemctl，当前脚本仅支持 systemd 环境。"
}

copy_binary() {
    local source_bin="$1"
    local version_output
    [[ -f "${source_bin}" ]] || die "找不到二进制文件：${source_bin}"

    mkdir -p "${INSTALL_DIR}"
    install -m 755 "${source_bin}" "${BIN_PATH}"
    version_output="$(mktemp)"
    if ! "${BIN_PATH}" version >"${version_output}" 2>&1; then
        local output
        output="$(cat "${version_output}" 2>/dev/null || true)"
        rm -f "${version_output}"
        die "安装后的二进制无法执行 version 命令：${output}"
    fi
    log_info "已安装二进制版本："
    sed 's/^/  /' "${version_output}"
    rm -f "${version_output}"
}

build_from_source() {
    [[ -f "${PROJECT_DIR}/go.mod" && -d "${PROJECT_DIR}/cmd/sing-box" ]] || return 1
    command -v go >/dev/null 2>&1 || return 1

    log_info "检测到本地源码，开始编译 SingR..."
    cd "${PROJECT_DIR}"
    export GOTOOLCHAIN=local
    make build
    [[ -f "${PROJECT_DIR}/sing-box" ]] || die "编译完成后未找到 ${PROJECT_DIR}/sing-box"
    copy_binary "${PROJECT_DIR}/sing-box"
    cd "${CUR_DIR}"
}

latest_release_version() {
    curl -fsSL "https://api.github.com/repos/${SINGR_RELEASE_REPO}/releases/latest" |
        grep '"tag_name":' |
        sed -E 's/.*"([^"]+)".*/\1/' |
        head -n 1
}

download_release() {
    local tmp_dir archive version url candidate
    tmp_dir="$(mktemp -d)"

    if [[ -n "${SINGR_RELEASE_URL}" ]]; then
        url="${SINGR_RELEASE_URL}"
        log_info "开始下载 SingR：${url}"
        archive="${tmp_dir}/$(basename "${url}")"
        curl -fL "${url}" -o "${archive}"
    else
        if [[ -n "${SINGR_VERSION}" ]]; then
            if [[ "${SINGR_VERSION}" == v* ]]; then
                version="${SINGR_VERSION}"
            else
                version="v${SINGR_VERSION}"
            fi
        else
            version="$(latest_release_version)"
        fi
        [[ -n "${version}" ]] || die "检测 SingR 最新版本失败，请手动指定版本或设置 SINGR_RELEASE_URL。"

        log_info "检测到 SingR 版本：${version}"
        for candidate in \
            "SingR-linux-${arch}.tar.gz" \
            "singr-linux-${arch}.tar.gz" \
            "sing-box-linux-${arch}.tar.gz" \
            "SingR-linux-${arch}.zip" \
            "singr-linux-${arch}.zip" \
            "sing-box-linux-${arch}.zip"; do
            url="https://github.com/${SINGR_RELEASE_REPO}/releases/download/${version}/${candidate}"
            archive="${tmp_dir}/${candidate}"
            if curl -fL "${url}" -o "${archive}"; then
                break
            fi
            rm -f "${archive}"
        done
        [[ -f "${archive}" ]] || die "下载 SingR ${version} 失败，请确认 release 文件名和架构。"
    fi

    case "${archive}" in
        *.zip)
            unzip -q "${archive}" -d "${tmp_dir}/extract"
            ;;
        *.tar.gz | *.tgz)
            mkdir -p "${tmp_dir}/extract"
            tar -xzf "${archive}" -C "${tmp_dir}/extract"
            ;;
        *)
            mkdir -p "${tmp_dir}/extract"
            cp "${archive}" "${tmp_dir}/extract/singr"
            ;;
    esac

    local found_bin
    found_bin="$(find "${tmp_dir}/extract" -type f \( -name "singr" -o -name "SingR" -o -name "sing-box" \) | head -n 1)"
    [[ -n "${found_bin}" ]] || die "release 包中未找到 singr、SingR 或 sing-box 二进制。"
    chmod +x "${found_bin}"
    copy_binary "${found_bin}"
    rm -rf "${tmp_dir}"
}

install_binary() {
    if [[ -n "${SINGR_BINARY}" ]]; then
        log_info "使用 SINGR_BINARY 安装：${SINGR_BINARY}"
        copy_binary "${SINGR_BINARY}"
        return
    fi

    if [[ -x "${SCRIPT_DIR}/singr" ]]; then
        log_info "使用 release 包内二进制：${SCRIPT_DIR}/singr"
        copy_binary "${SCRIPT_DIR}/singr"
        return
    fi

    if [[ -x "${PROJECT_DIR}/sing-box" ]]; then
        log_info "使用本地已编译二进制：${PROJECT_DIR}/sing-box"
        copy_binary "${PROJECT_DIR}/sing-box"
        return
    fi

    if build_from_source; then
        return
    fi

    download_release || die "未找到可安装的 SingR。请设置 SINGR_BINARY、在源码目录运行脚本，或设置 SINGR_RELEASE_REPO/SINGR_RELEASE_URL。"
}

install_config() {
    mkdir -p "${CONFIG_DIR}" "${CERT_DIR}"

    if [[ ! -f "${CONFIG_DIR}/panel.json" ]]; then
        if [[ -f "${SCRIPT_DIR}/panel.json" ]]; then
            cp "${SCRIPT_DIR}/panel.json" "${CONFIG_DIR}/panel.json"
            log_info "已安装默认面板配置：${CONFIG_DIR}/panel.json"
        elif [[ -f "${PROJECT_DIR}/release/poet/panel_anytls.json" ]]; then
            cp "${PROJECT_DIR}/release/poet/panel_anytls.json" "${CONFIG_DIR}/panel.json"
            log_info "已安装默认面板配置：${CONFIG_DIR}/panel.json"
        else
            cat >"${CONFIG_DIR}/panel.json" <<'EOF'
{
  "name": "connect old sspanel v2ray node and run it as anytls",
  "nodes": [
    {
      "paneltype": "SSpanel",
      "intag": "anytls-in",
      "outtag": "anytls-out",
      "apiconfig": {
        "apihost": "https://your-sspanel.example.com",
        "apikey": "your-apikey",
        "nodeid": 1,
        "nodetype": "V2ray",
        "disablecustomconfig": true
      }
    }
  ]
}
EOF
            log_info "已生成默认面板配置：${CONFIG_DIR}/panel.json"
        fi
    else
        log_warn "保留已有面板配置：${CONFIG_DIR}/panel.json"
    fi

    if [[ ! -f "${CONFIG_DIR}/server.json" ]]; then
        if [[ -f "${SCRIPT_DIR}/server.json" ]]; then
            cp "${SCRIPT_DIR}/server.json" "${CONFIG_DIR}/server.json"
            log_info "已安装默认 sing-box 配置：${CONFIG_DIR}/server.json"
        elif [[ -f "${PROJECT_DIR}/release/poet/server.json" ]]; then
            cp "${PROJECT_DIR}/release/poet/server.json" "${CONFIG_DIR}/server.json"
            log_info "已安装默认 sing-box 配置（anytls + hysteria2 超集）：${CONFIG_DIR}/server.json"
        elif [[ -f "${PROJECT_DIR}/release/poet/server_anytls.json" ]]; then
            cp "${PROJECT_DIR}/release/poet/server_anytls.json" "${CONFIG_DIR}/server.json"
            log_info "已安装默认 sing-box 配置：${CONFIG_DIR}/server.json"
        else
            cat >"${CONFIG_DIR}/server.json" <<'EOF'
{
  "log": {
    "disabled": false,
    "level": "info",
    "timestamp": true,
    "output": "/var/log/singr.log"
  },
  "dns": {
    "servers": [
      {
        "tag": "google",
        "type": "udp",
        "server": "8.8.8.8"
      }
    ],
    "strategy": "prefer_ipv6"
  },
  "inbounds": [
    {
      "type": "anytls",
      "tag": "anytls-in",
      "listen": "::",
      "listen_port": 0,
      "users": [],
      "tls": {
        "enabled": true,
        "server_name": "",
        "certificate_path": "/etc/singr/certs/anytls.crt",
        "key_path": "/etc/singr/certs/anytls.key"
      }
    },
    {
      "type": "hysteria2",
      "tag": "hysteria2-in",
      "listen": "::",
      "listen_port": 0,
      "users": [],
      "up_mbps": 0,
      "down_mbps": 0,
      "ignore_client_bandwidth": false,
      "tls": {
        "enabled": true,
        "server_name": "",
        "certificate_path": "/etc/singr/certs/hysteria2.crt",
        "key_path": "/etc/singr/certs/hysteria2.key"
      }
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "anytls-out",
      "domain_strategy": "prefer_ipv6"
    },
    {
      "type": "direct",
      "tag": "hysteria2-out",
      "domain_strategy": "prefer_ipv6"
    },
    {
      "type": "direct",
      "tag": "direct",
      "domain_strategy": "prefer_ipv6"
    }
  ],
  "route": {
    "rules": [
      {
        "inbound": "anytls-in",
        "outbound": "anytls-out"
      },
      {
        "inbound": "hysteria2-in",
        "outbound": "hysteria2-out"
      }
    ],
    "final": "direct",
    "auto_detect_interface": false
  }
}
EOF
            log_info "已生成默认 sing-box 配置（anytls + hysteria2 超集）：${CONFIG_DIR}/server.json"
        fi
    else
        log_warn "保留已有 sing-box 配置：${CONFIG_DIR}/server.json"
    fi
}

migrate_config() {
    local cfg="${CONFIG_DIR}/server.json"
    [[ -f "${cfg}" ]] || return 0
    command -v jq >/dev/null 2>&1 || { log_warn "未安装 jq，跳过 server.json 迁移；建议手动加上 dns/domain_strategy。"; return 0; }

    local needs_migrate
    needs_migrate="$(jq -r '
        (.dns // null) as $dns
        | (.route.auto_detect_interface // false) as $adi
        | (.outbounds // []) as $outs
        | ((($outs | map(select(.type=="direct" and (.domain_strategy // "")==""))) | length) > 0) as $missDS
        | (($dns == null) or $missDS or ($adi == true))
    ' "${cfg}" 2>/dev/null || echo "false")"

    if [[ "${needs_migrate}" != "true" ]]; then
        return 0
    fi

    local backup="${cfg}.bak.$(date +%Y%m%d%H%M%S)"
    cp "${cfg}" "${backup}"
    log_info "迁移 server.json，备份 -> ${backup}"

    local tmp
    tmp="$(mktemp)"
    if jq '
        .dns = (.dns // {servers:[{tag:"google",type:"udp",server:"8.8.8.8"}], strategy:"prefer_ipv6"})
        | .outbounds = ((.outbounds // []) | map(
            if .type == "direct" and ((.domain_strategy // "") == "")
            then . + {domain_strategy:"prefer_ipv6"}
            else .
            end))
        | .route = ((.route // {}) | .auto_detect_interface = false)
    ' "${cfg}" > "${tmp}"; then
        mv "${tmp}" "${cfg}"
        log_info "已为 server.json 添加 IPv6 出站策略 (prefer_ipv6) 并关闭 auto_detect_interface。"
    else
        rm -f "${tmp}"
        log_warn "server.json 迁移失败，已保留原配置。"
    fi
}

install_service() {
    cat >"${SYSTEMD_SERVICE}" <<EOF
[Unit]
Description=SingR SSPanel backend
After=network.target nss-lookup.target network-online.target
Wants=network-online.target

[Service]
User=root
Group=root
Type=simple
WorkingDirectory=${INSTALL_DIR}
ExecStart=${BIN_PATH} run -c ${CONFIG_DIR}/server.json -p ${CONFIG_DIR}/panel.json
Restart=on-failure
RestartSec=10
LimitAS=infinity
LimitRSS=infinity
LimitCORE=infinity
LimitNOFILE=999999
Environment="http_proxy="
Environment="https_proxy="
Environment="HTTP_PROXY="
Environment="HTTPS_PROXY="
Environment="ALL_PROXY="
Environment="all_proxy="

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}"
}

install_management_script() {
    local target="/usr/bin/SingR"
    local lower="/usr/bin/singr"
    local source_script="${PROJECT_DIR}/SingR.sh"
    local script_url="https://raw.githubusercontent.com/${SINGR_RELEASE_REPO}/${SINGR_RELEASE_BRANCH}/SingR.sh"

    if [[ -f "${SCRIPT_DIR}/SingR.sh" ]]; then
        install -m 755 "${SCRIPT_DIR}/SingR.sh" "${target}"
    elif [[ -f "${source_script}" ]]; then
        install -m 755 "${source_script}" "${target}"
    else
        curl -fsSL "${script_url}" -o "${target}" || die "下载管理脚本失败：${script_url}"
        chmod +x "${target}"
    fi

    ln -sf "${target}" "${lower}"
}

print_usage() {
    echo ""
    echo "SingR 安装完成。"
    echo ""
    echo "管理命令已安装："
    echo "  SingR"
    echo "  singr"
    echo ""
    echo "两个命令等价，大小写都可以。直接输入 SingR 或 singr 打开管理菜单。"
    echo ""
    echo "常用管理命令："
    echo "------------------------------------------"
    echo "singr start        启动 SingR"
    echo "singr stop         停止 SingR"
    echo "singr restart      重启 SingR"
    echo "singr status       查看状态"
    echo "singr log          查看日志"
    echo "singr update       更新 SingR"
    echo "singr config       修改配置"
    echo "singr version      查看版本"
    echo "------------------------------------------"
    echo ""
    echo "systemd 命令仍可直接使用：systemctl restart ${SERVICE_NAME}"
    echo "运行二进制路径：${BIN_PATH}"
    echo ""
    echo "配置文件："
    echo "${CONFIG_DIR}/panel.json"
    echo "${CONFIG_DIR}/server.json"
    echo ""
    echo "首次安装后请先修改配置和证书，再运行："
    echo "singr config"
    echo "singr restart"
}

main() {
    check_root
    detect_os
    detect_arch
    check_systemd

    log_info "开始安装 SingR"
    install_base
    install_binary
    install_config
    migrate_config
    install_service
    install_management_script

    log_info "SingR 已设置开机自启。"
    print_usage
}

main "$@"
