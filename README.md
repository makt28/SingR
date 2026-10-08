# SingR

基于 sing-box 的 **旧版 SSPanel（`/mod_mu` 接口）后端**。面板里节点类型照旧填 `V2ray`，SingR 根据节点地址里的 `path` 把它跑成 AnyTLS 或 Hysteria2：

| 节点地址里的 `path` | 实际运行的协议 |
| --- | --- |
| `path=/anytls` | AnyTLS（TCP） |
| `path=/hy2` | Hysteria2（QUIC/UDP） |

两种协议共用同一套用户同步、流量上报、限速和审计。项目地址：<https://github.com/makt28/SingR>

---

## 快速开始

### 第 1 步：在 SSPanel 里配节点

节点类型保持 `V2ray`，节点地址按旧格式填写：

```text
节点域名;监听端口;0;ws;;path=/anytls|host=TLS域名
```

例如：

```text
sa.example.com;14555;0;ws;;path=/anytls|host=example.com
sa.example.com;14556;0;ws;;path=/hy2|host=example.com
```

- `14555`：监听端口。
- `path=/anytls` 或 `path=/hy2`：选择协议（不带 `/` 也认）。
- `host=`：TLS 的 SNI，证书需要覆盖这个域名。
- 末尾还可以追加 `|relay_server=...|relay_port=...`，SingR 会解析保存，但目前不会据此生成中转。

### 第 2 步：准备证书

AnyTLS 和 Hysteria2 都必须有 TLS 证书，没有证书进程不会启动。三种方式任选一种：

| 方式 | 做法 |
| --- | --- |
| 放到默认路径 | 证书放 `/etc/singr/certs/default.pem`、私钥放 `default.key`（Docker 是 `/etc/singr-docker/certs/`） |
| 指定已有证书 | 安装或添加节点时带 `--cert-path` / `--key-path`，例如 certbot 的 `live/` 目录 |
| 从 https 地址下载 | 带 `--cert-url` / `--key-url`，SingR 会每天检查，到期前自动更新 |

详见下文[「证书」](#证书)。

### 第 3 步：安装（裸机和 Docker 二选一）

**裸机（systemd）**，需要 root：

```sh
bash <(curl -Ls https://raw.githubusercontent.com/makt28/SingR/main/install.sh)

singr add \
  --api-url https://your-sspanel.example.com \
  --api-key your-apikey \
  --node-id 44 \
  --protocol anytls \
  --cert-path /etc/letsencrypt/live/a.example.com/fullchain.pem \
  --key-path  /etc/letsencrypt/live/a.example.com/privkey.pem
```

`singr add` 缺必填参数（`--api-url` / `--api-key` / `--node-id` / `--protocol`）时会逐项询问；四项都给齐则不再追问可选的 SNI 和证书路径，留空按默认处理。`--protocol` 可选 `anytls` 或 `hysteria2`。

**Docker**：

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/makt28/SingR/main/install-docker.sh) \
  --api-url https://your-sspanel.example.com \
  --api-key your-apikey \
  --node-id 44 \
  --protocol anytls \
  --cert-path /etc/letsencrypt/live/a.example.com/fullchain.pem \
  --key-path  /etc/letsencrypt/live/a.example.com/privkey.pem
