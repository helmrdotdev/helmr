"""Selection checks run from reviewed workflow source, never candidate scripts."""
import json
import os
from pathlib import Path
import re
import subprocess
from contract import REPOSITORY, SHA, SEMVER, preview_version, require, signer


def git(root, *args):
    return subprocess.check_output(['git', '-C', str(root), *args], text=True).strip()


def pr_head(pr, number, commit, repository_id):
    require(pr['number'] == number and pr['state'] == 'open', 'PR must be open')
    require(pr['base']['ref'] == 'main' and pr['base']['repo']['id'] == repository_id, 'PR must target Product main')
    require(pr['head']['repo'] is not None and pr['head']['repo']['id'] == repository_id, 'fork PR cannot publish')
    require(pr['head']['sha'] == commit, 'input is not current PR head')


def ci_jobs(api, run):
    return list(api.pages(f'actions/runs/{run["id"]}/attempts/{run["run_attempt"]}/jobs', 'jobs'))


def ci_job_success(jobs, name):
    aggregate = [j for j in jobs if j['name'] == name]
    require(len(aggregate) == 1 and aggregate[0]['status'] == 'completed' and aggregate[0]['conclusion'] == 'success',
            'missing successful exact CI aggregate: ' + name)


def ci_success(api, run, workflow_id, repository_id, commit, event, number=None):
    require(run['repository']['id'] == repository_id and run['workflow_id'] == workflow_id, 'wrong CI producer')
    require(run['path'] == '.github/workflows/ci.yaml' and run['event'] == event, 'wrong CI workflow/event')
    require(run['head_sha'] == commit and run['status'] == 'completed' and run['conclusion'] == 'success', 'exact source CI unsuccessful')
    if event == 'push':
        require(run['head_branch'] == 'main', 'CI must be main push')
    else:
        require(any(p['number'] == number and p['head']['sha'] == commit for p in run['pull_requests']), 'CI PR/head association differs')
    jobs = ci_jobs(api, run)
    if event == 'push':
        ci_job_success(jobs, 'source-ci-complete')
        ci_job_success(jobs, 'preview-ready')
    else:
        ci_job_success(jobs, 'ci complete')
    return run['id'], jobs


def ci_artifact_skipped(jobs):
    """True when the producer CI run reported a successful documentation-only artifact skip."""
    skip = [j for j in jobs if j['name'] == 'artifact build skipped']
    require(len(skip) <= 1, 'ambiguous artifact skip job')
    if not skip:
        return False
    marker = skip[0]
    require(marker['status'] == 'completed', 'artifact skip job incomplete')
    if marker['conclusion'] == 'skipped':
        return False
    require(marker['conclusion'] == 'success', 'artifact skip job unsuccessful')
    return True


def main_superseded(api, selection):
    """Automatic main preview superseded when Product main moved past the admitted source."""
    require(selection['build']['mode'] == 'main', 'superseded check is main-only')
    main = api.request('git/ref/heads/main')
    require(main['ref'] == 'refs/heads/main' and main['object']['type'] == 'commit', 'invalid Product main reference')
    return selection['sourceCommit'] != main['object']['sha']


def preview_pointer_source():
    from preview_store import fetch_pointer
    record = fetch_pointer()
    return None if record is None else record['sourceCommit']


def artifact_build_needed(api, root, head):
    base = preview_pointer_source()
    if base is None:
        return True
    return relevant(root, base, head)


