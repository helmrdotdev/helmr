"""Exercise the actual platform archive/provenance writer with a fixture Nix output."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT=Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts/release'))
from contract import digest

class Provenance(unittest.TestCase):
    def test_platform_output_uses_selected_source_not_workflow_sha(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);bin_dir=root/'bin';bin_dir.mkdir();release=root/'release';release.mkdir()
            (release/'platform-release.json').write_text('{"formatVersion":0}')
            nix=bin_dir/'nix';nix.write_text('#!/bin/sh\nprintf "%s\\n" "$PLATFORM_FIXTURE"\n');nix.chmod(0o755)
            source=subprocess.check_output(['git','-C',str(ROOT),'rev-parse','HEAD'],text=True).strip()
            env=dict(os.environ,PATH=str(bin_dir)+os.pathsep+os.environ['PATH'],PLATFORM_FIXTURE=str(release),
                     RELEASE_SOURCE_COMMIT=source,RELEASE_SOURCE_REF='refs/pull/7/head',GITHUB_SHA='0'*40,GITHUB_REF='refs/heads/main')
            subprocess.run(['bash',str(ROOT/'scripts/build-platform-release.sh'),str(root/'output')],env=env,check=True)
            provenance=json.loads((root/'output/platform-release-provenance.json').read_text())
            self.assertEqual(provenance['sourceCommit'],source)
            self.assertEqual(provenance['sourceRef'],'refs/pull/7/head')
            self.assertEqual(provenance['archive']['digest'],digest(root/'output/platform-release.tar'))
            result=subprocess.run(['bash',str(ROOT/'scripts/build-platform-release.sh'),str(root/'bad')],env=dict(env,RELEASE_SOURCE_COMMIT='0'*40),capture_output=True)
            self.assertNotEqual(result.returncode,0);self.assertFalse((root/'bad').exists())
