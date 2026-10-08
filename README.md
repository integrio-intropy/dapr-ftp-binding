# dapr-ftp-binding

A [Dapr pluggable component](https://docs.dapr.io/developing-applications/develop-components/pluggable-components/pluggable-components-overview/)
providing an **FTP and FTPS output binding**. Dapr ships an SFTP binding but
nothing for plain FTP or FTPS; this fills that gap without waiting on upstream.

It runs as a separate process or container, registers with the Dapr sidecar over
a Unix domain socket, and is used through the ordinary bindings API.

```bash
curl -X POST localhost:3500/v1.0/bindings/ftp \
  -d '{"operation":"create","data":"aGVsbG8=","metadata":{"fileName":"hello.txt"}}'
```

## Status

Alpha. The operations and metadata mirror Dapr's SFTP binding, so moving
between them should be mechanical.

| Operation | Behaviour |
|---|---|
| `create` | Upload the request data to `fileName` |
| `get` | Download `fileName` and return it as the response data |
| `list` | List the directory in `fileName`, defaulting to `rootPath` |
| `delete` | Delete `fileName` |


## Configuration

```yaml
apiVersion: dapr.io/v1alpha1
kind: Component
metadata:
  name: ftp
spec:
  type: bindings.ftp
  version: v1
  metadata:
    - name: address
      value: "ftp.example.com:21"
    - name: username
      value: "ftpuser"
    - name: password
      secretKeyRef:
        name: ftp-credentials
        key: password
    - name: rootPath
      value: "/upload"
    - name: tlsMode
      value: "explicit"
```

| Field | Required | Default | Notes |
|---|---|---|---|
| `address` | **yes** | — | `host:port`. |
| `username` | no | `anonymous` | |
| `password` | no | `anonymous@` | Supports `secretKeyRef`. Never logged. |
| `rootPath` | **yes** | — | Working directory. `fileName` is joined to it. |
| `tlsMode` | no | `none` | `none`, `explicit` (AUTH TLS) or `implicit` (TLS from connect). |
| `insecureSkipVerify` | no | `false` | Disables certificate verification. |
| `timeout` | no | `30s` | Must carry a unit. See below. |
| `disableEPSV` | no | `false` | Disable extended passive mode. |
| `trustPASVIP` | no | `false` | Use the server's advertised PASV address. Usually needed behind NAT or Docker. |
| `maxConnections` | no | `5` | Concurrent FTP sessions, **per component instance**. |

`timeout` bounds connecting and logging in, and how long a request waits for a
free connection from the pool. It does **not** bound an operation already in
flight — see "Scope" below.

### Operations in detail

**`create`** — `fileName` is required; the request data is the file content.
Existing files are overwritten, as they are on every mainstream FTP server.
Responds `{"fileName":"/upload/hello.txt"}` with the joined server path. Parent
directories are not created.

**`get`** — `fileName` is required. Responds with the raw bytes and a `size`
metadata field.

**`list`** — `fileName` is optional and defaults to `rootPath`. Responds with
`[{"fileName":"a.txt","isDirectory":false}, ...]` in the server's own order,
matching the SFTP binding's shape.

**`delete`** — `fileName` is required. Responds HTTP 200 with an empty body.


## Security notes

- **Certificate verification is on by default.** `insecureSkipVerify` exists but
  warns loudly at startup.
- **`tlsMode: none` sends credentials and file contents in cleartext**, and says
  so in the log at startup.
- **`rootPath` is not a boundary.** Paths are joined, not confined.


## Connection pooling

A single FTP session is not safe for concurrent use and supports one data
connection at a time, so operations take exclusive sessions from a pool capped
at `maxConnections`.

Two things to keep in mind:

- **`maxConnections` is per component instance.** The Dapr sidecar may create
  more than one instance against the same socket, so keep the value below the
  server's per-user login cap divided by the number of instances.
- Running out of connections surfaces as a timeout naming `maxConnections`
  rather than as a hang.

## Running it

See [`examples/standalone`](examples/standalone) and
[`examples/kubernetes`](examples/kubernetes).


## Command-line flags

The component is configured through Dapr metadata; these flags only cover how
the process itself is hosted.

| Flag | Default | Notes |
|---|---|---|
| `-socket-name` | `ftp` | Socket file name without `.sock`. Determines `spec.type` (`bindings.<name>`). |
| `-socket-perm` | `0660` | Permission bits for the socket, **read as octal** whether or not you write the leading zero. |
| `-max-message-size` | `5242880` | Maximum gRPC message in bytes. Raise alongside the sidecar's `--max-body-size`; |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error`. |
| `-version` | | Print the version and exit. |


## Security

Please report vulnerabilities privately through
[GitHub's private vulnerability reporting](https://github.com/integrio-intropy/dapr-ftp-binding/security/advisories/new),
not as a public issue.

## License

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). Third-party
dependencies and their licences are listed in
[THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES).

Dapr is a trademark of the Linux Foundation. This project is a community
component and is not affiliated with or endorsed by the Dapr project.
