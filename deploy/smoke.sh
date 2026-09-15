#!/bin/sh
# Run against a built image on a host with Docker. No real model calls/keys.
set -eu
image=${1:?image required}
name=fathom-smoke-$$
trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
# Exercise the read-only root, non-root user, writable volumes, and startup.
docker run -d --name "$name" --read-only --cap-drop=ALL \
  --security-opt=no-new-privileges --tmpfs /tmp:rw,nosuid,nodev,size=100m \
  --mount type=volume,destination=/data "$image" >/dev/null
attempt=0
until docker exec "$name" node -e 'fetch("http://127.0.0.1:8790/api/v1/ready").then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))'; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then echo 'Container failed to become ready'; exit 1; fi
  sleep 1
done
docker exec "$name" node -e '
const fs=require("fs");
for(const p of ["/data/api-token","/data/devices.db","/data/audit.db","/data/threads.db"]){if(!fs.existsSync(p))throw Error("missing persistent state: "+p)}
const token=fs.readFileSync("/data/api-token","utf8").trim();
fetch("http://127.0.0.1:8790/api/v1/settings",{headers:{Authorization:"Bearer "+token}}).then(r=>{if(!r.ok)process.exit(1)}).catch(()=>process.exit(1));
'
docker restart "$name" >/dev/null
echo 'Container startup, state paths, settings authentication, and restart succeeded.'
