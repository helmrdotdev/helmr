#!/usr/bin/env python3
"""Check native self-host and image root plans, without test-only outputs."""
import base64
import json
import sys
from pathlib import Path

assert len(sys.argv) == 4, "quickstart, standard and worker-image plans are required"

for filename in sys.argv[1:3]:
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
        if run == 'fixed_capacity_plan':
            tasks = {r['name']: json.loads(r['change']['after']['container_definitions'])
                     for r in resources if r['type'] == 'aws_ecs_task_definition'}
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
