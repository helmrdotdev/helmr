#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
bun build "$repo_root/tests/fixtures/native-continuation/model.ts" --target=node --outfile "$tmp/model.mjs" >/dev/null
python3 - "$tmp/model.mjs" <<'PY_CHECK'
import json,subprocess,sys,tempfile,time,urllib.request
from pathlib import Path
with tempfile.TemporaryDirectory() as td:
 p=Path(td)
 def launch(output, saved=None):
  args=['node',sys.argv[1],str(output),'203.0.113.1']
  if saved:args.append(str(saved))
  process=subprocess.Popen(args,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
  try:
   for _ in range(100):
    if output.exists():
     try: ports=json.loads(output.read_text())
     except json.JSONDecodeError: pass
     else: return process,ports
    if process.poll() is not None:raise RuntimeError(process.communicate())
    time.sleep(.03)
   raise RuntimeError('model startup timeout')
  except BaseException:
   process.terminate()
   try:process.wait(timeout=5)
   except subprocess.TimeoutExpired:process.kill();process.wait()
   raise

 def request(port,provider):
  body={'tools':[{'name':'enqueue'}],'messages':[{'role':'user','content':'input'}]}
  r=urllib.request.Request('http://127.0.0.1:'+str(port)+('/v1/messages' if provider=='claude' else '/responses'),data=json.dumps(body).encode(),headers={'Content-Type':'application/json'})
  return urllib.request.urlopen(r).read().decode()
 first,ports=launch(p/'first.json')
 try:
  for provider in ['codex','claude']:
   request(ports[provider],provider);request(ports[provider],provider)
  history=json.load(urllib.request.urlopen('http://127.0.0.1:'+str(ports['proof'])))
  assert all(len(history[provider])==2 for provider in ['codex','claude'])
 finally:first.terminate();first.wait(timeout=5)
 (p/'saved.json').write_text(json.dumps({'ModelConfig':ports,'ModelHistory':history}))
 second,restored=launch(p/'second.json',p/'saved.json')
 try:
  assert restored==ports,(restored,ports)
  for provider in ['codex','claude']:
   reply=request(restored[provider],provider)
   assert provider+'-3' in reply,reply
  after=json.load(urllib.request.urlopen('http://127.0.0.1:'+str(restored['proof'])))
  assert all(len(after[x])==3 and after[x][:2]==history[x] for x in history)
 finally:second.terminate();second.wait(timeout=5)
 print('PASS: independent model process binds identical ports and preserves request history/tool sequence')
PY_CHECK
