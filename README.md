# SingR

项目地址：<https://github.com/makt28/SingR>

SingR 是基于 sing-box 的 SSPanel 后端，面向旧版 SSPanel `/mod_mu` 接口。

它的主要用途是：在 SSPanel 里继续把节点配置成 `V2ray`，但通过节点地址里的 `path` 别名，把节点作为 sing-box 的 **AnyTLS** 或 **Hysteria2** 入站运行：

- `path=/anytls` → AnyTLS 入站（TCP）
- `path=/hy2` → Hysteria2 入站（QUIC/UDP）

两种协议共用同一套用户同步、流量上报、限速、审计逻辑；面板侧两种节点除了 `path` 和 `intag`/`outtag` 之外没有区别（节点类型都保持 `V2ray`）。

## 工作方式

SSPanel 节点地址示例（AnyTLS）：

```text
sa.example.com;14555;0;ws;;path=/anytls|host=example.com|relay_server=relay.example.com|relay_port=42132
```

Hysteria2 只需把 `path` 换成 `hy2`：

```text
sa.example.com;14555;0;ws;;path=/hy2|host=example.com
```

SingR 会按旧 SSPanel V2ray 格式解析：

- `14555` 作为监听端口。
- `ws` 表示旧版 WebSocket 传输模式。
- `path=/anytls`（或 `anytls`）触发 AnyTLS 兼容模式；`path=/hy2`（或 `hy2`）触发 Hysteria2 兼容模式。
- `host=example.com` 会作为 TLS `server_name`。
- `relay_server` 和 `relay_port` 会被解析保存，但当前不会自动生成 relay 出站或路由。

触发兼容模式后：

- 面板节点类型仍可保持为 `V2ray`。
- SingR 内部有效节点类型会变为 `anytls` 或 `hysteria2`。
- 用户认证密码优先使用 SSPanel 用户的 `uuid`，为空时才回退到 `passwd`。
- 用户运行时名固定为 `u<用户ID>`；流量和在线 IP 仍会上报到旧 SSPanel `/mod_mu` 接口。

### 一份 server.json 同时支持两种协议

默认的 `/etc/singr/server.json` 是一个**超集**，同时声明了 `anytls-in` 和 `hysteria2-in` 两个入站。启动时 SingR 只会真正创建 **`panel.json` 里被引用到的那些入站**（按 `intag` 过滤），没用到的协议入站根本不会创建，也就不需要证书、不占端口。

也就是说：**切换 / 增加协议只改 `panel.json`，不用动 `server.json`**。`panel.json` 里有 AnyTLS 节点就起 AnyTLS，有 Hysteria2 节点就起 Hysteria2，两个都有就都起（多节点共存，见下）。

## 环境要求

- Linux 服务器，推荐 systemd 环境。
- Go `1.24.7` 或兼容版本，用于源码编译。
- 一个可访问的旧 SSPanel 面板。
- SSPanel 节点类型配置为 `V2ray`。
- AnyTLS 需要 TLS 证书。生产环境建议使用可信证书；自签证书需要客户端开启允许不安全证书或手动信任证书。

## 编译安装

也可以使用安装脚本（脚本要求 root 运行，下面命令均假设已是 root；非 root 请自行加 `sudo`）：

```sh
bash install.sh
```

脚本会优先使用当前源码目录下已编译好的 `sing-box`，没有二进制时会尝试从源码编译。也可以显式指定二进制：

```sh
env SINGR_BINARY=/path/to/sing-box bash install.sh
```

仓库已内置默认 Release 地址（`makt28/SingR`），发布到 GitHub Release 后直接指定版本即可下载安装：

```sh
bash install.sh v0.3.1
```

如需从其它仓库（fork）下载，再显式覆盖：

```sh
env SINGR_RELEASE_REPO=owner/repo bash install.sh v0.3.1
```

正式发布后，也可以直接拉取最新版本安装：

