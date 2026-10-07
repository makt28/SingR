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
CERT_DIR="${CONFIG_DIR}/certs"
PANEL_CONFIG="${CONFIG_DIR}/panel.json"
SERVER_CONFIG="${CONFIG_DIR}/server.json"
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
# 用真实 State.Status 判断，而不是 `docker ps`——崩溃重启中的容器 `docker ps`
# 也会列出（Restarting），会把 crash-loop 误判成"已运行"。只有 status==running
# 才算真在跑（restarting/exited/created 都不算）。
container_running() { [[ "$(docker inspect -f '{{.State.Status}}' "${CONTAINER}" 2>/dev/null)" == "running" ]]; }

# 删旧建新（update / 首次 bootstrap / 证书挂载变化共用）。RUN_FLAGS 是首启引导参数；
# 因为 entrypoint 只在 panel.json 不存在时生成，重建不会覆盖已有配置。
#
# 除了整体挂载 ${CONFIG_DIR}，还把 server.json 引用的证书所在目录按原路径只读挂进去
# （cert_mount_dirs），容器里看到的证书路径和宿主机一模一样。
container_recreate() {
    require_docker
    local -a cert_mounts=()
    local d
    while IFS= read -r d; do
        [[ -n "${d}" ]] && cert_mounts+=(--mount "type=bind,source=${d},target=${d},readonly")
    done < <(cert_mount_dirs)
    docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
    docker run -d \
        --name "${CONTAINER}" \
        --network host \
        --restart "${RESTART}" \
        --log-opt max-size="${LOG_MAX_SIZE}" \
        --log-opt max-file="${LOG_MAX_FILE}" \
        -v "${CONFIG_DIR}:${CONFIG_DIR}" \
        "${cert_mounts[@]}" \
        "${IMAGE}" "${RUN_FLAGS[@]}"
}

