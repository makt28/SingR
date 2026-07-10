package constant

// Version 是 sing-box 核心版本。SingR 的构建（release.yml / Makefile / Dockerfile）
// 只用 ldflags 注入 SingR 自己的版本（poet/constant.Version），从不注入核心版本，
// 所以这里的默认值就是 `singr version` 和启动日志里显示的「sing-box core version」。
// 保持与当前同步的上游基线一致（见 AGENTS.md / memory 的 sing-box 基线记录）；
// 每次 resync 上游核心时一并更新此处。
var Version = "1.13.14"
