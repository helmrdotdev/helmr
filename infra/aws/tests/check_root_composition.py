#!/usr/bin/env python3
"""Check native self-host and image root plans, without test-only outputs."""
import base64
import gzip
import json
import sys
from pathlib import Path

assert len(sys.argv) == 5, "quickstart, standard, worker-image and private plans are required"

for filename in (sys.argv[2],):
    found = set()
    for line in Path(filename).read_text().splitlines():
        event = json.loads(line)
        if event.get('type') != 'test_plan':
            continue
        run = event['@testrun']
        if run not in ('rollback_default', 'rollback_disabled'):
            continue
        expected = run == 'rollback_default'
        services = {
            resource['address']: resource['change']['after']['deployment_circuit_breaker']
            for resource in event['test_plan']['resource_changes']
            if resource['type'] == 'aws_ecs_service'
        }
        assert services == {
            f'module.controlplane.aws_ecs_service.{name}[0]': [{'enable': True, 'rollback': expected}]
            for name in ('controlplane', 'dispatcher')
        }, (filename, run, services)
        found.add(run)
    assert found == {'rollback_default', 'rollback_disabled'}, (filename, found)
    print(f'ok - {filename}: both compiled services respect root rollback policy')
image_plans = [json.loads(line)['test_plan']
               for line in Path(sys.argv[3]).read_text().splitlines()
               if json.loads(line).get('type') == 'test_plan']
assert len(image_plans) == 1, 'one generic image root plan is required'
plan = image_plans[0]
roles = {r['address']: r['change']['after']['permissions_boundary']
         for r in plan['resource_changes'] if r['type'] == 'aws_iam_role'}
assert roles == {'module.worker_image.aws_iam_role.image_builder':
                 plan['variables']['permissions_boundary_arn']['value']}, roles
assert plan['variables']['permissions_boundary_arn']['value'], 'non-null caller ceiling required'
print('ok - generic worker-image root forwards caller ceiling to actual image-builder role')

