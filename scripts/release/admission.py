"""Selection checks run from reviewed workflow source, never candidate scripts."""
import json
import os
from pathlib import Path
import re
import subprocess
from contract import REPOSITORY, SHA, SEMVER, preview_version, require


def git(root, *args):
    return subprocess.check_output(['git', '-C', str(root), *args], text=True).strip()


def pr_head(pr, number, commit, repository_id):
    require(pr['number'] == number and pr['state'] == 'open', 'PR must be open')
    require(pr['base']['ref'] == 'main' and pr['base']['repo']['id'] == repository_id, 'PR must target Product main')
    require(pr['head']['repo'] is not None and pr['head']['repo']['id'] == repository_id, 'fork PR cannot publish')
    require(pr['head']['sha'] == commit, 'input is not current PR head')


def workflow_tree(api, commit):
    """Read immutable Git tree identities, without changed-files pagination/history."""
    require(re.fullmatch(SHA, commit), 'workflow comparison source SHA invalid')
    source = api.request(f'git/commits/{commit}')
    require(source['sha'] == commit, 'workflow comparison commit differs')
    tree = source['tree']['sha']
    for name in ('.github', 'workflows'):
        require(re.fullmatch(SHA, tree), 'workflow tree SHA invalid')
        listing = api.request(f'git/trees/{tree}')
        require(listing['sha'] == tree and not listing['truncated'], 'incomplete workflow tree metadata')
        matches = [entry for entry in listing['tree'] if entry['path'] == name]
        if not matches:
            return None
        require(len(matches) == 1 and matches[0]['type'] == 'tree' and matches[0]['mode'] == '040000', 'workflow path must be a directory')
        tree = matches[0]['sha']
    require(re.fullmatch(SHA, tree), 'workflow tree SHA invalid')
    return tree


def pr_workflows(api, commit):
    main = api.request('git/ref/heads/main')
    require(main['ref'] == 'refs/heads/main' and main['object']['type'] == 'commit', 'invalid Product main reference')
    require(workflow_tree(api, commit) == workflow_tree(api, main['object']['sha']),
            'PR preview publication requires .github/workflows identical to current Product main; '
            'GITHUB_TOKEN lacks workflow-write authority for release create/update at differing workflows. '
            'Update the PR to match main workflows and rerun with its full current head SHA.')


def ci_success(api, run, workflow_id, repository_id, commit, event, number=None):
    require(run['repository']['id'] == repository_id and run['workflow_id'] == workflow_id, 'wrong CI producer')
    require(run['path'] == '.github/workflows/ci.yaml' and run['event'] == event, 'wrong CI workflow/event')
    require(run['head_sha'] == commit and run['status'] == 'completed' and run['conclusion'] == 'success', 'exact source CI unsuccessful')
    if event == 'push':
        require(run['head_branch'] == 'main', 'CI must be main push')
    else:
        require(any(p['number'] == number and p['head']['sha'] == commit for p in run['pull_requests']), 'CI PR/head association differs')
    jobs = list(api.pages(f'actions/runs/{run["id"]}/attempts/{run["run_attempt"]}/jobs', 'jobs'))
    aggregate = [j for j in jobs if j['name'] == 'ci complete']
    require(len(aggregate) == 1 and aggregate[0]['status'] == 'completed' and aggregate[0]['conclusion'] == 'success', 'missing successful exact CI aggregate')
    return run['id']


def admit(api, env, event, tools):
    require(env['GITHUB_REPOSITORY'] == REPOSITORY, 'wrong repository')
    repository = api.request('')
    require(event['repository']['id'] == repository['id'], 'repository identity mismatch')
    workflow = api.request('actions/workflows/ci.yaml')
    require(workflow['path'] == '.github/workflows/ci.yaml', 'wrong CI workflow')
    name = env['GITHUB_EVENT_NAME']
    number = None
    if name == 'workflow_run':
        require(env['GITHUB_REF'] == 'refs/heads/main', 'workflow must run from main')
        run = api.request(f'actions/runs/{event["workflow_run"]["id"]}')
        commit = run['head_sha']
        ci = ci_success(api, run, workflow['id'], repository['id'], commit, 'push')
        source_ref, mode = 'refs/heads/main', 'main'
    elif name == 'workflow_dispatch':
        require(env['GITHUB_REF'] == 'refs/heads/main', 'manual workflow must execute main')
        commit = event['inputs']['commit']
        require(re.fullmatch(SHA, commit), 'full input SHA required')
        require(re.fullmatch(r'[1-9][0-9]*', event['inputs']['pr']), 'PR number required')
        number = int(event['inputs']['pr'])
        pr_head(api.request(f'pulls/{number}'), number, commit, repository['id'])
        pr_workflows(api, commit)
        runs = list(api.pages(f'actions/workflows/{workflow["id"]}/runs?event=pull_request&head_sha={commit}', 'workflow_runs'))
        require(runs, 'PR CI missing')
        ci = ci_success(api, max(runs, key=lambda r: r['id']), workflow['id'], repository['id'], commit, 'pull_request', number)
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
        ci = ci_success(api, max(runs, key=lambda r: r['id']), workflow['id'], repository['id'], commit, 'push')
        source_ref, mode = env['GITHUB_REF'], 'tag'
    require(re.fullmatch(SHA, commit), 'source SHA invalid')
    # Read selected source metadata as data; never evaluate its code here.
    subprocess.run(['git', '-C', str(tools), 'fetch', '--no-tags', 'origin', commit], check=True)
    core = json.loads(git(tools, 'show', f'{commit}:sdk/typescript/package.json'))['version'].split('-')[0]
    version = env['GITHUB_REF'].removeprefix('refs/tags/') if mode == 'tag' else preview_version(core, commit, env['GITHUB_RUN_ID'])
    return dict(version=version, sourceCommit=commit, sourceRef=source_ref,
                build=dict(runId=env['GITHUB_RUN_ID'], attempt=env['GITHUB_RUN_ATTEMPT'],
                           workflowCommit=env['GITHUB_WORKFLOW_SHA'], workflowRef=env['GITHUB_REF'],
                           ciRun=str(ci), pr=number, mode=mode))


def recheck_pr(api, selection):
    if selection['build']['mode'] == 'pr':
        number = selection['build']['pr']
        pr_head(api.request(f'pulls/{number}'), number, selection['sourceCommit'], api.request('')['id'])
        pr_workflows(api, selection['sourceCommit'])


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
