#!/usr/bin/env python3
"""Native offline IAM migration plan; fixture state only, never AWS state.

Use the actual IAM blocks/moved declarations with literal artifact dependencies.
Seed realistic singleton state from a native mock apply, then use the real AWS
provider planner with refresh and all credential/metadata discovery disabled.
"""
import json
import os
from pathlib import Path
import subprocess
import tempfile

MODULE = Path(__file__).resolve().parents[1]


def run(root, *args):
    result = subprocess.run(['tofu', *args], cwd=root, env=ENV,
                            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if result.returncode:
        raise RuntimeError(result.stdout)
    return result.stdout


ENV = {k: v for k, v in os.environ.items() if not k.startswith('AWS_')}
ENV.update(AWS_EC2_METADATA_DISABLED='true', AWS_CONFIG_FILE='/dev/null',
           AWS_SHARED_CREDENTIALS_FILE='/dev/null')
with tempfile.TemporaryDirectory(prefix='bootstrap-publisher-moves-') as directory:
    root = Path(directory)
    source = (MODULE / 'main.tf').read_text()
    iam = source[source.index('resource "aws_iam_role" "platform_publisher"'):source.index('resource "aws_s3_bucket_policy"')]
    for reference, literal in {
        'aws_s3_bucket.platform_store.arn': '"arn:aws:s3:::fixture-platform"',
        'aws_kms_key.platform_store.arn': '"arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000001"',
        'aws_ecr_repository.controlplane_releases.arn': '"arn:aws:ecr:us-east-1:000000000000:repository/fixture"',
        'data.aws_region.current.region': '"us-east-1"',
    }.items():
        # Preserve interpolation syntax by substituting local references.
        name = 'fixture_' + reference.replace('.', '_')
        iam = iam.replace(reference, 'local.' + name)
        iam = f'locals {{ {name} = {literal} }}\n' + iam
    (root / 'main.tf').write_text('locals { name = lower(var.name) }\n' + iam)
    for name in ('variables.tf', 'moved.tf'):
        (root / name).write_text((MODULE / name).read_text())
    (root / 'versions.tf').write_text('''terraform {
  required_providers { aws = { source = "hashicorp/aws", version = ">= 6.0" } }
}
provider "aws" {
  region = "us-east-1"
  access_key = "fixture-not-a-credential"
  secret_key = "fixture-not-a-credential"
  skip_credentials_validation = true
  skip_requesting_account_id = true
  skip_metadata_api_check = true
  skip_region_validation = true
}
''')
    (root / 'fixture.auto.tfvars.json').write_text(json.dumps({
        'name': 'fixture', 'platform_publisher_principal_arns': ['arn:aws:iam::000000000000:role/publisher']}))
    (root / 'fixture.tftest.hcl').write_text('''mock_provider "aws" {}
run "seed" { command = apply }
''')
    run(root, 'init', '-backend=false', '-input=false', '-no-color')
    events = [json.loads(line) for line in run(root, 'test', '-json', '-verbose').splitlines() if line.startswith('{')]
    state = next(event['test_state'] for event in events if event.get('type') == 'test_state')
    # Mock providers omit real provider defaults (notably role path="/").
    # Obtain those known defaults from an empty-state native plan; retain mock
    # values only for computed unknown attributes. No deployment state is used.
    run(root, 'plan', '-refresh=false', '-input=false', '-out=initial.plan', '-no-color')
    initial = json.loads(run(root, 'show', '-json', 'initial.plan'))
    initial_changes = {change['address']: change['change'] for change in initial['resource_changes']}
    resources = []
    for resource in state['values']['root_module']['resources']:
        planned = initial_changes[resource['address']]
        attributes = {key: (value if planned['after_unknown'].get(key) is True else planned['after'].get(key))
                      for key, value in resource['values'].items()}
        resources.append(dict(mode='managed', type=resource['type'], name=resource['name'],
                              provider='provider["registry.opentofu.org/hashicorp/aws"]',
                              instances=[dict(schema_version=resource['schema_version'], attributes=attributes, sensitive_attributes=[])]))
    (root / 'terraform.tfstate').write_text(json.dumps(dict(version=4, terraform_version=state['terraform_version'], serial=1,
                                                          lineage='11111111-1111-1111-1111-111111111111', outputs={}, resources=resources)))
    run(root, 'plan', '-refresh=false', '-input=false', '-out=enabled.plan', '-no-color')
    enabled = json.loads(run(root, 'show', '-json', 'enabled.plan'))
    changes = enabled['resource_changes']
    assert len(changes) == 2, changes
    for change in changes:
        assert change['previous_address'] == change['address'].removesuffix('[0]'), change
        assert change['change']['actions'] == ['no-op'], change
    run(root, 'plan', '-refresh=false', '-input=false', '-var=create_platform_publisher=false', '-out=disabled.plan', '-no-color')
    disabled = json.loads(run(root, 'show', '-json', 'disabled.plan'))
    assert len(disabled['resource_changes']) == 2
    assert all(change['change']['actions'] == ['delete'] for change in disabled['resource_changes'])
    print('PASS: native singleton moves preserve both enabled IAM resources; disabling deletes only those fixture resources.')

# Full composition retention, using native mock lifecycle state from two runs.
# OpenTofu 1.10 assertions cannot reference prior run outputs directly.
events = [json.loads(line) for line in run(MODULE, 'test', '-filter=tests/optional_publisher.tftest.hcl',
                                         '-json', '-verbose').splitlines() if line.startswith('{')]
states = {event['@testrun']: event['test_state'] for event in events if event.get('type') == 'test_state'}
def stores(state):
    return {resource['address']: resource for resource in state['values']['root_module']['resources']
            if not resource['type'].startswith('aws_iam_')}
assert stores(states['explicit_enabled'])
assert stores(states['explicit_enabled']) == stores(states['disable_existing_publisher_preserves_stores'])
print('PASS: full-module native mock disable retains every non-IAM resource and its attributes.')