# Inspect the actual nested ASG and task-definition plans, not only root locals.
for filename in sys.argv[1:3]:
    plans = {event['@testrun']: event['test_plan']
             for line in Path(filename).read_text().splitlines()
             if (event := json.loads(line)).get('type') == 'test_plan'}
    if filename == sys.argv[2]:
        slack_tasks = {r['name']: json.loads(r['change']['after']['container_definitions'])[0]
                       for r in plans['slack_no_global_credentials']['resource_changes']
                       if r['type'] == 'aws_ecs_task_definition'}
        for name in ('controlplane', 'dispatcher'):
            assert all(not item['name'].startswith('SLACK_') or item['name'] == 'SLACK_TRANSPORT'
                       for item in slack_tasks[name]['environment'] + slack_tasks[name]['secrets']), (filename, name)
        print(f'ok - {filename}: dedicated Slack credentials are absent from process configuration')
    baseline = next(event['test_state']['values']['outputs']['worker_generation_definitions']['value']
                    for line in Path(filename).read_text().splitlines()
                    if (event := json.loads(line)).get('type') == 'test_state'
                    and event['@testrun'] == 'baseline_execution_generation')
    resized = plans['count_change_does_not_rotate_generation']['output_changes']['worker_generation_definitions']['after']
    assert baseline.keys() == resized.keys(), (filename, 'count change must preserve Pool identity')
    for run, expected_counts in (
        ('fixed_capacity_plan', [2]),
        ('prepare_inert_target', [0, 2]),
        ('activate_after_full_stop', [0, 1]),
        ('same_build_recovery_has_fresh_binding', [0, 2]),
    ):
        resources = plans[run]['resource_changes']
        groups = [r for r in resources if r['type'] == 'aws_autoscaling_group']
        after = [r['change']['after'] for r in groups]
        assert all(g is not None for g in after), (filename, run, 'unexpected ASG deletion')
        assert sorted(g['desired_capacity'] for g in after) == expected_counts, (filename, run, after)
        assert all(g['min_size'] == 0 and g['desired_capacity'] == g['max_size'] for g in after), (filename, run)
        assert all(not g.get('instance_refresh') and g['protect_from_scale_in'] and g['wait_for_capacity_timeout'] == '0' for g in after), (filename, run)
        if run in ('prepare_inert_target', 'same_build_recovery_has_fresh_binding'):
            source = next(r for r in groups if r['change']['after']['desired_capacity'] == 2)
            before = source['change']['before']
            assert before is not None, (filename, 'serving source must already exist')
            assert 'delete' not in source['change']['actions'], (filename, source)
            source_module = source['module_address']
            source_resources = [r for r in resources if r.get('module_address') == source_module]
            assert source_resources and all(r['change']['actions'] == ['no-op'] for r in source_resources), (filename, run, source_resources)
            for key in ('name', 'min_size', 'desired_capacity', 'max_size', 'launch_template'):
                assert before[key] == source['change']['after'][key], (filename, key, source)
        if run == 'fixed_capacity_plan' and filename == sys.argv[2]:
            tasks = {r['name']: json.loads(r['change']['after']['container_definitions'])
                     for r in resources if r['type'] == 'aws_ecs_task_definition'}
            upload_env = {item['name']: item['value'] for item in tasks['controlplane'][0]['environment']}
            assert upload_env['DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY'] == '24', filename
            assert upload_env['DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY'] == '18', filename
            assert upload_env['DEPLOYMENT_UPLOAD_IDLE_TIMEOUT'] == '2m', filename
            assert upload_env['DEPLOYMENT_UPLOAD_MAX_DURATION'] == '2h', filename
            assert upload_env['CONTROL_PLANE_HTTP_IDLE_TIMEOUT'] == '270s', filename
            alb = next(r['change']['after'] for r in resources if r['type'] == 'aws_lb')
            assert alb['idle_timeout'] == 240, filename
            expected = plans[run]['variables']['capacity_token_secret_arn']['value']
            assert any(env['name'] == 'ADMIN_EMAILS' and env['value'] == 'operator@example.test'
                       for container in tasks['controlplane'] for env in container.get('environment', [])), filename
            assert all(env['name'] != 'ADMIN_EMAILS'
                       for container in tasks['dispatcher'] for env in container.get('environment', [])), filename
            assert any(secret['name'] == 'CAPACITY_TOKEN' and secret['valueFrom'] == expected
                       for container in tasks['controlplane'] for secret in container.get('secrets', [])), filename
            assert all(secret['name'] != 'CAPACITY_TOKEN'
                       for container in tasks['dispatcher'] for secret in container.get('secrets', [])), filename
    print(f'ok - {filename}: explicit count, inert preparation, retained source, target activation and Capacity credential')

    staging_plan = plans['computer_staging_rotates_and_retains_generation']
    templates = [r['change']['after'] for r in staging_plan['resource_changes']
                 if r['type'] == 'aws_launch_template']
    environments = [base64.b64decode(t['user_data']).decode() for t in templates]
    assert len(environments) == 2, (filename, len(environments))
    assert sum('WORKER_COMPUTER_STAGING_MIB=65536' in env for env in environments) == 1, filename
    assert sum('WORKER_COMPUTER_STAGING_MIB=32768' in env for env in environments) == 1, filename
    print(f'ok - {filename}: new and retained Computer staging reach actual launch templates')

# Inspect the actual private root and nested modules, not only template locals.
private_events = [json.loads(line) for line in Path(sys.argv[4]).read_text().splitlines()]
private_state = next(event['test_state'] for event in private_events
                     if event.get('type') == 'test_state' and event['@testrun'] == 'private_baseline')
def flatten(module):
    yield from module.get('resources', [])
    for child in module.get('child_modules', []):
        yield from flatten(child)
