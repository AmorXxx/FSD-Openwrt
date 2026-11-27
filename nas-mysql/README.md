# 群晖反向代理 → 日志接收服务 → MySQL

FSD 使用 HTTPS 上传事件，数据库不开放公网。MySQL 和接收服务均用 Docker host 网络，不使用 macvlan，也不创建独立 Docker 网络。

## 1. 部署

将全部文件上传到 `/volume1/docker/fsd-mysql/`，目录中直接包含 compose.yaml、prepare.sh、mysql-entrypoint.sh、initdb/、receiver/。把下面的示例域名替换为准备给群晖反向代理使用的域名。

```sh
cd /volume1/docker/fsd-mysql
sudo sh set-root-password.sh
sudo sh prepare.sh fsd-db.example.com 192.168.1.3
sudo docker compose up -d --build
sudo docker compose ps
sudo docker compose logs --tail=80 mysql receiver
```

两个容器应显示 healthy。首次需要拉取镜像并安装 Python MySQL 驱动，可能需要几分钟。支持先执行 prepare.sh，再在 Container Manager 中创建项目；旧版命令可将 docker compose 换为 docker-compose。

| 服务 | NAS 本机地址 | 用途 |
| --- | --- | --- |
| MySQL（fsd-mysql） | 127.0.0.1:13306、NAS 局域网 IP:13306 | 本机写库和局域网管理；不配置公网转发 |
| 接收服务（fsd-log-receiver） | 127.0.0.1:18080 | 群晖反向代理的目标 |
| FSD 上传入口 | https://你的域名:5001/v1/events | HTTPS + 上传令牌 |

不配置 Docker ports；接收服务只绑定回环地址，MySQL 绑定回环地址和 prepare.sh 第二个参数指定的 NAS 局域网 IPv4。MySQL X Plugin 已关闭，避免另开 33060 端口。部署前确认 NAS 上 13306 和 18080 未被其他服务占用。

set-root-password.sh 让你在终端输入并确认自己的 root 密码（至少 16 个字符，输入隐藏）；prepare.sh 自动生成 INSERT 账号密码、上传令牌和内部数据库 TLS 证书。密码不打印。已有数据库初始化后，不能靠修改密码文件更换 root 密码；设置脚本会拒绝这样操作，需要通过 ALTER USER 改库内密码并同步更新密码文件。重复运行保留已有密码与证书；CA 和服务器私钥仅留在 NAS。服务器证书有效期 825 天，过期前需更新。

## 2. 群晖反向代理

群晖“控制面板 → 登录门户 → 高级 → 反向代理”创建规则：

- 来源协议：HTTPS。
- 来源主机名：你的上传域名，例如 fsd-db.example.com。
- 来源端口：NAS 实际接收公网映射的端口。若路由器公网 5001 → NAS 443，则填 443；其他映射填实际 NAS 端口，不能仅凭公网 URL 推断。
- 目标协议：HTTP。
- 目标主机名：127.0.0.1。
- 目标端口：18080。

在“安全性 → 证书 → 设置”中给该规则分配覆盖上传域名的有效证书。FSD 默认使用系统 CA 验证 HTTPS 证书；若用私有 CA，需额外在设备配置 ca_file，不能关闭证书验证。内部 MySQL 的私有 CA 不是群晖 HTTPS 入口证书的 CA。

保留原始 /v1/events 路径和 Authorization 请求头。请求体大小上限至少配置为 65 MiB（包含协议元数据），上传/读取超时建议至少 90 秒。若 DSM 界面不提供大小设置，需要按实际 DSM 支持方式调整该反向代理规则；出现 HTTP 413 时检查请求体限制。反向代理可能把大请求临时缓冲在 NAS 磁盘，这是 NAS 侧行为，不占用 MU300 闪存。

5001 默认可能已被 DSM 管理界面使用，必须按实际映射避开监听冲突。移除此前计划的公网 MySQL 23306 转发；无需开放 MySQL 端口。

检查本机接收服务：

```sh
curl -fsS http://127.0.0.1:18080/healthz
```

检查反向代理（不要加 -k）：

