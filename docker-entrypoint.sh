#!/usr/bin/env bash
#
# SingR Docker entrypoint.
#
# Renders /etc/singr-docker/{server.json,panel.json} from flags/env on FIRST
# start (never overwrites an existing file — the mounted files are the source
# of truth, exactly like the bare-metal install), then execs the SingR binary.
#
# Certificates are NOT auto-generated: if the active protocol's cert/key is
# missing the container refuses to start, matching the bare-metal binary.
#
# Parameters may be passed as CLI flags (primary) or env vars (fallback):
#   --api-url / SINGR_API_URL              panel apihost           (required*)
#   --api-key / SINGR_API_KEY              panel apikey            (required*)
#   --node-id / SINGR_NODE_ID             panel nodeid (int)      (required*)
#   --protocol / SINGR_PROTOCOL           anytls | hysteria2      (required*)
#   --panel-type / SINGR_PANEL_TYPE       default SSpanel
#   --node-type / SINGR_NODE_TYPE         default V2ray
#   --timeout / SINGR_TIMEOUT             default 20
#   --speed-limit / SINGR_SPEED_LIMIT     default 0
#   --device-limit / SINGR_DEVICE_LIMIT   default 0
#   --enable-device-limit / SINGR_ENABLE_DEVICE_LIMIT   default false
#   --update-periodic / SINGR_UPDATE_PERIODIC           default 60
#   --sni / SINGR_SNI                     inbound server_name (optional)
#   --cert-path / SINGR_CERT_PATH         override cert path (optional)
#   --key-path / SINGR_KEY_PATH           override key path (optional)
#   --log-level / SINGR_LOG_LEVEL         default info
#   (*) only required when panel.json does not already exist.
#
set -euo pipefail

red='\033[0;31m'; green='\033[0;32m'; yellow='\033[0;33m'; plain='\033[0m'
log_info() { echo -e "${green}[singr] $*${plain}"; }
log_warn() { echo -e "${yellow}[singr] $*${plain}"; }
die()      { echo -e "${red}[singr] $*${plain}" >&2; exit 1; }

CONFIG_DIR="${SINGR_CONFIG_DIR:-/etc/singr-docker}"
CERT_DIR="${CONFIG_DIR}/certs"
PANEL="${CONFIG_DIR}/panel.json"
SERVER="${CONFIG_DIR}/server.json"
# 首个节点的协议标记。自多节点支持（singr add）起，证书检查改为遍历 panel.json
# 的 .nodes[].intag，不再依赖这个文件；保留只为向后兼容与排障时看一眼。
PROTO_MARK="${CONFIG_DIR}/.protocol"
DEFAULT_SERVER="/opt/singr/server.default.json"

# --- defaults (env is the fallback layer under CLI flags) -------------------
API_URL="${SINGR_API_URL:-}"
API_KEY="${SINGR_API_KEY:-}"
NODE_ID="${SINGR_NODE_ID:-}"
PROTOCOL="${SINGR_PROTOCOL:-}"
PANEL_TYPE="${SINGR_PANEL_TYPE:-SSpanel}"
NODE_TYPE="${SINGR_NODE_TYPE:-V2ray}"
TIMEOUT="${SINGR_TIMEOUT:-20}"
SPEED_LIMIT="${SINGR_SPEED_LIMIT:-0}"
DEVICE_LIMIT="${SINGR_DEVICE_LIMIT:-0}"
ENABLE_DEVICE_LIMIT="${SINGR_ENABLE_DEVICE_LIMIT:-false}"
UPDATE_PERIODIC="${SINGR_UPDATE_PERIODIC:-60}"
SNI="${SINGR_SNI:-}"
CERT_PATH="${SINGR_CERT_PATH:-}"
KEY_PATH="${SINGR_KEY_PATH:-}"
LOG_LEVEL="${SINGR_LOG_LEVEL:-info}"