private_resources = list(flatten(private_state['values']['root_module']))
resources = {r['address']: r['values'] for r in private_resources}
assert resources['module.controlplane.aws_lb.controlplane']['internal'] is True
assert not any(r['type'] == 'aws_cloudfront_distribution' for r in private_resources)
assert not any(address.startswith(('module.controlplane.aws_lb_listener.http[', 'module.controlplane.aws_lb_listener.http_redirect[')) for address in resources)
assert 'module.controlplane.aws_vpc_security_group_egress_rule.controlplane[0]' not in resources
for service in ('controlplane', 'dispatcher'):
    task = resources[f'module.controlplane.aws_ecs_task_definition.{service}']
    env = {item['name']: item['value'] for item in json.loads(task['container_definitions'])[0]['environment']}
    assert env['APP_ORIGIN'] == 'https://helmr.example.test'
    assert env['SLACK_TRANSPORT'] == 'socket'
    assert env['OUTBOUND_PROXY_URL'].startswith('http://proxy.')
    if service == 'controlplane':
        assert env['DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY'] == '24'
        assert env['DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY'] == '18'
        assert env['DEPLOYMENT_UPLOAD_IDLE_TIMEOUT'] == '2m'
        assert env['DEPLOYMENT_UPLOAD_MAX_DURATION'] == '2h'
        assert env['CONTROL_PLANE_HTTP_IDLE_TIMEOUT'] == '270s'
        assert resources['module.controlplane.aws_lb.controlplane']['idle_timeout'] == 240
    if service == 'dispatcher':
        assert env['CONTROL_PLANE_URL'] == env['APP_ORIGIN']
    network = resources[f'module.controlplane.aws_ecs_service.{service}[0]']['network_configuration'][0]
    assert network['assign_public_ip'] is False
for address, value in resources.items():
    if address.startswith('aws_vpc_security_group_egress_rule.platform_'):
        assert value.get('cidr_ipv4') != '0.0.0.0/0', (address, value)
        assert value['ip_protocol'] == 'tcp'
        assert value['from_port'] == value['to_port']
assert resources['aws_route53_resolver_firewall_config.platform']['firewall_fail_open'] == 'DISABLED'
assert resources['aws_route53_resolver_firewall_rule.block']['action'] == 'BLOCK'
assert resources['aws_route53_resolver_firewall_domain_list.all']['domains'] == ['*.']
assert resources['aws_route.execution_to_platform']['destination_cidr_block'] == '10.90.0.0/16'
for address, value in resources.items():
    if value.get('user_data') and 'aws_launch_template.' in address:
        script = base64.b64decode(value['user_data']).decode()
        for cidr in ('10.90.0.0/16', '10.91.0.0/16', '10.92.0.0/16'):
            assert cidr in script, (address, cidr)
print('ok - private root: internal ingress, explicit task egress, DNS restriction and guest management denies')

# Inspect planned nested task definitions to prove each preserved root forwards
# caller-selected diagnostics, including independently chosen CP/exporter values.
admission_keys = {
    'DIAGNOSTIC_CHUNK_BYTES', 'DIAGNOSTIC_SOURCE_BYTES', 'DIAGNOSTIC_SOURCE_RECORDS',
    'DIAGNOSTIC_ENVIRONMENT_BYTES', 'DIAGNOSTIC_ENVIRONMENT_RECORDS',
    'DIAGNOSTIC_QUEUE_BYTES', 'DIAGNOSTIC_QUEUE_RECORDS', 'DIAGNOSTIC_DB_MAX_CONNECTIONS',
}
exporter_keys = admission_keys | {'DIAGNOSTIC_EXPORT_BATCH_BYTES', 'DIAGNOSTIC_EXPORT_BATCH_RECORDS'}
for filename in (sys.argv[2], sys.argv[4]):
    plans = [event['test_plan'] for line in Path(filename).read_text().splitlines()
             if (event := json.loads(line)).get('type') == 'test_plan'
             and event['@testrun'] == 'diagnostic_environment']
    assert len(plans) == 1, (filename, 'one diagnostic plan is required')
    plan = plans[0]
    tasks = {resource['name']: json.loads(resource['change']['after']['container_definitions'])[0]
             for resource in plan['resource_changes'] if resource['type'] == 'aws_ecs_task_definition'}
    for process, keys in (('controlplane', admission_keys), ('dispatcher', exporter_keys)):
        expected = plan['variables'][f'{process}_environment']['value']
        assert keys <= expected.keys(), (filename, process, 'explicit caller limits missing')
        actual = {item['name']: item['value'] for item in tasks[process]['environment']}
        assert all(actual.get(key) == expected[key] for key in keys), (filename, process, actual, expected)
    print(f'ok - {filename}: actual CP/Dispatcher containers receive all caller-selected diagnostic limits')