```

Docker 版会装上同样的 `singr` 管理命令，后续操作和裸机完全一样。配置在 `/etc/singr-docker`，与裸机的 `/etc/singr` 互不影响。一台机器选一种即可。

### 第 4 步：客户端填写

| 项 | 填什么 |
| --- | --- |
| 地址 | 节点域名 |
| 端口 | 节点地址里的端口；Hysteria2 开了端口跳跃就填区间，如 `40000-60000` |
| SNI | 节点地址里的 `host=` |
| 密码 | 用户的 **UUID**（不是 passwd） |
| Hysteria2 obfs | 默认开启：`obfs=salamander`，`obfs-password=<SNI>`。不填连不上，见[「Hysteria2」](#hysteria2) |

---

## 日常管理

`singr` 和 `SingR` 等价。直接输入 `singr` 打开管理菜单。

| 命令 | 作用 |
| --- | --- |
| `singr status` / `start` / `stop` / `restart` | 查看状态、启停 |
| `singr log` | 查看日志 |
| `singr update [版本]` | 更新到最新版或指定版本 |
| `singr config` | 编辑 `panel.json` / `server.json`，保存后可选择重启 |
| `singr list` | 查看所有节点及证书剩余天数 |
| `singr add` / `singr del` | 添加 / 删除节点，见[「多节点」](#多节点) |
| `singr cert-source` | 设置默认证书的下载地址（菜单第 15 项） |
| `singr cert-update --force` | 立即重新下载默认证书 |
| `singr porthop` | Hysteria2 端口跳跃规则（菜单第 13 项） |
| `singr version` | 查看 SingR 和 sing-box 核心版本 |
| `singr uninstall` | 卸载 |

日志：裸机写在 `/var/log/singr.log`（`singr log` 会先显示 systemd journal，再跟随该文件）；Docker 输出到 stdout，用 `singr log` 或 `docker logs` 查看。

---

## 多节点

一个 SingR 进程可以同时对接多个面板节点，可以跨面板、混用协议。每个节点的用户、流量、限速和审计规则相互独立，不同节点的用户 ID 相同也不会串账。管理菜单第 14 项「节点管理」提供同样的功能。

```sh
singr list                    # 查看节点
singr add --api-url ... --api-key ... --node-id 57 --protocol hysteria2 --sni b.example.com
singr del @2                  # @序号取自 list 的 # 列
```

`singr list` 输出示例：

```text
  #   NodeID   PROTO      DOMAIN                  INTAG            CERT
  1   1        anytls     https://a.example.com   anytls-in        OK 剩 86 天
  2   7        hysteria2  https://a.example.com   hysteria2-in-7   OK 剩 11 天
  3   8        anytls     https://b.example.com   anytls-in-8      已过期
  4   9        anytls     https://b.example.com   anytls-in-9      缺失
  5   99       anytls     https://b.example.com   ghost-in         无 inbound
```

CERT 列：

- 剩余不足 30 天显示黄色，已过期显示红色。机器上没有 `openssl` 时只显示 `OK`。
- `缺失`：路径上没有证书文件。
- `配置不全`：`certificate_path` 和 `key_path` 只写了一个。要么两个都写，要么两个都留空（使用默认路径）。
- `无 inbound`：`panel.json` 里的 `intag` 在 `server.json` 里找不到对应入站。
- 后两种都会让进程起不来。

必须知道的几点：

- **任何一个节点起不来，整个进程都会退出。** 所以 `add` / `del` 每次都会先备份，改完重启并检查，失败就自动回滚。
- **端口要在面板侧错开。** 两个节点下发到同一个端口时，第二个节点会监听失败，进程看起来正常，但这个节点实际连不上。
- **删除时优先用 `@序号` 或 InTag 指定节点。** NodeID 只在单个面板内唯一，有歧义时 `singr del 16` 会拒绝执行并列出候选。不要写 `#1`，shell 会把 `#` 后面当成注释。
- 只剩一个节点时不能删除。要换节点，先 `add` 新的再 `del` 旧的。
- InTag 自动分配：同协议的第一个节点用 `anytls-in`，之后的节点用 `anytls-in-<节点ID>`；hysteria2 同理。

---

## 证书

### 默认路径

`server.json` 里 `certificate_path` 和 `key_path` **都留空** 时，使用配置目录下的默认证书：

```text
/etc/singr/certs/default.pem   # 找不到时再找 default.crt
/etc/singr/certs/default.key
```

Docker 的配置目录是 `/etc/singr-docker`。启动日志会打印实际使用的路径：

```text
inbound/anytls[anytls-in]: no TLS certificate configured, using default /etc/singr/certs/default.pem + /etc/singr/certs/default.key
```

