"""Runs three stub CometBFT RPCs for selftest.sh. Each serves heights 1 to 3,
and the third serves a different app hash at height 2. When all three listen,
the script writes their ports to the given file. It serves until it is killed.
"""

import json, os, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

def machine(diverge_at):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path.startswith("/status"):
                result = {"sync_info": {"earliest_block_height": "1", "latest_block_height": "3"}}
            else:
                result = {"block_metas": [{"header": {
                    "height": str(h),
                    "app_hash": "DIVERGED" if h == diverge_at else "HASH%d" % h,
                }} for h in (1, 2, 3)]}
            body = json.dumps({"result": result}).encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server.server_address[1]

ports = "%d %d %d\n" % (machine(None), machine(None), machine(2))
with open(sys.argv[1] + ".tmp", "w") as f:
    f.write(ports)
os.rename(sys.argv[1] + ".tmp", sys.argv[1])
threading.Event().wait()
