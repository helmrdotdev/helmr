"""Admit an explicit main checkpoint or stable tag using exact native source CI."""
import json
import re
import subprocess
from contract import REPOSITORY, SHA, SEMVER, preview_version, require


def git(root, *args):
    return subprocess.check_output(['git', '-C', str(root), *args], text=True).strip()


def ci_success(api, run, workflow_id, repository_id, commit):
    require(run['repository']['id'] == repository_id and run['workflow_id'] == workflow_id, 'wrong CI producer')
    require(run['path'] == '.github/workflows/ci.yaml' and run['event'] == 'push', 'wrong CI workflow/event')
    require(run['head_branch'] == 'main', 'CI must be main push')
    require(run['head_sha'] == commit and run['status'] == 'completed' and run['conclusion'] == 'success', 'exact source CI unsuccessful')
    jobs = list(api.pages(f'actions/runs/{run["id"]}/attempts/{run["run_attempt"]}/jobs', 'jobs'))
    gates = [j for j in jobs if j['name'] == 'ci complete']
    require(len(gates) == 1 and gates[0]['status'] == 'completed' and gates[0]['conclusion'] == 'success',
            'missing successful exact CI aggregate: ci complete')
    return run['id']


def admit(api, env, event, tools):
    require(env['GITHUB_REPOSITORY'] == REPOSITORY, 'wrong repository')
    repository = api.request('')
    require(event['repository']['id'] == repository['id'], 'repository identity mismatch')
    workflow = api.request('actions/workflows/ci.yaml')
    require(workflow['path'] == '.github/workflows/ci.yaml', 'wrong CI workflow')
    name = env['GITHUB_EVENT_NAME']
    if name == 'workflow_dispatch':
        require(env['GITHUB_REF'] == 'refs/heads/main', 'manual workflow must execute main')
        commit = event['inputs']['commit']
        require(re.fullmatch(SHA, commit), 'full input SHA required')
        source_ref, mode = 'refs/heads/main', 'main'
    else:
        require(name == 'push' and env['GITHUB_REF'].startswith('refs/tags/v'), 'unsupported release event')
        version = env['GITHUB_REF'].removeprefix('refs/tags/')
        require('-preview.' not in version and re.fullmatch('v' + SEMVER, version), 'generated preview tag cannot enter tag graph')
        require(env['GITHUB_RUN_ATTEMPT'] == '1', 'human tag cohorts are single-attempt')
        require(api.request(f'releases/tags/{version}', missing=True) is None, 'human tag release already exists')
        commit = env['GITHUB_SHA']
        source_ref, mode = env['GITHUB_REF'], 'tag'
    require(re.fullmatch(SHA, commit), 'source SHA invalid')
    subprocess.run(['git', '-C', str(tools), 'fetch', '--no-tags', 'origin', commit], check=True)
    if mode == 'main':
        # workflow_dispatch ran from trusted main; fix membership at that workflow
        # revision. Later main movement cannot replace or cancel this checkpoint.
        require(subprocess.run(['git', '-C', str(tools), 'merge-base', '--is-ancestor',
                                commit, env['GITHUB_WORKFLOW_SHA']], capture_output=True).returncode == 0,
                'selected commit is not on workflow main history')
    runs = list(api.pages(f'actions/workflows/{workflow["id"]}/runs?event=push&head_sha={commit}', 'workflow_runs'))
    require(runs, 'main source CI missing')
    ci = ci_success(api, max(runs, key=lambda r: r['id']), workflow['id'], repository['id'], commit)
    core = json.loads(git(tools, 'show', f'{commit}:sdk/typescript/package.json'))['version'].split('-')[0]
    version = env['GITHUB_REF'].removeprefix('refs/tags/') if mode == 'tag' else preview_version(core, commit, env['GITHUB_RUN_ID'])
    return dict(version=version, sourceCommit=commit, sourceRef=source_ref,
                build=dict(runId=env['GITHUB_RUN_ID'], workflowCommit=env['GITHUB_WORKFLOW_SHA'],
                           workflowRef=env['GITHUB_REF'], ciRun=str(ci), pr=None, mode=mode))
