# SingR

SingR 是基于 sing-box 的 SSPanel 后端，面向旧版 SSPanel `/mod_mu` 接口。

它的主要用途是：在 SSPanel 里继续把节点配置成 `V2ray`，但当节点地址使用旧格式 WebSocket 并带有 `path=/anytls` 时，SingR 会把这个节点作为 sing-box `anytls` 入站运行。

## 工作方式

SSPanel 节点地址示例：

```text
sa.example.com;14555;0;ws;;path=/anytls|host=example.com|relay_server=relay.example.com|relay_port=42132
```

SingR 会按旧 SSPanel V2ray 格式解析：

- `14555` 作为监听端口。
- `ws` 表示旧版 WebSocket 传输模式。
- `path=/anytls` 或 `path=anytls` 会触发 AnyTLS 兼容模式。
- `host=example.com` 会作为 AnyTLS TLS `server_name`。
- `relay_server` 和 `relay_port` 会被解析保存，但当前不会自动生成 relay 出站或路由。

触发 AnyTLS 兼容模式后：

- 面板节点类型仍可保持为 `V2ray`。
- SingR 内部有效节点类型会变为 `anytls`。
- 用户认证密码优先使用 SSPanel 用户的 `uuid`，为空时才回退到 `passwd`。
- 流量和在线 IP 仍会上报到旧 SSPanel `/mod_mu` 接口。

## 环境要求

- Linux 服务器，推荐 systemd 环境。
- Go `1.24.7` 或兼容版本，用于源码编译。
- 一个可访问的旧 SSPanel 面板。
- SSPanel 节点类型配置为 `V2ray`。
- AnyTLS 需要 TLS 证书。生产环境建议使用可信证书；自签证书需要客户端开启允许不安全证书或手动信任证书。

## 编译安装

也可以使用安装脚本：

```sh
sudo bash install.sh
```

脚本会优先使用当前源码目录下已编译好的 `sing-box`，没有二进制时会尝试从源码编译。也可以显式指定二进制：

```sh
sudo env SINGR_BINARY=/path/to/sing-box bash install.sh
```

如果已经发布到 GitHub Release，可以指定仓库和版本下载安装：

```sh
sudo env SINGR_RELEASE_REPO=owner/repo bash install.sh v0.3.1
```

正式发布后，也可以直接拉取最新版本安装：

```sh
sudo bash <(curl -Ls https://raw.githubusercontent.com/makt28/SingR/main/install.sh)
```

脚本会安装二进制到 `/usr/local/SingR/singr`，安装管理命令到 `/usr/bin/SingR` 和 `/usr/bin/singr`，生成 `/etc/singr/panel.json`、`/etc/singr/server.json` 和 `singr.service`。已有配置不会被覆盖。

> 0.2.5 起，`install.sh` / `singr update` 在保留已有 `server.json` 的同时会自动迁移老配置：补上 `dns` 块、给 `direct` 出站加 `domain_strategy: prefer_ipv6`、关闭 `auto_detect_interface`，老配置会被备份成 `server.json.bak.<时间戳>`。这是为了让节点出口正确走 IPv6（旧默认配置只会走 IPv4）。

`SingR` 和 `singr` 两个管理命令等价，大小写都可以。

管理命令示例：

```sh
singr status
singr log
singr update
singr restart
```

手动编译安装方式如下。

克隆或上传源码后，在项目根目录执行：

```sh
go mod download
make build
```

编译完成后会在当前目录生成 `sing-box` 二进制。建议安装为 `/usr/local/bin/singr`：

```sh
sudo install -m 755 ./sing-box /usr/local/bin/singr
```

创建配置目录：

```sh
sudo mkdir -p /etc/singr/certs /var/log
```

复制示例配置：

```sh
sudo cp release/poet/panel_anytls.json /etc/singr/panel.json
sudo cp release/poet/server_anytls.json /etc/singr/server.json
```

## 配置 SSPanel 节点

在 SSPanel 中把节点类型保持为 `V2ray`，节点地址填写旧格式：

```text
你的节点域名;监听端口;0;ws;;path=/anytls|host=TLS域名
```

例如：

```text
sa.example.com;14555;0;ws;;path=/anytls|host=example.com
```

如果你需要保留 relay 元数据，可以追加：

```text
sa.example.com;14555;0;ws;;path=/anytls|host=example.com|relay_server=relay.example.com|relay_port=42132
```

注意：`relay_server` 和 `relay_port` 目前只会被解析保存，不会自动接管转发逻辑。

## 配置面板连接

编辑 `/etc/singr/panel.json`：

