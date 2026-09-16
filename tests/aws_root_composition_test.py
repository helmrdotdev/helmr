#!/usr/bin/env python3
"""Check native self-host and image root plans, without test-only outputs."""
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
