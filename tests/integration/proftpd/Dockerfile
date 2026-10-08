# ProFTPD test server for the FTPS cases, built natively for the host arch.
#
# Why not vsftpd for TLS: vsftpd segfaults (exit 139) on the data connection
# when OpenSSL 3 negotiates TLS 1.3. The control handshake succeeds, so the
# failure looks like an unexplained EOF from the client, and vsftpd offers no
# setting to cap the TLS version. ProFTPD handles TLS 1.3 correctly and also
# exposes session-reuse enforcement directly, which we want to test.
#
# Why not a prebuilt image: popular FTP images are amd64-only and fail under
# Rosetta emulation on arm64.
FROM debian:12-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends proftpd-core proftpd-mod-crypto openssl ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