```json
{
  "name": "connect old sspanel v2ray node and run it as anytls",
  "nodes": [
    {
      "paneltype": "SSpanel",
      "intag": "anytls-in",
      "outtag": "anytls-out",
      "apiconfig": {
        "apihost": "https://your-sspanel.example.com",
        "apikey": "your-apikey",
        "nodeid": 1,
        "nodetype": "V2ray",
        "disablecustomconfig": true
      }
    }
  ]
}
```

字段说明：

- `paneltype`：旧 SSPanel 使用 `SSpanel`。
- `intag`：必须和 sing-box 主配置里的入站 `tag` 一致。
- `outtag`：对应路由使用的出站 `tag`。
- `apihost`：SSPanel 地址，不要以 `/mod_mu` 结尾。
- `apikey`：面板 API Key。
- `nodeid`：SSPanel 节点 ID。
- `nodetype`：保持为 `V2ray`。

SingR 请求旧 SSPanel 时会同时带上 `key=<apikey>` 和 `muKey=<apikey>`，兼容 XrayR v0.9.0 的旧接口行为。

## 配置 sing-box 入站

编辑 `/etc/singr/server.json`。AnyTLS 模式下必须提前声明一个 `anytls` 入站，`tag` 要和 `panel.json` 的 `intag` 一致：

```json
{
  "log": {
    "disabled": false,
    "level": "info",
    "timestamp": true,
    "output": "/var/log/singr.log"
  },
  "dns": {
    "servers": [
      { "tag": "google", "type": "udp", "server": "8.8.8.8" }
    ],
    "strategy": "prefer_ipv6"
  },
  "inbounds": [
    {
      "type": "anytls",
      "tag": "anytls-in",
      "listen": "::",
      "listen_port": 0,
      "users": [],
      "tls": {
        "enabled": true,
        "server_name": "",
        "certificate_path": "/etc/singr/certs/anytls.crt",
        "key_path": "/etc/singr/certs/anytls.key"
      }
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "anytls-out",
      "domain_strategy": "prefer_ipv6"
    },
    {
      "type": "direct",
      "tag": "direct",
      "domain_strategy": "prefer_ipv6"
    }
  ],
  "route": {
    "rules": [
      {
        "inbound": "anytls-in",
        "outbound": "anytls-out"
      }
    ],
    "final": "direct",
    "auto_detect_interface": false
  }
}
```

启动时，如果面板节点被识别为 AnyTLS 兼容模式：

- 如果 SSPanel 节点地址里解析到有效端口，`listen_port` 会被该端口覆盖。
- 如果 SSPanel 节点地址里存在非空 `host=`，`tls.server_name` 会被该值覆盖。
- 证书路径和私钥路径仍然来自本地 `server.json`，不会从面板获取。

也就是说，本地 JSON 可以先写默认值；只有面板对应字段非空、有效时才会替换本地值。

从 0.2.5 起，面板的 `port` 和 `host=` 改动支持运行中热更新：监听端口变化时会先在新端口起 listener、成功后才关掉旧端口（失败自动回滚保留旧 listener）；SNI 变化只重建 TLS、不重启 listener。日志里会出现 `anytls listener hot-reloaded to port ...` 或 `anytls TLS hot-reloaded with SNI ...`。**证书材料、入站类型、路由规则仍然不会被热更新**。

### 出口 IPv6

默认配置里的 `direct` 出站都设了 `domain_strategy: prefer_ipv6`，并配了 `dns.strategy: prefer_ipv6`。如果你删掉这些字段，sing-box 的串行拨号会在第一个 IPv4 命中后立刻返回，节点出口会退化成 IPv4 only。需要纯 v4 才把 `prefer_ipv6` 换成 `prefer_ipv4` 或显式 `ipv4_only`。

`auto_detect_interface` 在服务器端建议保持 `false`，它是给 client/TUN 场景用的；开着会把 outbound socket 强行绑到默认网卡，并在某些 IPv6-only 目的地下失效。

## 准备 TLS 证书

把证书放到配置中指定的位置：

```sh
sudo cp fullchain.pem /etc/singr/certs/anytls.crt
sudo cp privkey.pem /etc/singr/certs/anytls.key
sudo chmod 600 /etc/singr/certs/anytls.key
```

证书应覆盖 SSPanel 节点地址中的 `host=` 值。没有可信证书时可以临时使用自签证书，但客户端必须允许不安全证书或信任该证书。

## 启动

前台运行：

```sh
env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u all_proxy \
  /usr/local/bin/singr run \
  -c /etc/singr/server.json \
  -p /etc/singr/panel.json
```

如果服务器环境没有代理变量，也可以直接运行：

```sh
/usr/local/bin/singr run -c /etc/singr/server.json -p /etc/singr/panel.json
```

确认监听端口：

```sh
ss -lntp | grep singr
```

日志默认写入：

```text
/var/log/singr.log
```