# --- escape hatch: `docker run <img> run|version|...` runs the raw binary ----
if [[ $# -gt 0 && "$1" != --* ]]; then
    exec /usr/local/bin/singr "$@"
fi

# --- flag parsing (overrides env) -------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        --api-url) API_URL="$2"; shift 2;;
        --api-key) API_KEY="$2"; shift 2;;
        --node-id) NODE_ID="$2"; shift 2;;
        --protocol) PROTOCOL="$2"; shift 2;;
        --panel-type) PANEL_TYPE="$2"; shift 2;;
        --node-type) NODE_TYPE="$2"; shift 2;;
        --timeout) TIMEOUT="$2"; shift 2;;
        --speed-limit) SPEED_LIMIT="$2"; shift 2;;
        --device-limit) DEVICE_LIMIT="$2"; shift 2;;
        --enable-device-limit) ENABLE_DEVICE_LIMIT="$2"; shift 2;;
        --update-periodic) UPDATE_PERIODIC="$2"; shift 2;;
        --sni) SNI="$2"; shift 2;;
        --cert-path) CERT_PATH="$2"; shift 2;;
        --key-path) KEY_PATH="$2"; shift 2;;
        --log-level) LOG_LEVEL="$2"; shift 2;;
        --) shift; break;;
        *) die "未知参数：$1";;
    esac
done

mkdir -p "${CONFIG_DIR}" "${CERT_DIR}"

normalize_protocol() {
    case "$(echo "${1:-}" | tr '[:upper:]' '[:lower:]')" in
        anytls) echo anytls;;
        hysteria2|hy2) echo hysteria2;;
        "") echo "";;
        *) die "不支持的协议：$1（仅 anytls / hysteria2）";;
    esac
}
PROTOCOL="$(normalize_protocol "${PROTOCOL}")"

# --- seed server.json (stdout logging) once ---------------------------------
if [[ ! -f "${SERVER}" ]]; then
    cp "${DEFAULT_SERVER}" "${SERVER}"
    log_info "已生成默认 sing-box 配置：${SERVER}"
fi

# --- seed panel.json from parameters once -----------------------------------
if [[ ! -f "${PANEL}" ]]; then
    [[ -n "${PROTOCOL}" ]] || die "首次启动缺少 --protocol（anytls / hysteria2）。"
    [[ -n "${API_URL}" ]]  || die "首次启动缺少 --api-url。"
    [[ -n "${API_KEY}" ]]  || die "首次启动缺少 --api-key。"
    [[ "${NODE_ID}" =~ ^[0-9]+$ ]] || die "首次启动缺少合法 --node-id（整数）。"
    [[ "${TIMEOUT}" =~ ^[0-9]+$ ]] || die "--timeout 必须是整数。"
    [[ "${DEVICE_LIMIT}" =~ ^[0-9]+$ ]] || die "--device-limit 必须是整数。"
    [[ "${UPDATE_PERIODIC}" =~ ^[0-9]+$ ]] || die "--update-periodic 必须是整数。"
    [[ "${SPEED_LIMIT}" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "--speed-limit 必须是数字。"
    case "${ENABLE_DEVICE_LIMIT}" in true|false) ;; *) die "--enable-device-limit 必须是 true / false。";; esac

    INTAG="${PROTOCOL}-in"
    OUTTAG="${PROTOCOL}-out"
    jq -n \
        --arg name "SingR docker node (${PROTOCOL})" \
        --arg paneltype "${PANEL_TYPE}" \
        --arg intag "${INTAG}" \
        --arg outtag "${OUTTAG}" \
        --arg apihost "${API_URL}" \
        --arg apikey "${API_KEY}" \
        --argjson nodeid "${NODE_ID}" \
        --arg nodetype "${NODE_TYPE}" \
        --argjson timeout "${TIMEOUT}" \
        --argjson speedlimit "${SPEED_LIMIT}" \
        --argjson devicelimit "${DEVICE_LIMIT}" \
        --argjson updateperiodic "${UPDATE_PERIODIC}" \
        --argjson enabledevicelimit "${ENABLE_DEVICE_LIMIT}" \
        '{
            name: $name,
            nodes: [{
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
            }]
        }' > "${PANEL}"
    echo "${PROTOCOL}" > "${PROTO_MARK}"
    log_info "已生成面板配置：${PANEL}（协议 ${PROTOCOL}，节点 ${NODE_ID}）"

    # Apply optional SNI / cert overrides to the active inbound at seed time.
    tmp="$(mktemp)"
    jq --arg tag "${INTAG}" --arg sni "${SNI}" --arg level "${LOG_LEVEL}" \
       --arg cert "${CERT_PATH}" --arg key "${KEY_PATH}" '
        (.log.level) = $level
        | .inbounds = (.inbounds | map(
            if .tag == $tag then
                (if $sni  != "" then .tls.server_name = $sni else . end)
                | (if $cert != "" then .tls.certificate_path = $cert else . end)
                | (if $key  != "" then .tls.key_path = $key else . end)
            else . end))
       ' "${SERVER}" > "${tmp}" && mv "${tmp}" "${SERVER}"