for filename in (sys.argv[1], sys.argv[2], sys.argv[4]):
    plans = [event['test_plan'] for line in Path(filename).read_text().splitlines()
             if (event := json.loads(line)).get('type') == 'test_plan'
             and event['@testrun'] == 'worker_log_environment_rotates_and_retains_generation']
    assert len(plans) == 1, (filename, 'one Worker environment rotation plan is required')
    templates = [resource for resource in plans[0]['resource_changes'] if resource['type'] == 'aws_launch_template']
    assert len(templates) == 2, (filename, templates)
    scripts = [base64.b64decode(resource['change']['after']['user_data']).decode() for resource in templates]
    for key, value in (('WORKER_LOG_CHUNK_BYTES', '65536'), ('WORKER_LOG_BUFFER_RECORDS', '256')):
        assert all(f'{key}={value}' in script for script in scripts), (filename, key)
    for value in ('1048576', '2097152'):
        assert sum(f'WORKER_LOG_BUFFER_BYTES={value}' in script for script in scripts) == 1, (filename, value)
    retained = [resource for resource in templates if resource['change']['before'] is not None]
    assert len(retained) == 1 and retained[0]['change']['actions'] == ['no-op'], (filename, retained)
    print(f'ok - {filename}: changed log environment reaches only the new Worker generation; retained user-data is unchanged')

# Tagged quarantine/immutable publication and exact version reclamation must pass
# both task IAM and the actual private VPC gateway endpoint policy.
cas_bucket = resources['module.controlplane.aws_s3_bucket.cas']['arn']
private_inputs = next(event['test_plan']['variables'] for event in private_events
                      if event.get('type') == 'test_plan' and event['@testrun'] == 'diagnostic_environment')
endpoint = json.loads(resources['module.controlplane_network.aws_vpc_endpoint.s3[0]']['policy'])
task_policy = json.loads(resources['module.controlplane.aws_iam_role_policy.controlplane_task']['policy'])
required = {'s3:PutObject', 's3:PutObjectTagging', 's3:GetObject', 's3:ListBucketVersions', 's3:DeleteObjectVersion'}
endpoint_cas = [statement for statement in endpoint['Statement'] if cas_bucket in statement['Resource']]
task_cas = [statement for statement in task_policy['Statement'] if cas_bucket in statement['Resource']]
assert len(endpoint_cas) == len(task_cas) == 1
assert required <= set(task_cas[0]['Action']), ('Control Plane CAS role lacks required publication/reclamation actions', required - set(task_cas[0]['Action']))
assert required <= set(endpoint_cas[0]['Action']), ('Private CAS endpoint lacks required publication/reclamation actions', required - set(endpoint_cas[0]['Action']))
assert set(task_cas[0]['Action']) <= set(endpoint_cas[0]['Action']), ('Private CAS endpoint lacks task CAS actions', set(task_cas[0]['Action']) - set(endpoint_cas[0]['Action']))
assert set(endpoint_cas[0]['Resource']) == {cas_bucket, f'{cas_bucket}/*',
    private_inputs['platform_store_bucket_arn']['value'],
    private_inputs['platform_store_bucket_arn']['value'] + '/*'}
