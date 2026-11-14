"""Record only task-tagged administrative request destinations, never headers/bodies."""
import json
import re
import select
import socket
import struct
import sys


def main(prefix):
    if not re.fullmatch(r'[a-f0-9]{8}', prefix):
        raise ValueError('invalid_observer_prefix')
    sock = socket.socket(socket.AF_PACKET, socket.SOCK_DGRAM, socket.htons(0x0003))
    print(json.dumps({'ready': True}), flush=True)
    try:
        while True:
            ready, _, _ = select.select([sock, sys.stdin], [], [], 1)
            if sys.stdin in ready:
                break
            if sock not in ready:
                continue
            packet, _ = sock.recvfrom(1400)
            if len(packet) < 40 or packet[0] >> 4 != 4 or packet[9] != 6:
                continue
            ihl = (packet[0] & 15) * 4
            if len(packet) < ihl + 20:
                continue
            offset = ihl + (packet[ihl + 12] >> 4) * 4
            payload = packet[offset:]
            if not payload.startswith((b'POST /api/channel/status/batch?', b'GET /api/status?')):
                continue
            match = re.search(rb'_cpa_ops_nonce=([a-f0-9]{32})\b', payload.split(b'\r\n', 1)[0])
            if match and match[1].decode().startswith(prefix):
                destination = socket.inet_ntoa(packet[16:20]) + ':' + str(struct.unpack_from('!H', packet, ihl + 2)[0])
                print(json.dumps({'nonce': match[1].decode(), 'backend': destination}), flush=True)
    finally:
        sock.close()


if __name__ == '__main__':
    main(sys.argv[1])
