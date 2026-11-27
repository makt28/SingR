# >>>>>>>>>>>>>>>> SYNC BLOCK: 节点独立证书 >>>>>>>>>>>>>>>>
# 本块在 SingR.sh 与 SingR-docker.sh 中逐字相同（源在 scripts/src/lib/node-cert.sh）。
#
# 和默认证书平行的另一套：某个节点独占一张从远程地址下载的证书，放在
# ${CERT_DIR}/node/<intag>.pem + <intag>.key。server.json 里写的就是这两个显式路径，
# 对二进制来说是普通的显式证书（Go 侧不需要知道这套机制）；对默认证书那套来说，这些
# 节点不算"用默认证书的节点"，默认更新源怎么换都碰不到它们，反过来也一样。
#
# 来源登记在 ${NODE_CERT_SOURCE}：{"<intag>": {"cert_url": ..., "key_url": ...}}，0600
# （地址里常带 token）。定时任务 singr-node-cert-update.timer 每天跑一次
# singr node-cert-update，规则与默认证书相同：剩余不足 CERT_RENEW_DAYS 天才下载、远端
# 更新才替换、只换文件不重启（进程按文件路径监视，换哪个节点的证书只重载那个节点）。
# 各节点互不影响：一个失败照样处理其余的，只要有失败就以非零退出。
#
# 生命周期跟着节点走：singr add --node-cert-url/--node-key-url 下载并登记，singr del
# 删节点时连证书和登记一起删，登记删空了顺带卸掉 timer。
#
# 依赖「默认证书更新源」块的 cert_pair_update / cert_tools_ready / cert_url_mask /
# cert_timer_write / cert_timer_remove_unit / CERT_RENEW_DAYS，和「节点管理」块的
# node_resolve_tag / node_effective_cert / node_inbound_exists / node_sni / node_desc /
# node_cert_status。

NODE_CERT_DIR="${CERT_DIR}/node"
NODE_CERT_SOURCE="${CONFIG_DIR}/node-certs.json"
NODE_CERT_TIMER="singr-node-cert-update"

node_cert_path() { printf '%s' "${NODE_CERT_DIR}/$1.pem"; }
node_cert_key_path() { printf '%s' "${NODE_CERT_DIR}/$1.key"; }

node_cert_source_get() { # <intag> <cert_url|key_url>
    jq -r --arg t "$1" --arg k "$2" '.[$t][$k] // ""' "${NODE_CERT_SOURCE}" 2>/dev/null
}

node_cert_registered() {
    jq -e --arg t "$1" 'has($t)' "${NODE_CERT_SOURCE}" >/dev/null 2>&1
}

node_cert_tags() {
    jq -r 'keys[]' "${NODE_CERT_SOURCE}" 2>/dev/null
}

# 该节点的 server.json 是否正指向它的独立证书路径。
node_cert_in_use() {
    node_inbound_exists "$1" || return 1
    [[ "$(node_effective_cert "$1")" == "$(node_cert_path "$1")|$(node_cert_key_path "$1")" ]]
}