```sh
curl -fsS https://你的域名:5001/healthz
```

/healthz 仅检查接收进程，不能证明入库成功。写入接口必须带正确上传令牌，并完成数据库提交才返回入库确认。

## 3. 接入 MU300

当前需要包含 NAS 上传功能的 FSD 新版代码；旧版 FSD 不认识此配置，不要只复制文件就假定上传已启用。

prepare.sh 生成两个私密文件：

- client/event-upload.json：默认 enabled=false、local_csv=false，endpoint=https://准备时填写的域名:5001/v1/events，device_id=mu300。
- client/upload-token：设备上传凭据，与 NAS 的 secrets/upload_token.txt 相同。

将它们安全复制到设备 `/etc/fsd/event-upload.json` 和 `/etc/fsd/upload-token`，权限均为 600，然后重启 fsd-service。先在接收服务和反向代理验证就绪后再启用；数据库密码和 CA 私钥不复制到设备。

配置文件独立于业务 YAML，切换业务 YAML 不改变日志上传设置。启用后不再写新的请求 CSV，已有 CSV 不删除。移走配置文件或设 enabled=false 并重启，会恢复旧的本地 CSV 方式；不会自动补传历史 CSV。

## 4. 状态与缓存

事件统一入表 fsd_logs.fsd_request_events，success 枚举：

- TLS_HANDSHAKE_FAILED：握手失败，记录源 IP/端口、能获取到的 SNI、错误；HTTP 路径与请求体为 NULL。
- REQUEST_INCOMPLETE：作用设备命中接口，但请求体读取失败，保存已收到的部分原始内容及原因。
- REQUEST_RECEIVED：作用设备命中接口且完整接收请求体。空请求体是零字节；之后按规则重置 HTTP/2 流或关闭 HTTP/1.1 连接仍属于此类。

握手成功但没有请求、未命中接口、非作用设备的请求不入表；仅监控模式不产生上述业务事件。日志服务缓存不足不能误记成 REQUEST_INCOMPLETE。

MU300 使用固定 64 MiB 内存预算，包含等待上传、正在上传、正在读取的数据和每条事件预留的 64 KiB 元数据空间。超限优先丢弃最旧待传事件；正在传输的事件不驱逐。如果单条请求放不下，整条事件丢弃，接口继续服务，不截断后冒充完整记录。设备重启会丢失未入库事件。

网络或数据库不可用时每 5 秒重试。事件 UUID 不变，数据库唯一主键去重；只有 HTTP 200 且确认编号匹配、stored=true 才计为已入库。400/413/422 表示事件或接收限制不兼容，丢弃并计数，避免阻塞整个队列；401/403 等权限故障继续重试并显示错误。

FSD 监控页面和 `fsd-setup status` 显示待传数、正在上传状态、内存占用、入库数、丢失数、重试数和最后错误，计数在进程重启时清零。内存预算不是整个进程 RSS 上限，TLS、HTTP 及运行时还会额外用内存。

## 5. 数据库权限与检查

FSD 不持有数据库账号。接收服务使用 fsd_ingest，仅有日志表 INSERT，最多 5 个并发连接，强制 TLS。它不能 SELECT、UPDATE、DELETE、ALTER 或 DROP；重复事件直接处理唯一键冲突，不使用 REPLACE 或 ON DUPLICATE KEY UPDATE。

数据持久化于 Docker 命名卷 fsd_mysql_data，避开群晖共享目录的继承 ACL；账号密码及内部证书位于 ./secrets。原 ./data 目录不删除。如果 ./data/mysql 已存在，prepare.sh 会停止，要求先迁移现有数据库，不能把旧数据静默丢到一旁。MySQL 字符集 utf8mb4、时区 +08:00，最大 SQL 包 256 MiB；接收服务最多两个并发处理线程，事件正文最多 64 MiB，保留原始字节。两容器各限制 1 GiB 内存，请确保 NAS 内存充足。数据库不自动删除历史记录。

管理员使用本机 socket：

```sh
sudo docker exec -it fsd-mysql sh -c 'export MYSQL_PWD="$(cat /run/secrets/mysql_root_password)"; exec mysql -uroot fsd_logs'
```

