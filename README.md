# FSD for OpenWrt

面向 ARM64 OpenWrt 的 DNS、HTTPS JSON 响应及 LuCI 管理服务。

## 功能

- 统一 DNS 规则、临时拦截和 DNS 查询监控。
- 多个 HTTPS 域名及各自的证书；支持 HTTP/2 和 HTTP/1.1。
- 完整接口路径匹配；单独 `*` 覆盖所有路径，路径内的 `*` 匹配一段非空路径。精确规则优先。
- 按指定客户端 IPv4/IPv6 地址应用规则。
- 多套独立 YAML 配置：另存、复制、重命名、切换、删除及失败回退。
- 活动连接与域名筛选、诊断、自启和出网保护。

网络、热点及上游由 OpenWrt 管理。FSD 忽略旧配置中的 `wifi` 节点，保存或切换后不再写入。FSD 仍会按启用流程配置其 DNS 服务、监听端口和必要的防火墙规则。

日志和 DNS 查询时间使用 OpenWrt 已应用的系统时区（优先读取 `/etc/TZ`，否则读取 `/etc/localtime`），显示完整日期和 24 小时时间，不显示 UTC 偏移，也不使用浏览器时区。系统时区修改并应用后，服务约 1 秒内读取新配置；已有记录保留事件发生时的时间。

## 实机界面

以下为 ZTE U30Air 上 FSD 的实机截图，使用 LuCI Aurora 主题。截图仅做裁剪、缩放和敏感信息遮盖；热点名、设备地址及业务域名未公开。不同 LuCI 主题的外观可能不同。

### YAML 配置文件与作用设备

保存多套 YAML 配置，随时切换；可将规则限定到指定设备。

![FSD YAML 配置文件管理与指定设备设置](docs/images/fsd-profiles.png)

### DNS 规则

在可视化页面维护统一 DNS 规则，选择指定 IP、上游解析或阻断。

![FSD DNS 记录与持久黑白名单编辑](docs/images/fsd-dns-editor.png)

### 服务自检

检查 HTTPS、JSON 接口、DNS、网络及服务状态。截图保留当时的真实结果（13 项通过、2 项失败）；失败项为 SIM IPv6 和 Wi-Fi 热点检查，实际结果取决于设备当时的网络状态。

![FSD 服务自检实机结果](docs/images/fsd-diagnostics.png)

## 项目结构

| 路径 | 内容 |
|---|---|
| `src/` | Go 服务与单元测试 |
| `files/luci/` | LuCI 页面、RPC 入口和权限定义 |
| `files/` | 初始化服务、安装钩子、通用配置示例 |
| `scripts/build.py` | ARM64 编译及 APK/IPK 构建 |
| `tests/` | 包检查、生命周期、迁移及网络测试 |
| `Makefile` | OpenWrt SDK 软件包定义 |
| `install.sh` | 安装及迁移预检入口 |
| `THIRD_PARTY_LICENSES/` | 第三方依赖许可证 |

服务和程序名称为 `fsd-service`，软件包名称为 `luci-app-fsd`，RPC 对象为 `fsd`。配置位于 `/etc/fsd/`，管理 socket 为 `/var/run/fsd-service/fsd.sock`，防火墙表为 `fsd_service`。

## 构建

需要 Python 3、Docker，以及满足 `src/go.mod` 的 Go 工具链。在项目根目录执行：

```sh
python3 scripts/build.py --go /path/to/go
python3 tests/inspect_packages.py
```

默认只编译一次 ARM64 程序，再分别封装 `aarch64_generic` 和 `aarch64_cortex-a53` 的 APK/IPK。可重复传入 `--arch` 指定其他 ARM64 固件架构标识：

```sh
python3 scripts/build.py --go /path/to/go --arch aarch64_cortex-a53
```

架构标识应以目标固件的 `apk --print-arch` 或 `opkg print-architecture` 输出为准。安装脚本自动选择固件接受的包；没有匹配包时停止，不强制绕过检查。其他目标的内核与依赖兼容性仍需验证。后续可使用 OpenWrt SDK 自动生成目标架构元数据。

生成的 `dist/` 包含 Linux ARM64 程序、APK、IPK 和 SHA256SUMS；不要提交生成文件到源码仓库。APK 使用固定摘要的 Alpine 构建环境，构建需要访问镜像仓库及 Go 依赖源。

