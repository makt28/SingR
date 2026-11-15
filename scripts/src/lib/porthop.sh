# >>>>>>>>>>>>>>>> SYNC BLOCK: 端口跳跃 >>>>>>>>>>>>>>>>
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
# <<<<<<<<<<<<<<<< SYNC BLOCK: 端口跳跃 <<<<<<<<<<<<<<<<
