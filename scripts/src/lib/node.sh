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
#   node_backend_idle                                   -> 没有节点了：停掉后端，开着开机自启就
#                                                          暂停它并留标记（否则开机后无限拉起）
#   node_backend_resume                                 -> 又有节点了：恢复被 idle 暂停的开机自启
#                                                          （只认标记，用户自己关的不动）
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
        naive) echo naive ;;
        *) return 1 ;;
    esac
}

# ---- 协议由面板决定（Go 侧判断的 shell 镜像）----
#
# 协议不用手填：面板节点 server 字符串里的 ws + path=/anytls、/hy2、/naive 已经决定了
# 它，二进制启动时也是按这个判断（poet/api/sspanel 的 ParseV2rayNodeResponse +
# isAnyTLSAlias / isHysteria2Alias / isNaiveAlias）。手填的协议和面板不一致，inbound
# 类型就错了。所以 add 先请求一次面板、照抄同一套规则判断，不认识就在改配置之前拒绝。
#
# 只照抄老 API 这一条路：脚本写的 panel.json 一律 disablecustomconfig: true，二进制
# 只走 ParseV2rayNodeResponse。Go 侧的 alias 规则改了，这里要一起改
# （scripts/test-node-detect.sh 用同一批 server 字符串守着）。

node_trim() {
    local s="$1"
    s="${s#"${s%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    printf '%s' "${s}"
}

node_lower_trim() {
    node_trim "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
}

# curl -K 配置文件里双引号字符串的转义：\ 和 " 前面加反斜杠。
node_curl_quote() {
    local s="${1//\\/\\\\}"
    printf '%s' "${s//\"/\\\"}"
}

# GET <api_url>/mod_mu/nodes/<id>/info（和二进制一样带 key 与 muKey 两个参数）。
# 响应体写到 <out>，stdout 只回显 HTTP 状态码。api-key 经 curl -K - 从 stdin 传，
# 不进命令行 —— 命令行参数 ps 谁都看得到。
node_panel_fetch() { # <api_url> <api_key> <node_id> <timeout> <out>
    printf 'url = "%s"\nget\ndata-urlencode = "key=%s"\ndata-urlencode = "muKey=%s"\n' \
        "$(node_curl_quote "${1%/}/mod_mu/nodes/$3/info")" \
        "$(node_curl_quote "$2")" "$(node_curl_quote "$2")" |
        curl -sS --connect-timeout 10 --max-time "$4" -o "$5" -w '%{http_code}' -K -
}

# 照抄 ParseV2rayNodeResponse 判断协议的那几步。
# stdout：协议<TAB>端口<TAB>host（host 可能为空）；认不出时原因走 stderr，返回 1。
node_panel_parse_server() { # <server 字符串>
    local raw="$1" f transport="" path="" host="" item key val
    local -a conf=() extra=()
    IFS=';' read -r -a conf <<<"${raw}"
    if [[ ${#conf[@]} -lt 6 ]]; then
        echo -e "${red}面板节点信息格式不对：${raw}（至少要 6 段，用 ; 分隔）${plain}" >&2
        return 1
    fi
    # 端口 / alterId 不是整数时二进制的 GetNodeInfo 一样会失败，这里提前拦下。
    if [[ ! "${conf[1]}" =~ ^[0-9]+$ ]] || ((10#${conf[1]} < 1 || 10#${conf[1]} > 65535)); then
        echo -e "${red}面板节点端口不对：「${conf[1]}」（要 1-65535 的整数）${plain}" >&2
        return 1
    fi
    if [[ ! "${conf[2]}" =~ ^-?[0-9]+$ ]]; then
        echo -e "${red}面板节点信息格式不对：第 3 段 alterId「${conf[2]}」不是整数${plain}" >&2
        return 1
    fi
    # 第 4、5 段：tls 是开关，另一个非空值就是传输方式（后者覆盖前者，与 Go 一致）。
    for f in "${conf[3]}" "${conf[4]}"; do
        f="$(node_lower_trim "${f}")"
        [[ "${f}" == "tls" || -z "${f}" ]] || transport="${f}"
    done
    # 第 6 段：key=value 用 | 分隔。path 取第一个 = 之后的全部（path 里可能带 =），
    # 其余的值只取到下一个 = 为止 —— 都和 Go 的 strings.Split 写法一致。
    IFS='|' read -r -a extra <<<"${conf[5]}"
    for item in "${extra[@]}"; do
        [[ "${item}" == *=* ]] || continue
        key="$(node_lower_trim "${item%%=*}")"
        val="${item#*=}"
        case "${key}" in
            path) path="$(node_trim "${val}")" ;;
            host) host="$(node_trim "${val%%=*}")" ;;
        esac
    done
    path="$(node_lower_trim "${path}")"
    path="${path#/}"
    if [[ "${transport}" == "ws" ]]; then
        case "${path}" in
            anytls) printf 'anytls\t%s\t%s\n' "$((10#${conf[1]}))" "${host}"; return 0 ;;
            hy2) printf 'hysteria2\t%s\t%s\n' "$((10#${conf[1]}))" "${host}"; return 0 ;;
            naive) printf 'naive\t%s\t%s\n' "$((10#${conf[1]}))" "${host}"; return 0 ;;
        esac
    fi
    echo -e "${red}认不出面板节点的协议：传输方式「${transport:-空}」，path「${path:-空}」${plain}" >&2
    echo -e "${yellow}SingR 只认 ws + path=/anytls、/hy2、/naive（在面板的节点地址里配置）。${plain}" >&2
    return 1
}

# 请求面板并判断协议。stdout 同 node_panel_parse_server；失败原因走 stderr。
node_panel_detect() { # <api_url> <api_key> <node_id> <node_type> <timeout>
    local api_url="$1" api_key="$2" node_id="$3" node_type="$4" timeout="$5"
    local out code rc=0 server msg

    # 其他 node-type 走不到 alias 分支，面板节点永远不会被认成这三种协议。
    case "$(node_lower_trim "${node_type}")" in
        v2ray | vmess) ;;
        *)
            echo -e "${red}--node-type ${node_type} 无法用于 anytls / hysteria2 / naive，请用默认的 V2ray${plain}" >&2
            return 1
            ;;
    esac
    command -v curl >/dev/null 2>&1 || { echo -e "${red}需要 curl 来请求面板${plain}" >&2; return 1; }

    out="$(mktemp)" || return 1
    code="$(node_panel_fetch "${api_url}" "${api_key}" "${node_id}" "${timeout}" "${out}")" || rc=$?
    if ((rc != 0)); then
        rm -f "${out}"
        echo -e "${red}连不上面板 ${api_url}（curl 退出码 ${rc}）${plain}" >&2
        return 1
    fi
    if ! jq -e '.ret == 1' "${out}" >/dev/null 2>&1; then
        msg="$(jq -r '.msg // .data // empty | tostring' "${out}" 2>/dev/null | head -c 200)"
        [[ -z "${msg}" ]] && msg="$(head -c 200 "${out}" | tr -d '\r' | tr '\n' ' ')"
        rm -f "${out}"
        echo -e "${red}面板拒绝了节点信息请求（HTTP ${code}）：${msg:-空响应}${plain}" >&2
        echo -e "${yellow}检查 --api-url、--api-key 与 --node-id。${plain}" >&2
        return 1
    fi
    server="$(jq -r '.data.server // empty | tostring' "${out}" 2>/dev/null)"
    rm -f "${out}"
    if [[ -z "${server}" ]]; then
        echo -e "${red}面板没有返回节点 #${node_id} 的地址信息（data.server 为空）${plain}" >&2
        return 1
    fi
    node_panel_parse_server "${server}"
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
# 节点引用它（默认配置铺的 anytls-in / hysteria2-in / naive-in 超集就是这种），那不是"占用"，
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

