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
