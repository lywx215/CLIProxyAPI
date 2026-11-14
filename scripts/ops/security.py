"""Current-user DPAPI custody and strict, host-bound credential references."""
from __future__ import annotations

import ctypes
import json
import os
from ctypes import wintypes
from pathlib import Path

from .core import require


class Blob(ctypes.Structure):
    _fields_ = [('size', wintypes.DWORD), ('data', ctypes.POINTER(ctypes.c_ubyte))]


def dpapi(data, decrypt=False):
    require(os.name == 'nt', 'dpapi_requires_windows')
    source_buffer = ctypes.create_string_buffer(data)
    source = Blob(len(data), ctypes.cast(source_buffer, ctypes.POINTER(ctypes.c_ubyte)))
    result = Blob()
    crypt = ctypes.WinDLL('crypt32', use_last_error=True)
    kernel = ctypes.WinDLL('kernel32', use_last_error=True)
    operation = crypt.CryptUnprotectData if decrypt else crypt.CryptProtectData
    operation.argtypes = [ctypes.POINTER(Blob), ctypes.c_void_p, ctypes.POINTER(Blob),
                          ctypes.c_void_p, ctypes.c_void_p, wintypes.DWORD, ctypes.POINTER(Blob)]
    operation.restype = wintypes.BOOL
    kernel.LocalFree.argtypes = [ctypes.c_void_p]
    kernel.LocalFree.restype = ctypes.c_void_p
    require(operation(ctypes.byref(source), None, None, None, None, 1, ctypes.byref(result)), 'dpapi_failed')
    try:
        return ctypes.string_at(result.data, result.size)
    finally:
        kernel.LocalFree(ctypes.cast(result.data, ctypes.c_void_p))


class Vault:
    def __init__(self, transform=dpapi):
        self.transform = transform

    def seal(self, value):
        return self.transform(json.dumps(value, ensure_ascii=False).encode(), False)

    def open(self, value):
        return json.loads(self.transform(value, True))

    def reference(self, ref, *, host=None, origin=None):
        """References contain a path/selector plus expected identity, never a password."""
        data = self.transform(Path(ref['path']).expanduser().read_bytes(), True)
        if ref.get('format', 'json') == 'text':
            value = data.decode('utf-8')
            require((host and ref.get('host') == host) or (origin and ref.get('origin') == origin),
                    'credential_reference_identity_mismatch')
            return value
        record = json.loads(data)
        for field, expected in ref.get('checks', {}).items():
            require(record.get(field) == expected, 'credential_record_identity_mismatch')
        for part in ref.get('selector', []):
            record = record[part]
        if host:
            require(record.get('host', ref.get('host')) == host, 'ssh_credential_host_mismatch')
        if origin:
            require(record.get('origin', ref.get('origin')) == origin, 'api_credential_origin_mismatch')
        return record
