# HTTPS 事件接收与单表存储

可部署的接收服务、MySQL Compose、初始化脚本与说明位于 ../nas-mysql/。本目录 schema.sql 是表结构源文件，部署包 initdb/10-schema.sql 保持相同内容。

最终链路为 MU300 HTTPS → 群晖反向代理 → 127.0.0.1:18080 接收服务 → 127.0.0.1:13306 MySQL。两个 NAS 容器使用 host 网络；不使用 macvlan，不开公网 MySQL。

success 枚举为 TLS_HANDSHAKE_FAILED、REQUEST_INCOMPLETE、REQUEST_RECEIVED。请求体以 LONGBLOB 保存原始字节，TLS 失败时为 NULL。完整接收后主动终止流仍属于 REQUEST_RECEIVED。UUID 主键用于重传去重；写库账号仅 INSERT。

设备上传配置独立于业务 YAML：/etc/fsd/event-upload.json，启用后采用 64 MiB 内存队列；本地 CSV 由独立开关控制。缓存不足继续接口服务，统计丢失；未提交的内存事件不会在设备重启后保留。详细部署与状态说明见 ../nas-mysql/README.md。
