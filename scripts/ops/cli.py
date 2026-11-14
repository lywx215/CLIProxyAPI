"""User-invoked CLI. Only update/resume enter production maintenance."""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys
import time

from .core import OpsError, require, validate_selection
from .engine import Engine
from .live import Live
from .release import Releases, describe
from .report import write as report_write
from .store import Lease, Store
from .inventory import install as install_inventory
from .plans import load_target_options, routing_fingerprint, validate_observed_options


DEFAULT_ROOT = Path.home() / '.codex/private/cliproxy-ops'


def emit(event, **values):
    print(json.dumps({'event': event, **values}, ensure_ascii=True), flush=True)


def no_inline_secrets(value):
    if isinstance(value, dict):
        require(not {'password', 'token', 'api_key', 'secret', 'cookies'}.intersection(value), 'plaintext_inventory_secret_rejected')
        for child in value.values():
            no_inline_secrets(child)
    elif isinstance(value, list):
        for child in value:
            no_inline_secrets(child)


def parser():
    result = argparse.ArgumentParser(description='Sequential CLIProxyAPI update controller. No automatic or scheduled deployment.')
    result.add_argument('--state-dir', type=Path, default=DEFAULT_ROOT)
    sub = result.add_subparsers(dest='action', required=True)
    init = sub.add_parser('init', help='Install a non-secret local inventory; no network calls')
    init.add_argument('--inventory', type=Path, required=True)
    plan = sub.add_parser('plan', help='Read-only production preflight; freeze an explicit target list and commit')
    plan.add_argument('--targets', required=True)
    plan.add_argument('--commit', required=True)
    plan.add_argument('--target-options', type=Path, help='JSON file with exceptions keyed by selected target alias')
    update = sub.add_parser('update', help='Execute the explicitly selected plan, serially')
    update.add_argument('--plan', required=True)
    for name in ['status', 'resume', 'pause', 'report']:
        command = sub.add_parser(name)
        command.add_argument('--run', required=True)
        if name == 'resume':
            command.add_argument('--exclude-model-test', help='Exclude an explicitly authorized provider:model after a recorded HTTP 400')
    return result


def main(arguments=None):
    args = parser().parse_args(arguments)
    root = args.state_dir.expanduser().resolve()
    if args.action == 'init':
        inventory = json.loads(args.inventory.read_text(encoding='utf-8-sig'))
        no_inline_secrets(inventory)
        path = install_inventory(inventory, root)
        emit('inventory_initialized', path=str(path), production_mutations=False)
        return 0
    store = Store(root)
    if args.action == 'status':
        run = store.run(args.run)
        emit('status', run=run['id'], status=run['status'], pause_after_current=bool(run['pause']),
             targets=[{'target': row['target'], 'phase': row['phase'], 'error': row['data'].get('last_error')}
                      for row in run['targets']])
        return 0
    if args.action == 'pause':
        store.pause(args.run)
        emit('pause_requested', run=args.run, finish_current_server_first=True)
        return 0
    if args.action == 'report':
        emit('report', **report_write(store, args.run))
        return 0
    inventory_path = root / 'inventory.json'
    require(inventory_path.is_file(), 'inventory_missing_run_init')
    inventory = json.loads(inventory_path.read_text(encoding='utf-8'))
    no_inline_secrets(inventory)
    driver = Live(inventory, store.vault)
    if args.action == 'plan':
        labels = [name.strip() for name in args.targets.split(',') if name.strip()]
        targets = validate_selection(inventory, labels)
        options = load_target_options(args.target_options, labels)
        release = describe(inventory['repository'], args.commit)
        results = driver.inspect(targets, release, options=options)
        for label, before, routes, _, _ in results:
            if label in options:
                selected = options[label]
                validate_observed_options(before, routes, selected['web_model_tests'], selected['hold_channels'], selected['fast_model_tests'])
                selected['routing_fingerprint'] = routing_fingerprint(routes, include_status=False)
        body = {'schema': 2, 'created_at': time.time(), 'targets': targets, 'release': release,
                'acceptance_mode': 'availability',
                'target_options': options,
                'inventory_fingerprint': driver.inventory_fingerprint(), 'authorization': 'explicit_target_list_only',
                'warnings': {label: before.get('warnings', []) for label, before, _, _, _ in results}}
        plan_id = store.plan(body, snapshots={label + ':preflight': {'target': before, 'routing': routes, 'backup': backup,
                                                                   'prepared_at': observed}
                                              for label, before, routes, backup, observed in results})
        emit('plan_ready', plan=plan_id, commit=release['commit'], targets=labels,
             warnings=body['warnings'], production_mutations=False)
        return 0
    if args.action == 'update':
        run_id = store.begin(args.plan)
    else:
        run_id = args.run
        store.pause(run_id, False)
    # The lease location cannot be overridden by another inventory or state directory.
    private = Path.home() / '.codex/private/newapi'
    lease = Lease(private / 'cliproxy-maintenance-active.json', private / 'cliproxy-hotconfig-active.json')
    releases = Releases(inventory['repository'], root / 'releases', inventory.get('go'), emit)
    engine = Engine(store, lease, driver, releases, emit=emit)
    emit('run_started', run=run_id)
    result = engine.run(run_id, exclude_model_test=getattr(args, 'exclude_model_test', None))
    paths = report_write(store, run_id)
    emit('run_finished', run=run_id, status=result['status'], **paths)
    return 0 if result['status'] in {'complete', 'complete_with_holds', 'paused_after_current'} else 2


if __name__ == '__main__':
    try:
        sys.exit(main())
    except OpsError as error:
        emit('blocked', code=error.code, category=error.category)
        sys.exit(2)
    except KeyboardInterrupt:
        emit('interrupted', instruction='Use status and resume; do not remove the maintenance lease.')
        sys.exit(130)
    except Exception as error:
        emit('blocked', code='local_' + type(error).__name__)
        sys.exit(2)
