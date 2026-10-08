# pure-ftpd test server, built natively for the host architecture.
#
# A second FTP implementation for PLAIN FTP, so the binding is not accidentally
# written against one server's quirks. Built from alpine rather than using
# stilliard/pure-ftpd, which is amd64-only and crashes under Rosetta on arm64.
#
# Alpine's pure-ftpd reports "v1.0.51 [privsep]" with no [TLS] marker: it is
# built WITHOUT TLS support, so the FTPS cases all use ProFTPD instead.
FROM alpine:3.24

RUN apk add --no-cache pure-ftpd

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