`SingR log` 会先显示最近的 systemd journal，再跟随 `/var/log/singr.log`。流量上报、在线 IP 上报和用户同步日志由 sing-box logger 写入该文件；`journalctl -u singr` 主要能看到 systemd 和标准输出/错误日志。

## systemd 服务

创建 `/etc/systemd/system/singr.service`：

```ini
[Unit]
Description=SingR SSPanel backend
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/singr run -c /etc/singr/server.json -p /etc/singr/panel.json
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

启用并启动：

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now singr
sudo systemctl status singr
```

查看日志：

```sh
journalctl -u singr -f
```

如果你的系统继承了本地代理环境变量，建议在 service 中加入：

```ini
Environment="http_proxy="
Environment="https_proxy="
Environment="HTTP_PROXY="
Environment="HTTPS_PROXY="
Environment="ALL_PROXY="
Environment="all_proxy="
```

## 客户端配置要点

AnyTLS 客户端需要：

- 服务器地址：你的节点域名。
- 端口：SSPanel 节点地址中的端口。
- SNI：SSPanel 节点地址中的 `host=` 值；没有 `host=` 时使用本地 `server.json` 的 `tls.server_name`。
- 密码：SSPanel 用户的 `uuid`。
- TLS：生产环境使用可信证书；自签证书测试时开启允许不安全证书。

## 用户同步和上报

SingR 会从旧 SSPanel 拉取用户列表，并把用户映射成运行时用户名：

```text
u<用户ID>
```

例如用户 ID `40493` 会显示为 `u40493`。

已支持：

- 新增和删除用户热更新。删除用户前会先把累计流量上报到面板；上报失败会保留用户与字节，下一轮重试，避免计费丢失。
- 已有用户的 UUID/password 变化热更新。
- 节点 `port` / TLS SNI (`host=`) 运行中热更新（先起新 listener 再关老 listener，失败回滚）。
- 流量上报到 `/mod_mu/users/traffic?node_id=<nodeid>`。
- 在线 IP 上报到 `/mod_mu/users/aliveip?node_id=<nodeid>`。

当前未完整接管：

- 节点和用户限速只解析，当前未接入完整限速路径。
- 设备数限制字段会解析，但连接准入路径仍需按部署重新确认。
- `relay_server` 和 `relay_port` 不会自动创建出站和路由。
- 不会从面板动态创建缺失的入站；主配置必须先声明对应 `intag` 的入站。
- 不支持运行中热切换入站类型；TLS 证书材料也仍然只在启动时加载。

## 测试

运行离线测试：

```sh
go test ./poet/... ./cmd/sing-box
```

如果你有本地面板配置，可以运行集成测试：

```sh
SINGR_SSPANEL_CONFIG=/etc/singr/panel.json \
  go test ./poet/api/sspanel -run TestIntegrationGetNodeInfo -v
```

## 常见问题

### 启动后没有监听面板端口

检查：

- SSPanel 节点地址是否包含 `ws` 和 `path=/anytls`。
- `/etc/singr/server.json` 是否声明了 `type: "anytls"` 的入站。
- 入站 `tag` 是否等于 `/etc/singr/panel.json` 里的 `intag`。
- 0.2.5 起 port/SNI 支持热更新，但首次启动仍需要面板返回有效 `port`；如果日志里有 `invalid anytls listen port from panel` 说明面板返回了 0 或越界值。

### 节点出口没有 IPv6

检查：

- 服务器本身能否 `curl -6 ifconfig.co`。如果服务器没有 v6 GUA，无论 SingR 怎么配都没用。
- `server.json` 的 `direct` 出站是否有 `domain_strategy: prefer_ipv6`，`dns` 块是否带 `strategy: prefer_ipv6`，`route.auto_detect_interface` 是否为 `false`。0.2.5 起 `singr update` 会自动迁移老配置，迁移前的备份在 `/etc/singr/server.json.bak.<时间戳>`。
- 如果是从老版本升级上来的，第一次 `singr update` 后必须 `singr restart`。

### 面板连接失败

检查：

- `apihost` 是否能从服务器访问。
- `apikey` 是否正确。
- `nodeid` 是否存在。
- 服务器是否设置了错误的代理环境变量。必要时按启动命令清空 `http_proxy`、`https_proxy`、`ALL_PROXY` 等变量。

### 客户端 TLS 失败

检查：

- 客户端 SNI 是否等于 SSPanel 节点地址中的 `host=`。
- 证书是否覆盖该 SNI。
- 自签证书测试时客户端是否允许不安全证书。

### 用户认证失败

AnyTLS 密码优先使用 SSPanel 用户 `uuid`。请确认客户端填写的是用户 UUID，而不是端口、passwd 或其他字段。

## 许可证

本项目基于 sing-box 修改，遵循上游 GPL-3.0-or-later 许可证。
