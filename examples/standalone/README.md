# Running the FTP binding in standalone mode

The component must be started **before** the Dapr sidecar: the sidecar scans
the socket directory once at startup and will not pick up a socket that appears
later.

```bash
# 1. Start an FTP server to talk to.
docker compose -f ../../tests/integration/docker-compose.yaml up -d ftp-plain

# 2. Start the component.
go run ./cmd/ftp-binding          # or: docker run ... harbor.intropy.io/platform/dapr-ftp-binding

# 3. Start the sidecar, pointed at this directory.
dapr run --app-id ftp-demo --dapr-http-port 3500 --resources-path ./components
```

## Exercising it

```bash
# create: data is base64 in the JSON body
curl -X POST localhost:3500/v1.0/bindings/ftp \
  -H 'Content-Type: application/json' \
  -d '{"operation":"create","data":"aGVsbG8gZnRw","metadata":{"fileName":"hello.txt"}}'

# get: returns the raw file bytes
curl -X POST localhost:3500/v1.0/bindings/ftp \
  -d '{"operation":"get","metadata":{"fileName":"hello.txt"}}'

# list: omit fileName to list rootPath itself
curl -X POST localhost:3500/v1.0/bindings/ftp \
  -d '{"operation":"list"}'

# delete: responds HTTP 200 with an empty body
curl -X POST localhost:3500/v1.0/bindings/ftp \
  -d '{"operation":"delete","metadata":{"fileName":"hello.txt"}}'
```

## Scope

Path confinement, payload size limits and per-operation timeouts are
deliberately not handled here, matching the built-in SFTP binding. See the
"Scope" section of the main README.
