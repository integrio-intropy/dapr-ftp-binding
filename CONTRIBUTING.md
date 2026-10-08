# Contributing

Security issues go through
[private vulnerability reporting](https://github.com/integrio-intropy/dapr-ftp-binding/security/advisories/new),
not a pull request. See [SECURITY.md](SECURITY.md).


## Getting set up

```bash
make test     # go test -race ./...; no containers needed
make lint     # installs the pinned golangci-lint if you do not have it
```

Unit tests run against an in-memory fake, and the FTP adapter is covered by an
in-process FTP server, so the bulk of the suite needs nothing installed.

For the integration suite:

```bash
make integration        # compose up --build --wait, then the tagged tests
make integration-down   # stop the containers and drop their volumes
```