```sh
bash <(curl -Ls https://raw.githubusercontent.com/makt28/SingR/main/install.sh)
```

> Release 同时提供 `SingR-linux-<arch>.tar.gz` 和 `.zip`；安装脚本优先下载 `tar.gz`（保留可执行权限、不依赖 `unzip`）。脚本还会安装 `vim` 和 `iptables`（v4/v6 同包，供 Hysteria2 端口跳跃管理使用）。

脚本会安装二进制到 `/usr/local/SingR/singr`，安装管理命令到 `/usr/bin/SingR` 和 `/usr/bin/singr`，生成 `/etc/singr/panel.json`、`/etc/singr/server.json` 和 `singr.service`。已有配置不会被覆盖。

> 0.2.5 起，`install.sh` / `singr update` 在保留已有 `server.json` 的同时会自动迁移老配置：补上 `dns` 块、把 `direct` 出站的老 `domain_strategy` 字段迁移成 `domain_resolver`（保留原策略值，缺省 `prefer_ipv6`）、关闭 `auto_detect_interface`，老配置会被备份成 `server.json.bak.<时间戳>`。这是为了让节点出口正确走 IPv6（旧默认配置只会走 IPv4），同时跟上 sing-box 1.12+ 的新配置写法。

`SingR` 和 `singr` 两个管理命令等价，大小写都可以。

管理命令示例：

```sh
singr status
singr log
singr update
singr restart
singr porthop      # Hysteria2 端口跳跃管理（增/删/查跳跃规则）
```

管理菜单里对应的是第 13 项「Hysteria2 端口跳跃管理」。

## 多节点（一个进程带多个面板节点）

一台机器可以同时对接多个面板节点，共用同一个 SingR 进程。裸机和 Docker 用法完全
一致，管理菜单里对应第 14 项「节点管理」。

```sh
singr list                    # 查看当前节点（NodeID / 协议 / 域名 / InTag / 证书状态）

singr add \
  --api-url https://panel-b.example.com \
  --api-key your-apikey \
  --node-id 57 \
  --protocol anytls \
  --sni b.example.com \
  --cert-path /etc/letsencrypt/live/b.example.com/fullchain.pem \
  --key-path  /etc/letsencrypt/live/b.example.com/privkey.pem

singr del @2                  # @序号取自上面 list 的 # 列，最省事
                              # 也可用 NodeID 或 InTag：singr del anytls-in-57
```

`singr add` 不带参数（或从菜单进入）会逐项询问。每个节点独立拥有用户表、流量统计、
限速桶和审计规则，**不同节点的用户 ID 撞车也不会串账**。

> **指定节点优先用 `@序号`。** NodeID 只在单个面板内唯一——对接多个面板时，两个面板
> 各有一个 16 号节点是很正常的。此时 `singr del 16` 会**拒绝执行**并列出候选（带 `@序号`
> 和面板域名），绝不会替你挑一个删掉。`@序号` 和 InTag 则永远唯一。
>
> shell 里 `#` 是注释起始，`singr del #1` 会被吞掉，所以用 `@1`（写 `'#1'` 加引号也认）。

几点必须知道的：

- **端口必须在面板侧错开。** 监听端口由面板下发，两个节点拿到同一个端口时第二个
  监听会起不来——anytls 会回滚到旧配置（随机端口），进程看着健康，节点其实不可达。
- **任一节点起不来会拖垮全部节点。** 同进程内只要有一个节点向面板取信息失败，整个
  进程就会退出并被拉起重试。所以 `singr add` / `singr del` 每次都会先备份配置，改完
  重启并校验，起不来就**自动回滚**到改动前的状态。
- **inbound 标签自动分配**：第一个 anytls 节点用 `anytls-in`，第二个用
  `anytls-in-<节点ID>`，以此类推；hysteria2 同理。
- 只剩一个节点时不允许删除（没有节点进程无法启动）。要换节点请先 `add` 新的、再
  `del` 旧的。

