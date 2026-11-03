"""Exercise the image build script and validation commands without root or cloud access.

The host installer is `helmr worker install`; its tests live with it in
internal/hostinstall."""
import base64
import gzip
import hashlib
import json
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[5]
TEMPLATES = ROOT / 'infra/aws/modules/worker-image/templates'
PREPARE = TEMPLATES / 'prepare-root.sh'


def digest(data):
    return hashlib.sha256(data).hexdigest()


class WorkerImage(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.runtime = {'initramfs': b'init', 'rootfs.squashfs': b'root', 'vmlinuz': b'kernel'}
        self.runtime_manifest = {'schema': 'helmr.runtime-artifacts.v0', 'arch': 'amd64',
                                 'vm_runtime_contract': 'helmr.vm-runtime.v0'}
        for key, name in [('kernel', 'vmlinuz'), ('initramfs', 'initramfs'), ('rootfs', 'rootfs.squashfs')]:
            data = self.runtime[name]
            self.runtime_manifest[key] = {'digest': 'sha256:' + digest(data), 'size_bytes': len(data)}

    def test_runtime_contract_cases_at_image_gate(self):
        # Evaluate the real component document, then execute the emitted runtime
        # validation command against relocated fixture files.
        source = (TEMPLATES.parent / 'main.tf').read_text()
        expression = source.split('  component_document = ', 1)[1].split('\n  component_definition = ', 1)[0]
        (self.root / 'main.tf').write_text(
            'locals {\n build_script = ""\n prepare_root_digest = "fixture"\n'
            'host_artifacts_manifest_digest = "fixture"\n runtime_artifacts_manifest_digest = "fixture"\n'
            'component_document = ' + expression + '\n}\n')
        result = subprocess.run(['tofu', 'console', '-no-color'], cwd=self.root,
                                input='jsonencode(flatten([for p in yamldecode(local.component_document).phases : [for s in p.steps : s.inputs.commands] if p.name == "validate"]))\n',
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        commands = json.loads(json.loads(result.stdout))
        prefix = 'cd /var/lib/helmr/images/guest/out && '
        runtime_commands = [command for command in commands if command.startswith(prefix)]
        self.assertEqual(len(runtime_commands), 1)
        command = 'cd ' + shlex.quote(str(self.root)) + ' && ' + runtime_commands[0][len(prefix):]
        for name, data in self.runtime.items():
            (self.root / name).write_bytes(data)
        cases = json.loads((ROOT / 'tests/fixtures/runtime-artifacts/contracts.json').read_text())
        for case in cases:
            with self.subTest(case=case['name']):
                manifest = dict(self.runtime_manifest)
                if 'value' in case:
                    manifest['vm_runtime_contract'] = case['value']
                else:
                    manifest.pop('vm_runtime_contract', None)
                (self.root / 'runtime-artifacts.json').write_text(json.dumps(manifest))
                result = subprocess.run(['bash', '-e', '-c', command], text=True, capture_output=True)
                self.assertEqual(result.returncode == 0, case['valid'], result.stdout + result.stderr)

    def test_image_template_runs_the_bundled_installer(self):
        template = TEMPLATES / 'build-worker-image.sh.tftpl'
        # Evaluate the real Terraform template, including its literal shell dollars.
        values = {
            'prepare_root_digest': digest(PREPARE.read_bytes()),
            'host_artifacts_bundle_s3_uri': 's3://fixture/host.tar',
            'runtime_artifacts_bundle_s3_uri': 's3://fixture/runtime.tar',
        }
        for name, fill in [('host_artifacts_bundle_digest', 'a'), ('host_artifacts_manifest_digest', 'b'),
                           ('runtime_artifacts_bundle_digest', 'c'), ('runtime_artifacts_manifest_digest', 'd')]:
            values[name] = fill * 64
        arguments = ['prepare_root_script = trimsuffix(file(' + json.dumps(str(PREPARE)) + '), "\\n")']
        arguments += [key + ' = ' + json.dumps(value) for key, value in values.items()]
        expression = 'templatefile(' + json.dumps(str(template)) + ', {\n' + '\n'.join(arguments) + '\n})'
        (self.root / 'main.tf').write_text('locals { script = ' + expression + ' }\n')
        result = subprocess.run(['tofu', 'console', '-no-color'], cwd=self.root,
                                input='jsonencode(local.script)\n',
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        rendered = json.loads(json.loads(result.stdout))
        embedded = rendered.split("<<'HELMR_PREPARE_ROOT'\n", 1)[1].split('HELMR_PREPARE_ROOT\n', 1)[0]
        self.assertEqual(embedded.encode(), PREPARE.read_bytes())
        # The bundle's own helmr runs only after the bundle matches its pin.
        check = rendered.index("'" + 'a' * 64 + "' \"$work/worker-host-artifacts.tar\" | sha256sum -c -")
        extract = rendered.index('-xf "$work/worker-host-artifacts.tar" helmr')
        self.assertLess(check, extract)
        install = rendered[rendered.index('"$work/installer/helmr" worker install'):]
        for flag, fill in [('--host-bundle-digest', 'a'), ('--host-manifest-digest', 'b'),
                           ('--runtime-bundle-digest', 'c'), ('--runtime-manifest-digest', 'd')]:
            self.assertIn(flag + " 'sha256:" + fill * 64 + "'", install)
        result = subprocess.run(['bash', '-n'], input=rendered, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def image_command(self, script):
        # Evaluate the exact production command with an inert build script.
        source = (TEMPLATES.parent / 'main.tf').read_text()
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


if __name__ == '__main__':
    unittest.main()
