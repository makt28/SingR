#!/usr/bin/env bash
# 默认配置模板一致性的回归测试。
#
# 同一份默认 inbound（anytls / hysteria2 / naive）散落在五个地方，没有哪个是从另一个
# 生成的，只能靠人肉保持一致：
#   1. release/poet/server.json            裸机示例 / 默认配置
#   2. release/poet/server-docker.json     镜像里的 server.default.json
#   3. install.sh 里的 heredoc             安装时实际写到 /etc/singr/server.json 的那份
#   4. release/poet/server_<协议>.json     单协议示例
#   5. node_inbound_template（SYNC BLOCK「节点管理」）   singr add 新增 inbound 的模板
# 漏改一处的后果不会报错，只会让"新装的机器"和"后加的节点"默默用上不同的默认值
# （例如 hysteria2 的 up_mbps/down_mbps）。本脚本把这五处钉在一起。
#
#   bash scripts/test-templates.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

FAILED=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; FAILED=1; }
same() { # <说明> <文件 a> <文件 b>
    if diff -u "$2" "$3" >"${TMP}/diff.out" 2>&1; then
        pass "$1"
    else
        fail "$1"
        sed 's/^/         /' "${TMP}/diff.out"
    fi
}

command -v jq >/dev/null 2>&1 || { echo "需要 jq"; exit 1; }

SERVER="${REPO_DIR}/release/poet/server.json"
SERVER_DOCKER="${REPO_DIR}/release/poet/server-docker.json"

# ---- install.sh 的 heredoc ----
awk '/cat >"\$\{CONFIG_DIR\}\/server.json" <<.EOF./{p=1;next} p&&/^EOF$/{exit} p' \
    "${REPO_DIR}/install.sh" >"${TMP}/install-server.json"
if [[ ! -s "${TMP}/install-server.json" ]] || ! jq -e . "${TMP}/install-server.json" >/dev/null 2>&1; then
    fail "从 install.sh 抠不出 server.json heredoc（heredoc 的写法变了？同步改本测试）"
else
    jq -S . "${TMP}/install-server.json" >"${TMP}/a.json"
    jq -S . "${SERVER}" >"${TMP}/b.json"
    same "install.sh 的 heredoc 与 release/poet/server.json 整份一致" "${TMP}/a.json" "${TMP}/b.json"
fi

# ---- 裸机 vs docker：只允许 log.output 不同 ----
jq -S 'del(.log.output)' "${SERVER}" >"${TMP}/a.json"
jq -S 'del(.log.output)' "${SERVER_DOCKER}" >"${TMP}/b.json"
same "server-docker.json 与 server.json 只差 log.output" "${TMP}/a.json" "${TMP}/b.json"

# ---- 抠出 node_inbound_template ----
BLOCK_START="$(grep -n '^node_inbound_template() {' "${REPO_DIR}/SingR.sh" | head -1 | cut -d: -f1)"
if [[ -z "${BLOCK_START}" ]]; then
    fail "SingR.sh 里找不到 node_inbound_template"
else
    awk -v s="${BLOCK_START}" 'NR>=s{print} NR>s && /^}/{exit}' "${REPO_DIR}/SingR.sh" >"${TMP}/tmpl.sh"
    # shellcheck disable=SC1091
    source "${TMP}/tmpl.sh"
fi

# ---- 每个协议：五处的 inbound 模板逐字段一致（tag 除外，模板里是空的） ----
for proto in anytls hysteria2 naive; do
    jq -S --arg p "${proto}" 'first(.inbounds[] | select(.type==$p)) | del(.tag)' "${SERVER}" >"${TMP}/want.json"
    if [[ ! -s "${TMP}/want.json" ]] || [[ "$(cat "${TMP}/want.json")" == "null" ]]; then
        fail "${proto}: release/poet/server.json 里没有这个协议的 inbound"
        continue
    fi

    single="${REPO_DIR}/release/poet/server_${proto}.json"
    if [[ -f "${single}" ]]; then
        jq -S --arg p "${proto}" 'first(.inbounds[] | select(.type==$p)) | del(.tag)' "${single}" >"${TMP}/got.json"
        same "${proto}: server_${proto}.json 与 server.json 的 inbound 一致" "${TMP}/want.json" "${TMP}/got.json"
    else
        fail "${proto}: 缺少 release/poet/server_${proto}.json"
    fi

    if declare -F node_inbound_template >/dev/null; then
        if node_inbound_template "${proto}" | jq -S 'del(.tag)' >"${TMP}/got.json" 2>/dev/null; then
            same "${proto}: node_inbound_template 与 server.json 的 inbound 一致" "${TMP}/want.json" "${TMP}/got.json"
        else
            fail "${proto}: node_inbound_template 没有输出合法 JSON"
        fi
    fi
done

echo
if [[ "${FAILED}" -eq 0 ]]; then
    echo "全部通过"
else
    echo "有失败项"
fi
exit "${FAILED}"
