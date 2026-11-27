# TLS 密钥日志的 NAS 目录

这是现有 `fsd-capture-sftp` 的附加挂载，不创建第二个 SFTP 服务，不改端口或密钥。该容器的 chroot 为 `/srv/sftp`，登录后可见 `/upload`（原抓包目录）和 `/tls-keylogs`（新增目录）。

将 `compose.keylogs.yaml` 放入 NAS 原 SFTP Compose 目录 `/volume1/docker/fsd-sftp`，执行：

```sh
cd /volume1/docker/fsd-sftp
sudo mkdir -p /volume1/docker/fsd-sftp/tls-keylogs
sudo chown "$(sudo docker exec fsd-capture-sftp id -u capture):$(sudo docker exec fsd-capture-sftp id -g capture)" /volume1/docker/fsd-sftp/tls-keylogs
sudo chmod 700 /volume1/docker/fsd-sftp/tls-keylogs
sudo SFTP_DATA_DIR=/volume1/docker/fsd-sftp docker compose -f compose.yaml -f compose.keylogs.yaml config
sudo SFTP_DATA_DIR=/volume1/docker/fsd-sftp docker compose -f compose.yaml -f compose.keylogs.yaml up -d
```

上述容器名、目录和 `capture` 用户均为示例，须替换为实际值；权限使用容器内上传用户的 UID/GID。此附加模板要求 Compose 服务名为 `sftp`、chroot 为 `/srv/sftp`，其他布局需对应修改。之后更新容器也须包含两个 `-f` 参数，或者将新增挂载合并进原 compose.yaml。重建容器会短暂中断上传，设备会保留未确认文件并重试。

FSD 在“服务 → FSD 日志记录”中提供默认关闭的 TLS 密钥记录开关。它复用设备 `/etc/packet-capture/config.json` 的主机、端口、用户，以及原有 SSH 密钥和主机指纹校验；抓包开关可以关闭，但 SFTP 客户端、包装脚本和配置必须保留。密钥文件上传到 `/tls-keylogs`，不会写入 MySQL 或 CSV。

设备缓存在 `/tmp/fsd/tls-keylogs`，目录 0700、文件 0600。每分钟或 1 MiB 轮转，最多 8 MiB，容量不足时丢弃新密钥记录，不删除待上传旧文件、不影响 TLS 服务。上传先写 `.part`，重命名并确认远端大小后才删除本地文件。关闭开关会封存当前文件并继续上传已有缓存。FSD 服务重启会恢复内存目录中的待上传文件；设备重启会清空该目录。状态计数随 FSD 进程重启归零。

下载对应时间段的 PCAP 和 `.keys` 文件到分析电脑，将需要的密钥文件合并，设置 Wireshark → Preferences → Protocols → TLS → `(Pre)-Master-Secret log filename`。密钥通过 Client Random 对应 TLS 会话，不要求和每个 PCAP 一一对应；跨抓包文件的长连接需要保留更早的密钥文件，且抓包应包含握手。建议让密钥与抓包采用相同保留周期。

仅能解密目标设备与 FSD 业务 TLS 建立的会话；不记录管理端口、NAS HTTPS 上传连接或仅监控模式的流量。开启后重新建立客户端 TLS 连接。握手失败可能只留下部分密钥，不能还原未发送的 HTTP 请求。密钥文件与 PCAP 组合可还原会话内容，请不要公开上传这两类文件。