### 证书续期

TLS 证书材料不热重载，**续期后必须重启才生效**，两种部署方式都是如此。

裸机直接引用你给的证书路径（不复制），certbot 原地更新文件即可：

```sh
certbot renew --deploy-hook "singr restart"
```

Docker 因为容器只挂载 `/etc/singr-docker`，宿主机别处（如 `/root/`、
`/etc/letsencrypt/`）的文件容器内看不见，所以证书必须复制进挂载目录。`singr` 会记住
你给的源路径（记在 `/etc/singr-docker/certs.json`），每次 `start` / `restart` /
`update` 之前自动重新复制一遍。挂上这行就全自动了（只有证书真的变了才会重启）：

```sh
certbot renew --deploy-hook "singr cert-sync"
```

> **从旧版本升级上来的 Docker 机器需要补一步。** 证书源只在首次安装和 `singr add` 时
> 登记，老机器上 `certs.json` 并不存在，此时同步是空转的（`singr cert-sync` 检测到未
> 登记会直接告诉你）。给每个已有节点补登记一次即可：
>
> ```sh
> singr cert @1 --cert-path /etc/letsencrypt/live/a.example.com/fullchain.pem \
>               --key-path  /etc/letsencrypt/live/a.example.com/privkey.pem
> ```
>
> 同一个命令也用于更换某个节点的证书源，不必 `del` 再 `add`。


## Docker 部署

除裸机 systemd 安装外，SingR 还提供 Docker 镜像，发布在 **`ghcr.io/makt28/singr`**
（多架构 `linux/amd64` + `linux/arm64`）。

- 稳定版：`ghcr.io/makt28/singr:latest`（发 Release 后自动更新），也可锁定
  `:vX.Y.Z`。
- 配置目录 `/etc/singr-docker`，与裸机的 `/etc/singr` **完全隔离**，互不影响，
  一台机器二选一即可。

> **证书是必须的（三种方式通用）**：和裸机二进制一致，没有 TLS 证书容器不会
> 启动。先建目录并放证书：
> ```sh
> mkdir -p /etc/singr-docker/certs
> # anytls 放 anytls.crt / anytls.key；hysteria2 放 hysteria2.crt / hysteria2.key
> ```
> 也可以自签测试证书（生产建议用真证书，anytls/hy2 对 TLS 指纹敏感）：
> ```sh
> openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
>   -keyout /etc/singr-docker/certs/anytls.key \
>   -out    /etc/singr-docker/certs/anytls.crt -subj "/CN=example.com"
> ```

### 方式一：管理脚本一键（推荐）

自动装 docker、拉镜像、建容器，并装上与裸机**完全一致**的 `singr` 管理命令
（`singr start/stop/restart/config/log/update/porthop`），底层驱动的是容器而非
systemd。

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/makt28/SingR/main/install-docker.sh) \
  --api-url https://your-sspanel.example.com \
  --api-key your-apikey \
  --node-id 44 \
  --protocol anytls          # 或 hysteria2
# 放好证书后： singr restart
```

之后的管理和裸机无差别：

```sh
singr config     # 改 /etc/singr-docker/panel.json，自动 docker restart
singr log        # docker logs -f
singr update     # 拉最新镜像重建容器（也可 singr update v0.5.0 指定版本）
singr porthop    # Hysteria2 端口跳跃
singr uninstall  # 删容器 + 配置 + 端口跳跃规则
```

### 方式二：直接 docker run（soga 风格）

不装管理脚本，纯 `docker run` + 环境变量：

```sh
docker run -d --name singr \
  --network host --restart always \
  --log-opt max-size=10m --log-opt max-file=5 \
  -v /etc/singr-docker:/etc/singr-docker \
  -e SINGR_API_URL=https://your-sspanel.example.com \
  -e SINGR_API_KEY=your-apikey \
  -e SINGR_NODE_ID=44 \
  -e SINGR_PROTOCOL=anytls \
  ghcr.io/makt28/singr:latest
