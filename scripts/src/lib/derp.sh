# >>>>>>>>>>>>>>>> SYNC BLOCK: DERP 中继 >>>>>>>>>>>>>>>>
# 本块在 SingR.sh 与 SingR-docker.sh 中逐字相同，改一处必须同步另一处。
#
# 节点内置 DERP，给林北客户端组网当中继。设计（详见 AGENTS.md 的「DERP relay」）：
#   - 客户端经代理隧道访问虚拟域名 DERP_DOMAIN，route.rules 前两条把它改写到本机：
#     443 端口 → 127.0.0.1:<DERP 端口>（中继本身），80 端口 → 127.0.0.1:<探测端口>
#     （明文 204，客户端经每个节点探一下，就知道哪些节点有中继）。DERP 开
#     verify_client_loopback，公网直连的普通客户端一律拒绝；于是只有有效订阅能用，
#     流量照常记到用户账上。
#   - 所有节点组成同一个 DERP 区域、两两 mesh（经公网 TCP <DERP 端口>）。节点每 60 秒向
#     sspapi 的 tailnet.php 报到（registry_url + 节点 token），报上端口、证书指纹和自己的
#     公网 IPv4 / IPv6，拿回 mesh PSK 和其它节点的名单，增删对端不重启。没有人需要手工
#     维护名单。地址由节点自己报（sspapi 前面是反代，看不到真实来源 IP）：默认自动探测，
#     也可以 --ipv4 / --ipv6 手写或填 none。
#   - mesh 连接按公钥指纹认证对端（握手后紧接着发出的就是 PSK）：每台节点用一张固定的
#     自签证书（DERP_TLS_CERT，首次开启时生成，之后不变），指纹随报到交给 sspapi，
#     名单经真证书 https 下发，所以可信。克隆机（同一张证书）会被 sspapi 拒绝。
#   - 配置在 server.json 的 services[tag=derp] 与 route.rules 前两条；节点 token 单独存在
#     DERP_TOKEN_FILE（0600），不写进 server.json。进程把 sspapi 的应答（含 PSK）缓存在
#     DERP_REGISTRY_CACHE（0600）：sspapi 暂时连不上时照样能重启、重建 mesh。
#
# 依赖节点管理块的 node_require_jq / node_configs_ready / node_backup / node_restore /
# node_apply，以及默认证书更新源块的 cert_url_mask。

DERP_TAG="derp"
DERP_DOMAIN="derp.linbei.internal"
DERP_DEFAULT_PORT=8443
DERP_DIR="${CONFIG_DIR}/derp"
DERP_KEY_FILE="${DERP_DIR}/derp.key"
# 不带换行：报到时 curl 原样把文件内容当 node_token 发出去。
DERP_TOKEN_FILE="${DERP_DIR}/registry.token"
# 上次成功开启时的 sspapi 地址、端口、IPv4 / IPv6 设置：关掉再开不用重新输入（token 文件同理保留）。
DERP_SAVED_FILE="${DERP_DIR}/enable.json"
# 进程把缓存放在 config_path（DERP_KEY_FILE）同目录下，文件名见 service/derp/singr_mesh.go。
DERP_REGISTRY_CACHE="${DERP_DIR}/mesh-registry.cache.json"
DERP_TLS_CERT="${DERP_DIR}/derp-tls.crt"
DERP_TLS_KEY="${DERP_DIR}/derp-tls.key"

# 当前 DERP 服务里某个字段的值；没开 DERP 或没有该字段时输出空。
derp_get() {
    jq -r --arg t "${DERP_TAG}" --arg k "$1" \
        '[(.services // [])[] | select(.tag == $t) | .[$k] // empty] | first // empty' \
        "${SERVER_CONFIG}" 2>/dev/null
}

derp_enabled() {
    jq -e --arg t "${DERP_TAG}" 'any((.services // [])[]; .tag == $t)' "${SERVER_CONFIG}" >/dev/null 2>&1
}

