#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../install.sh
source "${ROOT_DIR}/install.sh"

TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "${TEST_ROOT}"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

assert_jq() {
    local expression="$1"
    local file="$2"
    jq -e "${expression}" "${file}" >/dev/null || fail "jq assertion failed: ${expression}"
}

test_legacy_migration_and_idempotency() {
    CONFIG_DIR="${TEST_ROOT}/legacy"
    mkdir -p "${CONFIG_DIR}"
    cat >"${CONFIG_DIR}/server.json" <<'JSON'
{
  "outbounds": [
    {"type":"direct","tag":"legacy","domain_strategy":"ipv4_only"},
    {"type":"direct","tag":"default"},
    {"type":"block","tag":"blocked"}
  ],
  "route": {
    "auto_detect_interface": true,
    "rules": [{"inbound":"anytls-in","outbound":"legacy"}]
  }
}
JSON

    migrate_config
    local cfg="${CONFIG_DIR}/server.json"
    assert_jq '.dns.strategy == "prefer_ipv6"' "${cfg}"
    assert_jq '.dns.servers[0].tag == "google"' "${cfg}"
    assert_jq '(.outbounds[] | select(.tag=="legacy") | .domain_resolver.strategy) == "ipv4_only"' "${cfg}"
    assert_jq '(.outbounds[] | select(.tag=="default") | .domain_resolver.strategy) == "prefer_ipv6"' "${cfg}"
    assert_jq '[.outbounds[] | has("domain_strategy")] | any | not' "${cfg}"
    assert_jq '(.outbounds[] | select(.tag=="blocked")) == {"type":"block","tag":"blocked"}' "${cfg}"
    assert_jq '.route.auto_detect_interface == false' "${cfg}"
    assert_jq '.route.rules == [{"inbound":"anytls-in","outbound":"legacy"}]' "${cfg}"

    local backups
    backups=("${cfg}".bak.*)
    [[ ${#backups[@]} -eq 1 && -f "${backups[0]}" ]] || fail "expected exactly one migration backup"
    local before
    before="$(jq -S . "${cfg}")"
    migrate_config
    local after
    after="$(jq -S . "${cfg}")"
    [[ "${before}" == "${after}" ]] || fail "second migration changed config"
    backups=("${cfg}".bak.*)
    [[ ${#backups[@]} -eq 1 ]] || fail "idempotent migration created another backup"
}

test_existing_resolver_is_preserved() {
    CONFIG_DIR="${TEST_ROOT}/existing"
    mkdir -p "${CONFIG_DIR}"
    cat >"${CONFIG_DIR}/server.json" <<'JSON'
{
  "dns":{"servers":[{"tag":"cloudflare","type":"tls","server":"1.1.1.1"}],"strategy":"ipv4_only"},
  "outbounds":[{"type":"direct","tag":"direct","domain_resolver":{"server":"cloudflare","strategy":"ipv4_only"}}],
  "route":{"auto_detect_interface":false}
}
JSON
    local cfg="${CONFIG_DIR}/server.json"
    local before
    before="$(jq -S . "${cfg}")"
    migrate_config
    local after
    after="$(jq -S . "${cfg}")"
    [[ "${before}" == "${after}" ]] || fail "existing resolver was overwritten"
    if compgen -G "${cfg}.bak.*" >/dev/null; then
        fail "no-op migration created backup"
    fi
}

test_invalid_json_is_left_untouched() {
    CONFIG_DIR="${TEST_ROOT}/invalid"
    mkdir -p "${CONFIG_DIR}"
    local cfg="${CONFIG_DIR}/server.json"
    printf '{invalid-json\n' >"${cfg}"
    local before
    before="$(cat "${cfg}")"
    migrate_config
    [[ "$(cat "${cfg}")" == "${before}" ]] || fail "invalid JSON was modified"
    if compgen -G "${cfg}.bak.*" >/dev/null; then
        fail "invalid JSON created backup"
    fi
}

test_missing_jq_skips_safely() {
    CONFIG_DIR="${TEST_ROOT}/no-jq"
    mkdir -p "${CONFIG_DIR}"
    local cfg="${CONFIG_DIR}/server.json"
    printf '{"outbounds":[{"type":"direct"}]}' >"${cfg}"
    local before old_path
    before="$(cat "${cfg}")"
    old_path="${PATH}"
    PATH="${TEST_ROOT}/empty-path"
    migrate_config
    PATH="${old_path}"
    [[ "$(cat "${cfg}")" == "${before}" ]] || fail "missing-jq path modified config"
    if compgen -G "${cfg}.bak.*" >/dev/null; then
        fail "missing-jq path created backup"
    fi
}

test_legacy_migration_and_idempotency
test_existing_resolver_is_preserved
test_invalid_json_is_left_untouched
test_missing_jq_skips_safely

echo "PASS: install.sh migrate_config tests"
