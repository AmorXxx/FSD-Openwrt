-- LAN root may use TLS or plaintext; the ingest account still requires SSL.
ALTER USER 'root'@'192.168.1.0/255.255.255.0' REQUIRE NONE;
