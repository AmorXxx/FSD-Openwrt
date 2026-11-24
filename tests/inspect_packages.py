#!/usr/bin/env python3
import hashlib
import io
from pathlib import Path
import tarfile

root = Path(__file__).resolve().parents[1]
data = (root/'dist/luci-app-fsd_1.7.0-1_aarch64_generic.ipk').read_bytes()
assert data[:8] == b'!<arch>\n'
offset, members = 8, {}
while offset < len(data):
    header = data[offset:offset+60]
    assert header[58:] == b'`\n'
    name = header[:16].decode().strip().rstrip('/')
    size = int(header[48:58])
    members[name] = data[offset+60:offset+60+size]
    offset += 60 + size + size % 2
assert members['debian-binary'] == b'2.0\n'
with tarfile.open(fileobj=io.BytesIO(members['control.tar.gz']), mode='r:gz') as tar:
    control = tar.extractfile('./control').read().decode()
    assert 'Architecture: aarch64_generic' in control and 'conntrack' in control
    assert tar.extractfile('./conffiles').read() == b'/etc/config/fsd\n'
    for name in ['preinst','postinst','prerm','postrm']:
        assert tar.getmember('./'+name).mode == 0o755
with tarfile.open(fileobj=io.BytesIO(members['data.tar.gz']), mode='r:gz') as tar:
    names = set(tar.getnames())
    assert './usr/sbin/fsd-setup' in names
    assert './etc/nftables.d/45-fsd-forward-guard.nft' not in names
    assert not any(name.endswith(('.key','.crt')) or '/etc/fsd/' in name for name in names)
    for item in tar.getmembers(): assert item.uid == item.gid == 0
    executable = tar.extractfile('./usr/sbin/fsd-service').read()
    assert executable[:4] == b'\x7fELF' and executable[18:20] == b'\xb7\x00'
    for name in ['usr/sbin/fsd-setup','usr/sbin/fsd-service','etc/init.d/fsd-service','usr/libexec/rpcd/fsd']:
        assert tar.getmember('./'+name).mode == 0o755
for line in (root/'dist/SHA256SUMS').read_text().splitlines():
    digest, name = line.split('  ',1)
    assert hashlib.sha256((root/'dist'/name).read_bytes()).hexdigest() == digest
print('PASS: IPK layout, ARM64 ELF, ownership, hooks, config preservation, no device secrets, SHA256')