```sql
SHOW GRANTS FOR 'fsd_ingest'@'%';
SELECT event_time, success, source_ip, domain, request_path,
       OCTET_LENGTH(request_body) AS body_bytes, error
FROM fsd_request_events ORDER BY event_time DESC LIMIT 20;
```

initdb 脚本仅在空数据目录首次执行。如果已部署过旧版权限，需由管理员执行 initdb/20-permissions.sql 中的三条语句；重建容器不会自动重置数据库密码或重新初始化。不要删除已有数据。停止项目用 docker compose down，不要加 -v；-v 会删除命名卷中的数据库。

旧版本内部证书如果没有 NAS 局域网 IP，prepare.sh 会备份旧服务器证书，并使用原 CA 和私钥重新签发包含该 IP、127.0.0.1、localhost 及域名的证书。CA、私钥和数据库内容保留。

## 局域网 root 连接

仅允许 `192.168.1.0/24` 来源使用网络 root 账号，不强制 SSL，可选择开启或关闭；不开放任意来源 root。接收服务账号仍仅 INSERT。局域网电脑客户端设置：

- 主机：NAS 局域网 IP，例如 `192.168.1.3`。
- 端口：`13306`。
- 用户：`root`。
- 密码：你在 set-root-password.sh 中输入的密码。
- 数据库：`fsd_logs`。
- SSL：可关闭。如选择启用并验证证书，CA 文件使用 `client/mysql-ca.crt`。

```sh
mysql -h 192.168.1.3 -P 13306 -u root -p \
  --ssl-mode=DISABLED --get-server-public-key fsd_logs
```

DSM 防火墙如已开启，允许局域网来源访问 TCP 13306。不要配置该端口的公网转发。若改了局域网网段，需要同时修改 Compose 中 MYSQL_ROOT_HOST 与 initdb/30-root-tls.sql；已初始化数据库还需管理员更新账号。

## 已遇到 data 目录 Permission denied 的首次部署

先停止旧项目，把新版文件覆盖到原项目目录，但不要覆盖/删除 secrets 和 data：

```sh
cd /volume1/docker/fsd-mysql
sudo docker compose stop
# 此时上传覆盖新版部署文件
sudo sh set-root-password.sh
sudo sh prepare.sh fsd-db.example.com 192.168.1.3
sudo docker compose up -d --build --force-recreate
sudo docker compose ps
```

新版使用 Docker 管理的 fsd_mysql_data 持久卷，不再挂载原来不可写的共享目录。旧 data 目录保持原样。若已经存在初始化完成的命名卷或旧 data/mysql，脚本会拒绝覆盖密码/跳过数据，应先处理密码修改或迁移。

## 6. 备份与边界

备份正在运行的数据库使用 mysqldump --single-transaction --hex-blob --max-allowed-packet=256M，并安全备份 secrets。关闭 binlog，故不支持基于 binlog 的时间点恢复。不要将日志、令牌、私钥或备份上传公开仓库。

本部署包只准备 NAS 服务；不会自动配置群晖反向代理、路由器或部署 MU300。外网映射与实际证书仍需部署后实测。

参考：[官方 MySQL 镜像](https://hub.docker.com/_/mysql)、[群晖反向代理](https://kb.synology.com/en-in/DSM/help/DSM/AdminCenter/system_login_portal_advanced)。

## 已部署数据库取消局域网 SSL 要求

覆盖新版 compose.yaml 和 relax-lan-ssl.sh，保留 secrets 与数据卷。数据库健康后执行：

```sh
cd /volume1/docker/fsd-mysql
sudo sh relax-lan-ssl.sh
sudo docker compose up -d --force-recreate mysql receiver
```

脚本只调整局域网 root 的 TLS 要求及全局安全传输开关，不更改账号密码或数据；新版 Compose 保证重启后保持该行为。接收服务账号的 REQUIRE SSL 不变，上传入口仍是 HTTPS。局域网客户端可关闭 SSL；若提示 Public Key Retrieval is not allowed，在可信局域网连接配置中开启公钥获取，命令行对应 --get-server-public-key。