AnyTLS 和 Hysteria2 默认用同一张证书，只要证书覆盖各自的 SNI 即可。不同节点要用不同证书时，用 `singr add --cert-path` 单独指定。写了具体路径的节点不受默认路径影响。

测试时可以先用自签证书，客户端需要开启「允许不安全证书」：

```sh
mkdir -m 700 -p /etc/singr/certs
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout /etc/singr/certs/default.key \
  -out    /etc/singr/certs/default.pem -subj "/CN=example.com"
```

### 续期：替换文件即可，无需重启

SingR 会监视证书所在的目录，文件一变就自动加载新证书，不需要重启，也不需要给 certbot 配 `--deploy-hook`。

- 裸机和 Docker 都直接读取你给的证书路径，不会另外复制一份。
- Docker 会把证书所在目录按原路径只读挂进容器（certbot 的 `live/` 软链指向的 `archive/` 目录也会一起挂上）。证书路径改了以后执行 `singr restart`，容器会按新路径重建。
- 证书请放在单独的目录里。`/etc`、`/usr`、`/tmp` 这类系统目录不能直接挂进容器。

> **例外：** 如果 `certs/default.pem` 是指向 certbot 证书的软链，续期时变化发生在 certbot 的 `live/` 目录，`certs/` 目录本身没变，所以监视不到。这种情况请直接把 certbot 的路径写进配置（`singr add --cert-path`），或者继续在 certbot 里配 `--deploy-hook "singr restart"`。

### 从 https 地址自动更新默认证书

证书由别的机器统一签发、通过 https 分发时，可以让 SingR 自己下载：

```sh
singr cert-source --cert-url https://example.com/a.pem --key-url https://example.com/a.key
```

- 设置后立即下载一次。只有证书有效、没过期、和私钥配对，才会放进默认路径。
- 之后每天检查一次（systemd timer `singr-cert-update.timer`）。本地证书剩余不足 7 天时才去下载，远端证书比本地新才替换。
- 只替换文件，不重启进程，用其他证书的节点不受影响。
- 只管默认证书，写了具体路径的节点不受影响。
- 只接受 `https://` 地址。地址存放在 `cert-source.json`（权限 600），里面可以带 token。
- 安装时可以直接带上：`install-docker.sh ... --cert-url URL --key-url URL`，或 `singr add ... --cert-url URL --key-url URL`。不能和 `--cert-path` 同时使用。

---

## Hysteria2

下面这些参数面板下发不了，需要写在本地 `server.json` 的 `hysteria2-in` 入站里。

### obfs（默认开启）

默认模板里已经写好：

```json
"obfs": { "type": "salamander", "password": "" }
```

- `password` 留空：用 TLS SNI（面板下发的 `host=`）作为密码。
- 填了值：用填的值。
- 删掉整个 `obfs` 块：关闭 obfs。

开着 obfs 时，所有客户端都必须带上相同的 obfs 设置，否则完全连不上。订阅里加上 `obfs=salamander&obfs-password=<SNI 或你填的值>`。

另一种混淆 `gecko` 会把 UDP 包切片填充，用来对抗按包长分析的封锁。用它时**必须显式填 `password`**：留空不会自动用 SNI，混淆会静默失效，也不报错。没遇到按包长封锁的话，继续用 salamander 就行。

### 带宽 `up_mbps` / `down_mbps`

这两个值是 **上限**：实际速率取它和客户端自报值中较小的那个。默认 `300`。

- 千兆节点想跑满，就调大。
- `0` 表示不设上限，客户端报多少就按多少发（Brutal 拥塞控制不管丢包），所以 `0` 不是保守值。
- 面板的限速（`node_speedlimit`）在此基础上仍然生效。
- 已经装好的机器保留原来 `server.json` 里的值，新增节点会沿用第一个 hysteria2 入站的设置。

### 端口跳跃

Hysteria2 只监听一个 UDP 端口，端口跳跃靠防火墙把一段端口转发到这个真实端口。

