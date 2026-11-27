# Collie's tools relay. Collie runs it in a container when Podman runs the
# containers in a VM (Podman machine on a Mac), whose agents can't reach a
# socket on the host:
#
#   python3 -u -c <this file> <socket> <source of bridge.py> [<proxy address>]
#
# It listens on <socket> for the agents' MCP bridges (bridge.py, which it
# writes next to the socket), and on a container with an allowlist on the
# proxy address (127.0.0.1:3128) for programs going out through Collie's
# proxy. It carries every connection over its own stdin/stdout to Collie,
# one JSON object per line:
#
#   out: {"ready":1} once listening    {"c":N,"o":1} tools connection N opened
#        {"c":N,"o":2} proxy connection N opened
#        {"c":N,"d":"<base64>"} data    {"c":N,"x":1} N closed its end
#        {"p":K} answers {"p":K}
#   in:  {"c":N,"d":"<base64>"} data    {"c":N,"x":1} Collie is done with N
#        {"p":K} a ping
#
# It exits when its stdin closes: Collie has stopped or let it go.
import base64
import json
import os
import socket
import sys
import threading

sock_path, bridge = sys.argv[1], sys.argv[2]
proxy_addr = sys.argv[3] if len(sys.argv) > 3 else ""
here = os.path.dirname(sock_path)
os.makedirs(here, exist_ok=True)
with open(os.path.join(here, ".bridge.py"), "w") as f:
    f.write(bridge)
os.replace(os.path.join(here, ".bridge.py"), os.path.join(here, "bridge.py"))
try:
    os.unlink(sock_path)
except FileNotFoundError:
    pass
server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(sock_path)
os.chmod(sock_path, 0o600)
server.listen(16)
proxy = None
if proxy_addr:
    host, port = proxy_addr.rsplit(":", 1)
    proxy = socket.create_server((host, int(port)), backlog=128)

out = sys.stdout.buffer
out_lock = threading.Lock()
conns = {}
conns_lock = threading.Lock()
last_id = 0


def emit(msg):
    line = json.dumps(msg, separators=(",", ":")).encode() + b"\n"
    with out_lock:
        out.write(line)
        out.flush()


def pump(n, c):
    try:
        while True:
            b = c.recv(65536)
            if not b:
                break
            emit({"c": n, "d": base64.b64encode(b).decode()})
    except OSError:
        pass
    emit({"c": n, "x": 1})


def accept(listener, kind):
    global last_id
    while True:
        c, _ = listener.accept()
        with conns_lock:
            last_id += 1
            n = last_id
            conns[n] = c
        emit({"c": n, "o": kind})
        threading.Thread(target=pump, args=(n, c), daemon=True).start()


threading.Thread(target=accept, args=(server, 1), daemon=True).start()
if proxy:
    threading.Thread(target=accept, args=(proxy, 2), daemon=True).start()
emit({"ready": 1})
for line in sys.stdin.buffer:
    try:
        msg = json.loads(line)
        if "p" in msg:
            emit({"p": msg["p"]})
            continue
        n = msg["c"]
    except (ValueError, KeyError, TypeError):
        continue
    with conns_lock:
        c = conns.get(n)
    if c is None:
        continue
    try:
        if "d" in msg:
            c.sendall(base64.b64decode(msg["d"]))
    except (OSError, ValueError):
        pass
    if msg.get("x"):
        with conns_lock:
            conns.pop(n, None)
        try:
            c.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        c.close()
os._exit(0)
