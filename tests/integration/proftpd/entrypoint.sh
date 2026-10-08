#!/bin/sh
set -eu

MODE="${MODE:-explicit}"              # explicit | implicit
FTP_USER="${FTP_USER:-ftpuser}"
FTP_PASS="${FTP_PASS:-ftppass}"
PASV_MIN="${PASV_MIN:-30050}"
PASV_MAX="${PASV_MAX:-30059}"
MASQUERADE="${MASQUERADE:-127.0.0.1}"
LISTEN_PORT="${LISTEN_PORT:-21}"
MAX_CLIENTS="${MAX_CLIENTS:-20}"
# When "YES", the data connection must resume the control connection's TLS
# session. Go only does that when tls.Config carries a ClientSessionCache.
REQUIRE_SSL_REUSE="${REQUIRE_SSL_REUSE:-NO}"
TIMEOUT_IDLE="${TIMEOUT_IDLE:-600}"

CRT=/etc/ssl/private/proftpd.crt
KEY=/etc/ssl/private/proftpd.key

if [ ! -f "$CRT" ]; then
  openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
    -keyout "$KEY" -out "$CRT" \
    -subj "/C=SE/O=dapr-ftp-binding-test/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null
  chmod 600 "$KEY"
fi

if ! id "$FTP_USER" >/dev/null 2>&1; then
  useradd -m -d "/home/$FTP_USER" -s /bin/false "$FTP_USER"
  echo "$FTP_USER:$FTP_PASS" | chpasswd
fi
mkdir -p "/home/$FTP_USER/upload"
chown -R "$FTP_USER:$FTP_USER" "/home/$FTP_USER"

# NoCertRequest was removed in ProFTPD 1.3.8 and is now a fatal config error;
# TLSVerifyClient off covers it.
TLS_OPTIONS=""
if [ "$REQUIRE_SSL_REUSE" != "YES" ]; then
  TLS_OPTIONS="$TLS_OPTIONS NoSessionReuseRequired"
fi
if [ "$MODE" = "implicit" ]; then
  TLS_OPTIONS="$TLS_OPTIONS UseImplicitSSL"
fi
TLS_OPTIONS_LINE=""
if [ -n "$TLS_OPTIONS" ]; then
  TLS_OPTIONS_LINE="TLSOptions ${TLS_OPTIONS}"
fi

# Debian ships mod_tls as a dynamic module, commented out in modules.conf.
# Without this the server answers "500 AUTH not understood" and implicit mode
# just serves plaintext.
cat > /etc/proftpd/proftpd.conf <<CONF
LoadModule mod_tls.c

ServerName "dapr-ftp-binding-test"
ServerType standalone
DefaultServer on
Port ${LISTEN_PORT}
UseIPv6 off
User proftpd
Group nogroup
Umask 022 022
MaxInstances ${MAX_CLIENTS}
MaxClients ${MAX_CLIENTS}
TimeoutIdle ${TIMEOUT_IDLE}
# The account has no real shell; without this proftpd refuses the login.
RequireValidShell off
# Confine the account to its home directory, mirroring a real deployment.
DefaultRoot ~
AllowOverwrite on
WtmpLog off
TransferLog none

PassivePorts ${PASV_MIN} ${PASV_MAX}
MasqueradeAddress ${MASQUERADE}

<IfModule mod_tls.c>
  TLSEngine on
  TLSRequired on
  TLSRSACertificateFile ${CRT}
  TLSRSACertificateKeyFile ${KEY}
  ${TLS_OPTIONS_LINE}
  TLSVerifyClient off
</IfModule>
CONF

echo "starting proftpd in ${MODE} mode on port ${LISTEN_PORT} (pasv ${PASV_MIN}-${PASV_MAX}, reuse=${REQUIRE_SSL_REUSE})"
exec proftpd --nodaemon --config /etc/proftpd/proftpd.conf
