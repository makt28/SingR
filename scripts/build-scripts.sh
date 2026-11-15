#!/usr/bin/env bash
# 从 scripts/src/ 生成仓库根目录的 SingR.sh 和 SingR-docker.sh。
#
# 为什么要生成：两个管理脚本都是单文件分发的（update_shell 用 curl 只拉一个文件，
# 装到 /usr/bin/SingR），没法在运行时 source 公共库。以前的做法是把公共部分在两个
# 文件里各放一份、靠人肉保持逐字一致；现在公共部分只在 scripts/src/lib/ 里存一份，
# 两个后端各自的部分在 scripts/src/*.tmpl 里，用 `#@include lib/<文件>` 引用。
#
# 生成物照常提交进仓库（服务器是从 raw.githubusercontent.com 拉成品的）。
#
#   bash scripts/build-scripts.sh           重新生成两个脚本
#   bash scripts/build-scripts.sh --check   只校验：成品与源不一致就以非零退出（CI 用）
#
# 改脚本的流程：改 scripts/src/ → 跑本脚本 → 两个成品一起提交。直接改成品会在下次
# 生成时被覆盖，--check 也会报出来。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
SRC_DIR="${SCRIPT_DIR}/src"
TARGETS=(SingR.sh SingR-docker.sh)

CHECK=0
case "${1:-}" in
    "") ;;
    --check) CHECK=1 ;;
    *) echo "用法：bash scripts/build-scripts.sh [--check]" >&2; exit 2 ;;
esac

# render <模板> —— 把成品写到标准输出。
#   - 第 1 行（shebang）之后插入"自动生成"说明；
#   - `#@include lib/x.sh` 整行替换成该文件的内容，其余行原样输出。
render() {
    local template="$1" name
    while IFS= read -r name; do
        [[ -f "${SRC_DIR}/${name}" ]] || { echo "${template}: 找不到被引用的 ${SRC_DIR}/${name}" >&2; return 1; }
    done < <(sed -n 's/^#@include[[:space:]]\{1,\}//p' "${template}")
    awk -v src="${SRC_DIR}" -v tmpl="$(basename "${template}")" '
        /^#@include[ \t]+/ {
            file = src "/" $2
            while ((getline line < file) > 0) print line
            close(file)
            next
        }
        { print }
        NR == 1 {
            print "#"
            print "# 本文件由 scripts/build-scripts.sh 从 scripts/src/" tmpl " 和 scripts/src/lib/ 生成，"
            print "# 请勿直接编辑：改 scripts/src/ 下的源文件，再运行 bash scripts/build-scripts.sh。"
        }
    ' "${template}"
}

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

status=0
for target in "${TARGETS[@]}"; do
    template="${SRC_DIR}/${target}.tmpl"
    [[ -f "${template}" ]] || { echo "找不到模板 ${template}" >&2; exit 1; }
    render "${template}" >"${TMP}/${target}"
    bash -n "${TMP}/${target}" || { echo "${target}: 生成结果有语法错误，未写入" >&2; exit 1; }

    if [[ "${CHECK}" -eq 1 ]]; then
        if ! cmp -s "${TMP}/${target}" "${REPO_DIR}/${target}"; then
            echo "${target} 与 scripts/src/ 不一致：" >&2
            diff -u "${REPO_DIR}/${target}" "${TMP}/${target}" | head -40 >&2 || true
            status=1
        fi
    elif cmp -s "${TMP}/${target}" "${REPO_DIR}/${target}"; then
        echo "${target}：无变化"
    else
        # 保留成品原有的权限位（仓库里是可执行的）。
        cat "${TMP}/${target}" >"${REPO_DIR}/${target}"
        echo "${target}：已更新"
    fi
done

if [[ "${CHECK}" -eq 1 ]]; then
    if [[ "${status}" -eq 0 ]]; then
        echo "SingR.sh / SingR-docker.sh 与 scripts/src/ 一致"
    else
        echo "请运行 bash scripts/build-scripts.sh 重新生成后一起提交（不要直接改成品）" >&2
    fi
fi
exit "${status}"