```

管理用原生命令：`docker logs -f singr` / `docker restart singr` / `docker pull ... && docker rm -f singr && <重新 run>`。

### 方式三：docker compose

仓库根目录有 [`docker-compose.yml`](docker-compose.yml)，改好里面的 `SINGR_*`
后：

```sh
mkdir -p singr-data/certs   # 放证书到 singr-data/certs/
docker compose up -d
docker compose logs -f
```

### 参数 / 环境变量对照

flag（方式一）与 `SINGR_*` 环境变量（方式二/三）一一对应，任选其一：

| flag | 环境变量 | 说明 | 默认 |
| --- | --- | --- | --- |
| `--api-url` | `SINGR_API_URL` | 面板 apihost（必填） | |
| `--api-key` | `SINGR_API_KEY` | 面板 apikey（必填） | |
| `--node-id` | `SINGR_NODE_ID` | 节点 ID（必填） | |
| `--protocol` | `SINGR_PROTOCOL` | `anytls` 或 `hysteria2`（必填） | |
| `--sni` | `SINGR_SNI` | 入站 `server_name` | 空 |
| `--cert-path` / `--key-path` | `SINGR_CERT_PATH` / `SINGR_KEY_PATH` | 证书/私钥路径 | 默认协议证书路径 |
| `--speed-limit` / `--device-limit` | `SINGR_SPEED_LIMIT` / `SINGR_DEVICE_LIMIT` | 限速 / 设备数 | 0 |
| `--enable-device-limit` | `SINGR_ENABLE_DEVICE_LIMIT` | 是否硬限设备 | false |
| `--update-periodic` | `SINGR_UPDATE_PERIODIC` | 面板同步周期（秒） | 60 |
| `--image` | —— | 镜像地址（仅方式一） | `ghcr.io/makt28/singr:latest` |

三种方式共同点：`--network=host`（面板动态下发端口、Hysteria2 走 UDP，必须
host 网络）、日志走 stdout（`docker logs` 查看，配合 `--log-opt` 轮转）。首次
启动用上面的参数生成 `/etc/singr-docker/panel.json`，**之后配置以该文件为准**
（改文件 + 重启即可，参数只做首次引导），更新/重建容器配置不丢。

Hysteria2 端口跳跃是宿主机 iptables NAT（host 网络下容器与宿主机共享网络栈），
方式一用 `singr porthop` 管理。方式二/三没有装管理脚本，可以只把脚本取下来用
（**不要**再跑一次 `install-docker.sh`：它检测到已有 `/etc/singr-docker/panel.json`
会直接拒绝，因为重复安装不会让新参数生效，却会先删掉正在服务的容器）：

```sh
curl -fsSL https://raw.githubusercontent.com/makt28/SingR/main/SingR-docker.sh -o /usr/bin/SingR
chmod +x /usr/bin/SingR && ln -sf /usr/bin/SingR /usr/bin/singr
```

或者直接手动配 iptables。

> 首次启动的参数只做引导，**之后一切以 `/etc/singr-docker/panel.json` 为准**。要加
> 节点请用 `singr add`（见上面「多节点」），不要重跑安装脚本。

## 手动编译安装

克隆或上传源码后，在项目根目录执行：

```sh
git clone https://github.com/makt28/SingR.git
cd SingR
go mod download
make build
```

编译完成后会在当前目录生成 `sing-box` 二进制。建议安装为 `/usr/local/bin/singr`：

```sh
install -m 755 ./sing-box /usr/local/bin/singr
```

创建配置目录：

```sh
mkdir -p /etc/singr/certs /var/log
```

复制示例配置：

```sh
cp release/poet/panel_anytls.json /etc/singr/panel.json
cp release/poet/server.json /etc/singr/server.json
```

`release/poet/server.json` 是 anytls + hysteria2 的超集；`panel.json` 决定实际启用哪个（见上文「一份 server.json 同时支持两种协议」）。只想跑 anytls 的话，`panel_anytls.json` 即可；要跑 hysteria2 用 `panel_hysteria2.json`。

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
- `intag`：必须和 sing-box 主配置里的入站 `tag` 一致（AnyTLS 用 `anytls-in`，Hysteria2 用 `hysteria2-in`）。
- `outtag`：对应路由使用的出站 `tag`。
- `apihost`：SSPanel 地址，不要以 `/mod_mu` 结尾。
- `apikey`：面板 API Key。
- `nodeid`：SSPanel 节点 ID。
- `nodetype`：保持为 `V2ray`（AnyTLS / Hysteria2 都是）。

Hysteria2 节点只是把 `intag`/`outtag` 换成 `hysteria2-in`/`hysteria2-out`，其余字段一样（`nodeid` 当然是各自面板节点的 ID）。

### 多节点 / 多协议共存

`nodes` 是数组，一个 SingR 进程可以同时跑多个节点，甚至混协议。例如同机同时跑一个 AnyTLS 和一个 Hysteria2 节点：

```json
{
  "name": "singr",
  "nodes": [
    {
      "paneltype": "SSpanel", "intag": "anytls-in", "outtag": "anytls-out",
      "apiconfig": { "apihost": "https://panel.example.com", "apikey": "key", "nodeid": 1, "nodetype": "V2ray", "disablecustomconfig": true }
    },
    {
      "paneltype": "SSpanel", "intag": "hysteria2-in", "outtag": "hysteria2-out",
      "apiconfig": { "apihost": "https://panel.example.com", "apikey": "key", "nodeid": 2, "nodetype": "V2ray", "disablecustomconfig": true }
    }
  ]
}
```

注意：**`nodes` 里的每个节点都必须是面板上真实存在、能拉取到信息的节点**。只要有任意一个节点拉取失败，整个进程会退出。所以默认只放你实际在用的节点，要加再加。

SingR 请求旧 SSPanel 时会同时带上 `key=<apikey>` 和 `muKey=<apikey>`，兼容 XrayR v0.9.0 的旧接口行为。

## 配置 sing-box 入站

默认的 `/etc/singr/server.json` 是 **anytls + hysteria2 超集**，两个入站都声明好，`tag` 分别是 `anytls-in` / `hysteria2-in`，和 `panel.json` 的 `intag` 对应。**实际只创建被 `panel.json` 引用到的入站**，没用到的那个不会创建、不需要证书、不占端口：

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
    },
    {
      "type": "hysteria2",
      "tag": "hysteria2-in",
      "listen": "::",
      "listen_port": 0,
      "users": [],
      "up_mbps": 0,
      "down_mbps": 0,
      "ignore_client_bandwidth": false,
      "tls": {
        "enabled": true,
        "server_name": "",
        "certificate_path": "/etc/singr/certs/hysteria2.crt",
        "key_path": "/etc/singr/certs/hysteria2.key"
      }
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "anytls-out",
      "domain_resolver": { "server": "google", "strategy": "prefer_ipv6" }
    },
    {
      "type": "direct",
      "tag": "hysteria2-out",
      "domain_resolver": { "server": "google", "strategy": "prefer_ipv6" }
    },
    {
      "type": "direct",
      "tag": "direct",
      "domain_resolver": { "server": "google", "strategy": "prefer_ipv6" }
    }
  ],
  "route": {
    "rules": [
      { "inbound": "anytls-in", "outbound": "anytls-out" },
      { "inbound": "hysteria2-in", "outbound": "hysteria2-out" }
    ],
    "final": "direct",
    "auto_detect_interface": false
  }
}
```