OpenWrt SDK 方式使用根目录 Makefile；其构建依赖预先生成的 `dist/fsd-service-linux-arm64`。

## 从下载安装包到首次使用

下面命令中 `192.168.50.1` 是示例路由器 LAN 地址，操作前替换为你自己的设备地址。网络名称、密码、SIM 上网均先在 OpenWrt 中配置好。

### 1. 确认设备条件

本项目不是固件，不能刷入，也不能安装在普通电脑或 Android 手机上。当前二进制只支持 Linux ARM64 的 OpenWrt；发行包包含 `aarch64_generic` 和 `aarch64_cortex-a53` 两种架构标识；APK 和 IPK 分别对应 apk、opkg 包管理器。

在设备 SSH 中检查：

```sh
uname -m
command -v apk
command -v opkg
uci get network.lan.device
uci get network.lan.proto
ip -4 addr show dev br-lan
```

应为 `aarch64`，至少有一个包管理器；LAN 使用 `br-lan` 和 `static`，且只有一个 IPv4 前缀。还需 firewall4/nftables、dnsmasq、LuCI、rpcd、uhttpd 等兼容组件，软件源需与固件匹配。其他架构、旧版 fw3/iptables 或不同 LAN 结构不能直接照本说明安装。

首次启用需要 5355、8444 端口空闲；53 可由 dnsmasq 使用，443 可由 uhttpd 使用。建议用网线或 USB 网络保持管理连接，避免首次启用调整 DNS/防火墙时失联。先在 LuCI 备份 OpenWrt 配置并下载到电脑。

### 2. 下载、解压和上传