# 私钥要走这条链路，只接受 https（下载时 --proto =https 还会拒绝重定向到 http）。
node_cert_check_urls() { # <cert_url> <key_url>
    local u
    for u in "$1" "$2"; do
        if [[ "${u}" != https://* ]]; then
            echo -e "${red}证书地址必须是 https://（私钥不能明文传输）：$(cert_url_mask "${u}")${plain}"
            return 1
        fi
    done
}

node_cert_registry_set() { # <intag> <cert_url> <key_url>
    local tmp src="${NODE_CERT_SOURCE}"
    [[ -s "${src}" ]] && jq -e 'type == "object"' "${src}" >/dev/null 2>&1 || src=/dev/null
    tmp="$(mktemp "${CONFIG_DIR}/.node-certs.XXXXXX")" || return 1
    chmod 600 "${tmp}"
    if jq -n --arg t "$1" --arg c "$2" --arg k "$3" --slurpfile m "${src}" \
        '($m[0] // {}) | .[$t] = {cert_url: $c, key_url: $k}' >"${tmp}" && mv -f "${tmp}" "${NODE_CERT_SOURCE}"; then
        return 0
    fi
    rm -f "${tmp}"
    return 1
}

node_cert_registry_del() { # <intag>
    local tmp
    node_cert_registered "$1" || return 0
    tmp="$(mktemp "${CONFIG_DIR}/.node-certs.XXXXXX")" || return 1
    chmod 600 "${tmp}"
    if jq --arg t "$1" 'del(.[$t])' "${NODE_CERT_SOURCE}" >"${tmp}" && mv -f "${tmp}" "${NODE_CERT_SOURCE}"; then
        return 0
    fi
    rm -f "${tmp}"
    return 1
}

# 登记删空了就卸掉 timer（连文件一起删，免得留一个每天空跑的任务）。
node_cert_timer_sync() {
    if [[ -n "$(node_cert_tags)" ]]; then
        cert_timer_write "${NODE_CERT_TIMER}" "SingR per-node certificate update check" "node-cert-update"
    else
        rm -f "${NODE_CERT_SOURCE}"
        cert_timer_remove_unit "${NODE_CERT_TIMER}"
    fi
}

# 下载、校验并换上某节点的独立证书。sni 缺省取该节点当前生效的 SNI。
node_cert_fetch() { # <mode> <intag> <cert_url> <key_url> [sni]
    local sni
    if [[ $# -ge 5 ]]; then sni="$5"; else sni="$(node_sni "$2")"; fi
    cert_pair_update "$1" "$(node_cert_path "$2")" "$(node_cert_key_path "$2")" "$3" "$4" \
        "节点 $2（SNI ${sni:-未知}）的证书" "$(printf '%s\t%s' "$2" "${sni}")"
}

# add 时、节点落盘之前：先把证书下好。失败时不留文件（cert_pair_update 自己清临时文件，
# 目标文件只有成功时才出现）。
node_cert_prepare() { # <intag> <cert_url> <key_url> <sni>
    cert_tools_ready || return 1
    echo -e "${green}正在下载节点 $1 的独立证书...${plain}"
    node_cert_fetch force "$1" "$2" "$3" "$4"
}

# add 成功（节点已生效）之后：登记来源、装 timer。
node_cert_commit() { # <intag> <cert_url> <key_url>
    if ! node_cert_registry_set "$1" "$2" "$3"; then
        echo -e "${red}写入 ${NODE_CERT_SOURCE} 失败：节点 $1 的证书已就位，但不会自动更新${plain}"
        return 1
    fi
    node_cert_timer_sync
    echo -e "${green}节点 $1 使用独立远程证书，每天检查一次，剩余不足 ${CERT_RENEW_DAYS} 天时自动下载更新${plain}"
}

# 节点没加成或已被删除：删掉它的独立证书与登记。没有就什么都不做。
node_cert_forget() { # <intag>
    local had=0
    [[ -e "$(node_cert_path "$1")" || -e "$(node_cert_key_path "$1")" ]] && had=1
    node_cert_registered "$1" && had=1
    ((had)) || return 0
    rm -f "$(node_cert_path "$1")" "$(node_cert_key_path "$1")" \
        "${NODE_CERT_DIR}/.$1-prev.cert" "${NODE_CERT_DIR}/.$1-prev.key"
    node_cert_registry_del "$1" || true
    node_cert_timer_sync
}

# 定时任务调用的就是不带参数的这个：逐个检查所有登记的节点。
node_cert_update_all() { # [--force]
    local mode="auto" tag rc=0
    [[ "${1:-}" == "--force" ]] && mode="force"
    node_require_jq || return 1
    if [[ -z "$(node_cert_tags)" ]]; then
        echo -e "${yellow}没有节点使用独立远程证书，无事可做（用 singr add --node-cert-url 配置）${plain}"
        return 0
    fi
    cert_tools_ready || return 1
    if command -v flock >/dev/null 2>&1; then
        exec 8>"${CONFIG_DIR}/.node-cert-update.lock"
        flock -n 8 || { echo -e "${yellow}[证书] 另一个节点证书更新正在进行，跳过${plain}"; return 0; }
    fi
    while IFS= read -r tag; do
        [[ -n "${tag}" ]] || continue
        if ! node_cert_in_use "${tag}"; then
            echo -e "${yellow}[证书] 节点 ${tag} 已不再使用独立证书（节点被删或证书路径被手改过），跳过。清理：singr node-cert ${tag} --clear${plain}"
            continue
        fi
        node_cert_fetch "${mode}" "${tag}" "$(node_cert_source_get "${tag}" cert_url)" \
            "$(node_cert_source_get "${tag}" key_url)" </dev/null || rc=1
    done < <(node_cert_tags)
    return "${rc}"
}

node_cert_show_one() { # <intag>
    local tag="$1" c
    c="$(node_cert_path "${tag}")"
    echo -e "${green}节点 $(node_desc "${tag}")${plain}"
    echo -e "  证书：${c} + $(node_cert_key_path "${tag}")"
    if [[ -s "${c}" ]]; then
        echo -e "  状态：$(node_cert_status "${c}")"
    else
        echo -e "  状态：${red}不存在${plain}"
    fi
    node_cert_in_use "${tag}" || echo -e "  ${yellow}注意：该节点的 server.json 已不指向这张证书（或节点已不存在）${plain}"
    if node_cert_registered "${tag}"; then
        echo "  证书地址：$(cert_url_mask "$(node_cert_source_get "${tag}" cert_url)")"
        echo "  私钥地址：$(cert_url_mask "$(node_cert_source_get "${tag}" key_url)")"
    else
        echo -e "  ${yellow}来源：未登记，不会自动更新${plain}"
    fi
}

node_cert_show_all() {
    local tag any=0
    while IFS= read -r tag; do
        [[ -n "${tag}" ]] || continue
        any=1
        node_cert_show_one "${tag}"
    done < <(node_cert_tags)
    if ((any == 0)); then
        echo -e "${yellow}没有节点使用独立远程证书（singr add --node-cert-url URL --node-key-url URL 添加）${plain}"
        return 0
    fi
    if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet "${NODE_CERT_TIMER}.timer"; then
        echo -e "每日检查：${green}已启用${plain}（剩余不足 ${CERT_RENEW_DAYS} 天时下载）"
    else
        echo -e "每日检查：${red}未启用${plain}"
    fi
}

# singr node-cert [<@序号|NodeID|InTag> [--cert-url URL --key-url URL | --force | --clear]]
node_cert_cmd() {
    local sel="" cert_url="" key_url="" force=0 clear=0 tag
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --cert-url) cert_url="$2"; shift 2 ;;
            --key-url) key_url="$2"; shift 2 ;;
            --force) force=1; shift ;;
            --clear) clear=1; shift ;;
            -*)
                echo -e "${red}未知参数：$1${plain}"
                echo "用法：singr node-cert [<@序号|NodeID|InTag> [--cert-url URL --key-url URL | --force | --clear]]"
                return 1
                ;;
            *) sel="$1"; shift ;;
        esac
    done
    node_require_jq || return 1

    if [[ -z "${sel}" ]]; then
        node_cert_show_all
        return 0
    fi
    # 已登记的 tag 直接认（节点被手动删掉后也要能 --clear）；否则按节点选择器解析。
    if node_cert_registered "${sel}"; then
        tag="${sel}"
    else
        tag="$(node_resolve_tag "${sel}")" || return 1
    fi

    if ((clear)); then
        node_cert_registry_del "${tag}" || { echo -e "${red}写入 ${NODE_CERT_SOURCE} 失败${plain}"; return 1; }
        if node_cert_in_use "${tag}"; then
            echo -e "${green}已清除节点 ${tag} 的证书来源。证书文件保留（节点还在用），不再自动更新${plain}"
        else
            rm -f "$(node_cert_path "${tag}")" "$(node_cert_key_path "${tag}")"
            echo -e "${green}已清除节点 ${tag} 的证书来源和证书文件${plain}"
        fi
        node_cert_timer_sync
        return 0
    fi

    if [[ -n "${cert_url}" || -n "${key_url}" ]]; then
        [[ -n "${cert_url}" && -n "${key_url}" ]] || { echo -e "${red}--cert-url 与 --key-url 必须同时给出${plain}"; return 1; }
        if ! node_cert_in_use "${tag}"; then
            echo -e "${red}节点 ${tag} 没有使用独立证书，不能只换来源。${plain}"
            echo -e "${yellow}要改用独立证书：singr del 后用 --node-cert-url/--node-key-url 重新添加${plain}"
            return 1
        fi
        node_cert_check_urls "${cert_url}" "${key_url}" || return 1
        cert_tools_ready || return 1
        node_cert_fetch force "${tag}" "${cert_url}" "${key_url}" || return 1
        node_cert_registry_set "${tag}" "${cert_url}" "${key_url}" || {
            echo -e "${red}写入 ${NODE_CERT_SOURCE} 失败（证书已更新，但不会自动续期）${plain}"
            return 1
        }
        node_cert_timer_sync
        echo -e "${green}已更换节点 ${tag} 的证书来源${plain}"
        return 0
    fi

    if ((force)); then
        node_cert_registered "${tag}" || { echo -e "${red}节点 ${tag} 没有登记独立证书来源${plain}"; return 1; }
        node_cert_in_use "${tag}" || { echo -e "${red}节点 ${tag} 已不再使用独立证书${plain}"; return 1; }
        cert_tools_ready || return 1
        node_cert_fetch force "${tag}" "$(node_cert_source_get "${tag}" cert_url)" "$(node_cert_source_get "${tag}" key_url)"
        return
    fi

    node_cert_show_one "${tag}"
}

# 菜单入口。
node_cert_menu() {
    local sel u1 u2
    node_cert_show_all
    [[ -t 0 ]] || return 0
    echo -e "
  1. 更换某个节点的证书来源
  2. 立即更新全部节点证书
  0. 返回
"
    read -r -p "请输入选择 [0-2]: " sel
    case "${sel}" in
        1)
            read -r -p "节点（@序号 / NodeID / InTag）: " sel
            read -r -p "证书地址 (https://...): " u1
            read -r -p "私钥地址 (https://...): " u2
            [[ -n "${sel}" && -n "${u1}" && -n "${u2}" ]] || { echo -e "${red}三项都要填${plain}"; return 1; }
            node_cert_cmd "${sel}" --cert-url "${u1}" --key-url "${u2}"
            ;;
        2) node_cert_update_all --force ;;
        *) return 0 ;;
    esac
}
# <<<<<<<<<<<<<<<< SYNC BLOCK: 节点独立证书 <<<<<<<<<<<<<<<<