# panel.json 里一个节点都没有（最后一个节点被 singr del 删掉之后）。这时进程以
# 'no Node with InTag found' 退出，start/restart 据此直接拦下。没有 jq、文件不在或
# 解析不了时返回 1 —— 判断不了就别拦（node_count 解析失败会回显 0，不能拿它判断）。
node_none_configured() {
    command -v jq >/dev/null 2>&1 || return 1
    jq -e '(.nodes // []) | length == 0' "${PANEL_CONFIG}" >/dev/null 2>&1
}

# 没有节点时进程起不来（见 node_none_configured），start/restart 先过这一关，别让后端白拉一次。
# 有节点了就顺带恢复被 node_backend_idle 暂停的开机自启 —— 用 singr config 手写节点的
# 情况走的是这里，而不是 node_add。
node_guard_start() {
    if node_none_configured; then
        echo -e "${red}当前没有节点，${APP_NAME} 无法启动。先用 singr add 添加节点（添加后会自动启动）${plain}"
        return 1
    fi
    node_backend_resume
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
    # 从零节点加第一个节点失败：回滚后又没有节点了，重启只会再退出一次，回到停止状态。
    if [[ "$(node_count)" == "0" ]]; then
        node_backend_idle
        echo -e "${yellow}已回滚到变更前的配置（没有节点），${APP_NAME} 保持停止。请用 singr log 查看失败原因${plain}"
        return 1
    fi
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
        naive)
            # 不写 network = 同一端口上 TCP（HTTP/2）与 UDP（HTTP/3）都开。
            echo '{"type":"naive","tag":"","listen":"::","listen_port":0,"users":[],"tls":{"enabled":true,"server_name":"","certificate_path":"","key_path":""}}'
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

    # 必填的四项没给全且在交互终端上，就逐项问（顺带问可选的 SNI / 证书路径）；
    # 必填项都给齐了说明是一次性的命令行调用，可选项留空即按默认处理，不再追问。
    # 非交互（脚本调用）则直接报缺参数。
    if [[ -t 0 ]]; then
        if [[ -z "${api_url}" || -z "${api_key}" || -z "${node_id}" ]]; then
            [[ -z "${api_url}" ]] && read -r -p "面板地址 (--api-url，如 https://panel.example.com): " api_url
            [[ -z "${api_key}" ]] && read -r -p "面板密钥 (--api-key): " api_key
            [[ -z "${node_id}" ]] && read -r -p "节点 ID  (--node-id): " node_id
            [[ -z "${sni}" ]] && read -r -p "SNI 域名 (--sni，留空则由面板 host= 决定): " sni
            if [[ -z "${cert_src}" && -z "${key_src}" && -z "${cert_url}" && -z "${key_url}" ]]; then
                echo -e "${yellow}证书留空则使用默认路径 ${CERT_DIR}/default.pem 与 default.key${plain}"
                read -r -p "证书路径 (--cert-path，回车用默认): " cert_src
            fi
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
    local proto=""
    if [[ -n "${protocol}" ]]; then
        proto="$(node_norm_proto "${protocol}")" || {
            echo -e "${red}--protocol 只能是 anytls / hysteria2 / naive${plain}"
            return 1
        }
    fi
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

    # 协议以面板为准（见 node_panel_detect）。--protocol 只做校验：和面板不一致就拒绝，
    # 而不是照它写出一个类型不对的 inbound。面板请求失败也在这里停下 —— 落盘后再靠
    # node_apply 回滚，会让同进程的其他节点白白重启一次。
    local detected panel_port panel_host
    echo -e "${green}正在向面板查询节点 #${node_id} 的协议...${plain}"
    detected="$(node_panel_detect "${api_url}" "${api_key}" "${node_id}" "${node_type}" "${timeout}")" || return 1
    IFS=$'\t' read -r detected panel_port panel_host <<<"${detected}"
    if [[ -n "${proto}" && "${proto}" != "${detected}" ]]; then
        echo -e "${red}--protocol ${proto} 与面板不一致：面板节点 #${node_id} 是 ${detected}${plain}"
        echo -e "${yellow}协议由面板节点的 path 决定（/anytls、/hy2、/naive）。去掉 --protocol，或先改面板。${plain}"
        return 1
    fi
    proto="${detected}"
    echo -e "${green}面板节点 #${node_id}：协议 ${proto}，端口 ${panel_port}${panel_host:+，SNI ${panel_host}}${plain}"

    # 面板给了 host= 就由它决定 SNI（会覆盖 --sni），两者都空才会落到空 SNI。
    if [[ "${proto}" == "hysteria2" && -z "${sni}" && -z "${panel_host}" ]]; then
        echo -e "${yellow}提示：hysteria2 默认开着 salamander obfs 且密码留空，此时密钥取自 SNI。"
        echo -e "      面板没给 host= 又没指定 --sni，obfs 会静默关闭（配置显示开着，但实际没生效），"
        echo -e "      客户端按文档配了 obfs 反而连不上。建议指定 --sni 或在面板节点地址里加 host=。${plain}"
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

    # 启动前预检。anytls、hysteria2 与 naive 都强制 TLS，缺证书的 inbound 建不起来，而
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
        node_backend_resume
        echo
        node_list
        echo
        echo -e "${yellow}提示：同一台机上的多个节点必须在面板侧配置不同端口，否则第二个监听会起不来。"
        echo -e "      naive 同时占用该端口的 TCP 与 UDP，和 anytls（TCP）、hysteria2（UDP）都不能共用端口号。${plain}"
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

    # 最后一个节点也能删：没有节点进程跑不起来，所以删完不走重启校验，而是让后端停下
    # （node_backend_idle，顺带暂停开机自启），singr add 新节点时再恢复。
    local last=0
    if [[ "$(node_count)" == "1" ]]; then
        last=1
        echo -e "${yellow}这是最后一个节点。没有节点 ${APP_NAME} 无法运行，删除后会停止 ${APP_NAME}；"
        echo -e "之后用 singr add 添加节点会自动重新启动。${plain}"
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
    if ((last)); then
        node_commit
        node_backend_idle
        echo -e "${green}${APP_NAME} 已停止（当前没有节点）。用 singr add 添加节点后会自动启动。${plain}"
        return 0
    fi
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