derp_valid_port() {
    [[ "$1" =~ ^[0-9]+$ ]] && ((10#$1 >= 1 && 10#$1 <= 65535))
}

# 探测口只听 127.0.0.1，不用放行防火墙；取 DERP 端口的下一个。
derp_probe_port() {
    if (($1 < 65535)); then echo $(($1 + 1)); else echo $(($1 - 1)); fi
}

derp_token() {
    [[ -s "${DERP_TOKEN_FILE}" ]] && tr -d ' \r\n' <"${DERP_TOKEN_FILE}"
}

# --ipv4 / --ipv6 的取值：空（自动探测）、none（不报）或一个该类型的地址。
derp_valid_advertise() {
    local family="$1" value="$2"
    [[ -z "${value}" || "${value}" == none ]] && return 0
    if [[ "${family}" == 4 ]]; then
        [[ "${value}" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]
    else
        [[ "${value}" == *:* && "${value}" =~ ^[0-9A-Fa-f:.]+$ ]]
    fi
}

# 固定的自签证书：只生成一次，之后一直用它（指纹在 sspapi 的名单里，换了会自动随报到更新）。
# 有效期写满一百年；对端按公钥指纹校验，不看有效期，写长只是避免别的工具误报过期。
derp_ensure_cert() {
    [[ -s "${DERP_TLS_CERT}" && -s "${DERP_TLS_KEY}" ]] && return 0
    command -v openssl >/dev/null 2>&1 || {
        echo -e "${red}生成 DERP 证书需要 openssl，请先安装（Debian/Ubuntu: apt install openssl）${plain}"
        return 1
    }
    mkdir -p "${DERP_DIR}" && chmod 700 "${DERP_DIR}" 2>/dev/null
    # 先单独生成 EC 私钥再签证书：req 的 -newkey ec + -pkeyopt 写法 LibreSSL 不认。
    if ! (umask 077 && openssl ecparam -name prime256v1 -genkey -noout -out "${DERP_TLS_KEY}" >/dev/null 2>&1 &&
        openssl req -new -x509 -key "${DERP_TLS_KEY}" -days 36500 -subj "/CN=derp" \
            -out "${DERP_TLS_CERT}" >/dev/null 2>&1); then
        rm -f "${DERP_TLS_CERT}" "${DERP_TLS_KEY}"
        echo -e "${red}生成 DERP 证书失败${plain}"
        return 1
    fi
    chmod 644 "${DERP_TLS_CERT}" 2>/dev/null
    echo -e "${green}已生成本机 DERP 证书，指纹：$(derp_pin)${plain}"
}

# 公钥指纹 = base64(SHA-256(公钥 DER))，与 sing-box 的 certificate_public_key_sha256 同一算法。
derp_pin() {
    [[ -s "${DERP_TLS_CERT}" ]] || return 1
    openssl x509 -in "${DERP_TLS_CERT}" -noout -pubkey 2>/dev/null |
        openssl pkey -pubin -outform DER 2>/dev/null |
        openssl dgst -sha256 -binary 2>/dev/null | base64 | tr -d '\n'
}

# 这一类地址此刻要报什么：手写的原样用；none 不报；自动时问 ip.sb（只走这一类网络）。
# 进程运行后改由它自己每 5 秒看本机地址、必要时问 ip.sb，这里只用于开启前那一次报到。
derp_detect_ip() {
    local family="$1" value="$2" ip
    [[ "${value}" == none ]] && return 0
    if [[ -n "${value}" ]]; then
        echo "${value}"
        return 0
    fi
    ip="$(curl "-${family}" -fsS --noproxy '*' --connect-timeout 5 --max-time 8 \
        "https://api-ipv${family}.ip.sb/ip" 2>/dev/null | tr -d ' \r\n')" || true
    derp_valid_advertise "${family}" "${ip}" && [[ -n "${ip}" ]] && echo "${ip}"
}

# 调 sspapi 的 tailnet.php。参数：地址 token文件 额外的表单字段（name=value，可多个）。
# 成功时把应答写到 stdout。token 用 --data-urlencode name@文件 读，不进命令行（ps 看得见）。
derp_registry_call() {
    local url="$1" token_file="$2" out code rc=0 field
    shift 2
    local -a args
    command -v curl >/dev/null 2>&1 || { echo -e "${red}需要 curl${plain}" >&2; return 1; }
    out="$(mktemp)" || return 1
    args=(--proto '=https' -sS --noproxy '*' --connect-timeout 15 --max-time 30
        -o "${out}" -w '%{http_code}' --data-urlencode "node_token@${token_file}")
    for field in "$@"; do
        args+=(--data-urlencode "${field}")
    done
    code="$(curl "${args[@]}" "${url}")" || rc=$?
    if ((rc != 0)); then
        echo -e "${red}连不上 sspapi（curl 退出码 ${rc}）${plain}" >&2
    elif [[ "${code}" == 200 ]] && jq -e '.status == "success"' "${out}" >/dev/null 2>&1; then
        cat "${out}"
    else
        rc=1
        case "$(jq -r '.code // empty' "${out}" 2>/dev/null)" in
            DERP_UNAUTHORIZED) echo -e "${red}sspapi 拒绝了节点 token（和 data/tailnet.json 的 derp.node_token 对不上）${plain}" >&2 ;;
            DERP_BAD_REQUEST) echo -e "${red}sspapi 不接受报到参数：多半是地址不是公网 IPv4 / IPv6，用 --ipv4 / --ipv6 手写${plain}" >&2 ;;
            DERP_PIN_CONFLICT) echo -e "${red}另一台机器在用同一张 DERP 证书（多半是克隆机）：删掉 ${DERP_TLS_CERT} 和 ${DERP_TLS_KEY} 后重新开启${plain}" >&2 ;;
            TAILNET_UNAVAILABLE) echo -e "${red}sspapi 上没配好 DERP（data/tailnet.json 的 derp 一节）${plain}" >&2 ;;
            *) echo -e "${red}sspapi 返回 HTTP ${code}：$(head -c 300 "${out}")${plain}" >&2 ;;
        esac
    fi
    rm -f "${out}"
    return "${rc}"
}