推荐用 `singr porthop`（菜单第 13 项）：输入起始端口、结束端口和真实端口，脚本会同时写好 IPv4 和 IPv6 规则，并在开机时自动恢复。它只管理带 `singr-porthop` 标记的规则，不会动你的其他防火墙规则。

也可以手动加规则（手动加的规则 `singr porthop` 不会管理）：

```sh
iptables  -t nat -A PREROUTING -p udp --dport 40000:60000 -j REDIRECT --to-ports <真实端口>
ip6tables -t nat -A PREROUTING -p udp --dport 40000:60000 -j REDIRECT --to-ports <真实端口>
```

订阅地址写成区间，例如 `hysteria2://<uuid>@host:40000-60000/?sni=...`。如果前面的中转已经在做端口跳跃，落地机上就不要再加。

### realm：不要开

realm 用于服务器在 NAT 后、没有公网端口的场景。SingR 节点一般有公网 IP，用不上。而且面板每次修改端口或 SNI，realm 都会重新注册一遍，修改 SNI 时还会短暂中断，所以不建议开启。

更多部署示例见 [release/poet/hysteria2.md](release/poet/hysteria2.md)。

---

## 常见问题

**启动后没有监听端口**

- 节点地址里是否有 `ws` 和 `path=/anytls`（或 `/hy2`）。
- `panel.json` 的 `intag` 是否和 `server.json` 里的入站 `tag` 一致（`singr list` 显示 `无 inbound` 就是不一致）。
- 日志出现 `invalid anytls listen port from panel` 或 `invalid hysteria2 listen port from panel`，说明面板返回的端口是 0 或超出范围。
- Hysteria2 走 UDP，用 `ss -lunp | grep singr` 查看（注意是 `-u`）。

**面板连接失败**

- `apihost` 在服务器上能否访问。`apihost` 不要以 `/mod_mu` 结尾。
- `apikey` 和 `nodeid` 是否正确。
- 服务器是否设置了 `http_proxy`、`ALL_PROXY` 这类代理环境变量。有的话需要清掉。

**客户端 TLS 失败**

- 客户端 SNI 是否等于节点地址里的 `host=`，证书是否覆盖这个域名。
- 用自签证书时，客户端是否开启了「允许不安全证书」。

**用户认证失败**

- 密码要填用户的 UUID（UUID 为空时才用 passwd）。
- Hysteria2 还要检查 obfs 是否和服务端一致。

**节点出口没有 IPv6**

- 先确认服务器本身有 IPv6：`curl -6 ifconfig.co`。
- `server.json` 的 `direct` 出站需要 `"domain_resolver": { "server": "local", "strategy": "prefer_ipv6" }`，`dns` 需要 `"strategy": "prefer_ipv6"`，`route.auto_detect_interface` 需要是 `false`。默认配置都已经写好，删掉的话出口会只走 IPv4。

---

## 升级注意

- **0.6.0（核心 1.14）起**：`direct` 出站的旧字段 `domain_strategy` 会导致无法启动。`singr update` 会自动把它迁移成 `domain_resolver`，迁移前会备份成 `server.json.bak.<时间戳>`。如果升级时看到 `未安装 jq，跳过 server.json 迁移` 的警告，说明没迁移成功，**先别重启**，手动改好再启动。
- **旧版 Docker（证书靠复制 + `singr cert-sync`）**：升级管理脚本后，第一次执行任意 `singr` 命令会自动迁移成「直接引用原证书路径」。原证书已经不存在的节点，以及使用默认证书的节点，迁移后不会再自动续期，会记录在 `/etc/singr-docker/cert-migration-notice.txt`，`singr list` 也会提示。处理办法：用 `singr cert-source` 设置下载地址，或用 `singr config` 把证书路径改成 certbot 的原始路径后 `singr restart`。certbot 里残留的 `--deploy-hook "singr cert-sync"` 不会报错，可以删掉。

---

## 进阶

### 安装脚本选项

