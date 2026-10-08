# What this changes

<!-- One or two sentences. Link the issue if there is one. -->

# Why

<!-- What was wrong, or what could not be done before. -->

# Checklist

- [ ] Commits are signed off (`git commit -s`) — see [CONTRIBUTING.md](/CONTRIBUTING.md).
- [ ] `go test -race ./...` passes.
- [ ] `golangci-lint run` is clean.
- [ ] There is a test that fails without this change.
- [ ] README, `metadata.yaml` and `CHANGELOG.md` are updated if behaviour or
      metadata changed.

# Anything reviewers should look at closely

<!--
If you touched internal/ftpclient/jlaffaye.go, confirm you have not introduced
DialWithDialFunc: it silently downgrades the FTPS data channel. See CONTRIBUTING.md.

If you ran the integration suite, say which servers you ran it against.
-->