# 报到一次并把应答写进进程的缓存：既验证了地址和 token，进程第一次启动也直接有 PSK。
derp_register() {
    local url="$1" token_file="$2" port="$3" v4="$4" v6="$5" resp tmp
    resp="$(derp_registry_call "${url}" "${token_file}" action=derp_sync "port=${port}" "pin=$(derp_pin)" \
        "ipv4=${v4}" "ipv6=${v6}")" || return 1
    if ! jq -e '(.psk | test("^[0-9a-f]{64}$")) and (.nodes | type == "array")' <<<"${resp}" >/dev/null 2>&1; then
        echo -e "${red}sspapi 返回的名单格式不对${plain}" >&2
        return 1
    fi
    mkdir -p "${DERP_DIR}" && chmod 700 "${DERP_DIR}" 2>/dev/null
    tmp="$(umask 077 && mktemp "${DERP_REGISTRY_CACHE}.XXXXXX")" || return 1
    printf '%s' "${resp}" >"${tmp}" && mv -f "${tmp}" "${DERP_REGISTRY_CACHE}" || { rm -f "${tmp}"; return 1; }
    echo -e "${green}已向 sspapi 报到（IPv4 ${v4:-无}，IPv6 ${v6:-无}），当前名单 $(jq '.nodes | length' <<<"${resp}") 个节点（含本机）${plain}"
}

# 主动离开名单（不离开的话别的节点要等 sspapi 的过期时间才不再连它）。失败不阻断。
derp_leave() {
    local url="$1" token_file="$2"
    [[ -n "${url}" && -s "${token_file}" && -s "${DERP_TLS_CERT}" ]] || return 0
    derp_registry_call "${url}" "${token_file}" action=derp_leave "pin=$(derp_pin)" >/dev/null ||
        echo -e "${yellow}没能从 sspapi 名单里撤下本机，它会在过期后自动移除${plain}"
}

