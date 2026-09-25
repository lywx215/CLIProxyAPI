"""Safety/control wrapper around the frozen DIAG-02 entry, without copying it."""
import importlib.util
import json
import os
from pathlib import Path
import sys


def audit(event, args):
    if event == 'socket.connect':
        address = args[1]
        if not isinstance(address, tuple) or address[0] != '127.0.0.1':
            raise RuntimeError('fixture outbound destination denied')


sys.addaudithook(audit)
repository = Path(sys.argv.pop(1)).resolve()
spec = importlib.util.spec_from_file_location('approved_harness', repository/'scripts/diagnostic_harness.py')
harness = importlib.util.module_from_spec(spec)
spec.loader.exec_module(harness)
harness.main()
# This observes the approved shutdown ordering; it does not reopen its writer.
print(json.dumps({'closed': True, 'pid': os.getpid()}), flush=True)
