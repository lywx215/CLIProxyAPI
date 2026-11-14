"""A TLS byte stream over a private, already authenticated SSH command channel."""

import io
import ssl


class _Reader(io.RawIOBase):
    def __init__(self, pipe):
        self.pipe = pipe

    def readable(self):
        return True

    def readinto(self, buffer):
        value = self.pipe.recv(len(buffer))
        buffer[:len(value)] = value
        return len(value)


class TLSPipe:
    def __init__(self, read, write, context, *, server_side=False, hostname=None):
        self.read, self.write = read, write
        self.incoming, self.outgoing = ssl.MemoryBIO(), ssl.MemoryBIO()
        self.tls = context.wrap_bio(self.incoming, self.outgoing, server_side=server_side,
                                    server_hostname=hostname)
        self._run(self.tls.do_handshake)

    def _flush(self):
        while self.outgoing.pending:
            self.write(self.outgoing.read())

    def _run(self, operation, *args):
        while True:
            try:
                result = operation(*args)
            except ssl.SSLWantReadError:
                self._flush()
                data = self.read(65536)
                if not data:
                    raise EOFError('tls_pipe_closed') from None
                self.incoming.write(data)
            else:
                self._flush()
                return result

    def sendall(self, data):
        remaining = memoryview(data)
        while remaining:
            remaining = remaining[self._run(self.tls.write, remaining):]

    def recv(self, count):
        if not count:
            return b''
        try:
            return self._run(self.tls.read, count)
        except ssl.SSLZeroReturnError:
            return b''

    def makefile(self, mode, buffering=-1):
        if mode != 'rb':
            raise ValueError('tls_pipe_read_mode_required')
        return io.BufferedReader(_Reader(self))

    def close(self):
        # The caller owns the SSH channel (or the SSH command's stdio).
        pass