# 按给定端口、sspapi 地址、IPv4 / IPv6 设置写入 DERP 服务与两条改写规则（替换已有的那一份）。
derp_write_server() {
    local port="$1" url="$2" v4="$3" v6="$4" tmp
    tmp="$(mktemp "${SERVER_CONFIG}.XXXXXX")" || return 1
    if ! jq --arg t "${DERP_TAG}" --arg d "${DERP_DOMAIN}" --argjson port "${port}" \
        --argjson probe "$(derp_probe_port "${port}")" --arg key "${DERP_KEY_FILE}" \
        --arg url "${url}" --arg token "${DERP_TOKEN_FILE}" --arg v4 "${v4}" --arg v6 "${v6}" \
        --arg crt "${DERP_TLS_CERT}" --arg tkey "${DERP_TLS_KEY}" '
        def is_derp_rule: (.action == "route-options") and (.domain == [$d]);
        .services = ([(.services // [])[] | select(.tag != $t)] + [
            {type: "derp", tag: $t, listen: "::", listen_port: $port,
             config_path: $key, home: "blank",
             tls: {enabled: true, certificate_path: $crt, key_path: $tkey},
             verify_client_loopback: true, probe_listen_port: $probe,
             registry_url: $url, registry_token_path: $token}
            + (if $v4 != "" then {advertise_ipv4: $v4} else {} end)
            + (if $v6 != "" then {advertise_ipv6: $v6} else {} end)
        ])
        | .route = (.route // {})
        # route-options 不是终止动作，两条都得带端口，否则后一条会盖掉前一条的改写。
        | .route.rules = ([{domain: [$d], port: [443], action: "route-options",
                            override_address: "127.0.0.1", override_port: $port},
                           {domain: [$d], port: [80], action: "route-options",
                            override_address: "127.0.0.1", override_port: $probe}]
                          + [(.route.rules // [])[] | select(is_derp_rule | not)])
    ' "${SERVER_CONFIG}" >"${tmp}" || ! mv -f "${tmp}" "${SERVER_CONFIG}"; then
        rm -f "${tmp}"
        echo -e "${red}写入 ${SERVER_CONFIG} 失败${plain}"
        return 1
    fi
}

derp_remove_server() {
    local tmp
    tmp="$(mktemp "${SERVER_CONFIG}.XXXXXX")" || return 1
    if ! jq --arg t "${DERP_TAG}" --arg d "${DERP_DOMAIN}" '
        def is_derp_rule: (.action == "route-options") and (.domain == [$d]);
        .services = [(.services // [])[] | select(.tag != $t)]
        | if .services == [] then del(.services) else . end
        | if .route.rules then .route.rules = [.route.rules[] | select(is_derp_rule | not)] else . end
    ' "${SERVER_CONFIG}" >"${tmp}" || ! mv -f "${tmp}" "${SERVER_CONFIG}"; then
        rm -f "${tmp}"
        echo -e "${red}写入 ${SERVER_CONFIG} 失败${plain}"
        return 1
    fi
}

# 备份 → 写入 → 重启校验，起不来就连同 token 文件一起回滚。
# 参数：write|remove [端口 sspapi地址 IPv4设置 IPv6设置 新token文件(不改传空)]
derp_change() {
    local mode="$1" port="${2:-}" url="${3:-}" v4="${4:-}" v6="${5:-}" new_token="${6:-}" token_backup=""
    node_configs_ready || return 1
    mkdir -p "${DERP_DIR}" || return 1
    chmod 700 "${DERP_DIR}" 2>/dev/null
    if [[ -n "${new_token}" && -e "${DERP_TOKEN_FILE}" ]]; then
        token_backup="${DERP_TOKEN_FILE}.bak"
        cp -fp "${DERP_TOKEN_FILE}" "${token_backup}" || return 1
    fi
    node_backup || { echo -e "${red}备份配置失败，未做任何修改${plain}"; return 1; }
    if [[ -n "${new_token}" ]]; then
        (umask 077 && cp -f "${new_token}" "${DERP_TOKEN_FILE}") || { node_restore; return 1; }
    fi
    if [[ "${mode}" == "remove" ]]; then
        derp_remove_server || { node_restore; return 1; }
    else
        derp_write_server "${port}" "${url}" "${v4}" "${v6}" || { node_restore; return 1; }
    fi
    # 没有节点时进程本来就不运行（见 node_none_configured），重启校验必然失败。只写配置，
    # 添加节点、进程启动时一并生效。
    if node_none_configured; then
        node_commit
        rm -f "${token_backup}"
        echo -e "${yellow}当前没有节点，${APP_NAME} 未运行：DERP 配置已写入，添加节点启动后生效${plain}"
        return 0
    fi
    if node_apply; then
        rm -f "${token_backup}"
        return 0
    fi
    if [[ -n "${new_token}" ]]; then
        if [[ -n "${token_backup}" ]]; then
            mv -f "${token_backup}" "${DERP_TOKEN_FILE}"
        else
            rm -f "${DERP_TOKEN_FILE}"
        fi
    fi
    return 1
}

derp_show() {
    local port v4 v6 peers
    if ! derp_enabled; then
        echo -e "${yellow}DERP 中继：未开启${plain}"
        return 0
    fi
    port="$(derp_get listen_port)"
    v4="$(derp_get advertise_ipv4)"
    v6="$(derp_get advertise_ipv6)"
    echo -e "${green}DERP 中继：已开启${plain}"
    echo "  端口：TCP ${port}（节点之间 mesh 互联要用，防火墙需放行）"
    echo "  虚拟域名：${DERP_DOMAIN} → 127.0.0.1:${port}，探测口 127.0.0.1:$(derp_get probe_listen_port)（只接受经代理隧道进来的连接）"
    echo "  sspapi：$(cert_url_mask "$(derp_get registry_url)")（运行中每 60 秒报到并刷新名单）"
    echo "  报的 IPv4：${v4:-自动（本机公网地址，没有就问 ip.sb）}"
    echo "  报的 IPv6：${v6:-自动（本机公网地址，没有就问 ip.sb）}"
    echo "  本机指纹：$(derp_pin || echo '读取失败')"
    if [[ -s "${DERP_REGISTRY_CACHE}" ]]; then
        peers="$(jq -r '(.nodes // []) | length' "${DERP_REGISTRY_CACHE}" 2>/dev/null)"
        echo "  最近一次拉到：${peers:-?} 个节点（含本机）"
    else
        echo -e "  最近一次拉到：${yellow}还没有成功拉取过${plain}"
    fi
}

derp_usage() {
    echo "用法："
    echo "  singr derp                    查看状态（终端里会出操作菜单）"
    echo "  singr derp enable --registry <https://.../tailnet.php> --token <节点 token> [--port 端口] [--ipv4 地址|none] [--ipv6 地址|none]"
    echo "                                开启 DERP 中继并加入 mesh，默认端口 ${DERP_DEFAULT_PORT}；已开启时可只改其中几项"
    echo "                                --ipv4 / --ipv6 不写 = 自动探测；写 auto 恢复自动；none = 不报这一类"
    echo "  singr derp disable            关闭 DERP 中继（同时从 sspapi 名单撤下本机）"
    echo "  singr derp port <端口>        修改端口"
    echo "节点 token 是 sspapi data/tailnet.json 里 derp.node_token 的那个。"
}

# 开启或修改。参数同 singr derp enable；没给的项沿用当前配置（关掉过的话沿用上次开启时的）。
derp_enable() {
    local url port v4 v6 token="" token_changed="" was_enabled="" token_file tmp_token="" d4 d6
    if derp_enabled; then
        was_enabled=1
        url="$(derp_get registry_url)"
        port="$(derp_get listen_port)"
        v4="$(derp_get advertise_ipv4)"
        v6="$(derp_get advertise_ipv6)"
    elif [[ -s "${DERP_SAVED_FILE}" ]]; then
        url="$(jq -r '.registry // empty' "${DERP_SAVED_FILE}" 2>/dev/null)"
        port="$(jq -r '.port // empty' "${DERP_SAVED_FILE}" 2>/dev/null)"
        v4="$(jq -r '.ipv4 // empty' "${DERP_SAVED_FILE}" 2>/dev/null)"
        v6="$(jq -r '.ipv6 // empty' "${DERP_SAVED_FILE}" 2>/dev/null)"
    fi
    port="${port:-${DERP_DEFAULT_PORT}}"
    while (($# > 0)); do
        case "$1" in
            --registry) url="${2:-}"; shift 2 || shift ;;
            --token) token="${2:-}"; token_changed=1; shift 2 || shift ;;
            --port) port="${2:-}"; shift 2 || shift ;;
            --ipv4) v4="${2:-}"; shift 2 || shift ;;
            --ipv6) v6="${2:-}"; shift 2 || shift ;;
            *) echo -e "${red}未知参数：$1${plain}"; derp_usage; return 1 ;;
        esac
    done
    [[ "${v4}" == auto ]] && v4=""
    [[ "${v6}" == auto ]] && v6=""
    derp_valid_port "${port}" || { echo -e "${red}端口无效：${port}${plain}"; return 1; }
    if [[ "${url}" != https://?* ]]; then
        echo -e "${red}需要 sspapi 地址 --registry https://.../tailnet.php（名单决定 PSK 发给谁，必须 https）${plain}"
        return 1
    fi
    derp_valid_advertise 4 "${v4}" || { echo -e "${red}--ipv4 要是一个 IPv4 地址、auto 或 none：${v4}${plain}"; return 1; }
    derp_valid_advertise 6 "${v6}" || { echo -e "${red}--ipv6 要是一个 IPv6 地址、auto 或 none：${v6}${plain}"; return 1; }
    [[ "${v4}" == none && "${v6}" == none ]] && { echo -e "${red}--ipv4 和 --ipv6 不能都是 none${plain}"; return 1; }

    if [[ -n "${token_changed}" ]]; then
        if ((${#token} < 32)); then
            echo -e "${red}节点 token 至少 32 个字符（sspapi data/tailnet.json 的 derp.node_token）${plain}"
            return 1
        fi
        tmp_token="$(umask 077 && mktemp)" || return 1
        printf '%s' "${token}" >"${tmp_token}"
        token_file="${tmp_token}"
    elif [[ -s "${DERP_TOKEN_FILE}" ]]; then
        token_file="${DERP_TOKEN_FILE}"
    else
        echo -e "${red}需要节点 token --token ...（sspapi data/tailnet.json 的 derp.node_token）${plain}"
        return 1
    fi

    derp_ensure_cert || { rm -f "${tmp_token}"; return 1; }
    d4="$(derp_detect_ip 4 "${v4}")"
    d6="$(derp_detect_ip 6 "${v6}")"
    if [[ -z "${d4}" && -z "${d6}" ]]; then
        echo -e "${red}探测不到本机公网 IPv4 / IPv6（ip.sb 连不上？），请用 --ipv4 / --ipv6 手写${plain}"
        rm -f "${tmp_token}"
        return 1
    fi
    derp_register "${url}" "${token_file}" "${port}" "${d4}" "${d6}" || { rm -f "${tmp_token}"; return 1; }
    if ! derp_change write "${port}" "${url}" "${v4}" "${v6}" "${tmp_token}"; then
        # 没开起来：刚报到的那条撤掉；本来就开着的话旧配置已回滚，进程会按旧配置继续报到。
        [[ -n "${was_enabled}" ]] || derp_leave "${url}" "${token_file}"
        rm -f "${tmp_token}"
        return 1
    fi
    rm -f "${tmp_token}"
    jq -n --arg r "${url}" --argjson p "${port}" --arg a "${v4}" --arg b "${v6}" \
        '{registry: $r, port: $p, ipv4: $a, ipv6: $b}' >"${DERP_SAVED_FILE}" 2>/dev/null
    echo -e "${yellow}记得在防火墙放行 TCP ${port}（节点之间 mesh 互联要用）${plain}"
}

# singr derp [enable ... | disable | port <端口>]
derp_cmd() {
    local action="${1:-}" arg="${2:-}" sel url token port
    node_require_jq || return 1
    node_configs_ready || return 1
    case "${action}" in
        "") ;;
        enable)
            shift
            derp_enable "$@"
            return
            ;;
        disable)
            derp_enabled || { echo -e "${yellow}DERP 中继本来就没开${plain}"; return 0; }
            derp_leave "$(derp_get registry_url)" "${DERP_TOKEN_FILE}"
            derp_change remove || return 1
            rm -f "${DERP_REGISTRY_CACHE}"
            return 0
            ;;
        port)
            derp_enabled || { echo -e "${red}DERP 中继未开启，请用 singr derp enable${plain}"; return 1; }
            derp_enable --port "${arg}"
            return
            ;;
        help | -h | --help)
            derp_usage
            return 0
            ;;
        *)
            derp_usage
            return 1
            ;;
    esac

    derp_show
    [[ -t 0 ]] || return 0
    echo -e "
  1. 开启 / 关闭
  2. 修改端口
  3. 修改 sspapi 地址与节点 token
  4. 手写 IPv4 / IPv6（留空 = 自动探测）
  0. 返回
"
    read -r -p "请输入选择 [0-4]: " sel
    case "${sel}" in
        1)
            if derp_enabled; then
                confirm "确定关闭 DERP 中继吗" "n" && derp_cmd disable
            else
                read -r -p "sspapi 地址 (https://.../tailnet.php): " url
                read -r -s -p "节点 token: " token
                echo
                read -r -p "端口 [${DERP_DEFAULT_PORT}]: " port
                derp_enable --registry "${url}" --token "${token}" --port "${port:-${DERP_DEFAULT_PORT}}"
            fi
            ;;
        2)
            read -r -p "新端口: " port
            derp_cmd port "${port}"
            ;;
        3)
            read -r -p "sspapi 地址 [$(derp_get registry_url)]: " url
            read -r -s -p "节点 token（留空不改）: " token
            echo
            if [[ -n "${token}" ]]; then
                derp_enable --registry "${url:-$(derp_get registry_url)}" --token "${token}"
            else
                derp_enable --registry "${url:-$(derp_get registry_url)}"
            fi
            ;;
        4)
            read -r -p "IPv4（地址 / none / 留空自动）: " url
            read -r -p "IPv6（地址 / none / 留空自动）: " token
            derp_enable --ipv4 "${url:-auto}" --ipv6 "${token:-auto}"
            ;;
        *) return 0 ;;
    esac
}
# <<<<<<<<<<<<<<<< SYNC BLOCK: DERP 中继 <<<<<<<<<<<<<<<<
