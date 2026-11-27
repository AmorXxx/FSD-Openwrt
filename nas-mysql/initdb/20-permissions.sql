-- Remove every grant initially created by the official image.
REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'fsd_ingest'@'%';
GRANT INSERT ON fsd_logs.fsd_request_events TO 'fsd_ingest'@'%';
ALTER USER 'fsd_ingest'@'%' REQUIRE SSL WITH MAX_USER_CONNECTIONS 5;
