"""Exercise real archive validation without root, cloud access or host mutation."""
import base64
import gzip
import hashlib
import io
import json
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[5]
INSTALL = ROOT / 'infra/aws/modules/worker-image/templates/install-worker-host.sh'
PREPARE = INSTALL.with_name('prepare-root.sh')


def digest(data):
    return hashlib.sha256(data).hexdigest()


class InstallArtifacts(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.host = {name: (b'payload-' + name.encode(), 0o755) for name in
                     ['cpu-template-helper', 'firecracker', 'jailer', 'mkfs.ext4', 'worker']}
        self.host['mke2fs.conf'] = (b'config', 0o444)
        self.manifest = {'schema': 'helmr.worker-host-artifacts.v0', 'arch': 'amd64',
                         'files': [{'path': name, 'mode': f'{mode:04o}', 'size_bytes': len(data),
                                    'digest': 'sha256:' + digest(data)}
                                   for name, (data, mode) in self.host.items()]}
        raw = json.dumps(self.manifest).encode()
        self.host['worker-host-artifacts.json'] = (raw, 0o644)
        self.host_manifest_digest = digest(raw)
        self.runtime = {'initramfs': (b'init', 0o444), 'rootfs.squashfs': (b'root', 0o444)}
        self.runtime_manifest = {'schema': 'helmr.runtime-artifacts.v0', 'arch': 'amd64',
                                 'vm_runtime_contract': 'helmr.vm-runtime.v0'}
        for key, name, data in [('kernel', 'vmlinuz', b'kernel'),
                                ('initramfs', 'initramfs', b'init'),
                                ('rootfs', 'rootfs.squashfs', b'root')]:
            self.runtime_manifest[key] = {'digest': 'sha256:' + digest(data), 'size_bytes': len(data)}
        raw = json.dumps(self.runtime_manifest).encode()
        self.runtime['runtime-artifacts.json'] = (raw, 0o444)
        self.runtime['vmlinuz'] = (b'kernel', 0o444)
        self.runtime_manifest_digest = digest(raw)

    def archive(self, name, members):
        path = self.root / name
        with tarfile.open(path, 'w', format=tarfile.USTAR_FORMAT) as archive:
            for name, value in members.items():
                if isinstance(value, tarfile.TarInfo):
                    archive.addfile(value)
                    continue
                data, mode = value
                entry = tarfile.TarInfo(name)
                entry.size, entry.mode = len(data), mode
                archive.addfile(entry, io.BytesIO(data))
        return path

    def verify(self, *, host_sha=None, manifest_sha=None, umask=-1):
        host = self.archive('host.tar', self.host)
        runtime = self.archive('runtime.tar', self.runtime)
        command = ['bash', str(INSTALL), 'verify', str(host), host_sha or digest(host.read_bytes()),
                   manifest_sha or self.host_manifest_digest, str(runtime),
                   digest(runtime.read_bytes()), self.runtime_manifest_digest, str(PREPARE)]
        return subprocess.run(command, capture_output=True, text=True, umask=umask)

    def test_valid_bundles(self):
        result = self.verify()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('verified Worker host and runtime artifacts', result.stdout)

    def test_restrictive_operator_umask(self):
        result = self.verify(umask=0o077)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_image_template_embeds_exact_installer(self):
        template = INSTALL.with_name('build-worker-image.sh.tftpl')
        # Evaluate the real Terraform template, including its literal shell dollars.
        values = {
            'prepare_root_script': PREPARE.read_text().removesuffix('\n'),
            'prepare_root_digest': digest(PREPARE.read_bytes()),
            'install_host_script': INSTALL.read_text().removesuffix('\n'),
            'install_host_digest': digest(INSTALL.read_bytes()),
            'host_artifacts_bundle_s3_uri': 's3://fixture/host.tar',
            'runtime_artifacts_bundle_s3_uri': 's3://fixture/runtime.tar',
        }
        for name in ['host_artifacts_bundle_digest', 'host_artifacts_manifest_digest',
                     'runtime_artifacts_bundle_digest', 'runtime_artifacts_manifest_digest']:
            values[name] = 'a' * 64
        arguments = []
        for key, value in values.items():
            if key == 'prepare_root_script':
                value_expression = 'trimsuffix(file(' + json.dumps(str(PREPARE)) + '), "\\n")'
            elif key == 'install_host_script':
                value_expression = 'trimsuffix(file(' + json.dumps(str(INSTALL)) + '), "\\n")'
            else:
                value_expression = json.dumps(value)
            arguments.append(key + ' = ' + value_expression)
        expression = 'templatefile(' + json.dumps(str(template)) + ', {\n' + '\n'.join(arguments) + '\n})'
        (self.root / 'main.tf').write_text('locals { script = ' + expression + ' }\n')
        result = subprocess.run(['tofu', 'console', '-no-color'], cwd=self.root,
                                input='jsonencode(local.script)\n',
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        rendered = json.loads(json.loads(result.stdout))
        embedded = rendered.split("<<'HELMR_INSTALL_HOST'\n", 1)[1].split('HELMR_INSTALL_HOST\n', 1)[0]
        self.assertEqual(embedded.encode(), INSTALL.read_bytes())
        self.assertIn(digest(INSTALL.read_bytes()), rendered)
        result = subprocess.run(['bash', '-n'], input=rendered, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def image_command(self, script):
        # Evaluate the exact production command with an inert build script.
        source = (INSTALL.parent.parent / 'main.tf').read_text()
        match = re.search(r'commands = \[<<-SCRIPT\n(.*?)\n\s*SCRIPT', source, re.S)
        self.assertIsNotNone(match)
        (self.root / 'main.tf').write_text(
            'locals {\n build_script = ' + json.dumps(script) +
            '\n command = <<-SCRIPT\n' + match.group(1) + '\nSCRIPT\n}\n')
        result = subprocess.run(['tofu', 'console', '-no-color'], cwd=self.root,
                                input='jsonencode(local.command)\n',
                                text=True, capture_output=True, check=True)
        return json.loads(json.loads(result.stdout))

    def test_image_command_isolates_script_from_child_stdin(self):
        command = self.image_command('set -eu\ncat >/dev/null\nprintf "completed\\n"\n')
        result = subprocess.run(['bash', '-c', command], text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'completed\n')

    def test_image_command_rejects_corrupt_stream_before_execution(self):
        script = 'printf "must-not-execute\\n"\n'
        command = self.image_command(script)
        # Retain the entire decoded script but corrupt the gzip trailer.
        truncated = base64.b64encode(gzip.compress(script.encode())[:-4]).decode()
        command = re.sub(r"printf '%s' '[^']+'", "printf '%s' '" + truncated + "'", command)
        result = subprocess.run(['bash', '-c', command], text=True, capture_output=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('must-not-execute', result.stdout)

    def test_transport_digest_mismatch(self):
        self.assertNotEqual(self.verify(host_sha='0' * 64).returncode, 0)

    def test_manifest_digest_mismatch(self):
        self.assertNotEqual(self.verify(manifest_sha='0' * 64).returncode, 0)

    def test_host_payload_tamper(self):
        self.host['worker'] = (b'wrong', 0o755)
        self.assertNotEqual(self.verify().returncode, 0)

    def test_host_mode_mismatch(self):
        self.host['worker'] = (self.host['worker'][0], 0o644)
        self.assertNotEqual(self.verify().returncode, 0)

    def test_runtime_payload_tamper(self):
        self.runtime['rootfs.squashfs'] = (b'wrong', 0o444)
        self.assertNotEqual(self.verify().returncode, 0)

    def test_link_member_rejected_before_extraction(self):
        entry = tarfile.TarInfo('worker')
        entry.type, entry.linkname = tarfile.SYMTYPE, '/etc/passwd'
        self.host['worker'] = entry
        self.assertNotEqual(self.verify().returncode, 0)

    def test_extra_member_rejected(self):
        self.runtime['unexpected'] = (b'no', 0o444)
        self.assertNotEqual(self.verify().returncode, 0)

    def test_parent_traversal_rejected(self):
        self.host['../escaped'] = (b'no', 0o444)
        self.assertNotEqual(self.verify().returncode, 0)
        self.assertFalse((self.root.parent / 'escaped').exists())


if __name__ == '__main__':
    unittest.main()