layer_statements = [statement for statement in endpoint['Statement'] if statement not in endpoint_cas]
assert len(layer_statements) == 1 and layer_statements[0]['Action'] == ['s3:GetObject']
assert layer_statements[0]['Resource'] == [f"arn:{cas_bucket.split(':')[1]}:s3:::prod-{private_inputs['aws_region']['value']}-starport-layer-bucket/*"]
print('ok - private S3 endpoint permits tagged CAS publication and exact version reclamation while ECR layers remain read-only')

# Quickstart's actual host plan replaces Fargate/RDS/ALB with local services.
quick_events = [json.loads(line) for line in Path(sys.argv[1]).read_text().splitlines()]
quick_state = next(event['test_state'] for event in quick_events
                   if event.get('type') == 'test_state' and event['@testrun'] == 'compact_baseline')
quick_resources = list(flatten(quick_state['values']['root_module']))
quick = {r['address']: r['values'] for r in quick_resources}
assert not ({r['type'] for r in quick_resources} & {
    'aws_lb', 'aws_db_instance', 'aws_ecs_service', 'aws_nat_gateway', 'aws_cloudfront_distribution'
})
host = quick['aws_instance.control']
cloud_config = json.loads(gzip.decompress(base64.b64decode(host['user_data_base64'])).decode().split('\n', 1)[1])
files = {item['path']: item for item in cloud_config['write_files']}
config = json.loads(files['/etc/helmr/quickstart.json']['content'])
assert config['cas_uri'].startswith('s3://')
assert config['blocked_ipv4_cidrs'] == ['3.112.1.2/32', '10.80.0.0/16', '169.254.0.0/16']
assert all(value.startswith('arn:aws:secretsmanager:') for value in config['secret_arns'].values())
assert 'checkpoint_encryption_key' not in config['secret_arns']
assert 'database-bootstrap' in files['/usr/local/sbin/helmr-quickstart-start']['content']
assert 'migrate' in files['/usr/local/sbin/helmr-quickstart-start']['content']
assert 'reverse_proxy 127.0.0.1:8080' in files['/etc/helmr/Caddyfile']['content']
assert '<listen_host replace="replace">127.0.0.1</listen_host>' in files['/etc/helmr/clickhouse.xml']['content']
ingress = quick['aws_security_group.control']['ingress']
assert {(item['from_port'], item['to_port'], item['protocol']) for item in ingress} == {(80, 80, 'tcp'), (443, 443, 'tcp')}
statements = json.loads(quick['aws_iam_role_policy.control']['policy'])['Statement']
secret_statements = [item for item in statements if 'secretsmanager:GetSecretValue' in item['Action']]
assert len(secret_statements) == 1
assert sorted(secret_statements[0]['Resource']) == sorted(config['secret_arns'].values())
assert len(secret_statements[0]['Resource']) == sum(r['type'] == 'aws_secretsmanager_secret' for r in quick_resources) - 1
assert all(not action.startswith(('ec2:', 'autoscaling:')) for item in statements for action in item['Action'])
assert not any(r['type'] == 'aws_secretsmanager_secret_version' for r in quick_resources)
quick_plan = next(event['test_plan'] for event in quick_events
                  if event.get('type') == 'test_plan' and event['@testrun'] == 'compact_worker')
worker_groups = [r['change']['after'] for r in quick_plan['resource_changes'] if r['type'] == 'aws_autoscaling_group']
assert len(worker_groups) == 1
assert worker_groups[0]['vpc_zone_identifier'] == [quick['aws_subnet.public']['id']]
assert worker_groups[0]['desired_capacity'] == worker_groups[0]['max_size'] == 1
print('ok - compact Quickstart: local services, public-subnet Worker, management denies, scoped role and secret references')
