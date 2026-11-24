#!/usr/bin/env python3
"""Build native OpenWrt APKv3 and IPK packages from one staged filesystem."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[1]
NAME = 'luci-app-fsd'
VERSION = '1.7.1'
ARCH = 'aarch64_generic'
DEPS = ['firewall4', 'nftables', 'dnsmasq', 'uhttpd', 'uhttpd-mod-ubus',
        'luci-ssl', 'rpcd', 'rpcd-mod-iwinfo', 'curl', 'ca-bundle', 'iw', 'conntrack']
IMAGE = 'alpine@sha256:020dfcbaaf4cc1078bf2d9c7ba31a8466e334061dcd2f248001d68f79e52c000'

def put(source, destination, mode=0o644):
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, destination)
    destination.chmod(mode)

def write(root, name, content, mode=0o644):
    target = root / name
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content)
    target.chmod(mode)

def stage(binary):
    root = ROOT / 'build/root'
    if root.exists(): shutil.rmtree(root)
    root.mkdir(parents=True)
    files = ROOT / 'files'
    put(binary, root / 'usr/sbin/fsd-service', 0o755)
    put(files / 'fsd-service.init', root / 'etc/init.d/fsd-service', 0o755)
    put(files / 'fsd-setup', root / 'usr/sbin/fsd-setup', 0o755)
    put(files / 'fsd.config', root / 'etc/config/fsd', 0o600)
    for name in ['config.example.yaml', 'fsd-forward-guard.nft']:
        put(files / name, root / 'usr/share/fsd' / name)
    for source, target in [('overview.js','overview-v2.js'), ('config-editor.js','config-editor-v2.js'), ('overview.css','overview.css')]:
        put(files / 'luci' / source, root / 'www/luci-static/resources/view/fsd' / target)
    put(files / 'luci/menu.json', root / 'usr/share/luci/menu.d/luci-app-fsd.json')
    put(files / 'luci/acl.json', root / 'usr/share/rpcd/acl.d/luci-app-fsd.json')
    put(files / 'luci/rpcd-fsd', root / 'usr/libexec/rpcd/fsd', 0o755)
    write(root, 'usr/share/fsd/package-version', VERSION + '-r1\n')
    write(root, 'lib/upgrade/keep.d/luci-app-fsd', '/etc/config/fsd\n/etc/fsd/\n/etc/fsd-package/\n/etc/nftables.d/45-fsd-forward-guard.nft\n')
    if (ROOT / 'THIRD_PARTY_LICENSES').exists():
        shutil.copytree(ROOT / 'THIRD_PARTY_LICENSES', root / 'usr/share/licenses/luci-app-fsd')
    for directory in root.rglob('*'):
        if directory.is_dir(): directory.chmod(0o755)
    return root

def tar_gz(root):
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode='w', format=tarfile.GNU_FORMAT) as archive:
        for path in sorted(root.rglob('*')):
            info = archive.gettarinfo(str(path), './' + str(path.relative_to(root)))
            info.uid = info.gid = 0
            info.uname = info.gname = 'root'
            info.mtime = 0
            if path.is_file():
                with path.open('rb') as source: archive.addfile(info, source)
            else: archive.addfile(info)
    output = io.BytesIO()
    with gzip.GzipFile(fileobj=output, mode='wb', mtime=0, filename='') as stream: stream.write(raw.getvalue())
    return output.getvalue()

def ipk(root):
    control = ROOT / 'build/control'
    if control.exists(): shutil.rmtree(control)
    control.mkdir()
    size = sum(p.stat().st_size for p in root.rglob('*') if p.is_file())
    write(control, 'control', f'Package: {NAME}\nVersion: {VERSION}-1\nArchitecture: {ARCH}\nDepends: {", ".join(DEPS)}\nSection: luci\nPriority: optional\nMaintainer: Local FSD project\nLicense: LicenseRef-Unspecified\nInstalled-Size: {size}\nDescription: FSD DNS, HTTPS JSON endpoints and LuCI management\n')
    write(control, 'conffiles', '/etc/config/fsd\n')
    for script in ['preinst','postinst','prerm','postrm']: put(ROOT/'files'/script, control/script, 0o755)
    members = [('debian-binary', b'2.0\n'), ('control.tar.gz', tar_gz(control)), ('data.tar.gz', tar_gz(root))]
    target = ROOT / 'dist' / f'{NAME}_{VERSION}-1_{ARCH}.ipk'
    with target.open('wb') as output:
        output.write(b'!<arch>\n')
        for name, data in members:
            header = f'{name + "/":<16}{0:<12}{0:<6}{0:<6}{"100644":<8}{len(data):<10}`\n'
            assert len(header) == 60
            output.write(header.encode('ascii')); output.write(data)
            if len(data) % 2: output.write(b'\n')
    return target

def apk(root):
    conf = '/etc/config/fsd'
    checksum = hashlib.sha256((root / conf.lstrip('/')).read_bytes()).hexdigest()
    write(root, 'lib/apk/packages/luci-app-fsd.conffiles', conf + '\n')
    write(root, 'lib/apk/packages/luci-app-fsd.conffiles_static', f'{conf} {checksum}\n')
    paths = ['/' + str(p.relative_to(root)) for p in root.rglob('*') if p.is_file()]
    write(root, 'lib/apk/packages/luci-app-fsd.list', '\n'.join(sorted(paths)) + '\n')
    target = ROOT / 'dist' / f'{NAME}-{VERSION}-r1.apk'
    # Work on container-owned files to record uid/gid 0 rather than host ownership.
    arguments = ['apk','mkpkg','--info',f'name:{NAME}','--info',f'version:{VERSION}-r1',
                 '--info',f'arch:{ARCH}','--info','license:LicenseRef-Unspecified',
                 '--info','description:FSD DNS, HTTPS JSON endpoints and LuCI management',
                 '--info','origin:local/fsd-openwrt','--info','depends:'+' '.join(DEPS),
                 '--files','/tmp/fsd-root','--output','/work/dist/'+target.name]
    for kind, script in [('pre-install','preinst'),('pre-upgrade','preinst'),('post-install','postinst'),
                         ('post-upgrade','postinst'),('pre-deinstall','prerm'),('post-deinstall','postrm')]:
        arguments += ['--script',kind+':/work/files/'+script]
    for path in [root, *root.rglob('*')]: os.utime(path, (0, 0))
    import shlex
    command = 'cp -a /work/build/root /tmp/fsd-root && chown -R 0:0 /tmp/fsd-root && ' + shlex.join(arguments)
    subprocess.run(['docker','run','--rm','-e','SOURCE_DATE_EPOCH=0','-v',f'{ROOT}:/work',IMAGE,'sh','-ec',command],check=True)
    return target

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--go', default=os.environ.get('GO', 'go'))
    parser.add_argument('--binary', type=Path, help='Use a previously built Linux ARM64 binary')
    args = parser.parse_args()
    (ROOT/'dist').mkdir(exist_ok=True)
    binary = args.binary or ROOT/'dist/fsd-service-linux-arm64'
    if not args.binary:
        env = dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH='arm64', GOTOOLCHAIN='local')
        subprocess.run([args.go,'build','-buildvcs=false','-trimpath','-ldflags=-s -w','-o',str(binary),'.'],cwd=ROOT/'src',env=env,check=True)
    header = binary.read_bytes()[:20]
    if header[:4] != b'\x7fELF' or header[4:6] != b'\x02\x01' or header[18:20] != b'\xb7\x00':
        raise SystemExit('Expected a little-endian Linux ARM64 ELF executable')
    root = stage(binary)
    packages = [ipk(root), apk(root)]
    manifest = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in packages}
    (ROOT/'dist/SHA256SUMS').write_text(''.join(f'{sha}  {name}\n' for name,sha in manifest.items()))
    print(json.dumps(manifest, indent=2))

if __name__ == '__main__': main()