def admit(api, env, event, tools):
    require(env['GITHUB_REPOSITORY'] == REPOSITORY, 'wrong repository')
    repository = api.request('')
    require(event['repository']['id'] == repository['id'], 'repository identity mismatch')
    workflow = api.request('actions/workflows/ci.yaml')
    require(workflow['path'] == '.github/workflows/ci.yaml', 'wrong CI workflow')
    name = env['GITHUB_EVENT_NAME']
    number = None
    main_run = None
    jobs = None
    if name == 'workflow_run':
        require(env['GITHUB_REF'] == 'refs/heads/main', 'workflow must run from main')
        main_run = api.request(f'actions/runs/{event["workflow_run"]["id"]}')
        commit = main_run['head_sha']
        ci, jobs = ci_success(api, main_run, workflow['id'], repository['id'], commit, 'push')
        source_ref, mode = 'refs/heads/main', 'main'
    elif name == 'workflow_dispatch':
        require(env['GITHUB_REF'] == 'refs/heads/main', 'manual workflow must execute main')
        commit = event['inputs']['commit']
        require(re.fullmatch(SHA, commit), 'full input SHA required')
        require(re.fullmatch(r'[1-9][0-9]*', event['inputs']['pr']), 'PR number required')
        number = int(event['inputs']['pr'])
        pr_head(api.request(f'pulls/{number}'), number, commit, repository['id'])
        runs = list(api.pages(f'actions/workflows/{workflow["id"]}/runs?event=pull_request&head_sha={commit}', 'workflow_runs'))
        require(runs, 'PR CI missing')
        ci, _ = ci_success(api, max(runs, key=lambda r: r['id']), workflow['id'], repository['id'], commit, 'pull_request', number)
        source_ref, mode = f'refs/pull/{number}/head', 'pr'
    else:
        require(name == 'push' and env['GITHUB_REF'].startswith('refs/tags/v'), 'unsupported release event')
        version = env['GITHUB_REF'].removeprefix('refs/tags/')
        require('-preview.' not in version and re.fullmatch('v' + SEMVER, version), 'generated preview tag cannot enter tag graph')
        require(env['GITHUB_RUN_ATTEMPT'] == '1', 'human tag cohorts are single-attempt')
        require(api.request(f'releases/tags/{version}', missing=True) is None, 'human tag release already exists')
        commit = env['GITHUB_SHA']
        runs = list(api.pages(f'actions/workflows/{workflow["id"]}/runs?event=push&head_sha={commit}', 'workflow_runs'))
        require(runs, 'tag source main CI missing')
        ci, _ = ci_success(api, max(runs, key=lambda r: r['id']), workflow['id'], repository['id'], commit, 'push')
        source_ref, mode = env['GITHUB_REF'], 'tag'
    require(re.fullmatch(SHA, commit), 'source SHA invalid')
    subprocess.run(['git', '-C', str(tools), 'fetch', '--no-tags', 'origin', commit], check=True)
    core = json.loads(git(tools, 'show', f'{commit}:sdk/typescript/package.json'))['version'].split('-')[0]
    if mode == 'tag':
        version = env['GITHUB_REF'].removeprefix('refs/tags/')
    elif mode == 'main':
        version = preview_version(core, commit, str(ci))
    else:
        version = preview_version(core, commit, env['GITHUB_RUN_ID'])
    selection = dict(version=version, sourceCommit=commit, sourceRef=source_ref,
                     build=dict(runId=env['GITHUB_RUN_ID'], workflowCommit=env['GITHUB_WORKFLOW_SHA'],
                                workflowRef=env['GITHUB_REF'], ciRun=str(ci), pr=number, mode=mode))
    if mode == 'main':
        selection['build'].update(runId=str(ci), workflowCommit=commit,
                                  workflowRef=signer(version).split('@', 1)[1], ciRun=str(ci))
    skip = (name == 'workflow_run' and mode == 'main' and ci_artifact_skipped(jobs))
    return selection, skip


def recheck_pr(api, selection):
    if selection['build']['mode'] == 'pr':
        number = selection['build']['pr']
        pr_head(api.request(f'pulls/{number}'), number, selection['sourceCommit'], api.request('')['id'])


def docs_only(paths):
    # README at root and website docs are not inputs of the common image/package graph.
    return bool(paths) and all(p == 'README.md' or p.startswith('packages/web/src/content/docs/') for p in paths)


def relevant(root, base, head):
    if subprocess.run(['git', '-C', str(root), 'merge-base', '--is-ancestor', base, head], capture_output=True).returncode:
        return True
    if base == head:
        return False
    # --no-renames retains both deleted and added names; ambiguous failures propagate.
    raw = subprocess.check_output(['git', '-C', str(root), 'diff', '--no-renames', '--name-only', '-z', base, head])
    paths = raw.decode().rstrip('\0').split('\0')
    return not docs_only(paths)
