# Collie's MCP bridge for agents in a container on Podman machine, written
# there by relay.py:
#
#   python3 bridge.py <socket> [session]
#
# Like `collie mcp-bridge`: tells Collie which session it serves, then
# copies stdin to the socket and the socket to stdout until Collie is done.
import json
import os
import socket
import sys
import threading

c = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
try:
    c.connect(sys.argv[1])
except OSError as e:
    sys.stderr.write("collie bridge: %s\n" % e)
    sys.exit(1)
if len(sys.argv) > 2 and sys.argv[2]:
    hello = {"jsonrpc": "2.0", "method": "collie/session", "params": {"id": sys.argv[2]}}
    c.sendall(json.dumps(hello).encode() + b"\n")


def up():
    try:
        while True:
            b = os.read(0, 65536)
            if not b:
                break
            c.sendall(b)
        c.shutdown(socket.SHUT_WR)
    except OSError:
        pass


threading.Thread(target=up, daemon=True).start()
out = sys.stdout.buffer
try:
    while True:
        b = c.recv(65536)
        if not b:
            break
        out.write(b)
        out.flush()
except OSError:
    pass
