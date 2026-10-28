# SingR

基于 sing-box 的 **旧版 SSPanel（`/mod_mu` 接口）后端**。面板里节点类型照旧填 `V2ray`，SingR 根据节点地址里的 `path` 把它跑成 AnyTLS、Hysteria2 或 NaiveProxy：

| 节点地址里的 `path` | 实际运行的协议 |
| --- | --- |
| `path=/anytls` | AnyTLS（TCP） |
| `path=/hy2` | Hysteria2（QUIC/UDP） |
| `path=/naive` | NaiveProxy（同一端口上 TCP 走 HTTP/2、UDP 走 HTTP/3） |

三种协议共用同一套用户同步、流量上报、限速和审计。项目地址：<https://github.com/makt28/SingR>

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
sa.example.com;14557;0;ws;;path=/naive|host=example.com
```

- `14555`：监听端口。
- `path=/anytls`、`path=/hy2` 或 `path=/naive`：选择协议（不带 `/` 也认）。
- `host=`：TLS 的 SNI，证书需要覆盖这个域名。
- 末尾还可以追加 `|relay_server=...|relay_port=...`，SingR 会解析保存，但目前不会据此生成中转。

### 第 2 步：准备证书

AnyTLS、Hysteria2 和 NaiveProxy 都必须有 TLS 证书，没有证书进程不会启动（NaiveProxy 原版客户端不认自签证书）。三种方式任选一种：

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

`singr add` 缺必填参数（`--api-url` / `--api-key` / `--node-id` / `--protocol`）时会逐项询问；四项都给齐则不再追问可选的 SNI 和证书路径，留空按默认处理。`--protocol` 可选 `anytls`、`hysteria2` 或 `naive`。

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
| 密码 | 用户的 **UUID**（不是 passwd）。NaiveProxy 的用户名随便填，只认密码 |
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
| `singr derp` | DERP 中继（组网），见[「DERP 中继」](#derp-中继组网)（菜单第 16 项） |
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
- **端口要在面板侧错开。** 两个节点下发到同一个端口时，第二个节点会监听失败，进程看起来正常，但这个节点实际连不上。NaiveProxy 同时占用端口的 TCP 和 UDP，和 AnyTLS（TCP）、Hysteria2（UDP）都不能共用端口号。
- **删除时优先用 `@序号` 或 InTag 指定节点。** NodeID 只在单个面板内唯一，有歧义时 `singr del 16` 会拒绝执行并列出候选。不要写 `#1`，shell 会把 `#` 后面当成注释。
- 只剩一个节点时不能删除。要换节点，先 `add` 新的再 `del` 旧的。
- InTag 自动分配：同协议的第一个节点用 `anytls-in`，之后的节点用 `anytls-in-<节点ID>`；hysteria2、naive 同理。

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

AnyTLS、Hysteria2 和 NaiveProxy 默认用同一张证书，只要证书覆盖各自的 SNI 即可。不同节点要用不同证书时，用 `singr add --cert-path` 单独指定。写了具体路径的节点不受默认路径影响。

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

## DERP 中继（组网）

给林北客户端组网用的中继：两台设备打不通 P2P 时，数据经节点上的 DERP 转发。默认关闭。

工作方式：

- **只服务自家用户。** 客户端经代理隧道访问虚拟域名 `derp.linbei.internal`，节点把它转到本机的 DERP。DERP 只接受来自本机回环的普通连接，公网直接连过来的一律拒绝。所以只有有效订阅能用，中继流量照常计进用户的已用流量。
- **所有节点组成一个中继网。** 节点之间互相连接（mesh），用户的两台设备连在不同节点上也能互通。这部分走公网，需要放行 DERP 端口（默认 TCP 8443）。
- **节点自己向 sspapi 报到。** 每 60 秒把自己的端口、证书指纹和公网 IPv4 / IPv6 报给 sspapi 的 `tailnet.php`，拿回 mesh 口令（PSK）和其它节点的名单。没有名单要手工维护，加节点、下线节点都不用碰别的机器。headscale 不参与，它整台没了中继网照常运转。
- **地址由节点自己报。** sspapi 前面是反向代理，看不到节点的真实 IP。节点每 5 秒看一次本机出网用的地址（不发网络请求），是公网地址就报它；机器在 NAT 后面（网卡上是内网地址）就在启动时问一次 ip.sb，问不到每 30 秒重试。地址一变立刻重新报到，其它节点连它失败时也会立刻重拉名单，所以换 IP 后几秒内中继网就跟上了。也可以手写地址。
- **节点之间要确认对方身份。** 每台节点有一张固定的自签证书，报到时把证书指纹交给 sspapi；名单经正规 https 下发，节点只和指纹对得上的对端互联。
- **互联用哪个地址**：双方都有 IPv4 就走 IPv4；有一方只有 IPv6 就走 IPv6；一边只有 IPv4、另一边只有 IPv6 就无法互联，日志里会警告。
- **客户端怎么知道哪些节点有中继。** 节点在本机回环上开一个探测口（DERP 端口 +1），客户端经每个节点访问 `http://derp.linbei.internal/`，有应答的就是有中继的节点。探测口只听本机，不用放行防火墙。

