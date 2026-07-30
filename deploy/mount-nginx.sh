#!/usr/bin/env bash
# Mount the Amnesia-Proof Agent demo at https://deploytest.theodoikenh.com/amnesia/
#
# WHY A PATH ON AN EXISTING SUBDOMAIN instead of amnesia.theodoikenh.com: DNS for this domain
# is managed manually at matbao (no API), so a fresh subdomain means waiting on a human to add
# an A record before Certbot can issue. deploytest.theodoikenh.com already resolves and already
# has a valid cert, so mounting a path gives a real HTTPS URL immediately. The exact-prefix
# location is evaluated before the catch-all `location /`, so the existing app on :8804 is
# untouched.
set -euo pipefail

CONF=/etc/nginx/sites-available/deploytest.theodoikenh.com.conf
MARKER='location /amnesia'

if grep -q "$MARKER" "$CONF"; then
  echo "already mounted; nothing to do"
else
  # Insert both location blocks immediately after the FIRST `server_name` line, which sits in
  # the HTTPS server block. Ordering inside the block does not matter to nginx (longest
  # prefix wins), but placing them before `location /` keeps the file readable.
  python3 - "$CONF" <<'PY'
import sys
path = sys.argv[1]
src = open(path).read()

block = '''
    # --- Amnesia-Proof Agent (CockroachDB x AWS Hackathon) -> 127.0.0.1:8805 ---
    location /amnesia/events {
        proxy_pass http://127.0.0.1:8805/events;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Connection '';
        # SSE: without these the dashboard's live cluster/memory panels never update,
        # which would silently break the one thing the demo exists to show.
        proxy_buffering off;
        proxy_cache off;
        chunked_transfer_encoding off;
        proxy_read_timeout 24h;
    }

    location /amnesia/ {
        proxy_pass http://127.0.0.1:8805/;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
    }

    location = /amnesia {
        return 301 /amnesia/;
    }
'''

anchor = '    server_name deploytest.theodoikenh.com;\n'
i = src.index(anchor) + len(anchor)
open(path, 'w').write(src[:i] + block + src[i:])
print("injected /amnesia location blocks")
PY
fi

nginx -t
systemctl reload nginx
echo "reloaded"