```sh
bash install.sh                                        # 优先用当前目录的二进制，没有就从源码编译
bash install.sh v0.7.0                                 # 从 GitHub Release 安装指定版本
env SINGR_BINARY=/path/to/sing-box bash install.sh     # 使用指定的二进制
env SINGR_RELEASE_REPO=owner/repo bash install.sh v0.7.0   # 从 fork 的 Release 安装
```

安装位置：二进制在 `/usr/local/SingR/singr`，管理命令在 `/usr/bin/singr`，配置在 `/etc/singr/`，另外会生成 `singr.service`。已有的配置文件不会被覆盖。脚本还会安装 `jq`、`vim`、`iptables`。

### Docker 的另外两种用法

镜像地址是 `ghcr.io/makt28/singr`，支持 amd64 和 arm64。`:latest` 跟随正式发布，也可以用 `:vX.Y.Z` 固定版本。

**直接 `docker run`**（不装管理脚本）：

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

**docker compose**：修改仓库里 [`docker-compose.yml`](docker-compose.yml) 中的 `SINGR_*`，证书放在 `singr-data/certs/default.pem` 和 `default.key`，然后执行 `docker compose up -d`。

参数对照（安装脚本参数 ↔ 环境变量）：

| 参数 | 环境变量 | 说明 | 默认 |
| --- | --- | --- | --- |
| `--api-url` | `SINGR_API_URL` | 面板地址（必填） | |
| `--api-key` | `SINGR_API_KEY` | 面板 apikey（必填） | |
| `--node-id` | `SINGR_NODE_ID` | 节点 ID（必填） | |
| `--protocol` | `SINGR_PROTOCOL` | `anytls` 或 `hysteria2`（必填） | |
| `--sni` | `SINGR_SNI` | 入站 `server_name` | 空 |
| `--cert-path` / `--key-path` | `SINGR_CERT_PATH` / `SINGR_KEY_PATH` | 证书路径。用安装脚本时填宿主机路径；用 `docker run` / compose 时填容器内路径，需要自己挂载 | 空，即使用默认路径 |
| `--cert-url` / `--key-url` | — | 仅安装脚本支持，见「证书」 | |
| `--speed-limit` / `--device-limit` | `SINGR_SPEED_LIMIT` / `SINGR_DEVICE_LIMIT` | 限速 / 设备数 | 0 |
| `--enable-device-limit` | `SINGR_ENABLE_DEVICE_LIMIT` | 是否强制限制设备数 | false |
| `--update-periodic` | `SINGR_UPDATE_PERIODIC` | 面板同步周期（秒） | 60 |
| `--image` | — | 镜像地址（仅安装脚本） | `ghcr.io/makt28/singr:latest` |

说明：

- 必须用 host 网络：端口由面板下发，Hysteria2 还要走 UDP。
- 上面的参数只在第一次启动时用来生成 `/etc/singr-docker/panel.json`，之后一律以这个文件为准。
- 要加节点请用 `singr add`。不要重跑 `install-docker.sh`：它检测到已安装会直接拒绝。
- 不装管理脚本的话，也可以单独下载它来用 `singr porthop`：

  ```sh
  curl -fsSL https://raw.githubusercontent.com/makt28/SingR/main/SingR-docker.sh -o /usr/bin/SingR
  chmod +x /usr/bin/SingR && ln -sf /usr/bin/SingR /usr/bin/singr
  ```

### 手动编译和运行

需要 Go 1.25.5 或更高版本（发布构建用的是 1.26.8）。

```sh
git clone https://github.com/makt28/SingR.git && cd SingR
make build
install -m 755 ./sing-box /usr/local/bin/singr
mkdir -p /etc/singr/certs
cp release/poet/panel_anytls.json /etc/singr/panel.json   # 用 hysteria2 就换成 panel_hysteria2.json
cp release/poet/server.json       /etc/singr/server.json
/usr/local/bin/singr run -c /etc/singr/server.json -p /etc/singr/panel.json
```

