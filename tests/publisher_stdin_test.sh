#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
python3 - "$root" <<'PY'
import io,json,os,pathlib,shutil,subprocess,sys,tarfile,tempfile
root=pathlib.Path(sys.argv[1])
packet={'version':1,'AWS_ACCESS_KEY_ID':'FAKEACCESS','AWS_SECRET_ACCESS_KEY':'FAKESECRET','AWS_SESSION_TOKEN':'FAKETOKEN'}
valid=json.dumps(packet).encode()
cases=[valid,b' \n'+valid+b'\n',b'',valid[:-1],valid+b'{}',valid+b'garbage',b'x'*16385,
       valid.replace(b'"version": 1',b'"version": 1, "version": 1'),
       valid.replace(b'FAKETOKEN',b'bad\\u0000token'),valid.replace(b'FAKETOKEN',b''),
       valid.replace(b'"version": 1',b'"version": true'),valid.replace(b'FAKETOKEN',b'bad token')]
with tempfile.TemporaryDirectory() as directory:
    w=pathlib.Path(directory);(w/'scripts').mkdir();(w/'bin').mkdir();(w/'input/objects/sha256').mkdir(parents=True)
    script=w/'scripts/publish-materialized-platform-release.sh';shutil.copy(root/'scripts'/script.name,script)
    (w/'input/platform-release.json').write_text('{}')
    programs={
      'git': '''#!/usr/bin/env python3
import sys,tarfile
if 'status' in sys.argv: sys.exit(0)
assert 'archive' in sys.argv
with tarfile.open(fileobj=sys.stdout.buffer,mode='w|'): pass
''',
      'docker': '''#!/usr/bin/env python3
import sys,os,json
from pathlib import Path
w=Path(__file__).resolve().parents[1]
a=sys.argv[1:]
assert '-i' in a and '--log-driver=none' in a
for key in ('AWS_ACCESS_KEY_ID','AWS_SECRET_ACCESS_KEY','AWS_SESSION_TOKEN'):
 assert key not in os.environ and not any(a[i:i+2]==['--env',key] for i in range(len(a)-1))
(w/'docker-config').write_text(json.dumps(dict(args=a,env=dict(os.environ))))
i=a.index('sh');os.execvp('sh',a[i:])
''',
      'nix': '''#!/usr/bin/env python3
import sys,os,json
from pathlib import Path
w=Path(__file__).resolve().parents[1]
assert not any(k in os.environ for k in ('AWS_ACCESS_KEY_ID','AWS_SECRET_ACCESS_KEY','AWS_SESSION_TOKEN'))
(w/'dev-environment').write_text(json.dumps(dict(os.environ)))
a=sys.argv[1:];a=a[a.index('-c')+1:];os.execvp(a[0],a)
''',
      'go': '''#!/usr/bin/env python3
import sys,os
from pathlib import Path
assert os.environ['AWS_ACCESS_KEY_ID']=='FAKEACCESS'
assert os.environ['AWS_SECRET_ACCESS_KEY']=='FAKESECRET'
assert os.environ['AWS_SESSION_TOKEN']=='FAKETOKEN'
assert sys.argv[1:]==['-C','/work','run','./cmd/control-plane','release','publish','--store','s3://fixture','--input','/input']
Path(__file__).resolve().parents[1].joinpath('published').touch()
'''}
    for name,body in programs.items():
        path=w/'bin'/name;path.write_text(body);path.chmod(0o755)
    env=dict(os.environ,PATH=str(w/'bin')+os.pathsep+os.environ['PATH'])
    # Stdin mode must remove even hostile ambient key values before Docker.
    env.update(AWS_ACCESS_KEY_ID='ambientkey',AWS_SECRET_ACCESS_KEY='ambientsecret',AWS_SESSION_TOKEN='ambienttoken')
    for index,data in enumerate(cases):
        (w/'published').unlink(missing_ok=True)
        result=subprocess.run([str(script),'--credentials-stdin','s3://fixture',str(w/'input')],
                              input=data,env=env,capture_output=True)
        assert result.returncode==(0 if index<2 else 65),(index,result.returncode,result.stderr)
        assert (w/'published').exists()==(index<2)
        for marker in ('FAKEACCESS','FAKESECRET','FAKETOKEN','ambientkey','ambientsecret','ambienttoken'):
            assert marker.encode() not in result.stdout+result.stderr
            assert marker not in (w/'docker-config').read_text()
            assert marker not in (w/'dev-environment').read_text()
print('ok - stdin receiver runs after Nix without Docker/config credentials; malformed packets reject')
PY