普通用户前往 [最新 Release](https://github.com/AmorXxx/FSD-Openwrt/releases/latest)，下载 **FSD-OpenWrt 安装 ZIP**，不需要源码包。解压后将安装目录重命名为 `FSD-OpenWrt`，便于直接使用下面的命令。目录结构如下（`*` 表示文件名中的版本部分，实际文件无需改名）：

```text
FSD-OpenWrt/
├── README.md
├── install.sh
└── dist/
    ├── luci-app-fsd-*_aarch64_generic.apk
    ├── luci-app-fsd-*_aarch64_cortex-a53.apk
    ├── luci-app-fsd_*_aarch64_generic.ipk
    ├── luci-app-fsd_*_aarch64_cortex-a53.ipk
    └── SHA256SUMS
```

把整个目录上传到设备 `/tmp/`。例如在电脑终端进入解压目录的上一级，再执行：

```sh
scp -O -r FSD-OpenWrt root@192.168.50.1:/tmp/
ssh root@192.168.50.1
```

Windows 用户也可以用支持 SCP 的文件传输工具上传。`/tmp` 重启后会清空，这是安装文件的临时目录；安装后的配置不会保存在这里。

安装后首次进入 LuCI 的 FSD 页面会显示启用说明，不会自动跳转到 8444。页面沿用当前 LuCI 地址；HTTP 下配置编辑保持只读，编辑请使用已配置且可访问的 LuCI HTTPS 地址。

### 3. 安装插件

以下命令均在设备 SSH 中执行：

```sh
cd /tmp/FSD-OpenWrt
(cd dist && sha256sum -c SHA256SUMS)
```

校验必须全部为 `OK`。按设备实际包管理器更新软件索引，二选一：

```sh
apk update
# 如果使用 opkg，则执行：opkg update
```

再安装：

```sh
sh ./install.sh
```

脚本自动选择 APK 或 IPK，并先做迁移预检。缺少依赖时需要可用的软件源；遇到架构、内核依赖或迁移错误应先处理，不能强制覆盖。APK 为本地构建包，脚本只对指定文件使用 `--allow-untrusted`，不修改全局信任设置。

**首次安装后默认未启用。** 已启用的同名软件包升级后会重新启动服务。

### 4. 核对设备接口并启用

FSD 的默认 AP 配置节是 `ap0`，蜂窝逻辑接口是 `wan`，其他设备可能不同。先查看：

```sh
uci show wireless | grep -E '=wifi-iface|\.mode='
uci show network | grep '=interface'
uci show fsd
```

找到 `mode='ap'` 对应的无线配置节，以及实际蜂窝接口。这里使用 **UCI 配置节名称**，不是 `wlan0` 或 `sipa_eth8` 这样的网卡名。例如确认实际为 `ap0` / `wan` 后：

```sh
uci set fsd.main.ap_section='ap0'
uci set fsd.main.modem_interface='wan'
uci commit fsd
fsd-setup check
```

若 AP 节是匿名节，可填写类似 `@wifi-iface[0]` 的实际名称。上面的 fsd 设置仅指定 FSD 读取哪个接口，不会替你创建热点或蜂窝网络。

看到 `FSD prerequisites passed.` 后再执行：

```sh
fsd-setup enable
fsd-setup status
```

成功时会输出管理页面地址。首次 enable 会把 DNS 转发器移到 5355，FSD 使用 53，业务 HTTPS 使用 443，LuCI HTTPS 调整到 8444；同时配置 DHCP DNS 和必要的防火墙保护，关闭流量卸载。失败时尝试恢复本次修改前的设置。不要在启用过程中关闭管理连接。

### 5. 打开管理页面

用连接设备 LAN 的电脑或手机访问：

```text
https://192.168.50.1:8444/cgi-bin/luci/admin/services/fsd
```

使用 **OpenWrt/LuCI 的账号密码**登录，不是单独的 FSD 密码。默认管理证书为设备生成的自签证书；确认访问的是自己的设备后处理浏览器的证书提示。

也可以从 LuCI「服务 → FSD」进入。若刚升级看不到菜单或新按钮，退出重新登录，并强制刷新浏览器。

本安装包没有预设 VPN，也没有自动开放 8445。远程访问应先自行配置 VPN 和管理访问权限，不能直接把管理端口暴露到公网。

## 配置 DNS、HTTPS 和 JSON 接口

### 6. 先运行通用示例

首次启动包含演示域名 `fsd-install.home.arpa` 和 `/api/demo` 接口。可以直接在「自检」页测试 DNS、HTTPS 和 JSON。

客户端应使用设备作为 DNS 服务器；通常重新连接热点或更新 DHCP 租约即可获取。浏览器安全 DNS、客户端 DoH/DoT、其他 VPN 或缓存可能绕过这个 DNS 设置。

如客户端有 curl，可运行下面的演示测试（替换设备 IP）：

```sh
nslookup fsd-install.home.arpa 192.168.50.1
curl -k --noproxy '*' --resolve fsd-install.home.arpa:443:192.168.50.1 \
  https://fsd-install.home.arpa/api/demo
```

预期 DNS 返回设备 IP，curl 返回包含 `"ok":true` 的 JSON。`--resolve` 只验证 HTTPS 服务，不替代 DNS 测试；`-k` 仅用于这个自签证书测试，真实客户端仍须信任服务端证书。

### 7. 创建自己的配置

在「高级 YAML」中修改完整配置，先点校验，再保存并应用。以下是一个完整演示配置，`demo.home.arpa` 可替换为你的测试域名：

```yaml
version: 1
device_scope:
  mode: all
  addresses: []
dns_rules: []
h2_sites:
- domain: demo.home.arpa
  h2_routes:
  - path: /api/demo
    json: '{"ok":true,"message":"Hello from FSD"}'
  - path: /api/devices/*/status
    json: '{"enabled":true}'
```

各部分用途：

| 配置 | 用途 |
|---|---|
| `dns_rules` | 统一 DNS 规则：`answer` 指定 IPv4（`ap` 表示 LAN 地址）、`forward` 上游解析、`block` 阻断 |
| `h2_sites[].domain` | HTTPS 服务接收的域名，应与对应 DNS 记录一致 |
| `h2_sites[].h2_routes` | 每个站点独立匹配完整路径并处理请求 |
| `device_scope` | 规则作用于全部设备或指定客户端 |

路径 `/api/demo` 不匹配 `/x/api/demo`；`/api/devices/*/status` 可匹配 `/api/devices/123/status`，不能匹配多层的 `/api/devices/a/b/status`。单独配置 `path: "*"` 可匹配 `/` 和任意层级路径。优先级为精确路径 > 单段通配规则 > 单独 `*`，不受兜底规则在列表中的位置影响。区分大小写和末尾斜杠。请求到达 FSD 后，未匹配接口返回 404，不会转发给原站。接口规则仅属于所在的 HTTPS 站点。先按 HTTP Host / HTTP/2 :authority 选择站点（精确域名优先，其次更具体的通配后缀），再匹配该站点的路径。未匹配站点或路径返回 404，不会回退到其他站点的接口；空接口列表同样返回 404。

可视化编辑区可以修改设备范围、DNS 规则、HTTPS 站点（域名、证书链路径、私钥路径）和 JSON 接口；高级 YAML 可查看完整配置。HTTPS 站点支持添加或删除，正常模式至少保留一项，最多 16 项。不要同时保留两种编辑器的未保存草稿。

### 8. 只对指定客户端生效

例如在可视化页面选择「指定设备」，或将 YAML 对应部分改为：

```yaml
device_scope:
  mode: selected
  addresses:
    - 192.168.50.100
```

在 OpenWrt 中为该设备设置 DHCP 静态租约。若客户端使用 IPv6，也需填写实际 IPv6 地址；地址变化后需更新。经过其他路由器或代理转发时，FSD 可能只能看到转发方地址。

### 9. DNS 规则与例外

```yaml
dns_rules:
  - name: '*.example.test'
    action: block
  - name: api.example.test
    action: forward
```

精确域名优先于通配符；通配符中后缀更具体的优先，与列表顺序无关。上例阻断子域名，但让 `api.example.test` 使用上游解析。普通域名仅匹配自身；`*.example.test` 匹配任意层级子域名但不包含 `example.test`。每个域名只允许一条规则。临时黑名单仍优先，重启后不保留。

### 10. 使用自己的 HTTPS 证书

将证书和对应私钥安全上传到设备，例如 `/etc/fsd/certs/`，目录权限设为 700，私钥权限设为 600。证书须覆盖配置的域名，客户端还必须信任其签发者。不要把私钥粘贴进 YAML 或公开仓库。

```yaml
h2_sites:
  - domain: demo.home.arpa
    cert: /etc/fsd/certs/demo.crt
    key: /etc/fsd/certs/demo.key
```

`cert` 和 `key` 只接受设备上的绝对文件路径，必须成对填写；不接受 PEM 文本、相对路径或 URL。文件须为不超过 4 MiB 的普通文件。保存/应用时检查文件可读性、证书与私钥是否配对以及域名覆盖范围。YAML 查看与导出保留路径，但不会读取或导出私钥正文。更换文件内容后需重新应用 YAML 或重启服务，才会加载新证书。

旧配置中的内嵌 PEM 不再支持：升级前先将证书和私钥分别保存为设备文件，再将所有 YAML 中的 `cert`、`key` 改为路径；否则这些 YAML 无法加载。移除两个路径字段会恢复使用自动生成的自签证书；不会自动删除原证书文件。

单域名和多域名都使用 `h2_sites` 列表，正常模式需配置 1 至 16 项。旧 `h2_site` 字段已移除，即使为空也会拒绝加载；升级前需将所有配置改为列表格式。多个域名分别配置证书：

```yaml
h2_sites:
  - domain: first.home.arpa
    cert: /etc/fsd/certs/first.crt
    key: /etc/fsd/certs/first.key
  - domain: second.home.arpa
    cert: /etc/fsd/certs/second.crt
    key: /etc/fsd/certs/second.key
```

HTTPS 站点会自动生成 DNS 映射，无需再添加同名 DNS 规则。这些证书用于业务 HTTPS，应答证书不等于 LuCI 管理页面的证书。未指定时使用自签证书；客户端如有证书固定校验，也可能拒绝连接。

### HTTPS 域名通配规则

`h2_sites` 支持前缀 `*.`，例如：

```yaml
h2_sites:
- domain: '*.a.com'
  h2_routes:
  - path: '*'
    json: '{"fallback":true}'
  - path: /api/status
    json: '{"status":"ok"}'
- domain: b.a.com
  h2_routes:
  - path: '*'
    json: '{"fallback":true}'
  - path: /api/status
    json: '{"status":"ok"}'
```

- `*.a.com` 匹配 `b.a.com`、`c.b.a.com` 等任意层级子域名，不匹配 `a.com` 或 `a.com.evil`。
- 精确域名优先；多个通配域名同时匹配时，选择后缀更长的规则。`*.b.a.com` 优先于 `*.a.com`。
- 自动 DNS 映射与显式 DNS 规则共同按精确域名、更具体通配后缀排序；同名配置不允许同时存在。临时黑名单和作用设备范围仍照常生效。
- **规则匹配不改变 TLS 证书标准。** `*.a.com` 证书只覆盖一层子域名，不覆盖 `c.b.a.com`。深层域名需要证书额外包含相应 SAN，或单独配置更具体的 HTTPS 域名及证书。自动生成的通配自签证书也有同样限制，客户端仍需信任证书。
- 自定义证书用于 `*.a.com` 规则时，其 SAN 必须包含 `*.a.com`。此规则不会自动给深层域名签发新证书。
- YAML 中以星号开头的域名或路径请加引号，避免被当作 YAML 别名。

## 多个 YAML 配置文件

1. 点击「导入 YAML 文件」，保留本地文件原名（支持 `.yaml` 和 `.yml`）。同名拒绝覆盖，导入后打开该文件，不自动应用。
2. 在下拉列表选择文件，立即加载它的表单和 YAML，无需先启用。未保存修改会提示确认放弃。
3. 「保存文件」和「保存 YAML 文件」只写入正在编辑的文件，不更新运行配置。其他页面已修改同一文件时，过期保存会被拒绝。
4. 确认内容后点击「应用此文件」，校验成功才热更新；无需重启服务。失败保留运行配置。
5. 页面分别显示「正在编辑」与「当前生效 YAML」。当前生效文件被保存但未应用时，会显示待应用提示。
6. 可以复制、重命名文件；不能删除当前生效文件。下载使用正在编辑的文件原名。

文件位于 `/etc/fsd/profiles/`，`/etc/fsd/config.yaml` 是启动时读取的当前文件链接。已有文件保留原名。**保存当前生效文件后，即使尚未手动应用，服务或设备重启时也会读取该文件的新内容；需要长期保留未启用配置时，应编辑另一个文件。** 网络、热点及 WireGuard 由 OpenWrt 管理。证书等外部文件共用；保存时保留所编辑文件自己的证书引用，不从当前运行文件复制证书。

## 仅监控模式

安装后会提供 `仅监控.yaml`（已有同名文件不会被覆盖）。选择它并点击「应用此文件」，即可监控全部设备经过本机的普通 DNS 查询和活动连接，同时不执行 FSD 的 DNS 改写、持久/临时黑名单或业务 HTTPS JSON 应答。管理页面保持可用，FSD 出网保护解除。切换回普通 YAML 后恢复对应规则；之前的临时黑名单仍保留，会重新参与匹配。

也可以导入 [仅监控.yaml](files/仅监控.yaml)，或创建以下文件：

```yaml
version: 1
monitor_only: true
device_scope:
  mode: all
  addresses: []
dns_rules: []
```

仅监控文件不配置 HTTPS 站点（`h2_sites` 可省略或为空），也不填写 DNS 改写或黑白名单。可视化拦截表单在此模式下禁用；高级 YAML 仍可查看和保存。未设置 `monitor_only` 的已有文件继续按普通模式运行。

此模式仍需 FSD 和上游 DNS 正常运行，并非停止 FSD。它不解密 DoH、DoT 或 VPN 内部的 DNS，也无法记录客户端缓存命中时未发出的查询。切换后客户端已有 DNS 缓存不会自动清除，必要时等待过期或刷新缓存。

## 自检、升级、停用与备份

```sh
fsd-setup status
ubus call fsd checks '{"group":"all"}'
logread -e fsd-service
```

自检是设备侧测试，还应从目标客户端验证实际 DNS 和接口访问。热点关闭或 SIM 接口不可用会使对应项失败，不等于所有 FSD 功能都失败。

升级前备份整个配置目录，保留符号链接，并下载备份到电脑：

```sh
umask 077
tar -czf /tmp/fsd-backup.tar.gz /etc/fsd /etc/fsd-package /etc/config
```

若证书存放在其他目录，还需一并备份；备份可能包含密码和私钥，不可上传 GitHub。随后上传新安装包并运行新版 `install.sh`。软件包升级不会用示例覆盖已有方案和证书；目录迁移保留必要的兼容链接。

**需要恢复普通上网时使用停用命令，不要仅停止服务：**

```sh
fsd-setup disable
```

停用会恢复 FSD 管理的 DNS、防火墙和管理端口设置，保留用户之后的修改和业务配置。停用后管理页端口可能恢复到原来的 443。直接停止 `fsd-service` 可能触发出网保护，导致受保护客户端无法上网。

确认停用成功后，如需卸载，再按实际包管理器二选一：

```sh
apk del luci-app-fsd
# 或：opkg remove luci-app-fsd
```

## 常见问题

| 现象 | 处理方法 |
|---|---|
| 架构或依赖不匹配 | 确认 ARM64、包管理器和固件软件源；不要强装其他架构或不匹配内核依赖 |
| `Uncommitted UCI changes` | 在 LuCI 保存并应用或放弃待提交修改，然后重跑 check |
| `Set fsd.main.ap_section` / cellular interface 错误 | 按第 4 步填写实际 UCI 节名称 |
| LAN 不是 br-lan/static | 当前自动启用流程不支持此布局，先确认网络规划，不要盲目修改远程设备 |
| 端口占用 | 查找占用 53/443/5355/8444 的服务，解决冲突后再启用 |
| 管理页面打不开 | 未启用时使用原 LuCI 地址查看启用说明；启用后使用命令输出的地址，或已配置的 LuCI HTTPS 地址；检查服务状态和日志 |
| DNS 解析未变化 | 核对客户端 DNS、作用设备 IP，重新连接热点或更新 DHCP，检查缓存及安全 DNS/VPN |
| HTTPS 证书错误 | 核对域名、证书/私钥及客户端信任；curl 的测试跳过校验不能解决客户端信任问题 |
| JSON 返回 404 | 检查完整路径、大小写、斜杠、单段通配符及当前文件 |
| 停止服务后无法上网 | 出网保护可能正在生效；要恢复原网络请执行 `fsd-setup disable` |
| 升级遇到目录冲突 | 保留两边配置和备份，先解决迁移冲突，不要强制覆盖或删除证书 |


## DNS 通配记录

DNS 记录支持 `*.a.com`，匹配 `b.a.com`、`c.b.a.com` 等任意层级子域名，不匹配根域名 `a.com`。仅支持开头的 `*.`，不支持单独 `*`、中间通配符或 `?`。

```yaml
dns:
  - name: "*.a.com"
    ip: ap
  - name: "*.b.a.com"
    ip: 192.0.2.20
  - name: c.b.a.com
    ip: 192.0.2.30
```

精确域名优先，其次使用后缀最长的通配记录，因此上例 `x.a.com` 返回设备 AP 地址，`x.b.a.com` 返回 `192.0.2.20`，`c.b.a.com` 返回 `192.0.2.30`。这些规则与 HTTPS 域名自动生成的 DNS 记录共同匹配；相同域名模式不能同时配置 HTTPS 站点和显式 DNS 规则。作用设备范围、黑白名单和仅监控模式的行为不变；DNS 通配记录不会自动配置 HTTPS 站点或扩展证书覆盖范围。YAML 中以星号开头的域名应使用引号。

## HTTPS JSON 响应模式

每个站点内的 `h2_routes` 条目可单独选择响应模式：省略 `response_mode`（或填 `json`）时正常返回 JSON；填 `close` 时先记录完整请求，再终止请求，不返回 HTTP 状态码和响应体：HTTP/2 发送 RST_STREAM，仅重置当前流；HTTP/1.1 关闭连接。可视化编辑中的“响应模式”中的“记录后终止请求”提供同样的选项；修改后需保存并应用当前 YAML。

```yaml
h2_sites:
- domain: demo.home.arpa
  h2_routes:
  - path: /api/demo
    response_mode: close
  - path: '*'
    json: '{"ok":true}'
```

关闭模式可省略 `json`；已有 JSON 内容会保留，方便切回正常响应。精确路径仍优先于通配规则。客户端可能收到流重置或连接中断错误并重试；HTTP/2 保留连接，同连接上的其他请求继续处理，HTTP/1.1 则关闭连接。此模式不是网络层 DROP，TLS 握手和请求接收已经发生。日志写入失败时会显示错误，仍按协议重置当前流或关闭连接；诊断页面对关闭模式跳过 HTTP 200/JSON 校验，不把普通网络错误当成验证成功。

## NAS 数据库日志（可选）

参见 [NAS 部署说明](nas-mysql/README.md)。在 OpenWrt「服务 → FSD 日志记录」页面配置 HTTPS 接收地址、设备标识、上传令牌和可选 CA 证书路径，开启 NAS 上传。上传设置独立保存在 `/etc/fsd/event-upload.json`，不随业务 YAML 切换。上传队列只使用内存。日志表用 `success` 区分 TLS_HANDSHAKE_FAILED、REQUEST_INCOMPLETE、REQUEST_RECEIVED。FSD 监控页显示上传及丢失计数。

页面显示已上传、待上传、缓存占用、丢失、重试、最近上传时间和错误，以及本地 CSV 保存/失败计数。令牌不回显，留空保留已有值。保存并应用无需重启 FSD；存在待上传缓存时不能修改 NAS 参数或关闭 NAS，但可独立切换 CSV。修改 NAS 配置会重新启动上传组件并重置其计数。测试按钮仅验证 HTTPS 与令牌，不新增数据库记录，也不代表数据库写入健康检查。CSV 仅记录命中的请求，TLS 握手失败仅由 NAS 事件记录。关闭任何开关均不删除历史记录。

内存预算 64 MiB，满时丢弃最旧待传事件；单条事件放不下则整条丢弃，不截断冒充完整记录。接口继续应答；重启会丢失未上传事件。本地 CSV 与 NAS 上传是两个独立开关，支持单独开启、同时开启或全部关闭，关闭 NAS 不会自动开启 CSV。旧配置首次读取时保持原有记录方式；页面保存后显式保存两个开关。

## HTTPS JSON 请求记录

开启本地 CSV 后，命中自定义 HTTPS JSON 路由时，在 `/etc/fsd/request-logs/` 保存 CSV；仅在本机保存，不上传 NAS。每次命中追加一条 CSV 记录，文件只在创建时写一行列名。当前文件达到 5 MiB 后，下一次请求创建新文件继续追加；单条记录不会拆分，因此单个文件可以超过 5 MiB。服务重启后继续追加到尚未达到轮转大小的最新文件。文件随重启保留，切换 YAML 不清除历史记录。

记录列为 `time`、`source_ip`、`request_domain`、`request_path`、`request_body`：时间使用系统时区，不显示时区偏移；源 IP 取实际连接地址，不采信转发请求头；域名取 HTTP Host/HTTP2 authority（去除端口）；路径包含查询参数；请求体以收到的字节内容保存，逗号、双引号和换行按 CSV 引号规则处理，不做 JSON 格式化或截断。压缩或二进制请求体不会自动解码。

CSV 内容总量上限为 100 MiB，同时最多保留 10,000 份，达到任一限制自动删除最旧文件。文件系统元数据可能额外占用空间。请求体采用流式写入，当前文件加上完整记录超过总容量上限、客户端传输中断或磁盘写入失败时，回退本次追加，保留此前的完整记录，并在 FSD 日志显示“HTTPS JSON 请求 CSV 保存失败”；JSON 接口继续正常应答。每次追加前保存恢复检查点，服务异常退出后在下一次记录前回退未提交的尾部内容；未完成的新文件 `.part` 同时清理。请求体中的换行保留在 CSV 单元格内，一条记录可能占据多个文本行。

目录权限为 0700、文件为 0600，可通过管理员 SSH/SFTP 下载；不通过公开 HTTP 接口提供。仅监控模式、非目标设备和未匹配路由不生成 CSV。诊断自检若命中 JSON 路由也会生成记录。请求内容来自客户端，使用表格软件打开时应按文本导入，避免将内容当作公式执行。日志文件含原始请求数据，不应加入 Git 或安装包。

## 测试

后端使用 Linux 专有网络接口，应在 Linux 环境运行：

```sh
cd src
go test ./...
```

带有网络命名空间和 nftables 副作用的测试默认跳过，仅在隔离测试环境启用。`tests/lifecycle.sh` 和 `tests/name-migration.sh` 会写入系统路径，只能在可丢弃容器中运行，不能在生产设备上直接执行。具体验证范围见 VALIDATION.md。

## 发布与隐私

此目录是独立源码副本，不含设备配置、私钥、证书、备份、日志、Git 历史或历史安装包。提交前仍应检查后续新增文件；`.gitignore` 不会移除已被 Git 跟踪的秘密。

项目自身尚未指定开源许可证，本次整理没有擅自授予授权。第三方依赖许可证保留在 THIRD_PARTY_LICENSES；如希望作为开源项目发布，请由权利人选择并加入项目许可证。

## 任何疑问请联系作者TG：@Amor_Paul

### 统一 DNS 规则的匹配

```yaml
dns_rules:
  - name: '*.a.com'
    action: answer
    ip: ap
  - name: b.a.com
    action: forward
  - name: blocked.a.com
    action: block
```

`b.a.com` 转发上游，保留 A、AAAA 等查询应答；`blocked.a.com` 返回 NXDOMAIN，其余 `*.a.com` 返回设备 LAN IPv4。`answer` 支持单个 IPv4 或 `ap`（省略 IP 默认 ap），AAAA 查询返回空应答；`forward`、`block` 不填写 IP。规则最多 1024 条，只支持精确域名和开头 `*.` 通配符，不支持 `?` 或中间通配符。

作用范围和仅监控模式先判断；目标设备的临时黑名单优先。显式 DNS 规则与 HTTPS 站点自动生成的 `answer: ap` 映射共同比较：精确域名优先，其次更具体的通配后缀；同名配置不允许同时存在，三种动作没有额外优先级。没有匹配项才查询上游。因此精确 HTTPS 站点可以覆盖宽泛 `block`，相同域名模式同时出现在 `h2_sites` 和 `dns_rules` 会拒绝保存或应用，并提示冲突域名。`dns_rules: []` 清空显式规则，但仍保留 HTTPS 站点自动映射。要让精确站点走上游，先删除该同名 HTTPS 站点，再添加 `forward` 规则；通配站点下可以配置更具体域名的 `forward` 例外。

仅支持 `dns_rules`。旧 `dns`、`dns_passthrough`、`dns_blacklist` 字段已移除，即使为空也会拒绝加载。升级前必须将所有 YAML 改为新格式；服务和可视化页面不执行自动迁移。

DNS 规则不会改变 HTTPS 接口行为，也不会执行反向代理。客户端缓存旧 IP 时应刷新 DNS 或等待缓存过期。上游结果可能来自缓存。基于域名的阻断还会使用已有域名/IP 关联执行过滤，因此共用 IP 的域名可能互相影响；DNS 放行不保证绕过针对共用 IP 的其他防火墙规则。

系统日志与监控页中的 HTTPS JSON 命中日志包含客户端 IP/端口、请求方法、Host 域名、实际请求路径（含查询参数）、HTTP 协议、TLS SNI、匹配的路径规则和处理动作。通配规则命中时仍显示实际请求路径。正常返回还记录写入字节数；写入失败会单独标明。命中请求体最多打印前 4 KiB，按读取顺序分段输出，通过请求编号与命中日志关联；结束行记录已读取和已打印字节数、读取是否完整以及日志是否截断。该限制仅影响系统日志和监控页日志，不截断 CSV 或数据库请求记录。系统日志容量有限，旧行会被覆盖，完整历史以数据库或 CSV 为准。日志不包含 Authorization 或 Cookie 请求头；字段中的控制字符会转义，避免伪造日志行。查询参数会原样记录，请留意其中可能包含的敏感信息。

### TLS 会话密钥与抓包分析

在「服务 → FSD 日志记录」启用独立的 TLS 密钥记录开关（默认关闭）。只记录设备范围内客户端与 FSD 业务 TLS 的会话，仅监控模式不记录。开启后需重新建立连接。密钥暂存 `/tmp/fsd/tls-keylogs`，每分钟或 1 MiB 轮转，总量上限 8 MiB；满时丢弃新密钥记录并显示丢失计数，继续接口服务。

复用独立抓包服务的 SFTP 配置上传到 NAS `/tls-keylogs`；先确认上传文件大小，再删除本地副本。关闭后仍上传缓存，设备重启会丢失未上传密钥。页面显示记录条数、丢失条数、已上传文件数、重试次数、缓存占用和错误。密钥不写入 MySQL、本地 CSV 或系统日志。

NAS 需增加隔离目录映射，详见 [TLS 密钥日志 NAS 配置及 Wireshark 使用方法](nas-keylogs/README.md)。无法解密客户端直接连接原站的流量。

监控页面的「清理日志」按钮清空 FSD 内存中的页面运行日志，刷新后不会恢复旧条目，新日志继续显示。需要写权限；不影响系统日志、DNS 查询记录、连接统计、CSV、数据库、TLS 密钥和抓包文件，也不暂停服务。

### 每个 HTTPS 站点单独配置接口

接口列表必须放在 `h2_sites` 对应站点的 `h2_routes` 内。顶层 `h2_routes` 已移除，即使是空列表也会拒绝导入和应用。旧 YAML 升级前需将原接口列表移入需要这些接口的各站点；不同站点允许相同路径，单个站点内路径不可重复，每站点最多 8 条。未填写接口或填写 `h2_routes: []` 表示该站点无 JSON 接口。

可视化页面在每个站点卡片内提供“添加此站点接口”，分别编辑路径、JSON 和响应模式。保存仍只修改所选 YAML，应用后才生效。自检按站点分别请求，结果包含域名和路径。
