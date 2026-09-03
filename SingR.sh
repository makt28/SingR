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
CERT_DIR="${CONFIG_DIR}/certs"
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
    rm -f /etc/logrotate.d/singr
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
#   node_backend_place_cert <tag> <cert_src> <key_src>  -> 仅在 stdout 回显
#                                                          "<certpath>|<keypath>"，其余输出走 stderr
#   node_backend_forget_cert <tag>                      -> 清理该 tag 的证书登记
#   node_backend_restart                                -> 重启后端
#   node_backend_verify                                 -> 0=确实起来了（须能识别 crash-loop）
#   node_cert_renew_hint                                -> 打印该后端的证书续期建议

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

# 回显某 inbound 实际生效的 "<certpath>|<keypath>"，空值按上面的默认规则补齐。
node_effective_cert() {
    local tag="$1" cert key
    cert="$(jq -r --arg t "${tag}" '(first(.inbounds[]? | select(.tag==$t) | .tls.certificate_path)) // ""' "${SERVER_CONFIG}" 2>/dev/null)"
    key="$(jq -r --arg t "${tag}" '(first(.inbounds[]? | select(.tag==$t) | .tls.key_path)) // ""' "${SERVER_CONFIG}" 2>/dev/null)"
    [[ -z "${cert}" ]] && cert="$(node_default_cert_path)"
    [[ -z "${key}" ]] && key="$(node_default_key_path)"
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
            if [[ -s "${cert}" && -s "${key}" ]]; then
                mark="${green}OK${plain}"
            else
                mark="${red}缺失${plain}"
            fi
        fi
        printf '  %-3s %-8s %-10s %-34s %-20s ' "${i}" "${nodeid}" "${proto}" "${apihost}" "${intag}"
        echo -e "${mark}"
        i=$((i + 1))
    done < <(jq -r '(.nodes // [])[] | [((.apiconfig.nodeid // "?")|tostring), (.apiconfig.apihost // "?"), (.intag // "")] | @tsv' "${PANEL_CONFIG}" 2>/dev/null)
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
            echo '{"type":"hysteria2","tag":"","listen":"::","listen_port":0,"users":[],"up_mbps":0,"down_mbps":0,"ignore_client_bandwidth":false,"obfs":{"type":"salamander","password":""},"tls":{"enabled":true,"server_name":"","certificate_path":"","key_path":""}}'
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
        if [[ -z "${cert_src}" && -z "${key_src}" ]]; then
            echo -e "${yellow}证书留空则使用默认路径 ${CERT_DIR}/default.pem 与 default.key${plain}"
            read -r -p "证书路径 (--cert-path，回车用默认): " cert_src
            [[ -n "${cert_src}" ]] && read -r -p "私钥路径 (--key-path): " key_src
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
        # 写空值，由二进制在启动时补成默认路径；不复制文件也不登记证书源。
        cert_path=""
        key_path=""
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
        echo -e "${yellow}（.crt 后缀也认；或用 --cert-path/--key-path 指定其他路径）${plain}"
        node_backend_forget_cert "${tag}"
        return 1
    fi

    node_backup || { echo -e "${red}备份配置失败，已中止${plain}"; return 1; }
    if ! node_write_server "${tag}" "${proto}" "${sni}" "${cert_path}" "${key_path}"; then
        echo -e "${red}写入 ${SERVER_CONFIG} 失败${plain}"
        node_restore
        node_backend_forget_cert "${tag}"
        return 1
    fi
    if ! node_write_panel "${tag}" "${proto}" "${api_url}" "${api_key}" "${node_id}" \
        "${panel_type}" "${node_type}" "${timeout}" "${speed_limit}" "${device_limit}" \
        "${update_periodic}" "${enable_device_limit}"; then
        echo -e "${red}写入 ${PANEL_CONFIG} 失败${plain}"
        node_restore
        node_backend_forget_cert "${tag}"
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
    node_backend_forget_cert "${tag}"
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
        node_backend_forget_cert "${tag}"
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

# ---- 节点管理：裸机后端实现（SYNC BLOCK 的四个钩子）----
#
# 证书原地引用，不复制。证书路径通常来自 certbot，复制一份会让续期在下次重启后
# 静默失效；裸机没有容器那样的挂载边界，直接用用户给的路径最省事也最正确。

node_backend_place_cert() {
    local cert_src="$2" key_src="$3"
    [[ -s "${cert_src}" ]] || {
        echo -e "${red}找不到证书文件：${cert_src}${plain}" >&2
        return 1
    }
    [[ -s "${key_src}" ]] || {
        echo -e "${red}找不到私钥文件：${key_src}${plain}" >&2
        return 1
    }
    printf '%s|%s' "${cert_src}" "${key_src}"
}

# 裸机没有证书副本，无需清理。
node_backend_forget_cert() { :; }

node_backend_restart() {
    systemctl restart "${SERVICE_NAME}" >/dev/null 2>&1 || true
}

# 与 docker 版同构：既要 active，又要重启计数不再增长，才算真起来了。
# 单次 is-active 抓不到 Restart=on-failure 的 crash-loop——而"任一节点 Start()
# 失败 → os.Exit(1)"正是加错节点时的表现，必须能识别出来才能触发回滚。
node_backend_verify() {
    local r1 r2
    sleep 2
    systemctl is-active --quiet "${SERVICE_NAME}" || return 1
    r1="$(systemctl show -p NRestarts --value "${SERVICE_NAME}" 2>/dev/null)"
    sleep 3
    systemctl is-active --quiet "${SERVICE_NAME}" || return 1
    r2="$(systemctl show -p NRestarts --value "${SERVICE_NAME}" 2>/dev/null)"
    [[ "${r1}" == "${r2}" ]]
}

node_cert_renew_hint() {
    echo -e "${yellow}证书续期提示：TLS 证书材料不热重载，续期后必须重启才生效。建议："
    echo -e "  certbot renew --deploy-hook \"singr restart\"${plain}"
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
    echo "SingR list                  查看当前节点"
    echo "SingR add [参数]            添加节点（不带参数则逐项询问）"
    echo "SingR del <@序号>           删除节点，序号取自 singr list 的 # 列"
    echo "                            也可用 NodeID 或 InTag；NodeID 在多面板下可能重复"
    echo "------------------------------------------"
    echo "添加节点参数："
    echo "  --api-url URL --api-key KEY --node-id N --protocol anytls|hysteria2"
    echo "  --sni HOST --cert-path PATH --key-path PATH"
    echo "  [--panel-type SSpanel] [--node-type V2ray] [--timeout 20]"
    echo "  [--speed-limit 0] [--device-limit 0] [--enable-device-limit false]"
    echo "  [--update-periodic 60]"
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
 14. 节点管理（查看 / 添加 / 删除）
"
    show_status
    echo
    node_list_brief
    echo
    read -r -p "请输入选择 [0-14]: " num
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
        14) check_install && node_menu && before_show_menu ;;
        *) echo -e "${red}请输入正确的数字 [0-14]${plain}" && before_show_menu ;;
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
        list | nodes) check_install 0 && node_list ;;
        add) check_install 0 && shift && node_add "$@" ;;
        del | delete | rm) check_install 0 && node_del "${2:-}" ;;
        porthop) porthop_menu ;;
        porthop-apply) porthop_reapply_all ;;
        porthop-flush) porthop_flush_all ;;
        *) show_usage ;;
    esac
else
    show_menu
fi
