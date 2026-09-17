"""Install real packed SDK/proto through npm; run a downloaded CLI and real builder."""
import base64
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
import urllib.parse
import urllib.request
from contract import canonical, descriptor, digest, read, require


def run(*args, **kwargs):
    subprocess.run(list(map(str, args)), check=True, **kwargs)


def buildx_state(env):
    raw = subprocess.check_output(['docker', 'buildx', 'ls', '--format', '{{json .}}'], env=env, text=True)
    # ls repeats identical context rows on native Buildx. Compare semantic identity.
    records = {}
    for line in raw.splitlines():
        item = json.loads(line)
        value = dict(name=item['Name'], driver=item['Driver'], current=item['Current'],
                     nodes=[{key: node.get(key) for key in ('Name', 'Endpoint', 'Status')} for node in item['Nodes']])
        require(item['Name'] not in records or records[item['Name']] == value, 'conflicting native builder rows')
        records[item['Name']] = value
    return records


def consumer(directory, cli_archive, work, expected_builder=None):
    directory, work = Path(directory), Path(work)
    if expected_builder is None:
        expected_builder = read(directory / 'bundle-builder.json')['image']
    public = os.environ.get('HELMR_PUBLIC_CONSUMER') == '1'
    build_env = dict(os.environ)
    owned_context = owned_builder = None
    packages = {}
    for file, name in (('sdk.tgz', '@helmr/sdk'), ('proto.tgz', '@helmr/proto')):
        with tarfile.open(directory / file) as archive:
            metadata = json.load(archive.extractfile('package/package.json'))
        packages[name] = (metadata, directory / file)
    version = packages['@helmr/sdk'][0]['version']
    cli_archive = Path(cli_archive)
    checksum_path = cli_archive.parent / 'checksums.txt'
    # Local bootstrap fixture; published verification uses the actual signed build index.
    install_index = (directory / 'release-build.json')
    index_bytes = install_index.read_bytes() if install_index.exists() else canonical(dict(schema='helmr.release.v0', version='v' + version, assets={'checksums.txt': descriptor(checksum_path)}))
    require(packages['@helmr/sdk'][0]['dependencies']['@helmr/proto'] == version, 'SDK sibling dependency differs')

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            path = urllib.parse.unquote(self.path).split('?', 1)[0].removeprefix('/')
            if path == cli_archive.name:
                raw = cli_archive.read_bytes()
            elif path == 'checksums.txt':
                raw = checksum_path.read_bytes()
            elif path == 'release-index.json':
                raw = index_bytes
            elif path in ('sdk.tgz', 'proto.tgz'):
                raw = (directory / path).read_bytes()
            elif path in packages:
                metadata, file = packages[path]
                raw = json.dumps(dict(name=path, versions={version: dict(metadata, dist=dict(
                    tarball=f'http://127.0.0.1:{server.server_port}/{file.name}',
                    integrity='sha512-' + base64.b64encode(hashlib.sha512(file.read_bytes()).digest()).decode()))})).encode()
            else:
                # Ordinary external dependencies remain ordinary registry packages.
                try:
                    with urllib.request.urlopen('https://registry.npmjs.org/' + self.path.removeprefix('/'), timeout=60) as response:
                        raw = response.read()
                except Exception:
                    self.send_error(502)
                    return
            self.send_response(200)
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        work.mkdir()
        if public:
            require(not os.environ.get('BUILDX_BUILDER'), 'public first-use proof must not select an ambient builder')
            context = os.environ.get('DOCKER_CONTEXT') or subprocess.check_output(['docker', 'context', 'show'], text=True).strip()
            endpoint = subprocess.check_output(['docker', 'context', 'inspect', context, '--format', '{{.Endpoints.docker.Host}}'], text=True).strip()
            require(endpoint.startswith('unix://'), 'public local-builder fixture requires a Unix-socket Docker daemon')
            original_context = subprocess.check_output(['docker', 'context', 'show'], text=True).strip()
            proposed_context = 'helmr-consumer-' + work.parent.name + '-' + str(os.getpid())
            run('docker', 'context', 'create', proposed_context, '--docker', 'host=' + endpoint)
            owned_context = proposed_context
            build_env.update(DOCKER_CONTEXT=owned_context, BUILDX_CONFIG=str(work / 'buildx'))
            build_env.pop('DOCKER_HOST', None)
            owned_builder = 'helmr-' + hashlib.sha256((owned_context + '\n' + endpoint).encode()).hexdigest()[:24]
            before = buildx_state(build_env)
            selected = {k: v for k, v in before.items() if v['current']}
            require(selected and all(v['driver'] == 'docker' for v in selected.values()), 'first use must start on Docker driver')
            require(owned_builder not in before, 'first use must not have a Helmr builder')

        shim = work / 'tools'
        shim.mkdir()
        real_curl = shutil.which('curl')
        require(real_curl, 'curl is required by the shipped installer')
        # Redirect only the release transport in this fixture, keeping the shipped
        # installer unchanged and using its real download/checksum/extraction path.
        prefix = f'https://github.com/helmrdotdev/helmr/releases/download/v{version}/'
        (shim / 'curl').write_text(f'#!{sys.executable}\nimport os,sys\na=[x.replace({prefix!r}, {("http://127.0.0.1:" + str(server.server_port) + "/")!r}) for x in sys.argv[1:]]\nos.execv({real_curl!r}, [{real_curl!r}]+a)\n')
        (shim / 'curl').chmod(0o755)
        run('bash', Path(__file__).resolve().parents[2] / 'install', '--version', 'v' + version, '--no-modify-path',
            env=dict(os.environ, PATH=str(shim) + os.pathsep + os.environ['PATH'], HELMR_INSTALL_DIR=str(work / 'bin')))
        project = work / 'project'
        project.mkdir()
        (project / 'tasks').mkdir()
        # Normal documented source selection: dependencies are installed in BuildKit.
        # Keep the ordinary npm cache selected, including its long filenames.
        (project / '.helmrignore').write_text('node_modules/\n')
        (project / 'package.json').write_text(json.dumps(dict(name='release-consumer', private=True, type='module',
            dependencies={'@helmr/sdk': version}, devDependencies={'typescript': '7.0.2'})))
        registry = 'https://registry.npmjs.org' if os.environ.get('HELMR_PUBLIC_CONSUMER') == '1' else f'http://127.0.0.1:{server.server_port}'
        env = dict(os.environ, npm_config_registry=registry, npm_config_cache=str(project / '.npm-cache'))
        run('npm', 'install', '--ignore-scripts', '--no-audit', '--no-fund', cwd=project, env=env)
        lock = read(project / 'package-lock.json')
        for name, (_, file) in packages.items():
            expected = 'sha512-' + base64.b64encode(hashlib.sha512(file.read_bytes()).digest()).decode()
            require(lock['packages']['node_modules/' + name]['integrity'] == expected, 'installed package bytes differ from signed set')
        installed = read(project / 'node_modules/@helmr/sdk/package.json')
        require(installed['helmr'] == packages['@helmr/sdk'][0]['helmr'], 'installed SDK stamp differs')
        require(read(project / 'node_modules/@helmr/proto/package.json')['version'] == version, 'installed proto differs')
        (project / 'helmr.config.ts').write_text('import {defineConfig} from "@helmr/sdk"; export default defineConfig({dirs:["tasks"]});\n')
        (project / 'tasks/hello.ts').write_text('import {task,sandbox,image} from "@helmr/sdk"; '
            'export const hello=task({id:"preview-hello",run:()=>"preview-ok"});\n'
            'export const machine=sandbox({id:"preview-machine"}).image(image("preview-base").from('
            + json.dumps(expected_builder) + ')).resources({cpu:1,memory:"1GiB"});\n')
        (project / 'consumer.ts').write_text('''import {HelmrClient, source} from "@helmr/sdk";
const file = source.file("./package.json");
if (!file) throw Error("source.file failed");
let called = false;
const client = new HelmrClient({url:"https://example.invalid",apiKey:"fixture",fetch:async (url, init)=>{
 if(String(url)!=="https://example.invalid/v1/tasks/preview-hello/start" || init?.method!=="POST") throw Error("SDK wire contract");
 called=true; return Response.json({run_id:"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"});
}});
await client.tasks.start("preview-hello", {payload:null,workspace:client.workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")});
if(!called) throw Error("SDK request missing");
''')
        run(project / 'node_modules/.bin/tsc', '--strict', '--skipLibCheck', 'false', '--target', 'ES2022', '--module', 'NodeNext', '--moduleResolution', 'NodeNext', 'consumer.ts', cwd=project)
        run('node', 'consumer.js', cwd=project)
        identity = subprocess.check_output([str(work / 'bin/helmr'), '--version'], text=True).strip()
        require(identity == 'v' + version + ' (' + installed['helmr']['sourceCommit'] + ')', 'CLI embedded source/version differs')
        require(expected_builder.encode() in (work / 'bin/helmr').read_bytes(), 'CLI embedded builder reference differs')
        # npm fetched exact package bytes normally. Its cache allows the same
        # lockfile install inside BuildKit without exposing a host test server.
        run(work / 'bin/helmr', 'build', project, '--output', work / 'bundle',
            '--install-command', 'npm ci --offline --cache .npm-cache --ignore-scripts --no-audit --no-fund', env=build_env)
        bundle = read(work / 'bundle/bundle.json')
        require(bundle['contract'] == 'helmr.deployment-bundle.v0', 'wrong bundle contract')
        require({(d['kind'], d['declaredId']) for d in bundle['plan']['definitions']} == {('task', 'preview-hello'), ('sandbox', 'preview-machine')}, 'compiled fixture definitions differ')
        require(len(bundle['workspaceImages']) == 1 and bundle['workspaceImages'][0]['declaredId'] == 'preview-machine', 'workspace OCI stage missing')
        runtime = read(directory / 'bundle-builder.json')['runtime']
        require(bundle['runtime']['artifact']['digest'] == runtime['digest'], 'bundle did not use canonical Runtime')
        if public:
            after = buildx_state(build_env)
            builder = after.get(owned_builder)
            require(builder and builder['driver'] == 'docker-container' and len(builder['nodes']) == 1, 'CLI did not prepare its builder')
            require(builder['nodes'][0]['Endpoint'] == owned_context and builder['nodes'][0]['Status'] == 'running', 'wrong builder endpoint/readiness')
            require({k: v for k, v in after.items() if v['current']} == selected, 'CLI changed selected builder')
            run(work / 'bin/helmr', 'build', project, '--output', work / 'bundle-reuse',
                '--install-command', 'npm ci --offline --cache .npm-cache --ignore-scripts --no-audit --no-fund', env=build_env)
            require(buildx_state(build_env)[owned_builder] == builder, 'second build replaced the prepared builder')
            require(read(work / 'bundle-reuse/bundle.json') == bundle, 'reused builder changed the bundle')
            require(subprocess.check_output(['docker', 'context', 'show'], text=True).strip() == original_context, 'global context changed')
            evidence = dict(context=owned_context, endpoint=endpoint, before=before, after=after, reused=owned_builder)
            (work / 'builder-preparation.json').write_text(json.dumps(evidence, sort_keys=True))
            print(json.dumps(evidence, sort_keys=True))
        print(json.dumps(dict(cli=digest(cli_archive), builder=expected_builder,
            sdk=digest(directory / 'sdk.tgz'), proto=digest(directory / 'proto.tgz'),
            identity=identity, bundle=digest(work / 'bundle/bundle.json'), runtime=runtime['digest']), sort_keys=True))
        print('actual downloaded CLI + installed SDK/proto + canonical builder consumer passed')
    finally:
        if owned_builder:
            subprocess.run(['docker', 'buildx', 'rm', owned_builder], env=build_env, check=False)
        if owned_context:
            subprocess.run(['docker', '--context', 'default', 'context', 'rm', owned_context], check=False)
        server.shutdown()
        thread.join()
        server.server_close()
