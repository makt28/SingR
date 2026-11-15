#!/usr/bin/env bash
# SingR 自有部分的全部检查，本地和 CI（.github/workflows/singr.yml）共用这一个入口。
#
# 上游带来的 test.yml / lint.yml 只在 stable/testing/unstable/oldstable 分支触发，
# SingR 的 main 上从来不跑；scripts/test-*.sh 更是没有任何 workflow 调用。结果是
# 回归测试只在有人记得手动跑的时候才生效（test-install-migrate.sh 就这样红了五周）。
#
#   bash scripts/test-all.sh            全部
#   bash scripts/test-all.sh shell      只跑脚本侧（生成物校验 + scripts/test-*.sh）
#   bash scripts/test-all.sh go         只跑 Go 侧（SingR 自有包，-race）
#
# 环境变量：
#   SINGR_GO_TEST_FLAGS   追加给 go test 的参数（默认 -race）
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_DIR}" || exit 1

# SingR 自己写或改过的 Go 包。上游其余的包由上游自己的 CI 负责，这里不重复跑：
# 一是慢，二是其中不少需要 root / 特定平台。新增 SingR 自有包时加到这里。
GO_PACKAGES=(
    ./poet/...
    ./sim/...
    ./protocol/anytls/...
    ./protocol/hysteria2/...
    ./protocol/naive/...
    ./cmd/sing-box/...
    ./service/derp/...
)

FAILED=()
run() { # <名字> <命令...>
    local name="$1"; shift
    echo "==> ${name}"
    if "$@"; then
        echo "    通过"
    else
        echo "    失败：${name}"
        FAILED+=("${name}")
    fi
}

shell_checks() {
    run "生成物与 scripts/src 一致" bash scripts/build-scripts.sh --check
    local f
    for f in SingR.sh SingR-docker.sh install.sh install-docker.sh docker-entrypoint.sh scripts/*.sh; do
        run "bash -n ${f}" bash -n "${f}"
    done
    for f in scripts/test-*.sh; do
        [[ "${f}" == "scripts/test-all.sh" ]] && continue
        run "${f}" bash "${f}"
    done
}

go_checks() {
    local tags
    tags="$(cat release/DEFAULT_BUILD_TAGS_OTHERS)"
    # shellcheck disable=SC2086
    run "go vet（SingR 自有包）" go vet -tags "${tags}" "${GO_PACKAGES[@]}"
    # shellcheck disable=SC2086
    run "go test（SingR 自有包）" go test -tags "${tags}" ${SINGR_GO_TEST_FLAGS--race} "${GO_PACKAGES[@]}"
}

case "${1:-all}" in
    all) shell_checks; go_checks ;;
    shell) shell_checks ;;
    go) go_checks ;;
    *) echo "用法：bash scripts/test-all.sh [all|shell|go]" >&2; exit 2 ;;
esac

echo
if [[ ${#FAILED[@]} -eq 0 ]]; then
    echo "全部通过"
    exit 0
fi
echo "失败 ${#FAILED[@]} 项："
printf '  - %s\n' "${FAILED[@]}"
exit 1