<details>
<summary>systemd 服务示例</summary>

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
# 系统带了代理环境变量时，在这里清空：
# Environment="http_proxy=" "https_proxy=" "HTTP_PROXY=" "HTTPS_PROXY=" "ALL_PROXY=" "all_proxy="

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload && systemctl enable --now singr
```

</details>

### 配置文件说明

**`panel.json`**：每个节点对应 `nodes` 里的一项。`singr add` 会自动写好，一般不需要手改。

```json
{
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

- `intag`：必须和 `server.json` 里某个入站的 `tag` 一致。`outtag`：对应的出站 `tag`。
- Hysteria2 节点就把这两项换成 `hysteria2-in` / `hysteria2-out`。
- `nodes` 里的每个节点都必须是面板上真实存在的节点，任何一个拉取失败，整个进程都会退出。

**`server.json`**：默认同时写好了 `anytls-in` 和 `hysteria2-in` 两个入站，但只有被 `panel.json` 引用到的入站才会真正启动。没用到的入站不需要证书，也不占端口。所以 **换协议或加协议只需要改 `panel.json`**。

<details>
<summary>默认 server.json</summary>

```json
{
  "log": { "disabled": false, "level": "info", "timestamp": true, "output": "/var/log/singr.log" },
  "dns": {
    "servers": [{ "tag": "local", "type": "local" }],
    "strategy": "prefer_ipv6"
  },
  "inbounds": [
    {
      "type": "anytls", "tag": "anytls-in", "listen": "::", "listen_port": 0, "users": [],
      "tls": { "enabled": true, "server_name": "", "certificate_path": "", "key_path": "" }
    },
    {
      "type": "hysteria2", "tag": "hysteria2-in", "listen": "::", "listen_port": 0, "users": [],
      "up_mbps": 300, "down_mbps": 300, "ignore_client_bandwidth": false,
      "obfs": { "type": "salamander", "password": "" },
      "tls": { "enabled": true, "server_name": "", "certificate_path": "", "key_path": "" }
    }
  ],
  "outbounds": [
    { "type": "direct", "tag": "anytls-out",    "domain_resolver": { "server": "local", "strategy": "prefer_ipv6" } },
    { "type": "direct", "tag": "hysteria2-out", "domain_resolver": { "server": "local", "strategy": "prefer_ipv6" } },
    { "type": "direct", "tag": "direct",        "domain_resolver": { "server": "local", "strategy": "prefer_ipv6" } }
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

</details>

### 面板改端口或 SNI 时会怎样

不用重启，SingR 会自动应用：

- **AnyTLS**：先在新端口上开始监听，成功后再关掉旧端口；失败就保留旧配置。只改 SNI 时只换 TLS 配置，连接不受影响。
- **Hysteria2**：会重建整个服务，并自动恢复用户表。只改 SNI（端口不变）时会短暂中断一下。

以下内容不随面板变化，需要改本地配置并重启：入站协议类型、路由规则、obfs、带宽、端口跳跃。证书文件的内容变化会自动加载，见「证书」。

### AnyTLS padding（抗指纹）

在 `anytls-in` 入站里加一行：

```json
"padding_scheme": ["random"]
```

- `["random"]`：每次启动随机生成一套 padding 方案，避免所有节点用同一套公开的默认方案。
- 填其他内容：按你写的方案使用。
- 不写：使用默认方案。

只需要配服务端，客户端会自动同步。padding 不计入用户流量，也不影响限速。它只能打乱默认方案带来的特征，不能隐藏协议本身是 AnyTLS。

### 其他说明

- 用户在节点上的名字是 `u<用户ID>`。新增、删除用户和改密码都会实时生效；删除用户前会先上报他剩余的流量。
- 流量上报到 `/mod_mu/users/traffic`，在线 IP 上报到 `/mod_mu/users/aliveip`。请求同时带 `key` 和 `muKey` 两个参数，兼容 XrayR 用的旧接口。
- 运行测试：`go test ./poet/... ./cmd/sing-box`。

---

## 许可证

基于 sing-box 修改，遵循上游 GPL-3.0-or-later。
