"""Thin adapter around the user's shared Zeabur client; no legacy task imports."""
from __future__ import annotations

from .concurrency import TaskExecutor as ThreadPoolExecutor
import importlib.util
from pathlib import Path
import re

from .core import READ_SLOTS, OpsError, require
from .telemetry import operation as measured_operation


class Zeabur:
    def __init__(self, path):
        path = Path(path)
        require(path.is_file(), 'shared_zeabur_client_missing')
        spec = importlib.util.spec_from_file_location('cliproxy_shared_zeabur', path)
        self.client = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.client)

    def call(self, operation, *args):
        try:
            with READ_SLOTS, measured_operation('zeabur_' + operation):
                return getattr(self.client, operation)(*args)
        except Exception:
            # The shared client's exceptions can include upstream error bodies.
            raise OpsError('zeabur_' + operation + '_failed', category='network') from None

    def graphql(self, query, variables):
        return self.call('graphql', query, variables)

    def profile(self, target):
        value = self.call('get_project', target['alias'])
        require(value.get('service_id') and value.get('environment_id'), 'zeabur_profile_incomplete')
        return value

    def snapshot(self, target):
        return self.call('snapshot', self.profile(target))

    def deployment_ids(self, targets):
        """Read peer deployment records without depending on their cluster health."""
        query = ('query($id:ObjectID!,$env:ObjectID!){'
                 'deployments(serviceID:$id,environmentID:$env,perPage:1){edges{node{_id}}}}')

        def read(target):
            profile = self.profile(target)
            response = self.graphql(query, {'id': profile['service_id'], 'env': profile['environment_id']})
            rows = (response.get('deployments') or {}).get('edges') or []
            node = (rows[0].get('node') or {}) if len(rows) == 1 else {}
            require(isinstance(node.get('_id'), str) and node['_id'], 'zeabur_deployment_identity_unavailable')
            return target['label'], node['_id']

        with ThreadPoolExecutor(max_workers=4) as pool:
            return dict(pool.map(read, targets))

    def variables(self, target):
        profile = self.profile(target)
        value = self.graphql('query($id:ObjectID!,$env:ObjectID!){service(_id:$id){domains{domain environmentID} variables(environmentID:$env){key value}}}',
                             {'id': profile['service_id'], 'env': profile['environment_id']})['service']
        domains = sorted(row['domain'] for row in value['domains'] if row['environmentID'] == profile['environment_id'])
        return {row['key']: row['value'] for row in value['variables']}, domains

    def bindings(self, repository_url):
        nodes, seen, skip = [], set(), 0
        while True:
            page = self.graphql(self.client.PROJECT_LIST_QUERY, {'skip': skip, 'limit': 100})['projects']
            fresh = [edge['node'] for edge in page['edges'] if edge['node']['_id'] not in seen]
            nodes.extend(fresh)
            seen.update(node['_id'] for node in fresh)
            if not page.get('pageInfo', {}).get('hasNextPage'):
                total = page.get('pageInfo', {}).get('totalCount')
                require(type(total) is int and total == len(seen), 'zeabur_inventory_incomplete')
                break
            require(fresh, 'zeabur_inventory_pagination_stalled')
            skip += len(page['edges'])
        projects = self.batch_read([(node['_id'],) for node in nodes], 'project(_id:$id{i})',
                                   '{_id environments{_id} services{_id}}', False)
        require(all(row.get('_id') == node['_id'] and isinstance(row.get('services'), list)
                    and isinstance(row.get('environments'), list) for row, node in zip(projects, nodes)),
                'zeabur_inventory_incomplete')
        tasks = [(service['_id'], env['_id']) for project in projects for service in project.get('services', [])
                 for env in project.get('environments', [])]
        metadata = self.batch_read(tasks, 'service(_id:$id{i})',
                                   '{{_id gitTrigger(environmentID:$env{i}){{repoURL branchName}}}}', True)
        result = []
        canonical = repository_url.rstrip('/').removesuffix('.git').lower()
        for pair, row in zip(tasks, metadata):
            require(row.get('_id') == pair[0] and 'gitTrigger' in row, 'zeabur_binding_inventory_incomplete')
            binding = row['gitTrigger'] or {}
            if str(binding.get('repoURL', '')).rstrip('/').removesuffix('.git').lower() == canonical:
                result.append({'service_id': pair[0], 'environment_id': pair[1], 'branch': binding.get('branchName')})
        return result

    def batch_read(self, tasks, field, selection, environments):
        """Use the same project/service fields, with separate aliases for every identity."""
        def fetch(batch):
            definitions, fields, variables = [], [], {}
            for i, row in enumerate(batch):
                definitions.append(f'$id{i}:ObjectID!')
                variables[f'id{i}'] = row[0]
                if environments:
                    definitions.append(f'$env{i}:ObjectID!')
                    variables[f'env{i}'] = row[1]
                fields.append(f'r{i}:' + field.format(i=i) + (selection.format(i=i) if environments else selection))
            response = self.graphql('query(' + ','.join(definitions) + '){' + ' '.join(fields) + '}', variables)
            require(isinstance(response, dict) and all(isinstance(response.get(f'r{i}'), dict) for i in range(len(batch))),
                    'zeabur_binding_inventory_incomplete')
            return [response[f'r{i}'] for i in range(len(batch))]

        batches = [tasks[i:i + 8] for i in range(0, len(tasks), 8)]
        with ThreadPoolExecutor(max_workers=4) as pool:
            return [row for batch in pool.map(fetch, batches) for row in batch]

    def unique_binding(self, target, repository_url):
        profile = self.profile(target)
        matches = [row for row in self.bindings(repository_url) if row['branch'] == target['branch']]
        require(len(matches) == 1 and matches[0]['service_id'] == profile['service_id']
                and matches[0]['environment_id'] == profile['environment_id'], 'shared_or_changed_deployment_branch')

    def execute_read(self, target, command):
        profile = self.profile(target)
        result = self.graphql('mutation($id:ObjectID!,$env:ObjectID!,$command:[String!]!){executeCommand(serviceID:$id,environmentID:$env,command:$command){exitCode output}}',
                              {'id': profile['service_id'], 'env': profile['environment_id'], 'command': command})['executeCommand']
        require(result['exitCode'] == 0, 'cloud_readonly_runtime_probe_failed')
        return result.get('output') or ''

    def runtime(self, target, secret_ref):
        require(not secret_ref or re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', secret_ref), 'invalid_oauth_environment_reference')
        command = ['sha256sum /CLIProxyAPI/CLIProxyAPI /CLIProxyAPI/static/management.html',
                   "df -Pk /CLIProxyAPI | tail -n 1 | awk '{print \"disk_free_kib=\" $4}'"]
        if secret_ref:
            command.append('test -n "$' + secret_ref + '" && echo oauth_secret_present=true')
        output = self.execute_read(target, ['sh', '-c', '\n'.join(command)])
        files = dict((path, value) for value, path in re.findall(r'(?m)^([a-f0-9]{64})\s+(/[^\r\n ]+)\s*$', output))
        require('/CLIProxyAPI/CLIProxyAPI' in files and '/CLIProxyAPI/static/management.html' in files,
                'runtime_file_hash_unavailable')
        free = re.search(r'disk_free_kib=(\d+)', output)
        return {'binary_sha256': files['/CLIProxyAPI/CLIProxyAPI'], 'disk_page_sha256': files['/CLIProxyAPI/static/management.html'],
                'oauth_secret_present': not secret_ref or 'oauth_secret_present=true' in output,
                'disk_free_bytes': int(free[1]) * 1024 if free else None}

    def redeploy(self, target):
        profile = self.profile(target)
        result = self.graphql(self.client.REDEPLOY_MUTATION, {'serviceId': profile['service_id'], 'environmentId': profile['environment_id']})
        require(result.get('redeployService') is True, 'zeabur_redeploy_unconfirmed')

    def rollback(self, deployment_id):
        result = self.graphql('mutation($id:ObjectID!){rollbackDeployment(deploymentID:$id)}', {'id': deployment_id})
        require(result.get('rollbackDeployment') is True, 'zeabur_rollback_unconfirmed')

    def image_digests(self, target, deployment_id):
        logs = self.call('fetch_logs', self.profile(target), 'build', deployment_id)
        return sorted({match[1] for row in logs
                       if (match := re.search(r'exporting manifest (sha256:[a-f0-9]{64})', str(row.get('message', ''))))})