else
    log_warn "保留已有配置：${PANEL}"
fi

# --- certificate check: refuse to start without it (matches bare-metal) -----
#
# Multi-node aware: panel.json may carry several nodes, each bound to its own
# inbound via `intag` (added with `singr add`). Every referenced inbound is
# checked, not just one protocol's — any single node failing Start() makes
# poet/panel/panel.go os.Exit(1) and takes the whole process down with it, so a
# loud, specific failure here beats a crash-loop with a vaguer error later.
node_tags="$(jq -r '(.nodes // [])[] | (.intag // "") | select(. != "")' "${PANEL}" 2>/dev/null || echo "")"
[[ -n "${node_tags}" ]] || die "panel.json 里没有任何带 intag 的节点，进程会以 'no Node with InTag found' 退出。"

while IFS= read -r tag; do
    [[ -z "${tag}" ]] && continue
    active_cert="$(jq -r --arg t "${tag}" \
        '(first(.inbounds[]? | select(.tag==$t) | .tls.certificate_path)) // ""' "${SERVER}" 2>/dev/null || echo "")"
    active_key="$(jq -r --arg t "${tag}" \
        '(first(.inbounds[]? | select(.tag==$t) | .tls.key_path)) // ""' "${SERVER}" 2>/dev/null || echo "")"
    if [[ -z "${active_cert}" ]]; then
        die "panel.json 的节点引用了 inbound '${tag}'，但 server.json 里没有这个 tag 的 inbound（或它没有 tls 配置）。"
    fi
    if [[ ! -s "${active_cert}" || ! -s "${active_key}" ]]; then
        hint=""
        # 证书路径不在挂载目录内 → 多半是宿主机路径（如 /root/xxx），容器里根本看不到。
        case "${active_cert}" in
            "${CONFIG_DIR}"/*) ;;
            *) hint="
注意：证书路径 ${active_cert} 不在挂载目录 ${CONFIG_DIR} 下。容器只挂载了
${CONFIG_DIR}，宿主机上别处（如 /root/）的文件在容器内不可见。请把证书复制到
${CERT_DIR}/ 下，或用 singr add 的 --cert-path/--key-path（脚本会复制进挂载目录
并登记源路径，之后每次重启自动重新同步）。" ;;
        esac
        die "节点 inbound '${tag}' 缺少 TLS 证书（容器内路径）：${active_cert} / ${active_key}${hint}
与裸机二进制一致：没有证书不会启动。"
    fi
done <<<"${node_tags}"

log_info "启动 SingR：singr run -c ${SERVER} -p ${PANEL}"
exec /usr/local/bin/singr run -c "${SERVER}" -p "${PANEL}"