> 这个「只创建被引用的入站」是 SingR 行为，要带 `-p`（panel 配置）才生效；不带 `-p` 就是原版 sing-box，所有入站都创建。如果 `panel.json` 引用了 `server.json` 里不存在的 `intag`，那个节点会被跳过，全部跳过则进程报错退出。

启动时，如果面板节点被识别为 AnyTLS / Hysteria2 兼容模式：

- 如果 SSPanel 节点地址里解析到有效端口，`listen_port` 会被该端口覆盖。
- 如果 SSPanel 节点地址里存在非空 `host=`，`tls.server_name` 会被该值覆盖。
- 证书路径和私钥路径仍然来自本地 `server.json`，不会从面板获取。

也就是说，本地 JSON 可以先写默认值；只有面板对应字段非空、有效时才会替换本地值。

从 0.2.5 起，面板的 `port` 和 `host=` 改动支持运行中热更新。

- AnyTLS：端口变化时先在新端口起 listener、成功后才关旧端口（失败自动回滚）；SNI 变化只重建 TLS、不重启 listener。日志 `anytls listener hot-reloaded to port ...` / `anytls TLS hot-reloaded with SNI ...`。
- Hysteria2：因为 TLS 焊在 QUIC service 里，端口/SNI 变化会**重建整个 service**并把当前用户表重新灌进去（端口变化先起新后关旧；同端口仅 SNI 变化要先关旧再起新，有极短重启窗口）。日志 `hysteria2 listener hot-reloaded to port ...` / `hysteria2 TLS hot-reloaded with SNI ...`。