# docker run -d 只要容器被创建就返回 0，即使入口随即退出（例如缺证书）。
# 采样两次 State.Status + RestartCount：既要当前在 running，又要重启计数不再增长，
# 才算真正起来了——否则是 crash-loop（缺证书等会不停重启，单次采样可能恰好抓到
# 短暂的 running）。
verify_running() {
    sleep 2
    container_running || return 1
    local r1 r2
    r1="$(docker inspect -f '{{.RestartCount}}' "${CONTAINER}" 2>/dev/null)"
    sleep 3
    container_running || return 1
    r2="$(docker inspect -f '{{.RestartCount}}' "${CONTAINER}" 2>/dev/null)"
    [[ "${r1}" == "${r2}" ]]
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

# 容器的 restart 策略（no / always / unless-stopped / on-failure）
container_restart_policy() {
    docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "${CONTAINER}" 2>/dev/null
}

# dockerd 本身是否开机自启——容器 --restart 策略只有在 docker 服务开机启动时才生效
docker_daemon_enabled() {
    systemctl is-enabled --quiet docker 2>/dev/null
}

# 开机自启 = 容器 restart 策略 + docker 服务开机自启，两者都要满足
show_enable_status() {
    local pol; pol="$(container_restart_policy)"
    if [[ "${pol}" == "always" || "${pol}" == "unless-stopped" ]]; then
        echo -e "是否开机自启：${green}是${plain}（容器 --restart=${pol}）"
    else
        echo -e "是否开机自启：${red}否${plain}（容器 --restart=${pol:-无}）"
    fi
    if command -v systemctl >/dev/null 2>&1 && ! docker_daemon_enabled; then
        echo -e "  ${yellow}⚠ docker 服务未开机自启，容器重启后不会自动拉起：systemctl enable docker${plain}"
    fi
}

start() {
    require_docker
    local rc=0
    if container_running; then
        echo -e "${green}${APP_NAME} 已运行，无需再次启动${plain}"
    elif container_ensure && verify_running; then
        echo -e "${green}${APP_NAME} 启动成功${plain}"
    else
        echo -e "${red}${APP_NAME} 启动失败（容器未在运行，可能缺证书），请用 singr log 查看日志${plain}"; rc=1
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
    # 证书挂载变了（singr config 改了证书路径等）就重建，否则就地重启。
    if container_ensure && verify_running; then
        echo -e "${green}${APP_NAME} 重启成功${plain}"
    else
        echo -e "${red}${APP_NAME} 重启失败（容器未在运行，可能缺证书），请用 singr log 查看日志${plain}"; rc=1
    fi
    if [[ $# == 0 ]]; then before_show_menu; fi
    return ${rc}
}

status() {
    require_docker
    docker ps -a --filter "name=^/${CONTAINER}$" \
        --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}' 2>/dev/null || true
    if container_exists; then echo; show_enable_status; fi
    if [[ $# == 0 ]]; then before_show_menu; fi
}

enable() {
    require_docker
    container_exists || { echo -e "${red}容器不存在，请先 singr start${plain}"; if [[ $# == 0 ]]; then before_show_menu; fi; return 1; }
    docker update --restart=always "${CONTAINER}" >/dev/null 2>&1 || true
    # 容器自启依赖 dockerd 自身开机启动，一并设上，否则重启后不生效
    systemctl enable docker >/dev/null 2>&1 || true
    RESTART="always"; save_restart
    echo -e "${green}${APP_NAME} 已设置开机自启（容器 --restart=always + docker 服务开机自启）${plain}"
    if [[ $# == 0 ]]; then before_show_menu; fi
}

disable() {
    require_docker
    # 只取消容器自启，不动 docker 服务（宿主机可能还跑着别的容器）
    docker update --restart=no "${CONTAINER}" >/dev/null 2>&1 || true
    RESTART="no"; save_restart
    echo -e "${green}${APP_NAME} 已取消开机自启（容器 --restart=no；docker 服务未改动）${plain}"
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
    cert_timer_remove

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
    # docker 下「版本」分两层：docker.conf 里锁定的镜像 tag，和容器内二进制自报版本。
    echo -e "${green}配置镜像（docker.conf）：${plain}${IMAGE}"
    if container_exists; then
        echo -e "${green}容器当前镜像：${plain}$(docker inspect -f '{{.Config.Image}}' "${CONTAINER}" 2>/dev/null)"
    fi
    echo -e "${green}二进制版本：${plain}"
    if container_running; then
        docker exec "${CONTAINER}" singr version 2>&1 | sed 's/^/  /'
    elif docker image inspect "${IMAGE}" >/dev/null 2>&1; then
        echo -e "  ${yellow}(容器未运行，取镜像内二进制)${plain}"
        docker run --rm --entrypoint singr "${IMAGE}" version 2>&1 | sed 's/^/  /'
    else
        echo -e "  ${red}容器未运行且本地无镜像 ${IMAGE}，无法查看。可先 singr start 或 docker pull${plain}"
    fi
    if [[ $# == 0 ]]; then before_show_menu; fi
}

# 先下载到同一文件系统上的临时文件，校验通过再原子替换。
#
# 旧写法是 `curl -fsSL ... -o /usr/bin/SingR`，有三个问题，凑在一起会把管理命令
# 自己写坏而且无从恢复（升级到一半磁盘满 / 网络断就会复现）：
#   1. curl 的退出码没被检查，写失败也照样打印"升级成功"；
#   2. 下载目标就是正在执行的脚本，curl 会先就地截断再逐块写入，中断即残缺；
#   3. bash 是按字节偏移边读边执行的，就地覆盖还会让当前这次运行读到错位内容。
# 换成 临时文件 + bash -n 校验 + mv(rename)：失败时原脚本一个字节都没动；成功时
# rename 换的是 inode，正在运行的 bash 继续持有旧 inode，第 3 条也一并消失。
update_shell() {
    local tmp
    tmp="$(mktemp /usr/bin/.SingR.XXXXXX 2>/dev/null)" || {
        echo -e "${red}无法在 /usr/bin 下创建临时文件（磁盘写满？先看 df -h / 和 df -i /）${plain}"
        return 1
    }
    if ! curl -fsSL "${SCRIPT_URL}" -o "${tmp}"; then
        rm -f "${tmp}"
        echo -e "${red}下载失败：${SCRIPT_URL}${plain}"
        echo -e "${yellow}管理脚本未改动。若 curl 报 (23) 写入失败，多半是磁盘满：df -h / && df -i /${plain}"
        return 1
    fi
    if [[ ! -s "${tmp}" ]] || ! bash -n "${tmp}" 2>/dev/null || ! grep -q '^show_menu()' "${tmp}"; then
        rm -f "${tmp}"
        echo -e "${red}下载到的脚本不完整或语法有误，已丢弃，管理脚本未改动${plain}"
        echo -e "${yellow}多半是下载被截断（磁盘满或网络中断）：df -h / && df -i /${plain}"
        return 1
    fi
    chmod 755 "${tmp}"
    if ! mv -f "${tmp}" /usr/bin/SingR; then
        rm -f "${tmp}"
        echo -e "${red}替换 /usr/bin/SingR 失败，管理脚本未改动${plain}"
        return 1
    fi
    ln -sf /usr/bin/SingR /usr/bin/singr
    echo -e "${green}管理脚本升级成功，请重新运行 SingR${plain}"
    exit 0
}

show_status() {
    check_status
    case $? in
        0) echo -e "${APP_NAME} 状态：${green}已运行${plain}"; show_enable_status ;;
        1) echo -e "${APP_NAME} 状态：${yellow}未运行（容器已创建）${plain}"; show_enable_status ;;
        2) echo -e "${APP_NAME} 状态：${red}未安装（无容器）${plain}" ;;
    esac
}

# >>>>>>>>>>>>>>>> SYNC BLOCK: 节点管理 >>>>>>>>>>>>>>>>
# 本块在 SingR.sh 与 SingR-docker.sh 中逐字相同，改一处必须同步另一处。
#
# 单进程多节点：panel.json 的 .nodes[] 每项是一个面板节点，靠唯一的 intag 绑定
# server.json 里的一个 inbound。Go 侧本来就按这个模型工作——
# poet/panel/panel.go 按 NodesConfig 循环建 Controller（每个 Controller 自带
# Authenticator，用户表与流量计数天然隔离），poet/poet.go 按 metadata.Inbound
# 找 Controller 计费，cmd/sing-box/cmd_run.go 的 filterInboundsByPanel 只实例化
# 被 intag 引用的 inbound。所以"加节点"纯粹是改这两个 JSON，无需改 Go。
#
# 注意：同一进程内任一节点 Start() 失败会让 poet/panel/panel.go 走 os.Exit(1)，
# 把所有节点一起带下线。因此本块的每次写入都走"备份 → 重启 → 校验 → 失败回滚"。
#
# 后端差异由下列钩子承担，各脚本自行实现，不在本块内：
#   node_backend_place_cert <tag> <cert_src> <key_src>  -> 校验证书路径对该后端可用，仅在
#                                                          stdout 回显 "<certpath>|<keypath>"，其余输出走 stderr
#   node_backend_restart                                -> 重启后端
#   node_backend_verify                                 -> 0=确实起来了（须能识别 crash-loop）
#   node_cert_renew_hint                                -> 打印该后端的证书续期建议
#
# 两种后端都原地引用证书（不复制）：server.json 写的就是证书的真实路径。docker 靠
# 同路径只读挂载证书所在目录让容器看到它们，见 SingR-docker.sh 的 cert_mount_dirs。

node_require_jq() {
    command -v jq >/dev/null 2>&1 && return 0
    echo -e "${yellow}节点管理需要 jq，正在尝试自动安装...${plain}"
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update -qq >/dev/null 2>&1
        apt-get install -y -qq jq >/dev/null 2>&1
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y jq >/dev/null 2>&1
    elif command -v yum >/dev/null 2>&1; then
        yum install -y jq >/dev/null 2>&1
    elif command -v apk >/dev/null 2>&1; then
        apk add --no-cache jq >/dev/null 2>&1
    fi
    command -v jq >/dev/null 2>&1 && return 0
    echo -e "${red}jq 安装失败，请手动安装后重试（Debian/Ubuntu: apt install jq）${plain}"
    return 1
}

node_norm_proto() {
    case "$(echo "${1:-}" | tr '[:upper:]' '[:lower:]')" in
        anytls) echo anytls ;;
        hysteria2 | hy2) echo hysteria2 ;;
        *) return 1 ;;
    esac
}

node_configs_ready() {
    if [[ ! -f "${PANEL_CONFIG}" || ! -f "${SERVER_CONFIG}" ]]; then
        echo -e "${red}未找到 ${PANEL_CONFIG} 或 ${SERVER_CONFIG}，请先完成安装${plain}"
        return 1
    fi
    return 0
}

# ---- 默认证书路径（Go 侧解析的 shell 镜像）----
#
# server.json 里 certificate_path/key_path 为空**不是**"没配证书"，而是"用默认
# 路径"：二进制在 cmd/sing-box/cmd_run.go 的 applyDefaultCertificatePaths 里把空
# 值补成 <panel.json 所在目录>/certs/default.pem（不存在则试 default.crt）加
# default.key。裸机是 /etc/singr/certs，docker 是 /etc/singr-docker/certs，同一份
# 代码同一个二进制，差别只来自 -p 的路径。
#
# 权威实现在 Go 那边，这里是它在 shell 侧的镜像，供 singr list 的证书状态、
# docker 的启动前预检和证书同步使用。改任一侧必须同步另一侧。
node_default_cert_path() {
    if [[ -s "${CERT_DIR}/default.pem" ]]; then
        printf '%s' "${CERT_DIR}/default.pem"
    elif [[ -s "${CERT_DIR}/default.crt" ]]; then
        printf '%s' "${CERT_DIR}/default.crt"
    else
        printf '%s' "${CERT_DIR}/default.pem"
    fi
}

node_default_key_path() {
    printf '%s' "${CERT_DIR}/default.key"
}

# server.json 里有没有这个 tag 的 inbound。和"证书为空"是两回事：前者是
# panel.json / server.json 的 tag 对不上（进程会以 no Node with InTag found 退出），
# 后者只是还没放证书。两种情况的处理完全不同，别用 -z cert 一并判断。
node_inbound_exists() {
    jq -e --arg t "$1" '[.inbounds[]? | select(.tag==$t)] | length > 0' "${SERVER_CONFIG}" >/dev/null 2>&1
}

# 证书剩余有效天数，向下取整。回显整数（已过期为负）；判定不了就回显空串，调用方
# 退回到只显示 OK —— 列个节点表不该因为解析不出日期就报错。
#
# 过期与否单独用 openssl x509 -checkend 0 判定（node_cert_expired），它不依赖任何
# 日期解析，在哪都准；天数才走下面的 date 换算。两种 date 都试：Linux 服务器是
# GNU date -d，macOS 上跑脚本是 BSD date -j -f。openssl 的 notAfter 形如
# "Sep  5 02:37:00 2026 GMT"，单数日会有两个空格，%e 认这个。
node_cert_days_left() {
    local end_epoch now_epoch
    end_epoch="$(node_cert_end_epoch "$1")"
    [[ -n "${end_epoch}" ]] || return 0
    now_epoch="$(date +%s)"
    printf '%s' "$(( (end_epoch - now_epoch) / 86400 ))"
}

# 证书 notAfter 的 epoch 秒；判定不了就回显空串。
node_cert_end_epoch() {
    local cert="$1" end_date end_epoch
    [[ -s "${cert}" ]] || return 0
    command -v openssl >/dev/null 2>&1 || return 0
    end_date="$(openssl x509 -enddate -noout -in "${cert}" 2>/dev/null)" || return 0
    end_date="${end_date#notAfter=}"
    [[ -n "${end_date}" ]] || return 0
    end_epoch="$(date -d "${end_date}" +%s 2>/dev/null)"
    [[ -n "${end_epoch}" ]] || end_epoch="$(date -j -f "%b %e %H:%M:%S %Y %Z" "${end_date}" +%s 2>/dev/null)"
    [[ "${end_epoch}" =~ ^-?[0-9]+$ ]] || return 0
    printf '%s' "${end_epoch}"
}

# 0 = 证书已过期。openssl 不在或读不了证书时返回 1（当作没过期），宁可漏报也不误报。
node_cert_expired() {
    local cert="$1"
    [[ -s "${cert}" ]] || return 1
    command -v openssl >/dev/null 2>&1 || return 1
    openssl x509 -checkend 0 -noout -in "${cert}" >/dev/null 2>&1 && return 1
    # -checkend 非零也可能是"读不出证书"，那种情况不该报成已过期。
    openssl x509 -enddate -noout -in "${cert}" >/dev/null 2>&1
}

# 证书状态列的文案："OK 剩 87 天" / 不足 30 天转黄 / 已过期转红。
# openssl 缺失或日期解析不了时退回原来的纯 OK。
node_cert_status() {
    local cert="$1" days
    if node_cert_expired "${cert}"; then
        printf '%s' "${red}已过期${plain}"
        return 0
    fi
    days="$(node_cert_days_left "${cert}")"
    if [[ -z "${days}" ]]; then
        printf '%s' "${green}OK${plain}"
    elif [[ "${days}" -lt 30 ]]; then
        printf '%s' "${yellow}OK 剩 ${days} 天${plain}"
    else
        printf '%s' "${green}OK 剩 ${days} 天${plain}"
    fi
}

# 回显某 inbound 实际生效的 "<certpath>|<keypath>"。
#
# 只有 certificate_path 和 key_path **都**为空才代入默认路径 —— 必须和 Go 侧的
# certificateUnset 完全一致。只填了一个是用户手写漏了，二进制会以 missing
# certificate / missing key 拒绝启动；如果这里各自独立地补默认值，singr list 会
# 给一个根本起不来的配置报 OK。半配置的情况照实回显（其中一项为空串），由调用方
# 判成"配置不全"。
node_effective_cert() {
    local tag="$1" cert key
    cert="$(jq -r --arg t "${tag}" '(first(.inbounds[]? | select(.tag==$t) | .tls.certificate_path)) // ""' "${SERVER_CONFIG}" 2>/dev/null)"
    key="$(jq -r --arg t "${tag}" '(first(.inbounds[]? | select(.tag==$t) | .tls.key_path)) // ""' "${SERVER_CONFIG}" 2>/dev/null)"
    if [[ -z "${cert}" && -z "${key}" ]]; then
        cert="$(node_default_cert_path)"
        key="$(node_default_key_path)"
    fi
    printf '%s|%s' "${cert}" "${key}"
}

# tag 是否已被某个节点占用。只看 panel.json：server.json 里存在同名 inbound 但没有
# 节点引用它（默认配置铺的 anytls-in / hysteria2-in 超集就是这种），那不是"占用"，
# 而是可以直接接管的模板——第一次 add 就该落在 anytls-in 上，与老装机保持一致。
node_tag_used() {
    jq -e --arg t "$1" '[(.nodes // [])[].intag] | index($t) != null' "${PANEL_CONFIG}" >/dev/null 2>&1
}

node_alloc_tag() {
    local proto="$1" nodeid="$2" cand n
    cand="${proto}-in"
    node_tag_used "${cand}" || { printf '%s' "${cand}"; return; }
    cand="${proto}-in-${nodeid}"
    node_tag_used "${cand}" || { printf '%s' "${cand}"; return; }
    n=2
    while node_tag_used "${cand}-${n}"; do n=$((n + 1)); done
    printf '%s' "${cand}-${n}"
}

node_count() {
    jq -r '(.nodes // []) | length' "${PANEL_CONFIG}" 2>/dev/null || echo 0
}

# install.sh 铺的占位节点（apihost=your-sspanel.example.com / apikey=your-apikey）
# 不是真节点，留着会在启动时 GetNodeInfo 失败，把同进程的真节点一起拖死。
node_has_placeholder() {
    jq -e '[(.nodes // [])[] | select(((.apiconfig.apihost // "") | test("your-sspanel\\.example\\.com"))
            or ((.apiconfig.apikey // "") == "your-apikey"))] | length > 0' "${PANEL_CONFIG}" >/dev/null 2>&1
}

node_drop_placeholder() {
    local tmp
    tmp="$(mktemp)" || return 1
    jq '(.nodes // []) |= map(select((((.apiconfig.apihost // "") | test("your-sspanel\\.example\\.com"))
            or ((.apiconfig.apikey // "") == "your-apikey")) | not))' \
        "${PANEL_CONFIG}" >"${tmp}" && mv -f "${tmp}" "${PANEL_CONFIG}"
}

node_list() {
    node_require_jq || return 1
    node_configs_ready || return 1

    echo -e "${green}当前节点（${PANEL_CONFIG}）：${plain}"
    if [[ "$(node_count)" == "0" ]]; then
        echo -e "  ${yellow}(无节点，用 singr add 添加)${plain}"
        return 0
    fi

    # 表头用 ASCII：printf 的宽度按字节算，中文表头会和下面的数据列对不齐。
    printf '  %-3s %-8s %-10s %-34s %-20s %s\n' "#" "NodeID" "PROTO" "DOMAIN" "INTAG" "CERT"
    local i=1 nodeid apihost intag proto paths cert key mark
    while IFS=$'\t' read -r nodeid apihost intag; do
        [[ -z "${intag}" ]] && continue
        proto="$(jq -r --arg t "${intag}" '(first(.inbounds[]? | select(.tag==$t) | .type)) // ""' "${SERVER_CONFIG}" 2>/dev/null)"
        [[ -z "${proto}" ]] && proto="${intag%%-in*}"
        if ! node_inbound_exists "${intag}"; then
            mark="${red}无 inbound${plain}"
        else
            paths="$(node_effective_cert "${intag}")"
            cert="${paths%%|*}"
            key="${paths##*|}"
            if [[ -z "${cert}" || -z "${key}" ]]; then
                # 只写了 certificate_path 或 key_path 其中一个，二进制会拒绝启动。
                mark="${red}配置不全${plain}"
            elif [[ -s "${cert}" && -s "${key}" ]]; then
                mark="$(node_cert_status "${cert}")"
            else
                mark="${red}缺失${plain}"
            fi
        fi
        printf '  %-3s %-8s %-10s %-34s %-20s ' "${i}" "${nodeid}" "${proto}" "${apihost}" "${intag}"
        echo -e "${mark}"
        i=$((i + 1))
    done < <(jq -r '(.nodes // [])[] | [((.apiconfig.nodeid // "?")|tostring), (.apiconfig.apihost // "?"), (.intag // "")] | @tsv' "${PANEL_CONFIG}" 2>/dev/null)
    # docker 版从复制模型迁移时，没法自动续期的证书记在这个文件里（见 certs_migrate）。
    # 列表是运维最常看的地方，提示挂在这里才不会被错过；处理完删掉文件即可。
    if [[ -s "${CONFIG_DIR}/cert-migration-notice.txt" ]]; then
        echo -e "  ${yellow}有证书在迁移后不再自动续期，详见 ${CONFIG_DIR}/cert-migration-notice.txt（处理完删掉该文件）${plain}"
    fi
}

# 菜单里用的简版：装好之前/没有 jq 时安静跳过，不要在主菜单上刷红字。
node_list_brief() {
    command -v jq >/dev/null 2>&1 || return 0
    [[ -f "${PANEL_CONFIG}" && -f "${SERVER_CONFIG}" ]] || return 0
    node_list
}

node_backup() {
    cp -f "${PANEL_CONFIG}" "${PANEL_CONFIG}.bak" 2>/dev/null || return 1
    cp -f "${SERVER_CONFIG}" "${SERVER_CONFIG}.bak" 2>/dev/null || return 1
}

node_restore() {
    [[ -f "${PANEL_CONFIG}.bak" ]] && mv -f "${PANEL_CONFIG}.bak" "${PANEL_CONFIG}"
    [[ -f "${SERVER_CONFIG}.bak" ]] && mv -f "${SERVER_CONFIG}.bak" "${SERVER_CONFIG}"
    return 0
}

node_commit() {
    rm -f "${PANEL_CONFIG}.bak" "${SERVER_CONFIG}.bak"
}

# 落盘后重启并校验；起不来就还原配置再重启回去。
node_apply() {
    echo -e "${green}正在重启 ${APP_NAME} 应用变更...${plain}"
    node_backend_restart
    if node_backend_verify; then
        node_commit
        echo -e "${green}变更已生效${plain}"
        return 0
    fi
    echo -e "${red}${APP_NAME} 未能正常启动，正在回滚配置...${plain}"
    node_restore
    node_backend_restart
    if node_backend_verify; then
        echo -e "${yellow}已回滚到变更前的配置，${APP_NAME} 已恢复运行。请用 singr log 查看失败原因${plain}"
    else
        echo -e "${red}回滚后仍未能启动，请立即用 singr log 排查${plain}"
    fi
    return 1
}

node_inbound_template() {
    case "$1" in
        anytls)
            echo '{"type":"anytls","tag":"","listen":"::","listen_port":0,"users":[],"tls":{"enabled":true,"server_name":"","certificate_path":"","key_path":""}}'
            ;;
        hysteria2)
            echo '{"type":"hysteria2","tag":"","listen":"::","listen_port":0,"users":[],"up_mbps":300,"down_mbps":300,"ignore_client_bandwidth":false,"obfs":{"type":"salamander","password":""},"tls":{"enabled":true,"server_name":"","certificate_path":"","key_path":""}}'
            ;;
        *) return 1 ;;
    esac
}

# 写 server.json：inbound / outbound / route 规则三处 upsert。
# inbound 模板优先从现有同协议 inbound 深拷贝，保住用户改过的 up_mbps、obfs 等；
# outbound 模板从现有 direct 出站拷贝，保住 domain_resolver（硬写会在用户删掉
# dns 块时引用到不存在的 server tag）。
node_write_server() {
    local tag="$1" proto="$2" sni="$3" cert="$4" key="$5" itmpl otmpl tmp
    itmpl="$(jq -c --arg p "${proto}" 'first(.inbounds[]? | select(.type==$p)) // empty' "${SERVER_CONFIG}" 2>/dev/null)"
    [[ -z "${itmpl}" ]] && itmpl="$(node_inbound_template "${proto}")"
    [[ -z "${itmpl}" ]] && return 1
    otmpl="$(jq -c 'first(.outbounds[]? | select(.type=="direct")) // {"type":"direct"}' "${SERVER_CONFIG}" 2>/dev/null)"
    [[ -z "${otmpl}" ]] && otmpl='{"type":"direct"}'

    tmp="$(mktemp)" || return 1
    jq --arg tag "${tag}" --arg out "${proto}-out" --arg sni "${sni}" \
        --arg cert "${cert}" --arg key "${key}" \
        --argjson itmpl "${itmpl}" --argjson otmpl "${otmpl}" '
        ($itmpl
            | .tag = $tag
            | .listen_port = 0
            | .users = []
            | .tls.enabled = true
            | .tls.server_name = $sni
            | .tls.certificate_path = $cert
            | .tls.key_path = $key) as $in
        | .inbounds = (((.inbounds // []) | map(select(.tag != $tag))) + [$in])
        | .outbounds = (if (((.outbounds // []) | map(.tag) | index($out)) != null)
                        then (.outbounds // [])
                        else (.outbounds // []) + [($otmpl | .tag = $out)] end)
        | .route.rules = (((.route.rules // []) | map(select((.inbound // "") != $tag)))
                        + [{inbound: $tag, outbound: $out}])
    ' "${SERVER_CONFIG}" >"${tmp}" && mv -f "${tmp}" "${SERVER_CONFIG}"
}

node_write_panel() {
    local tag="$1" proto="$2" api_url="$3" api_key="$4" node_id="$5" panel_type="$6" \
        node_type="$7" timeout="$8" speed_limit="$9" device_limit="${10}" \
        update_periodic="${11}" enable_device_limit="${12}" tmp
    tmp="$(mktemp)" || return 1
    jq --arg intag "${tag}" --arg outtag "${proto}-out" --arg paneltype "${panel_type}" \
        --arg apihost "${api_url}" --arg apikey "${api_key}" --arg nodetype "${node_type}" \
        --argjson nodeid "${node_id}" --argjson timeout "${timeout}" \
        --argjson speedlimit "${speed_limit}" --argjson devicelimit "${device_limit}" \
        --argjson updateperiodic "${update_periodic}" \
        --argjson enabledevicelimit "${enable_device_limit}" '
        {
            paneltype: $paneltype,
            intag: $intag,
            outtag: $outtag,
            apiconfig: {
                apihost: $apihost,
                apikey: $apikey,
                nodeid: $nodeid,
                nodetype: $nodetype,
                timeout: $timeout,
                speedlimit: $speedlimit,
                devicelimit: $devicelimit,
                disablecustomconfig: true
            },
            controllerconfig: {
                updateperiodic: $updateperiodic,
                enabledevicelimit: $enabledevicelimit
            }
        } as $node
        | .name = (.name // "SingR nodes")
        | .nodes = (((.nodes // []) | map(select((.intag // "") != $intag))) + [$node])
    ' "${PANEL_CONFIG}" >"${tmp}" && mv -f "${tmp}" "${PANEL_CONFIG}"
}

node_add() {
    node_require_jq || return 1
    node_configs_ready || return 1

    local api_url="" api_key="" node_id="" protocol="" sni="" cert_src="" key_src=""
    local cert_url="" key_url=""
    local panel_type="SSpanel" node_type="V2ray" timeout="20" speed_limit="0"
    local device_limit="0" enable_device_limit="false" update_periodic="60"

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --api-url) api_url="$2"; shift 2 ;;
            --api-key) api_key="$2"; shift 2 ;;
            --node-id) node_id="$2"; shift 2 ;;
            --protocol) protocol="$2"; shift 2 ;;
            --sni) sni="$2"; shift 2 ;;
            --cert-path) cert_src="$2"; shift 2 ;;
            --key-path) key_src="$2"; shift 2 ;;
            --cert-url) cert_url="$2"; shift 2 ;;
            --key-url) key_url="$2"; shift 2 ;;
            --panel-type) panel_type="$2"; shift 2 ;;
            --node-type) node_type="$2"; shift 2 ;;
            --timeout) timeout="$2"; shift 2 ;;
            --speed-limit) speed_limit="$2"; shift 2 ;;
            --device-limit) device_limit="$2"; shift 2 ;;
            --enable-device-limit) enable_device_limit="$2"; shift 2 ;;
            --update-periodic) update_periodic="$2"; shift 2 ;;
            *) echo -e "${red}未知参数：$1${plain}"; return 1 ;;
        esac
    done

    # 没给全且在交互终端上，就逐项问；非交互（脚本调用）则直接报缺参数。
    if [[ -t 0 ]]; then
        [[ -z "${api_url}" ]] && read -r -p "面板地址 (--api-url，如 https://panel.example.com): " api_url
        [[ -z "${api_key}" ]] && read -r -p "面板密钥 (--api-key): " api_key
        [[ -z "${node_id}" ]] && read -r -p "节点 ID  (--node-id): " node_id
        [[ -z "${protocol}" ]] && read -r -p "协议 anytls / hysteria2 (--protocol): " protocol
        [[ -z "${sni}" ]] && read -r -p "SNI 域名 (--sni，留空则由面板 host= 决定): " sni
        if [[ -z "${cert_src}" && -z "${key_src}" && -z "${cert_url}" && -z "${key_url}" ]]; then
            echo -e "${yellow}证书留空则使用默认路径 ${CERT_DIR}/default.pem 与 default.key${plain}"
            read -r -p "证书路径 (--cert-path，回车用默认): " cert_src
        fi
        # 只给了一对里的一个就把另一个问出来，别直接报错——命令行敲漏一个参数是常事。
        if [[ -n "${cert_src}" && -z "${key_src}" ]]; then
            read -r -p "私钥路径 (--key-path): " key_src
        elif [[ -z "${cert_src}" && -n "${key_src}" ]]; then
            read -r -p "证书路径 (--cert-path): " cert_src
        fi
    fi

    [[ -n "${api_url}" ]] || { echo -e "${red}缺少 --api-url${plain}"; return 1; }
    [[ -n "${api_key}" ]] || { echo -e "${red}缺少 --api-key${plain}"; return 1; }
    [[ "${node_id}" =~ ^[0-9]+$ ]] || { echo -e "${red}--node-id 必须是整数${plain}"; return 1; }
    [[ "${timeout}" =~ ^[0-9]+$ ]] || { echo -e "${red}--timeout 必须是整数${plain}"; return 1; }
    [[ "${device_limit}" =~ ^[0-9]+$ ]] || { echo -e "${red}--device-limit 必须是整数${plain}"; return 1; }
    [[ "${update_periodic}" =~ ^[0-9]+$ ]] || { echo -e "${red}--update-periodic 必须是整数${plain}"; return 1; }
    [[ "${speed_limit}" =~ ^[0-9]+([.][0-9]+)?$ ]] || { echo -e "${red}--speed-limit 必须是数字${plain}"; return 1; }
    case "${enable_device_limit}" in
        true | false) ;;
        *) echo -e "${red}--enable-device-limit 必须是 true / false${plain}"; return 1 ;;
    esac
    local proto
    proto="$(node_norm_proto "${protocol}")" || {
        echo -e "${red}--protocol 只能是 anytls / hysteria2${plain}"
        return 1
    }
    # 证书两个参数要么都给要么都不给：只给一个多半是敲漏了，别猜。都不给就把
    # server.json 的路径留空，交给二进制解析默认路径（见 node_default_cert_path）。
    if [[ -n "${cert_src}" && -z "${key_src}" ]] || [[ -z "${cert_src}" && -n "${key_src}" ]]; then
        echo -e "${red}--cert-path 与 --key-path 必须同时给出（都不给则使用默认路径）${plain}"
        return 1
    fi
    # --cert-url/--key-url 不是第三种证书模式，而是"顺手配置默认证书更新源"：证书下载到
    # 默认路径，本节点 TLS 留空走默认证书。所以和 --cert-path 互斥。
    if [[ -n "${cert_url}" && -z "${key_url}" ]] || [[ -z "${cert_url}" && -n "${key_url}" ]]; then
        echo -e "${red}--cert-url 与 --key-url 必须同时给出${plain}"
        return 1
    fi
    if [[ -n "${cert_src}" && -n "${cert_url}" ]]; then
        echo -e "${red}--cert-path 与 --cert-url 只能二选一${plain}"
        return 1
    fi
    # 路径原样写进 server.json，由后端进程解析：systemd 的工作目录是 /，容器里更是
    # 另一个命名空间，相对路径到那边就指错了。这里按当前目录补成绝对路径，但不解析
    # 符号链接 —— certbot 的 live/ 软链要保留原样，续期换的正是软链。
    [[ -n "${cert_src}" && "${cert_src}" != /* ]] && cert_src="${PWD}/${cert_src}"
    [[ -n "${key_src}" && "${key_src}" != /* ]] && key_src="${PWD}/${key_src}"

    if jq -e --arg h "${api_url}" --argjson id "${node_id}" \
        '[(.nodes // [])[] | select((.apiconfig.apihost // "") == $h and (.apiconfig.nodeid // -1) == $id)] | length > 0' \
        "${PANEL_CONFIG}" >/dev/null 2>&1; then
        echo -e "${red}该节点已存在：${api_url} #${node_id}${plain}"
        echo -e "${yellow}如需改配置，先 singr del ${node_id} 再重新添加${plain}"
        return 1
    fi

    if [[ "${proto}" == "hysteria2" && -z "${sni}" ]]; then
        echo -e "${yellow}提示：hysteria2 默认开着 salamander obfs 且密码留空，此时密钥取自 SNI。"
        echo -e "      SNI 也为空会让 obfs 静默关闭（配置显示开着，但实际没生效），"
        echo -e "      客户端按文档配了 obfs 反而连不上。建议指定 --sni。${plain}"
    fi

    if node_has_placeholder; then
        echo -e "${yellow}检测到占位节点（your-sspanel.example.com），将其移除后再添加${plain}"
        node_drop_placeholder || { echo -e "${red}清理占位节点失败${plain}"; return 1; }
    fi

    local tag paths cert_path key_path eff_cert eff_key
    tag="$(node_alloc_tag "${proto}" "${node_id}")"
    if [[ -n "${cert_src}" ]]; then
        paths="$(node_backend_place_cert "${tag}" "${cert_src}" "${key_src}")" || return 1
        cert_path="${paths%%|*}"
        key_path="${paths##*|}"
        eff_cert="${cert_path}"
        eff_key="${key_path}"
    else
        # 写空值，由二进制在启动时补成默认路径。
        cert_path=""
        key_path=""
        if [[ -n "${cert_url}" ]]; then
            # 更新源是全局的（只有一张默认证书）：已配了同一个就沿用，配了别的就拒绝
            # —— 不能因为加一个节点就悄悄换掉其他节点正在用的证书。
            if [[ -s "${CERT_SOURCE}" ]]; then
                if [[ "$(cert_source_get cert_url)" == "${cert_url}" && "$(cert_source_get key_url)" == "${key_url}" ]]; then
                    echo -e "${green}沿用已配置的默认证书更新源${plain}"
                else
                    echo -e "${red}已配置了另一个默认证书更新源：$(cert_url_mask "$(cert_source_get cert_url)")${plain}"
                    echo -e "${yellow}默认证书只有一张，更新源是全局的。要更换请用 singr cert-source。${plain}"
                    return 1
                fi
            else
                cert_source_set "${cert_url}" "${key_url}" || return 1
            fi
        fi
        eff_cert="$(node_default_cert_path)"
        eff_key="$(node_default_key_path)"
    fi

    # 启动前预检。anytls 与 hysteria2 都强制 TLS，缺证书的 inbound 建不起来，而
    # 任一节点 Start() 失败会让 poet/panel/panel.go 走 os.Exit(1)，把同进程的其他
    # 节点一起带下线。在这里拒绝，比落盘后靠 node_apply 回滚干净得多。
    if [[ ! -s "${eff_cert}" || ! -s "${eff_key}" ]]; then
        echo -e "${red}找不到证书：${eff_cert} 与 ${eff_key}${plain}"
        echo -e "${yellow}没有证书进程起不来。放好证书后重试：${plain}"
        echo -e "  cp 证书 ${CERT_DIR}/default.pem"
        echo -e "  cp 私钥 ${CERT_DIR}/default.key"
        echo -e "${yellow}（.crt 后缀也认；或用 --cert-path/--key-path 指定其他路径，"
        echo -e "  或用 --cert-url/--key-url 从远程地址下载并每日检查更新）${plain}"
        return 1
    fi

    node_backup || { echo -e "${red}备份配置失败，已中止${plain}"; return 1; }
    if ! node_write_server "${tag}" "${proto}" "${sni}" "${cert_path}" "${key_path}"; then
        echo -e "${red}写入 ${SERVER_CONFIG} 失败${plain}"
        node_restore
        return 1
    fi
    if ! node_write_panel "${tag}" "${proto}" "${api_url}" "${api_key}" "${node_id}" \
        "${panel_type}" "${node_type}" "${timeout}" "${speed_limit}" "${device_limit}" \
        "${update_periodic}" "${enable_device_limit}"; then
        echo -e "${red}写入 ${PANEL_CONFIG} 失败${plain}"
        node_restore
        return 1
    fi

    echo -e "${green}已添加节点：${api_url} #${node_id}（${proto}，InTag=${tag}）${plain}"
    if node_apply; then
        echo
        node_list
        echo
        echo -e "${yellow}提示：同一台机上的多个节点必须在面板侧配置不同端口，否则第二个监听会起不来。${plain}"
        node_cert_renew_hint
        return 0
    fi
    return 1
}

# 把 <@序号|NodeID|InTag> 解析成唯一的 intag。stdout 只回显 intag，其余输出走 stderr。
#
# 三种选择器：
#   @N      singr list 里的 # 列序号，永不歧义，最省事。shell 里 # 会被当成注释
#           起始（`singr del #1` 会把 #1 整个吞掉），所以主推 @N；'#N' 加引号也认。
#   InTag   唯一，由 node_alloc_tag 保证。
#   NodeID  **在多面板场景下不唯一**：两个不同面板各有一个 56 号节点很正常。
#
# NodeID 命中多个时绝不能像 jq 的 first(...) 那样静默挑一个 —— 对 del 来说那是
# 删错节点。这里直接报错，并把候选连同序号、面板域名列出来，让用户用 @N 重来。
node_resolve_tag() {
    local sel="$1" matches n idx tag

    if [[ "${sel}" =~ ^[@#]([0-9]+)$ ]]; then
        idx="${BASH_REMATCH[1]}"
        if [[ "${idx}" -lt 1 ]]; then
            echo -e "${red}序号从 1 开始${plain}" >&2
            return 1
        fi
        tag="$(jq -r --argjson i "$((idx - 1))" '((.nodes // [])[$i] // {}) | (.intag // "")' "${PANEL_CONFIG}" 2>/dev/null)"
        if [[ -z "${tag}" ]]; then
            echo -e "${red}序号 ${idx} 超出范围（当前 $(node_count) 个节点，用 singr list 查看）${plain}" >&2
            return 1
        fi
        printf '%s' "${tag}"
        return 0
    fi

    # 输出 "序号<TAB>intag<TAB>域名"，冲突时好直接列给用户看。
    matches="$(jq -r --arg s "${sel}" '
        (.nodes // []) | to_entries[]
        | select(((.value.apiconfig.nodeid // "") | tostring) == $s or (.value.intag // "") == $s)
        | [((.key + 1) | tostring), (.value.intag // ""), (.value.apiconfig.apihost // "")] | @tsv
    ' "${PANEL_CONFIG}" 2>/dev/null)"
    n="$(printf '%s\n' "${matches}" | grep -c .)"

    if [[ "${n}" -eq 0 ]]; then
        echo -e "${red}未找到节点：${sel}${plain}" >&2
        echo -e "${yellow}用 singr list 查看现有节点，可用 @序号 / NodeID / InTag 指定。${plain}" >&2
        return 1
    fi
    if [[ "${n}" -gt 1 ]]; then
        echo -e "${red}「${sel}」同时命中 ${n} 个节点（NodeID 在多面板下会重复），无法确定是哪一个：${plain}" >&2
        local i t h
        while IFS=$'\t' read -r i t h; do
            [[ -z "${t}" ]] && continue
            printf '  @%-4s %-22s %s\n' "${i}" "${t}" "${h}" >&2
        done <<<"${matches}"
        echo -e "${yellow}请改用上面的 @序号（如 @${matches%%$'\t'*}）或 InTag 精确指定。${plain}" >&2
        return 1
    fi
    printf '%s' "$(printf '%s' "${matches}" | cut -f2)"
}

node_del() {
    node_require_jq || return 1
    node_configs_ready || return 1

    local sel="${1:-}"
    if [[ -z "${sel}" ]]; then
        node_list || return 1
        echo
        read -r -p "输入要删除的 @序号 / NodeID / InTag（回车取消）: " sel
        [[ -z "${sel}" ]] && return 0
    fi

    local tag
    tag="$(node_resolve_tag "${sel}")" || return 1

    if [[ "$(node_count)" == "1" ]]; then
        echo -e "${red}这是最后一个节点。删掉之后 panel.json 将没有任何节点，进程会以"
        echo -e "'no Node with InTag found' 退出。${plain}"
        echo -e "${yellow}要换节点请先 singr add 新节点、再删旧的；要彻底移除请用 singr uninstall。${plain}"
        return 1
    fi

    confirm "确定删除节点 ${sel}（InTag=${tag}）吗" "n" || return 0

    node_backup || { echo -e "${red}备份配置失败，已中止${plain}"; return 1; }
    local tmp
    tmp="$(mktemp)" || return 1
    jq --arg t "${tag}" '.nodes = ((.nodes // []) | map(select((.intag // "") != $t)))' \
        "${PANEL_CONFIG}" >"${tmp}" && mv -f "${tmp}" "${PANEL_CONFIG}" || {
        echo -e "${red}写入 ${PANEL_CONFIG} 失败${plain}"
        node_restore
        return 1
    }
    tmp="$(mktemp)" || return 1
    jq --arg t "${tag}" '
        .inbounds = ((.inbounds // []) | map(select((.tag // "") != $t)))
        | .route.rules = ((.route.rules // []) | map(select((.inbound // "") != $t)))
    ' "${SERVER_CONFIG}" >"${tmp}" && mv -f "${tmp}" "${SERVER_CONFIG}" || {
        echo -e "${red}写入 ${SERVER_CONFIG} 失败${plain}"
        node_restore
        return 1
    }

    echo -e "${green}已删除节点 ${sel}（InTag=${tag}）${plain}"
    if node_apply; then
        echo
        node_list
        return 0
    fi
    return 1
}

node_menu() {
    while true; do
        echo -e "
  ${green}节点管理${plain}
  1. 查看节点
  2. 添加节点
  3. 删除节点
  0. 返回主菜单
"
        read -r -p "请输入选择 [0-3]: " nm
        case "${nm}" in
            1) node_list ;;
            2) node_add ;;
            3) node_del ;;
            0) return ;;
            *) echo -e "${red}请输入正确的数字 [0-3]${plain}" ;;
        esac
    done
}
# <<<<<<<<<<<<<<<< SYNC BLOCK: 节点管理 <<<<<<<<<<<<<<<<

# ---- 节点管理：docker 后端实现（SYNC BLOCK 的钩子 + 证书挂载）----
#
# 证书和裸机一样原地引用：server.json 里写的就是宿主机上的真实路径，容器靠"同路径
# 只读挂载"看到它们。要挂哪些目录由 cert_mount_dirs 从 server.json 现算，不另存一份
# 登记；container_ensure 发现挂载集合变了就重建容器（挂载在 docker run 时就定死了，
# docker restart 改不了）。
#
# 只挂目录，不挂文件：certbot 之类都是 rename 原子替换，文件级 bind mount 钉住的是
# 旧 inode，容器里永远读不到新证书。挂目录则续期后容器内立刻可见，二进制自己监视
# 证书文件（common/tls/std_server.go 的 fswatch）并重新加载，不用重启。
#
# 旧版本是"复制进挂载目录 + certs.json 登记源 + certs_sync 每次重启前重新复制"，
# 前提是"证书不热重载，反正要重启"——这个前提不成立。certs_migrate 负责把旧机器
# 迁过来。

CERTS_MAP="${CONFIG_DIR}/certs.json"   # 旧版本的证书源登记，只有 certs_migrate 还认它
CERT_MIGRATION_NOTICE="${CONFIG_DIR}/cert-migration-notice.txt"

# 目录 d 能否同路径挂进容器。不能时把原因回显到 stdout 并返回 0（"有问题"）。
#   · `:` 和 `,` 是 --mount 的分隔符，没法转义。
#   · 系统目录：同路径挂载会把容器里的同名目录整个盖掉 —— /etc 盖掉就没了 CA 证书，
#     /usr 盖掉连二进制都没了，/tmp 只读会让 entrypoint 的 mktemp 失败。
cert_mount_problem() {
    local d="$1"
    case "${d}" in
        *:* | *,*)
            printf '%s' "${d} 含 : 或 ,，docker --mount 无法表达"
            return 0
            ;;
        / | /bin | /boot | /dev | /etc | /lib | /lib32 | /lib64 | /libx32 | /opt | /proc | /run | /sbin | /sys | /tmp | /usr | /var | \
            /bin/* | /dev/* | /lib/* | /lib64/* | /proc/* | /run/* | /sbin/* | /sys/* | /usr/* | /opt/singr | /opt/singr/*)
            printf '%s' "${d} 是系统目录，同路径挂载会盖掉容器里的同名目录；请把证书放进单独的子目录"
            return 0
            ;;
    esac
    return 1
}

# 一个证书文件要挂的目录：它所在的目录，加上解析符号链接后真实文件所在的目录。
# certbot 的 live/<域名>/fullchain.pem 是指向 ../../archive/<域名>/ 的软链，只挂
# live/<域名> 容器里就是断链。落在 ${CONFIG_DIR} 下的已经整体挂载，不再输出。
cert_dirs_for() {
    local p="$1" d real=""
    [[ -e "${p}" ]] && real="$(readlink -f "${p}" 2>/dev/null)"
    for d in "$(dirname "${p}")" "${real:+$(dirname "${real}")}"; do
        [[ -n "${d}" ]] || continue
        case "${d}/" in "${CONFIG_DIR}/"*) continue ;; esac
        printf '%s\n' "${d}"
    done
}

# 添加节点 / 首次安装时的预检：0 = 这个证书路径挂得进容器。不行时原因打到 stderr。
cert_path_mountable() {
    local p="$1" d why
    if [[ "${p}" != /* ]]; then
        echo -e "${red}证书路径必须是绝对路径：${p}${plain}" >&2
        return 1
    fi
    while IFS= read -r d; do
        [[ -n "${d}" ]] || continue
        if why="$(cert_mount_problem "${d}")"; then
            echo -e "${red}无法把证书目录挂进容器：${why}${plain}" >&2
            return 1
        fi
    done < <(cert_dirs_for "${p}")
    return 0
}

# 容器需要的证书挂载目录，每行一个：排序去重，并去掉已被其他目录包含的子目录。
#
# 来源：server.json 存在就只看它（含 TLS 为空、走默认证书的 inbound —— default.pem
# 可以是指向别处的软链）；还不存在（首次安装、entrypoint 尚未生成配置）就看 RUN_FLAGS
# 里交给 entrypoint 的 --cert-path / --key-path。两者不能取并集：RUN_FLAGS 永久留在
# docker.conf 里，用户事后换了证书、删了旧目录，--mount 遇到不存在的源会直接拒绝
# 建容器。同理，不存在的目录不挂，交给 entrypoint 报出"缺证书：<路径>"。
cert_mount_dirs() {
    local -a paths=()
    local p d why tag pair i
    if [[ -f "${SERVER_CONFIG}" ]]; then
        if ! command -v jq >/dev/null 2>&1; then
            echo -e "${yellow}[证书] 未安装 jq，无法计算证书挂载${plain}" >&2
            return 0
        fi
        while IFS= read -r tag; do
            [[ -n "${tag}" ]] || continue
            pair="$(node_effective_cert "${tag}")"
            paths+=("${pair%%|*}" "${pair##*|}")
        done < <(jq -r '.inbounds[]? | select(.tls.enabled == true) | (.tag // empty)' "${SERVER_CONFIG}" 2>/dev/null)
    else
        for ((i = 0; i < ${#RUN_FLAGS[@]}; i++)); do
            case "${RUN_FLAGS[i]}" in
                --cert-path | --key-path) paths+=("${RUN_FLAGS[i + 1]:-}") ;;
            esac
        done
    fi
    for p in "${paths[@]}"; do
        [[ -n "${p}" ]] || continue
        if [[ "${p}" != /* ]]; then
            echo -e "${yellow}[证书] 不是绝对路径，容器内无法解析：${p}${plain}" >&2
            continue
        fi
        while IFS= read -r d; do
            [[ -n "${d}" && -d "${d}" ]] || continue
            if why="$(cert_mount_problem "${d}")"; then
                echo -e "${yellow}[证书] 跳过挂载：${why}${plain}" >&2
                continue
            fi
            printf '%s\n' "${d}"
        done < <(cert_dirs_for "${p}")
    done | LC_ALL=C sort -u |
        awk '{ for (k in kept) if (index($0, kept[k] "/") == 1) next; kept[NR] = $0; print }'
}

# 容器当前的证书挂载（不含整体挂载的 ${CONFIG_DIR}），格式同 cert_mount_dirs。
container_cert_mounts() {
    docker inspect -f '{{range .Mounts}}{{if eq .Type "bind"}}{{println .Source}}{{end}}{{end}}' "${CONTAINER}" 2>/dev/null |
        grep -vxF "${CONFIG_DIR}" | grep -v '^$' | LC_ALL=C sort -u
}

# 让容器跑在当前配置上：挂载没变就 docker restart / start，变了（或容器不存在）就
# 重建。start / restart / 节点增删都走这里，所以 singr config、singr add 改了证书
# 路径之后，下一次重启自然带上新挂载。
container_ensure() {
    if container_exists && [[ "$(cert_mount_dirs 2>/dev/null)" == "$(container_cert_mounts)" ]]; then
        if container_running; then
            docker restart "${CONTAINER}" >/dev/null
        else
            docker start "${CONTAINER}" >/dev/null
        fi
    else
        container_recreate >/dev/null
    fi
}

node_backend_place_cert() {
    local cert_src="$2" key_src="$3" p
    for p in "${cert_src}" "${key_src}"; do
        [[ -s "${p}" ]] || {
            echo -e "${red}找不到证书文件：${p}${plain}" >&2
            return 1
        }
        cert_path_mountable "${p}" || return 1
    done
    printf '%s|%s' "${cert_src}" "${key_src}"
}

node_backend_restart() {
    container_ensure || true
}

node_backend_verify() { verify_running; }

node_cert_renew_hint() {
    echo -e "${yellow}证书续期提示：容器按原路径挂载证书所在目录，续期（certbot renew 等）后进程会"
    echo -e "自动重新加载，无需重启，也不需要 --deploy-hook（default.pem 软链到 certbot 的除外，"
    echo -e "那种情况监视不到 live/ 里的变化，仍需 --deploy-hook \"singr restart\"）。默认证书可以用"
    echo -e "  singr cert-source"
    echo -e "配置远程更新源，每天检查一次、剩余不足 7 天时自动下载。${plain}"
}

# 把旧版本（复制模型）的机器迁到原地引用。certs.json 存在才跑，跑完改名，所以是一次
# 性的。调用点在脚本入口（见末尾分发），任何一条 singr 命令都会触发 —— 包括 certbot
# 钩子里残留的 `singr cert-sync`。只挂在 start/restart 上的话，升级了脚本却没重启的
# 机器会一直用着不再同步的旧副本，直到证书过期。
#
# 逐条处理 {"<intag>": {"cert": 源, "key": 源}}：
#   · inbound 已不存在            → 残留登记，丢弃
#   · inbound 用默认证书（TLS 为空）→ 丢弃登记。default.pem 只剩一份不再同步的旧副本，
#                                    记进提示文件
#   · 显式路径指向挂载目录里的副本 → 改写回源路径，副本移进 certs/.migrated-<时间>/
#   · 源已不存在 / 源目录挂不进容器 → 保留副本和现有路径，记进提示文件
# 有改写且容器在运行，就重建并校验；起不来则还原 server.json、按旧挂载重建，保留
# certs.json，下次再试。
certs_migrate() {
    [[ -f "${CERTS_MAP}" ]] || return 0
    command -v jq >/dev/null 2>&1 || return 0
    command -v docker >/dev/null 2>&1 || return 0

    local stamp notices="" rewritten=0 tag src_cert src_key cur_cert cur_key tmp c
    local -a copies=()
    stamp="$(date +%Y%m%d%H%M%S)"
    echo -e "${yellow}[证书] 检测到旧版本的证书登记 ${CERTS_MAP}，迁移为原地引用（容器直接挂载证书所在目录）...${plain}"

    if [[ -f "${SERVER_CONFIG}" ]]; then
        cp -f "${SERVER_CONFIG}" "${SERVER_CONFIG}.migrate-bak" || return 0
        while IFS=$'\t' read -r tag src_cert src_key; do
            [[ -n "${tag}" ]] || continue
            node_inbound_exists "${tag}" || continue
            cur_cert="$(jq -r --arg t "${tag}" '(first(.inbounds[]? | select(.tag==$t) | .tls.certificate_path)) // ""' "${SERVER_CONFIG}" 2>/dev/null)"
            cur_key="$(jq -r --arg t "${tag}" '(first(.inbounds[]? | select(.tag==$t) | .tls.key_path)) // ""' "${SERVER_CONFIG}" 2>/dev/null)"
            if [[ -z "${cur_cert}" && -z "${cur_key}" ]]; then
                notices+="${tag}: 使用默认证书 $(node_default_cert_path)，它是 ${src_cert} 的旧副本，今后不再自动同步。
    处理办法：singr cert-source 配置默认证书更新源；或用 singr config 把该 inbound 的
    certificate_path / key_path 写成 ${src_cert} / ${src_key} 后重启。
"
                continue
            fi
            case "${cur_cert}" in "${CONFIG_DIR}"/*) ;; *) continue ;; esac
            [[ "${src_cert}" == "${cur_cert}" ]] && continue
            if [[ ! -s "${src_cert}" || ! -s "${src_key}" ]]; then
                notices+="${tag}: 证书源 ${src_cert} 已不存在，继续使用副本 ${cur_cert}，今后不再自动同步。
"
                continue
            fi
            if ! cert_path_mountable "${src_cert}" 2>/dev/null || ! cert_path_mountable "${src_key}" 2>/dev/null; then
                notices+="${tag}: 证书源 ${src_cert} 所在目录挂不进容器，继续使用副本 ${cur_cert}，今后不再自动同步。
"
                continue
            fi
            tmp="$(mktemp)" || continue
            if jq --arg t "${tag}" --arg c "${src_cert}" --arg k "${src_key}" '
                .inbounds |= map(if .tag == $t then (.tls.certificate_path = $c | .tls.key_path = $k) else . end)
            ' "${SERVER_CONFIG}" >"${tmp}" && mv -f "${tmp}" "${SERVER_CONFIG}"; then
                rewritten=1
                copies+=("${cur_cert}" "${cur_key}")
                echo -e "${green}[证书] ${tag}: 改为直接引用 ${src_cert}${plain}"
            else
                rm -f "${tmp}"
            fi
        done < <(jq -r 'to_entries[] | [.key, (.value.cert // ""), (.value.key // "")] | @tsv' "${CERTS_MAP}" 2>/dev/null)
    fi

    if [[ "${rewritten}" == 1 ]] && container_running; then
        container_recreate >/dev/null
        if ! verify_running; then
            echo -e "${red}[证书] 迁移后容器未能正常启动，已还原 server.json；${CERTS_MAP} 保留，下次执行 singr 命令时重试${plain}"
            mv -f "${SERVER_CONFIG}.migrate-bak" "${SERVER_CONFIG}"
            container_recreate >/dev/null
            verify_running || echo -e "${red}[证书] 还原后仍未能启动，请立即用 singr log 排查${plain}"
            return 1
        fi
    fi
    rm -f "${SERVER_CONFIG}.migrate-bak"

    # 不再被任何 inbound 引用的旧副本收起来（不删：里面有私钥，留给运维决定）。
    for c in "${copies[@]}"; do
        [[ -f "${c}" ]] || continue
        jq -e --arg p "${c}" '[.inbounds[]?.tls | (.certificate_path // ""), (.key_path // "")] | index($p) != null' \
            "${SERVER_CONFIG}" >/dev/null 2>&1 && continue
        mkdir -m 700 -p "${CERT_DIR}/.migrated-${stamp}" && mv -f "${c}" "${CERT_DIR}/.migrated-${stamp}/"
    done
    mv -f "${CERTS_MAP}" "${CERTS_MAP}.migrated-${stamp}"

    if [[ -n "${notices}" ]]; then
        printf '# %s 从复制模型迁移时，以下证书没法自动续期。处理完删掉本文件。\n%s\n' \
            "$(date '+%F %T')" "${notices}" >>"${CERT_MIGRATION_NOTICE}"
        echo -e "${yellow}[证书] 以下证书迁移后不再自动续期（已记入 ${CERT_MIGRATION_NOTICE}）：${plain}"
        printf '%s' "${notices}"
    fi
    echo -e "${green}[证书] 迁移完成，不再需要 singr cert-sync / certbot --deploy-hook${plain}"
    return 0
}

# >>>>>>>>>>>>>>>> SYNC BLOCK: 默认证书更新源 >>>>>>>>>>>>>>>>
# 本块在 SingR.sh 与 SingR-docker.sh 中逐字相同，改一处必须同步另一处。
#
# 只管"默认证书"：server.json 里 certificate_path 与 key_path 都为空的 inbound 用的
# ${CERT_DIR}/default.pem（或 .crt）+ default.key。显式写了证书路径的节点一概不碰。
#
# 证书和私钥分别从两个 https 地址下载。地址存在 ${CERT_SOURCE}（0600：地址里常带
# 鉴权 token），只在这里存一份。systemd timer 每天跑一次 singr cert-update：本地证书
# 剩余不足 CERT_RENEW_DAYS 天才去下载，远端比本地新才替换。
#
# 只换文件，从不重启。进程监视证书所在目录（common/tls/std_server.go 的 fswatch），
# 文件一换就自动重新加载；新证书加载失败时进程继续用内存里的旧证书。重启则会让同一
# 进程里用别的证书的节点也一起断线 —— 默认证书的事不该牵连它们。
#
# 依赖节点管理块里的 node_default_cert_path / node_cert_end_epoch / node_cert_days_left。

CERT_SOURCE="${CONFIG_DIR}/cert-source.json"
CERT_TIMER="singr-cert-update"
CERT_TIMER_DIR="${CERT_TIMER_DIR:-/etc/systemd/system}"
CERT_RENEW_DAYS=7

cert_source_get() {
    jq -r --arg k "$1" '.[$k] // ""' "${CERT_SOURCE}" 2>/dev/null
}

# 显示用：藏掉 query（?token=...）和 userinfo（user:pass@）。
cert_url_mask() {
    local u="$1" scheme rest
    [[ "${u}" == *://* ]] || { printf '%s' "${u}"; return; }
    scheme="${u%%://*}"
    rest="${u#*://}"
    [[ "${rest%%/*}" == *@* ]] && rest="***@${rest#*@}"
    [[ "${rest}" == *\?* ]] && rest="${rest%%\?*}?***"
    printf '%s://%s' "${scheme}" "${rest}"
}

# 私钥要走这条链路，只接受 https。--proto =https 连重定向到 http 也一并拒绝。
cert_source_fetch() {
    curl --proto '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 60 --retry 2 \
        -o "$2" "$1"
}

# 证书与私钥是否成对可用。不可用时把原因打到 stdout 并返回 1。
# -passin pass: 让加密私钥直接失败，而不是在 timer 里卡住等人输口令。
cert_check_pair() {
    local c="$1" k="$2" cpub kpub
    openssl x509 -noout -in "${c}" >/dev/null 2>&1 || { echo "证书不是有效的 PEM 证书"; return 1; }
    openssl pkey -in "${k}" -passin pass: -noout >/dev/null 2>&1 || { echo "私钥无效（或带口令加密）"; return 1; }
    openssl x509 -checkend 0 -noout -in "${c}" >/dev/null 2>&1 || { echo "证书已过期"; return 1; }
    cpub="$(openssl x509 -in "${c}" -noout -pubkey 2>/dev/null)"
    kpub="$(openssl pkey -in "${k}" -passin pass: -pubout 2>/dev/null)"
    [[ -n "${cpub}" && "${cpub}" == "${kpub}" ]] || { echo "证书与私钥不匹配"; return 1; }
    return 0
}

cert_fingerprint() {
    openssl x509 -noout -fingerprint -sha256 -in "$1" 2>/dev/null
}

# 正在使用默认证书的节点：每行 "<intag>\t<server_name>"。
cert_default_inbounds() {
    [[ -f "${PANEL_CONFIG}" && -f "${SERVER_CONFIG}" ]] || return 0
    jq -r --slurpfile s "${SERVER_CONFIG}" '
        (.nodes // [])[] | (.intag // "") as $t | select($t != "")
        | ($s[0].inbounds // [])[] | select(.tag == $t)
        | select(((.tls.certificate_path // "") == "") and ((.tls.key_path // "") == ""))
        | [$t, (.tls.server_name // "")] | @tsv' "${PANEL_CONFIG}" 2>/dev/null
}

# 新证书没覆盖到的 SNI 只警告不拦：SNI 也可能由面板 host= 下发，server.json 里看不到。
# 只认明确的 "does NOT match"；openssl 不支持 -checkhost（LibreSSL）时判定不了，不报。
cert_warn_sni() {
    local c="$1" tag sni
    while IFS=$'\t' read -r tag sni; do
        [[ -n "${sni}" ]] || continue
        if openssl x509 -noout -in "${c}" -checkhost "${sni}" 2>/dev/null | grep -q 'does NOT match'; then
            echo -e "${yellow}[证书] 注意：新证书不包含节点 ${tag} 的 SNI ${sni}，客户端校验会失败${plain}"
        fi
    done < <(cert_default_inbounds)
}

# 下载、校验并换上默认证书（只换文件，见块头）。
#   mode  auto  —— 定时任务：本地剩余不足 CERT_RENEW_DAYS 天才下载，且远端须比本地新
#         force —— 立即下载，只要和本地不是同一张就替换（首次配置、手动更新）
cert_default_update() {
    local mode="$1" cert_url="$2" key_url="$3"
    local cur_cert cur_key new_cert new_key why days cur_end new_end
    command -v openssl >/dev/null 2>&1 || { echo -e "${red}[证书] 需要 openssl${plain}"; return 1; }
    command -v curl >/dev/null 2>&1 || { echo -e "${red}[证书] 需要 curl${plain}"; return 1; }

    # 定时任务与手动执行撞在一起时，后来的直接退出。
    if command -v flock >/dev/null 2>&1; then
        exec 9>"${CONFIG_DIR}/.cert-update.lock"
        flock -n 9 || { echo -e "${yellow}[证书] 另一个证书更新正在进行，跳过${plain}"; return 0; }
    fi

    cur_cert="$(node_default_cert_path)"
    cur_key="$(node_default_key_path)"
    if [[ "${mode}" == "auto" && -s "${cur_cert}" ]] &&
        openssl x509 -checkend $((CERT_RENEW_DAYS * 86400)) -noout -in "${cur_cert}" >/dev/null 2>&1; then
        echo -e "${green}[证书] 默认证书剩余 $(node_cert_days_left "${cur_cert}") 天，未到 ${CERT_RENEW_DAYS} 天更新线，无需下载${plain}"
        return 0
    fi

    [[ -d "${CERT_DIR}" ]] || mkdir -m 700 -p "${CERT_DIR}" || return 1
    # 下到同目录的临时文件，换上去才是同一文件系统内的 rename。
    new_cert="$(mktemp "${CERT_DIR}/.download.XXXXXX")" || return 1
    new_key="$(mktemp "${CERT_DIR}/.download.XXXXXX")" || { rm -f "${new_cert}"; return 1; }
    if ! cert_source_fetch "${cert_url}" "${new_cert}"; then
        rm -f "${new_cert}" "${new_key}"
        echo -e "${red}[证书] 下载证书失败：$(cert_url_mask "${cert_url}")，现有证书未改动${plain}"
        return 1
    fi
    if ! cert_source_fetch "${key_url}" "${new_key}"; then
        rm -f "${new_cert}" "${new_key}"
        echo -e "${red}[证书] 下载私钥失败：$(cert_url_mask "${key_url}")，现有证书未改动${plain}"
        return 1
    fi
    if ! why="$(cert_check_pair "${new_cert}" "${new_key}")"; then
        rm -f "${new_cert}" "${new_key}"
        echo -e "${red}[证书] 下载到的${why}，现有证书未改动${plain}"
        return 1
    fi

    if [[ -s "${cur_cert}" ]]; then
        if [[ "$(cert_fingerprint "${new_cert}")" == "$(cert_fingerprint "${cur_cert}")" ]]; then
            rm -f "${new_cert}" "${new_key}"
            if [[ "${mode}" == "auto" ]]; then
                echo -e "${yellow}[证书] 默认证书剩余 $(node_cert_days_left "${cur_cert}") 天，但远端还是同一张证书，远端尚未续期${plain}"
                return 1
            fi
            echo -e "${green}[证书] 远端证书与本地相同，无需替换${plain}"
            return 0
        fi
        if [[ "${mode}" == "auto" ]]; then
            cur_end="$(node_cert_end_epoch "${cur_cert}")"
            new_end="$(node_cert_end_epoch "${new_cert}")"
            if [[ -n "${cur_end}" && -n "${new_end}" && "${new_end}" -le "${cur_end}" ]]; then
                rm -f "${new_cert}" "${new_key}"
                echo -e "${yellow}[证书] 远端证书不比本地新，不替换（远端尚未续期？）${plain}"
                return 1
            fi
        fi
    fi

    days="$(node_cert_days_left "${new_cert}")"
    if [[ -n "${days}" && "${days}" -lt "${CERT_RENEW_DAYS}" ]]; then
        echo -e "${yellow}[证书] 注意：远端证书只剩 ${days} 天，远端该续期了${plain}"
    fi
    cert_warn_sni "${new_cert}"

    # 留一份旧证书：私钥换上了、证书却没换成时要换回去，否则磁盘上是不配对的一对，
    # 下次启动直接起不来。
    local have_bak=0
    if [[ -s "${cur_cert}" && -s "${cur_key}" ]]; then
        cp -p "${cur_cert}" "${CERT_DIR}/.default-prev.cert" &&
            cp -p "${cur_key}" "${CERT_DIR}/.default-prev.key" && have_bak=1
    fi
    chmod 644 "${new_cert}"
    chmod 600 "${new_key}"
    if ! mv -f "${new_key}" "${cur_key}" || ! mv -f "${new_cert}" "${cur_cert}"; then
        rm -f "${new_cert}" "${new_key}"
        [[ "${have_bak}" == 1 ]] && mv -f "${CERT_DIR}/.default-prev.cert" "${cur_cert}" &&
            mv -f "${CERT_DIR}/.default-prev.key" "${cur_key}"
        echo -e "${red}[证书] 写入 ${cur_cert} 失败，已保留原证书${plain}"
        return 1
    fi
    echo -e "${green}[证书] 已更新默认证书 ${cur_cert}（剩余 $(node_cert_days_left "${cur_cert}") 天），运行中的进程会自动重新加载，无需重启${plain}"
    rm -f "${CERT_DIR}/.default-prev.cert" "${CERT_DIR}/.default-prev.key"
    return 0
}

cert_timer_install() {
    if ! command -v systemctl >/dev/null 2>&1; then
        echo -e "${yellow}[证书] 未检测到 systemd，无法安装每日检查。请自行每天执行一次：${SELF_CMD} cert-update${plain}"
        return 0
    fi
    cat >"${CERT_TIMER_DIR}/${CERT_TIMER}.service" <<EOF
[Unit]
Description=SingR default certificate update check
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=${SELF_CMD} cert-update
EOF
    cat >"${CERT_TIMER_DIR}/${CERT_TIMER}.timer" <<EOF
[Unit]
Description=Daily SingR default certificate update check

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
EOF
    systemctl daemon-reload 2>/dev/null || true
    systemctl enable --now "${CERT_TIMER}.timer" >/dev/null 2>&1 ||
        echo -e "${yellow}[证书] 启用 ${CERT_TIMER}.timer 失败，请检查 systemctl status ${CERT_TIMER}.timer${plain}"
}

cert_timer_remove() {
    command -v systemctl >/dev/null 2>&1 || return 0
    [[ -f "${CERT_TIMER_DIR}/${CERT_TIMER}.timer" ]] || return 0
    systemctl disable --now "${CERT_TIMER}.timer" >/dev/null 2>&1 || true
    rm -f "${CERT_TIMER_DIR}/${CERT_TIMER}.timer" "${CERT_TIMER_DIR}/${CERT_TIMER}.service"
    systemctl daemon-reload 2>/dev/null || true
}

# 配置（或更换）默认证书更新源并立即下载一次。下载和校验都通过才写配置；失败什么
# 都不改。
cert_source_set() {
    local cert_url="$1" key_url="$2" u users tmp
    node_require_jq || return 1
    for u in "${cert_url}" "${key_url}"; do
        if [[ "${u}" != https://* ]]; then
            echo -e "${red}更新源地址必须是 https://（私钥不能明文传输）：$(cert_url_mask "${u}")${plain}"
            return 1
        fi
    done

    # 第一次接管：默认证书已经在被节点使用，换掉会影响它们，得让人确认。
    if [[ ! -s "${CERT_SOURCE}" && -s "$(node_default_cert_path)" ]]; then
        users="$(cert_default_inbounds | cut -f1 | paste -sd' ' -)"
        if [[ -n "${users}" ]]; then
            echo -e "${yellow}以下节点正在使用默认证书 $(node_default_cert_path)，配置更新源后它会被换成远端证书：${plain}"
            echo "  ${users}"
            if [[ -t 0 ]]; then
                confirm "确定继续吗" "n" || return 1
            else
                echo -e "${red}非交互模式下不替换正在使用的默认证书。请在终端里执行 singr cert-source${plain}"
                return 1
            fi
        fi
    fi

    cert_default_update force "${cert_url}" "${key_url}" || return 1

    tmp="$(mktemp "${CONFIG_DIR}/.cert-source.XXXXXX")" || return 1
    chmod 600 "${tmp}"
    if ! jq -n --arg c "${cert_url}" --arg k "${key_url}" '{cert_url: $c, key_url: $k}' >"${tmp}" ||
        ! mv -f "${tmp}" "${CERT_SOURCE}"; then
        rm -f "${tmp}"
        echo -e "${red}写入 ${CERT_SOURCE} 失败（证书已更新，但不会自动续期）${plain}"
        return 1
    fi
    cert_timer_install
    echo -e "${green}已配置默认证书更新源，每天检查一次，剩余不足 ${CERT_RENEW_DAYS} 天时自动下载更新${plain}"
}

cert_source_clear() {
    rm -f "${CERT_SOURCE}"
    cert_timer_remove
    echo -e "${green}已清除默认证书更新源，现有证书文件保留，不再自动更新${plain}"
}

cert_source_show() {
    local c
    c="$(node_default_cert_path)"
    echo -e "${green}默认证书：${plain}${c} + $(node_default_key_path)"
    if [[ -s "${c}" ]]; then
        echo -e "  状态：$(node_cert_status "${c}")"
    else
        echo -e "  状态：${red}不存在${plain}"
    fi
    if [[ -s "${CERT_SOURCE}" ]]; then
        echo -e "${green}更新源：${plain}"
        echo "  证书：$(cert_url_mask "$(cert_source_get cert_url)")"
        echo "  私钥：$(cert_url_mask "$(cert_source_get key_url)")"
        if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet "${CERT_TIMER}.timer"; then
            echo -e "  每日检查：${green}已启用${plain}（剩余不足 ${CERT_RENEW_DAYS} 天时下载）"
        else
            echo -e "  每日检查：${red}未启用${plain}"
        fi
    else
        echo -e "${yellow}更新源：未配置${plain}"
    fi
}

# singr cert-source [--cert-url URL --key-url URL | --clear]
cert_source_cmd() {
    local cert_url="" key_url="" clear=0 sel
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --cert-url) cert_url="$2"; shift 2 ;;
            --key-url) key_url="$2"; shift 2 ;;
            --clear) clear=1; shift ;;
            *)
                echo -e "${red}未知参数：$1${plain}"
                echo "用法：singr cert-source [--cert-url URL --key-url URL | --clear]"
                return 1
                ;;
        esac
    done
    node_require_jq || return 1
    if [[ "${clear}" == 1 ]]; then
        cert_source_clear
        return 0
    fi
    if [[ -n "${cert_url}" || -n "${key_url}" ]]; then
        if [[ -z "${cert_url}" || -z "${key_url}" ]]; then
            echo -e "${red}--cert-url 与 --key-url 必须同时给出${plain}"
            return 1
        fi
        cert_source_set "${cert_url}" "${key_url}"
        return
    fi

    cert_source_show
    [[ -t 0 ]] || return 0
    echo -e "
  1. 设置 / 更换更新源
  2. 立即下载更新
  3. 清除更新源
  0. 返回
"
    read -r -p "请输入选择 [0-3]: " sel
    case "${sel}" in
        1)
            read -r -p "证书地址 (https://...): " cert_url
            read -r -p "私钥地址 (https://...): " key_url
            [[ -n "${cert_url}" && -n "${key_url}" ]] || { echo -e "${red}两个地址都要填${plain}"; return 1; }
            cert_source_set "${cert_url}" "${key_url}"
            ;;
        2) cert_update_cmd --force ;;
        3) cert_source_clear ;;
        *) return 0 ;;
    esac
}

# singr cert-update [--force]：定时任务调用的就是不带参数的这个。
cert_update_cmd() {
    local mode="auto"
    [[ "${1:-}" == "--force" ]] && mode="force"
    if [[ ! -s "${CERT_SOURCE}" ]]; then
        echo -e "${yellow}未配置默认证书更新源，无事可做（用 singr cert-source 配置）${plain}"
        return 0
    fi
    node_require_jq || return 1
    cert_default_update "${mode}" "$(cert_source_get cert_url)" "$(cert_source_get key_url)"
}
# <<<<<<<<<<<<<<<< SYNC BLOCK: 默认证书更新源 <<<<<<<<<<<<<<<<

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
    echo "singr list                  查看当前节点"
    echo "singr add [参数]            添加节点（不带参数则逐项询问）"
    echo "singr del <@序号>           删除节点，序号取自 singr list 的 # 列"
    echo "                            也可用 NodeID 或 InTag；NodeID 在多面板下可能重复"
    echo "singr cert-source           查看/配置默认证书的远程更新源"
    echo "singr cert-update [--force] 检查并更新默认证书（每日定时任务调用的就是它）"
    echo "------------------------------------------"
    echo "添加节点参数："
    echo "  --api-url URL --api-key KEY --node-id N --protocol anytls|hysteria2"
    echo "  --sni HOST --cert-path PATH --key-path PATH（宿主机路径，容器按原路径挂载）"
    echo "  或 --cert-url URL --key-url URL（下载为默认证书，并每日检查更新）"
    echo "  [--panel-type SSpanel] [--node-type V2ray] [--timeout 20]"
    echo "  [--speed-limit 0] [--device-limit 0] [--enable-device-limit false]"
    echo "  [--update-periodic 60]"
    echo "------------------------------------------"
}

show_menu() {
    local line="  ${green}──────────────────────────────────────${plain}"
    echo -e "
  ${green}${APP_NAME} 后端管理脚本（Docker）${plain}
${line}
  0. 修改配置
${line}
  2. 更新 SingR（拉镜像重建）
  3. 卸载 SingR
${line}
  4. 启动 SingR
  5. 停止 SingR
  6. 重启 SingR
  7. 查看 SingR 状态
  8. 查看 SingR 日志
${line}
  9. 设置开机自启
 10. 取消开机自启
${line}
 11. 查看 SingR 版本
 12. 更新管理脚本
${line}
 13. Hysteria2 端口跳跃管理
 14. 节点管理（查看 / 添加 / 删除）
 15. 默认证书更新源
${line}"
    show_status | sed 's/^/  /'
    echo -e "${line}"
    # 没装好 / 没有 jq 时节点列表是空的，别留下两条挨着的分割线。
    local nodes
    nodes="$(node_list_brief)"
    if [[ -n "${nodes}" ]]; then
        printf '%s\n' "${nodes}"
        echo -e "${line}"
    fi
    read -r -p "请输入选择 [0-15]: " num
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
        14) node_menu && before_show_menu ;;
        15) cert_source_cmd; before_show_menu ;;
        *) echo -e "${red}请输入正确的数字 [0-15]${plain}" && before_show_menu ;;
    esac
}

# 旧版本证书登记的一次性迁移（见 certs_migrate）。卸载、更新脚本自身、开机重放端口
# 跳跃规则这几条不触发：前两者马上就不需要它了，后者跑在开机路径上，dockerd 此时
# 也正在拉起容器，不该在这里重建。
case "${1:-}" in
    uninstall | update_shell | porthop-apply | porthop-flush) ;;
    *) certs_migrate ;;
esac

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
        list | nodes) node_list ;;
        add) shift; node_add "$@" ;;
        del | delete | rm) node_del "${2:-}" ;;
        cert-source) shift; cert_source_cmd "$@" ;;
        cert-update) cert_update_cmd "${2:-}" ;;
        porthop) porthop_menu ;;
        porthop-apply) porthop_reapply_all ;;
        porthop-flush) porthop_flush_all ;;
        _bootstrap) container_recreate ;;   # 供 install-docker.sh 首次创建调用
        _cert-mount-check) cert_path_mountable "${2:-}" && cert_path_mountable "${3:-}" ;;   # 供 install-docker.sh 预检
        *) show_usage ;;
    esac
else
    show_menu
fi
