# vsftpd test server, built natively for the host architecture.
#
# Two deliberate choices here:
#
#  * Built from a base image rather than pulled. Popular FTP images are
#    amd64-only, and pure-ftpd dies under Rosetta on arm64 with
#    "rosetta error: mmap_anonymous_rw mmap failed" as soon as it forks per
#    connection.
#  * Debian rather than Alpine. Alpine's vsftpd segfaults (exit 139) on the
#    first TLS connection; the musl/OpenSSL combination is not viable for the
#    FTPS cases.
#
# MODE selects plain, explicit or implicit TLS. vsftpd cannot serve explicit and
# implicit on one listener, so those are separate containers.
FROM debian:12-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends vsftpd openssl ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