**两种协议的证书材料、入站类型、路由规则、obfs/带宽/masquerade 都不会被热更新。**

### AnyTLS padding 方案（抗指纹）

AnyTLS 入站支持在 `server.json` 里设置 `padding_scheme`，用来打乱记录长度分布、削弱指纹：

```json
{
  "type": "anytls",
  "tag": "anytls-in",
  "padding_scheme": ["random"]
}
```

三种取值：

- **`["random"]`**（不区分大小写的哨兵值）：进程**每次启动**随机合成一套 padding 方案。这样每个默认节点不再共用同一套公开默认方案的指纹（长度分布 / md5）。方案的 md5 会在 debug 日志打印。
- **任意其它非空列表**：按写入内容逐行拼接，作为自定义方案原样使用。
- **留空 / 不写**：使用 sing-box fork 内置的公开默认方案。

要点：

- **服务端权威，客户端自动同步。** 服务端在握手 settings 帧里比对客户端的 `padding-md5`，不一致时主动把自己的方案推给客户端，所以只需配置服务端，客户端下次会话自动更新——这也是「每次启动随机」不需要改客户端的原因。
- **在 `NewService` 时定型，不参与热更新。** AnyTLS 热更新只换 listener/TLS，方案在进程生命周期内固定，只有重启才变化（正好就是「每次启动」语义）。
- **不影响流量计费和性能。** padding 帧在子流计数器和限速之下，既不计费也不限速；开销集中在每个会话的前几条记录，之后为零。
- 只随机化「公开默认方案」这一个指纹，**不隐藏协议本身是 AnyTLS**。稳定的自定义方案同样是合理选择；跨重启变化的形状本身也是一种轻微特征。

### 出口 IPv6

默认配置里的 `direct` 出站都设了 `domain_resolver: { "server": "google", "strategy": "prefer_ipv6" }`（sing-box 1.12 起的新写法，老的出站 `domain_strategy` 字段已废弃并将在 1.14 移除），并配了 `dns.strategy: prefer_ipv6`。如果你删掉这些字段，sing-box 的串行拨号会在第一个 IPv4 命中后立刻返回，节点出口会退化成 IPv4 only。需要纯 v4 才把 `prefer_ipv6` 换成 `prefer_ipv4` 或显式 `ipv4_only`。

`auto_detect_interface` 在服务器端建议保持 `false`，它是给 client/TUN 场景用的；开着会把 outbound socket 强行绑到默认网卡，并在某些 IPv6-only 目的地下失效。

