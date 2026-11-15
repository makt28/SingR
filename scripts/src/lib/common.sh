# >>>>>>>>>>>>>>>> SYNC BLOCK: 通用函数 >>>>>>>>>>>>>>>>
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

# TCP 拥塞控制是不是 BBR。naive 的 HTTP/2 和 anytls 都跑在 TCP 上，下载方向用的是
# 服务端内核的拥塞算法，cubic 在跨境丢包线路上掉速很明显。docker 版走 host 网络，
# 看的同样是宿主机内核。读不到（非 Linux）就不显示。
show_bbr_status() {
    local cc
    cc="$(cat /proc/sys/net/ipv4/tcp_congestion_control 2>/dev/null)"
    [[ -n "${cc}" ]] || return 0
    if [[ "${cc}" == bbr* ]]; then
        echo -e "BBR：${green}已开启${plain}"
    else
        echo -e "BBR：${red}未开启${plain}（当前 ${cc}）"
        echo -e "  ${yellow}在 /etc/sysctl.conf 加入 net.core.default_qdisc=fq 和 net.ipv4.tcp_congestion_control=bbr，再执行 sysctl -p${plain}"
    fi
}
# <<<<<<<<<<<<<<<< SYNC BLOCK: 通用函数 <<<<<<<<<<<<<<<<