需要 sspapi 的 `tailnet.php`，和它 `data/tailnet.json` 里的**节点 token**（`derp.node_token`）。

### 开启

在每台节点上执行（不写端口用 8443）：

```sh
singr derp enable --registry https://api.example.com/tailnet.php --token <节点 token>
```

它会生成本机证书、探测本机公网地址、向 sspapi 报到一次，确认地址和 token 都对了才改配置并重启 SingR。然后**防火墙放行 TCP 8443**（或你设置的端口），节点之间要能互相连到这个端口。

- 手写地址：`--ipv4 203.0.113.5`、`--ipv6 2001:db8::5`；某一类不报写 `none`（比如 `--ipv6 none`），恢复自动写 `auto`。地址固定、又在 NAT 后面的机器适合手写。
- 随时用 `singr derp` 查看状态（终端里会出操作菜单）。

### 修改与生效

- **增删节点**：新节点照「开启」做一遍；下线用 `singr derp disable`，它会先从名单撤下本机。直接关机的节点在 sspapi 的过期时间（默认 30 分钟）后移出名单；别的节点连它失败时会提前重拉名单。
- **sspapi 出问题时保持原样**：连不上、应答格式不对、名单为空，节点都继续用上一份名单，并在日志里警告。SingR 重启时用缓存的名单和 PSK（`/etc/singr/derp/mesh-registry.cache.json`），sspapi 暂时不在也能把 mesh 建起来。
- **改端口**：`singr derp port 9443`，防火墙也要放行新端口。
- **换 sspapi 地址或 token**：`singr derp enable --registry <新地址> --token <新 token>`（只给要改的那项也行）。
- **关掉再开**：`singr derp disable` 之后直接 `singr derp enable`，沿用上次的地址、端口、IPv4 / IPv6 设置和 token。
- **PSK 变了**（sspapi 里改了 `derp.psk`）：节点会记下新 PSK 并在日志里提示，**重启 SingR 后生效**。
- **换证书**：删掉 `/etc/singr/derp/derp-tls.crt` 和 `derp-tls.key`（Docker 在 `/etc/singr-docker/derp/`），再执行 `singr derp enable`。证书默认用一百年，正常不需要换。
- **克隆机**：用同一个镜像克隆出来的机器证书相同，sspapi 会认出来，保留原来那台、拒绝克隆机（`DERP_PIN_CONFLICT`）。在克隆机上按上一条换证书即可。
- 每次改动都会重启 SingR 并检查是否正常启动；起不来会自动回滚到改动前的配置。

### 排查

用 `singr log` 查看，DERP 相关的日志都带 `service/derp`：

| 日志 | 含义 |
| --- | --- |
| `reported IPv4: ... -> ...` / `reported IPv6: ...` | 报的地址变了（启动时也会打一次） |
| `mesh peers added (registry): ...` / `mesh peers removed (registry): ...` | 名单变化已生效（`(cache)` 是重启时从缓存恢复的） |
| `sync with the registry (keeping current peers): ...` | 连不上 sspapi，或被拒（`DERP_UNAUTHORIZED` token 不对、`DERP_PIN_CONFLICT` 克隆机），继续用上一份名单 |
| `public IPv4 lookup (retrying in 30s): ...` | 机器在 NAT 后面，ip.sb 暂时问不到；问到之前这一类地址不报 |
| `no public IPv4 or IPv6 to report` | 两类地址都没有：用 `--ipv4` / `--ipv6` 手写 |
| `relay ... shares no address family with us` | 对方只有 IPv4、我们只有 IPv6（或反过来），不互联 |
| `mesh(...): ... unrecognized remote public key: <指纹>` | 对端出示的指纹和它报到的不一致：对端换过证书但还没重新报到，一分钟内自愈 |
| `mesh(...): ... connection refused` / `timeout` | 对端端口没放行，或者它报的地址连不到 |
| `mesh PSK changed in the registry; restart SingR to apply it` | sspapi 换了 PSK，重启 SingR |
| `no mesh PSK from the registry or cache` | 第一次启动时 sspapi 不可达且没有缓存：中继能用，但不和其它节点互联，重启后重试 |

