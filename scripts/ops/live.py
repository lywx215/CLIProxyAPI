"""Production dependency wiring. Instantiation performs no server mutations."""
from __future__ import annotations

from .concurrency import TaskExecutor as ThreadPoolExecutor

from .adapters import Adapter
from .core import digest, require, validate_selection
from .routing import Routing
from .transport import SSHFactory
from .zeabur import Zeabur
from .testing import model_call
from .inventory import validate as validate_inventory


class Live:
    def __init__(self, inventory, vault):
        self.inventory, self.vault = inventory, vault
        validate_inventory(inventory)
        self.ssh = SSHFactory(inventory['connections'], vault)
        self.z = None
        self.guard = lambda: require(False, 'mutation_outside_maintenance')

    def inventory_fingerprint(self):
        return digest(self.inventory)

    def adapter(self, target, guard):
        validate_selection(self.inventory, [target['label']])
        if target['kind'] == 'zeabur' and self.z is None:
            self.z = Zeabur(self.inventory['zeabur_client'])
        peers = [dict(item, label=label) for label, item in self.inventory['targets'].items()
                 if item.get('kind') == 'zeabur' and not item.get('retired')]
        return Adapter(target, guard, self.ssh, self.z, self.inventory['repository'],
                       self.inventory['repository_url'], peers)

    def routing(self, guard):
        return Routing(self.inventory['sites'], self.ssh, self.vault, guard)

    def session(self):
        if self.z is None:
            self.z = Zeabur(self.inventory['zeabur_client'])
        return self.ssh.session()

    def prepare(self, target, release, options=None):
        readonly = lambda: require(False, 'mutation_during_readonly_preparation')
        adapter, routing = self.adapter(target, readonly), self.routing(readonly)
        try:
            return adapter.prepare(routing, release, deferred_web_tests=(options or {}).get('web_model_tests', ()))
        finally:
            routing.close()
            adapter.close()

    def inspect(self, targets, release, *, options=None):
        def one(target):
            prepared = self.prepare(target, release, (options or {}).get(target['label']))
            return target['label'], prepared['target'], prepared['routing'], prepared['backup'], prepared['prepared_at']
        # Initialize the shared read client once before worker threads start.
        if any(target['kind'] == 'zeabur' for target in targets):
            self.z = Zeabur(self.inventory['zeabur_client'])
        with self.session(), ThreadPoolExecutor(max_workers=4) as executor:
            futures = [executor.submit(one, target) for target in targets]
            return [future.result() for future in futures]

    @staticmethod
    def capture_drain():
        # The existing deployed APIs expose provider-specific counters only.
        # They cannot establish a provider-wide baseline; use the explicit fallback.
        return None

    @staticmethod
    def live_counts(handle):
        return None

    @staticmethod
    def model_call(adapter, job, snapshot):
        return model_call(adapter, job, snapshot)
