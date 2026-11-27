-- Run inside the chosen FSD logging database. Never store DB credentials here.
-- event_id stays unchanged across upload retries to prevent duplicate rows.
CREATE TABLE IF NOT EXISTS fsd_request_events (
    event_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    device_id VARCHAR(128) NOT NULL,
    event_time DATETIME(6) NOT NULL COMMENT 'Device system local time',
    success ENUM(
        'TLS_HANDSHAKE_FAILED',
        'REQUEST_INCOMPLETE',
        'REQUEST_RECEIVED'
    ) NOT NULL,
    source_ip VARCHAR(45) CHARACTER SET ascii NOT NULL,
    source_port SMALLINT UNSIGNED NULL,
    domain VARCHAR(253) NULL,
    sni VARCHAR(253) NULL,
    request_method VARCHAR(32) NULL,
    request_path TEXT NULL,
    request_body LONGBLOB NULL COMMENT 'Raw bytes; partial only for REQUEST_INCOMPLETE',
    error TEXT NULL,
    received_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (event_id),
    KEY device_time (device_id, event_time),
    KEY result_time (success, event_time),
    KEY source_time (source_ip, event_time)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