## Hysteria2 专属说明

Hysteria2 走 QUIC/UDP，和 AnyTLS 有几处不同，这些参数面板下发不了，只能写在本地 `server.json`：

- **必须 TLS**。Hysteria2 没有明文模式，证书放在 `hysteria2-in` 的 `tls` 里。自签证书时客户端要开允许不安全。
- **obfs（抗 DPI，默认开启，用 SNI 当密码）**。它把 UDP 包打乱让流量不像裸 QUIC，密码是**全节点共享的一个值**（不是 per-user）。默认模板里 obfs 块就写好了，`password` 留空：

  ```json
  "obfs": { "type": "salamander", "password": "" }
  ```

  规则:**`password` 留空 → 自动用 TLS SNI(`server_name`,即面板下发的 `host=`)当 obfs 密码;写了具体值 → 用写的那个。** 这样 SSPanel 不用额外字段也能"顺带"下发 obfs 密码。

  ⚠️ **obfs 开着,所有客户端就必须带相同 obfs**,否则连不上(obfs 不匹配 = 彻底连不上,不是降级)。订阅里要同步带 `obfs=salamander&obfs-password=<SNI 或你写的值>`。**完全不想用 obfs,就把整个 `obfs` 块删掉**(删掉才是关闭;留着空密码是"用 SNI 开启")。
- **带宽 `up_mbps` / `down_mbps`**：`0` = 不限 / 让客户端自报（走 BBR 或 Brutal）。面板的 `node_speedlimit` 仍然独立生效（每用户限速叠加在 Hysteria2 自身拥塞控制之上）。
- **端口跳跃（可选，纯运维，代码不管）**。Hysteria2 进程只绑 1 个 UDP 端口；端口跳跃是用防火墙把一段端口 NAT 到真实端口实现的。

  **推荐用内置管理器**：`SingR porthop`（或管理菜单第 13 项），输入 起始端口 / 结束端口 / 目标（真实）端口即可，自动下 v4+v6 的 REDIRECT 规则、写进 `/etc/singr/porthop.rules`，并由生成的 `singr-porthop.service` 开机重放。规则都带 `singr-porthop` 的 iptables comment 标记，所以列出 / 删除只动 SingR 自己的规则，不碰你其它防火墙规则；持久化由 SingR 自管，不依赖 `iptables-persistent` / `iptables-services`。

  也可以手动下规则：

  ```sh
  iptables  -t nat -A PREROUTING -p udp --dport 40000:60000 -j REDIRECT --to-ports <真实端口>
  ip6tables -t nat -A PREROUTING -p udp --dport 40000:60000 -j REDIRECT --to-ports <真实端口>
  ```

  `<真实端口>` 就是面板下发的那个监听端口。然后订阅地址写成区间，例如 `hysteria2://<uuid>@host:40000-60000/?sni=...`。范围别和真实端口或其它服务冲突；如果前面已有中转（如 nyanpass）在做端口跳跃，就别在落地再加这条 NAT，让跳跃只由一层负责。手动加的规则没有 `singr-porthop` 标记，`SingR porthop` 列表里看不到、也不会去管它。

更详细的部署示例见 [release/poet/hysteria2.md](release/poet/hysteria2.md)。

## 准备 TLS 证书

把证书放到配置中指定的位置：

```sh
cp fullchain.pem /etc/singr/certs/anytls.crt
cp privkey.pem /etc/singr/certs/anytls.key
chmod 600 /etc/singr/certs/anytls.key
```

证书应覆盖 SSPanel 节点地址中的 `host=` 值。没有可信证书时可以临时使用自签证书，但客户端必须允许不安全证书或信任该证书。

