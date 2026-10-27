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
| `tempFileNamePattern` | no | — (off) | Upload under this name, then rename onto `fileName`. `{name}` is the base name, e.g. `{name}.tmp`. See below. |
| `createDirs` | no | `false` | Create missing directories under `rootPath` on `create`. See below. |

`timeout` bounds connecting and logging in, and how long a request waits for a
free connection from the pool. It does **not** bound an operation already in
flight — see "Scope" below.

### Operations in detail

**`create`** — `fileName` is required; the request data is the file content.
Existing files are overwritten, as they are on every mainstream FTP server.
Responds `{"fileName":"/upload/hello.txt"}` with the joined server path. Parent
directories are not created unless `createDirs` is set — see below. With
`tempFileNamePattern` set, the upload goes to a temp name first — also below.

**`get`** — `fileName` is required. Responds with the raw bytes and a `size`
metadata field.

**`list`** — `fileName` is optional and defaults to `rootPath`. Responds with
`[{"fileName":"a.txt","isDirectory":false}, ...]` in the server's own order,
matching the SFTP binding's shape.

**`delete`** — `fileName` is required. Responds HTTP 200 with an empty body.


### Writing through a temp name

A partner polling the folder can otherwise pick up a file while it is still
uploading. Set `tempFileNamePattern` and `create` uploads to that name instead,
then renames it onto `fileName` with `RNFR`/`RNTO`:

```yaml
    - name: tempFileNamePattern
      value: "{name}.tmp"
```

`POST create` with `fileName: out/a.xml` then writes `out/a.xml.tmp` and renames
it to `out/a.xml`. The rename is a control-channel command: nothing is
re-uploaded. `{name}` is replaced with the base name, and the pattern may not
contain `/` or `\`.

What this does and does not buy you:

- **A partner never sees a half-written file under the final name.** This is
  the point of the feature, and it holds wherever rename is supported.
- **Leftover temp files are not cleaned up.** A failed rename removes the temp
  file on a best-effort basis, but a hard crash between the upload and the
  rename leaves one behind.
- **Concurrent `create` calls for the same `fileName` collide** on one temp
  name. The last writer wins. Do not run two writers against one name.


### Creating directories

By default the binding uploads and nothing else: if the directory above
`fileName` does not exist, the server refuses the write and you get its reply.
What that reply looks like depends on the server, and none of them say "the
directory is missing":

| Server | Reply to a `STOR` into a directory that does not exist |
|---|---|
| vsftpd | `553 Could not create file.` |
| pure-ftpd | `553 Can't open that file: No such file or directory` |
| proftpd | `550 <path>: No such file or directory` |

All three use those same codes for a permission problem, so the code alone
never tells you which it is.

Set `createDirs` and `create` answers such a refusal by creating the missing
directories and uploading again:

```yaml
    - name: createDirs
      value: "true"
```

`POST create` with `fileName: IQPL/DispatchForecast/a.xml` against
`rootPath: /upload` then issues `MKD /upload/IQPL`, `MKD
/upload/IQPL/DispatchForecast` and stores the file.

What this does and does not do:

- **The directories are only walked after an upload has been refused.** The
  ordinary case — the folder is already there, which is every request after the
  first — is still a single `STOR` and costs nothing. The price is paid on the
  request that needs it, and on any request failing for another reason, which
  spends one `MKD` per level before failing anyway.
- **Nothing is created outside `rootPath`.** A `fileName` that climbs out with
  `../` still gets its plain upload — paths are joined, not confined, and the
  server decides — but the binding will not build a tree out there to make it
  work.
- **A level that already exists is expected to fail.** `MKD` has no "create if
  missing" and servers answer 550 either way, so the walk shrugs those off. If
  a level genuinely could not be created, the second upload fails and you get
  the server's own reason for that, not a guess about the directory.
- **Concurrent creates may race on `MKD`.** The loser's `MKD` fails, which is
  indistinguishable from "already there", so both requests still succeed.
- **Directories are never removed.** There is no `delete` for them.


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
| `-exit-when-idle` | `0` (off) | Exit once daprd's HTTP port has answered and then gone quiet for this long. For Jobs and CronJobs; see the Kubernetes example. Leave off for Deployments. |
| `-exit-when-no-sidecar` | `1m` | With `-exit-when-idle`, also exit if daprd's HTTP port never answers within this long — the case where daprd died before reaching us. `0` waits forever. Ignored without `-exit-when-idle`. |
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