连不上的对端每分钟最多警告一次。

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

## NaiveProxy

一个 naive 节点在同一个端口上同时开两种承载：TCP 走 HTTP/2（客户端写 `https://`），UDP 走 HTTP/3（客户端写 `quic://`）。只想开一种，就在 `server.json` 的 `naive-in` 里加 `"network": "tcp"` 或 `"udp"`。

- **客户端自己不会在两者之间切换**，也不会自动回落。想让用户在 UDP 被封时还能用，订阅里同一个节点出两条：`naive+https://任意:<uuid>@host:port` 和 `naive+quic://任意:<uuid>@host:port`，交给客户端的自动选择去挑。
- **只认密码**（UUID，没有时用 passwd），用户名随便填。
- **证书必须是真的。** 原版 NaiveProxy 客户端基于 Chromium，严格校验证书，没有跳过校验的选项。
- 代理 UDP 只有 sing-box 客户端能用（`udp_over_tcp`），原版 NaiveProxy 只代理 TCP。
- 面板改端口时，两种承载一起迁移：任一边绑不上就整体不变，下个周期重试。改 SNI 只换 TLS 配置，不断线。
- 防火墙要同时放行这个端口的 TCP 和 UDP。

更多说明见 [release/poet/naive.md](release/poet/naive.md)。

---

## 常见问题

**启动后没有监听端口**

- 节点地址里是否有 `ws` 和 `path=/anytls`（或 `/hy2`、`/naive`）。
- `panel.json` 的 `intag` 是否和 `server.json` 里的入站 `tag` 一致（`singr list` 显示 `无 inbound` 就是不一致）。
- 日志出现 `invalid anytls listen port from panel`、`invalid hysteria2 listen port from panel` 或 `invalid naive listen port from panel`，说明面板返回的端口是 0 或超出范围。
- Hysteria2 走 UDP，用 `ss -lunp | grep singr` 查看（注意是 `-u`）。NaiveProxy TCP 和 UDP 都要有。

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

- **DERP 中继（2026-10）**：二进制多编了 gvisor 和 tailscale，体积大了约 17 MB（linux/amd64 41 MB → 58 MB）。不开 DERP 的节点行为不变。

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
| `--protocol` | `SINGR_PROTOCOL` | `anytls`、`hysteria2` 或 `naive`（必填） | |
| `--sni` | `SINGR_SNI` | 入站 `server_name` | 空 |
| `--cert-path` / `--key-path` | `SINGR_CERT_PATH` / `SINGR_KEY_PATH` | 证书路径。用安装脚本时填宿主机路径；用 `docker run` / compose 时填容器内路径，需要自己挂载 | 空，即使用默认路径 |
| `--cert-url` / `--key-url` | — | 仅安装脚本支持，见「证书」 | |
| `--speed-limit` / `--device-limit` | `SINGR_SPEED_LIMIT` / `SINGR_DEVICE_LIMIT` | 限速 / 设备数 | 0 |
| `--enable-device-limit` | `SINGR_ENABLE_DEVICE_LIMIT` | 是否强制限制设备数 | false |
| `--update-periodic` | `SINGR_UPDATE_PERIODIC` | 面板同步周期（秒） | 60 |
| `--image` | — | 镜像地址（仅安装脚本） | `ghcr.io/makt28/singr:latest` |

说明：

- 必须用 host 网络：端口由面板下发，Hysteria2 和 NaiveProxy 还要走 UDP。
- 上面的参数只在第一次启动时用来生成 `/etc/singr-docker/panel.json`，之后一律以这个文件为准。
- 要加节点请用 `singr add`。不要重跑 `install-docker.sh`：它检测到已安装会直接拒绝。
- 不装管理脚本的话，也可以单独下载它来用 `singr porthop`：

  ```sh
  curl -fsSL https://raw.githubusercontent.com/makt28/SingR/main/SingR-docker.sh -o /usr/bin/SingR
  chmod +x /usr/bin/SingR && ln -sf /usr/bin/SingR /usr/bin/singr
  ```

### 手动编译和运行

需要 Go 1.25.5 或更高版本（发布构建用的是 1.26.10）。

```sh
git clone https://github.com/makt28/SingR.git && cd SingR
make build
install -m 755 ./sing-box /usr/local/bin/singr
mkdir -p /etc/singr/certs
cp release/poet/panel_anytls.json /etc/singr/panel.json   # 用 hysteria2 / naive 就换成 panel_hysteria2.json / panel_naive.json
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
