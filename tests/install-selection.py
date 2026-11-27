#!/usr/bin/env python3
"""Exercise installer selection with fake managers; never install a real package."""
from pathlib import Path
import os, shutil, subprocess, tempfile
root=Path(__file__).resolve().parents[1]
for manager,arch,want in [('apk','aarch64_generic','aarch64_generic'),('apk','aarch64_cortex-a53','aarch64_cortex-a53'),('apk','aarch64_cortex-a72',None),('opkg','aarch64_cortex-a53','aarch64_cortex-a53'),('opkg','aarch64_cortex-a72',None)]:
 with tempfile.TemporaryDirectory() as tmp:
  d=Path(tmp);bin=d/'bin';bin.mkdir();(d/'dist').mkdir();shutil.copy(root/'install.sh',d/'install.sh')
  for name in ['dirname','sort','awk']: (bin/name).symlink_to(shutil.which(name))
  scripts={'id':'echo 0','uname':'echo aarch64'}
  if manager=='apk':scripts[manager]=f'if [ "$1" = --print-arch ]; then echo {arch}; else echo "INSTALL $*"; fi'
  else:scripts[manager]=f'if [ "$1" = print-architecture ]; then echo "arch all 1"; echo "arch {arch} 20"; else echo "INSTALL $*"; fi'
  for name,body in scripts.items():p=bin/name;p.write_text('#!/bin/sh\n'+body+'\n');p.chmod(0o755)
  for a in ['aarch64_generic','aarch64_cortex-a53']:
   (d/'dist'/f'luci-app-fsd-1.7.16-r1_{a}.apk').touch();(d/'dist'/f'luci-app-fsd_1.7.16-1_{a}.ipk').touch()
  result=subprocess.run(['/bin/sh',str(d/'install.sh')],env={**os.environ,'PATH':str(bin)},text=True,capture_output=True)
  if want: assert result.returncode==0 and 'INSTALL ' in result.stdout and want in result.stdout,(result.stdout,result.stderr)
  else: assert result.returncode!=0 and 'INSTALL ' not in result.stdout
print('PASS: APK/opkg architecture selection and unsupported architecture refusal')