Hysteria2 节点同理，证书放到 `hysteria2-in` 配置里指定的路径（默认 `/etc/singr/certs/hysteria2.crt` / `.key`）。两个协议可以指向同一张证书，只要它覆盖各自的 SNI。

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
systemctl daemon-reload
systemctl enable --now singr
systemctl status singr
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

AnyTLS / Hysteria2 客户端都需要：

- 服务器地址：你的节点域名。
- 端口：SSPanel 节点地址中的端口（Hysteria2 开了端口跳跃则填区间，如 `40000-60000`）。
- SNI：SSPanel 节点地址中的 `host=` 值；没有 `host=` 时使用本地 `server.json` 的 `tls.server_name`。
- 密码：SSPanel 用户的 `uuid`。
- TLS：生产环境使用可信证书；自签证书测试时开启允许不安全证书。

Hysteria2 额外：协议选 `hysteria2`；**默认服务端开着 obfs(用 SNI 当密码)**,客户端必须填 `obfs=salamander` + `obfs-password=<SNI 值>`(或服务端写死的那个值),否则连不上;带宽(up/down)由客户端自填,不影响连通。

## 用户同步和上报

SingR 会从旧 SSPanel 拉取用户列表，并把用户映射成运行时用户名：

```text
u<用户ID>
```

例如用户 ID `40493` 会显示为 `u40493`。

已支持（AnyTLS 和 Hysteria2 共用同一套逻辑）：

- 新增和删除用户热更新（增量）。删除用户前会先把累计流量上报到面板。
- 已有用户的 UUID/password 变化热更新。
- 节点 `port` / TLS SNI (`host=`) 运行中热更新（AnyTLS 换 listener；Hysteria2 重建 QUIC service 并回灌用户表）。
- 流量上报到 `/mod_mu/users/traffic?node_id=<nodeid>`。
- 在线 IP 上报到 `/mod_mu/users/aliveip?node_id=<nodeid>`。
- 多节点 / 多协议共存（`panel.json` 的 `nodes` 数组）。
- `server.json` 写成 anytls + hysteria2 超集，按 `panel.json` 引用的 `intag` 只创建需要的入站。

当前未完整接管：

- `relay_server` 和 `relay_port` 不会自动创建出站和路由。
- 不会从面板动态创建缺失的入站；`server.json` 必须先声明对应 `intag` 的入站（超集默认已含 anytls/hysteria2 两个）。
- 不支持运行中热切换入站类型；TLS 证书材料、obfs、带宽、masquerade、端口跳跃 NAT 也仍然只在启动 / 运维时配置，不随面板热更新。

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

- SSPanel 节点地址是否包含 `ws` 和 `path=/anytls`（或 `path=/hy2`）。
- `/etc/singr/server.json` 是否声明了对应的入站（`type: "anytls"` 或 `type: "hysteria2"`）。
- 入站 `tag` 是否等于 `/etc/singr/panel.json` 里的 `intag`（超集模式下：`panel.json` 没引用的入站不会创建，这是预期行为）。
- 首次启动需要面板返回有效 `port`；日志里 `invalid anytls listen port from panel` / `invalid hysteria2 listen port from panel` 说明面板返回了 0 或越界值。
- Hysteria2 是 UDP,用 `ss -lunp | grep singr`(注意是 `-u`)查监听;端口跳跃只是 NAT,进程仍只绑那个真实端口。

### 节点出口没有 IPv6

检查：

- 服务器本身能否 `curl -6 ifconfig.co`。如果服务器没有 v6 GUA，无论 SingR 怎么配都没用。
- `server.json` 的 `direct` 出站是否有 `domain_resolver: { "server": "google", "strategy": "prefer_ipv6" }`（老写法 `domain_strategy: prefer_ipv6` 已废弃），`dns` 块是否带 `strategy: prefer_ipv6`，`route.auto_detect_interface` 是否为 `false`。0.2.5 起 `singr update` 会自动迁移老配置，迁移前的备份在 `/etc/singr/server.json.bak.<时间戳>`。
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
