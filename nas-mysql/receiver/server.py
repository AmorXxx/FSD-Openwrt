"""Authenticated event ingestion. One JSON metadata line followed by raw body bytes."""
import datetime
import hmac
import ipaddress
import json
import os
import socket
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import pymysql

TOKEN = Path('/run/secrets/upload_token').read_text().strip()
PASSWORD = Path('/run/secrets/mysql_ingest_password').read_text().strip()
MAX_BODY = 64 * 1024 * 1024
MAX_META = 65536
STATES = {'TLS_HANDSHAKE_FAILED', 'REQUEST_INCOMPLETE', 'REQUEST_RECEIVED'}
if len(TOKEN) < 32:
    raise RuntimeError('Upload token is missing or too short')


def validate(meta, body):
    if str(uuid.UUID(meta['event_id'])) != meta['event_id']:
        raise ValueError('Invalid event UUID')
    ipaddress.ip_address(meta['source_ip'])
    datetime.datetime.strptime(meta['event_time'], '%Y-%m-%d %H:%M:%S.%f')
    if meta['success'] not in STATES or meta['device_id'] != os.environ.get('DEVICE_ID', 'mu300'):
        raise ValueError('Invalid event identity or result')
    port = meta.get('source_port')
    if port is not None and (type(port) is not int or not 0 <= port <= 65535):
        raise ValueError('Invalid source port')
    for key, maximum in [('device_id', 128), ('domain', 253), ('sni', 253), ('request_method', 32), ('request_path', 32768), ('error', 8192)]:
        value = meta.get(key)
        if value is not None and (not isinstance(value, str) or len(value.encode('utf-8')) > maximum):
            raise ValueError('Invalid metadata field')
    if meta['success'] == 'TLS_HANDSHAKE_FAILED':
        if body or meta.get('request_path') is not None or meta.get('request_method') is not None:
            raise ValueError('TLS failure cannot contain an HTTP request')
    elif not meta.get('request_path') or not meta.get('request_method'):
        raise ValueError('Missing HTTP request metadata')


def insert(meta, body):
    connection = pymysql.connect(
        host=os.environ['DB_HOST'], port=int(os.environ.get('DB_PORT', '13306')), user='fsd_ingest', password=PASSWORD,
        database='fsd_logs', charset='utf8mb4', autocommit=True,
        ssl_ca='/run/secrets/mysql_tls_ca', ssl_verify_cert=True, ssl_verify_identity=True,
        connect_timeout=5, read_timeout=30, write_timeout=30, local_infile=False)
    try:
        columns = ['event_id', 'device_id', 'event_time', 'success', 'source_ip', 'source_port',
                   'domain', 'sni', 'request_method', 'request_path', 'error']
        values = [meta.get(key) for key in columns]
        values.append(None if meta['success'] == 'TLS_HANDSHAKE_FAILED' else body)
        with connection.cursor() as cursor:
            cursor.execute('INSERT INTO fsd_request_events (' + ','.join(columns) + ',request_body) VALUES (' + ','.join(['%s'] * len(values)) + ')', values)
        return False
    except pymysql.err.IntegrityError as error:
        if error.args[0] != 1062:
            raise
        # This schema has only one unique key: the producer-generated event UUID.
        return True
    finally:
        connection.close()


class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def setup(self):
        super().setup()
        self.connection.settimeout(15)

    def log_message(self, *args):
        pass  # Never log request payloads, tokens or SQL errors containing data.

    def reply(self, status, payload):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        self.send_header('Connection', 'close')
        self.end_headers()
        self.close_connection = True
        self.wfile.write(data)

    def do_GET(self):
        self.reply(200 if self.path == '/healthz' else 404, {'ok': self.path == '/healthz'})

    def do_POST(self):
        if self.path != '/v1/events':
            return self.reply(404, {'error': 'not_found'})
        auth = self.headers.get('Authorization', '')
        if not hmac.compare_digest(auth.encode(), ('Bearer ' + TOKEN).encode()):
            return self.reply(401, {'error': 'unauthorized'})
        self.connection.settimeout(90)
        try:
            if self.headers.get('Transfer-Encoding') or len(self.headers.get_all('Content-Length', [])) != 1:
                return self.reply(400, {'error': 'content_length_required'})
            size = int(self.headers['Content-Length'])
            if not 0 < size <= MAX_BODY + MAX_META:
                return self.reply(413, {'error': 'event_too_large'})
            line = self.rfile.readline(min(size, MAX_META) + 1)
            if len(line) > size or len(line) > MAX_META or not line.endswith(b'\n'):
                return self.reply(400, {'error': 'invalid_metadata'})
            remaining = size - len(line)
            if remaining > MAX_BODY:
                return self.reply(413, {'error': 'event_too_large'})
            body = self.rfile.read(remaining)
            if len(body) != remaining:
                return self.reply(503, {'error': 'incomplete_upload'})
            meta = json.loads(line)
            validate(meta, body)
        except (ValueError, KeyError, TypeError, UnicodeError):
            return self.reply(400, {'error': 'invalid_event'})
        except (OSError, socket.timeout):
            return
        try:
            duplicate = insert(meta, body)
        except pymysql.MySQLError:
            return self.reply(503, {'error': 'database_unavailable'})
        self.reply(200, {'event_id': meta['event_id'], 'stored': True, 'duplicate': duplicate})


class Server(ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 8
    slots = threading.BoundedSemaphore(2)

    def process_request(self, request, client_address):
        if not self.slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except Exception:
            self.slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.slots.release()

    def handle_error(self, request, client_address):
        print('request_failed', flush=True)


if __name__ == '__main__':
    Server((os.environ.get('LISTEN_HOST', '127.0.0.1'), int(os.environ.get('LISTEN_PORT', '18080'))), Handler).serve_forever()
