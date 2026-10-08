#!/bin/sh
set -eu

MODE="${MODE:-plain}"                 # plain | explicit | implicit
FTP_USER="${FTP_USER:-ftpuser}"
FTP_PASS="${FTP_PASS:-ftppass}"
PASV_MIN="${PASV_MIN:-30000}"
PASV_MAX="${PASV_MAX:-30009}"
PASV_ADDRESS="${PASV_ADDRESS:-127.0.0.1}"
LISTEN_PORT="${LISTEN_PORT:-21}"
REQUIRE_SSL_REUSE="${REQUIRE_SSL_REUSE:-NO}"
IDLE_TIMEOUT="${IDLE_TIMEOUT:-300}"
PEM=/etc/ssl/private/vsftpd.pem

if [ ! -f "$PEM" ]; then
  # Key and certificate must go to SEPARATE files and then be concatenated.
  # Passing one path to both -keyout and -out truncates one with the other, and
  # vsftpd then exits 2 with no message because it cannot load the key.
  openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
    -keyout /tmp/k.pem -out /tmp/c.pem \
    -subj "/C=SE/O=dapr-ftp-binding-test/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null
  cat /tmp/k.pem /tmp/c.pem > "$PEM"
  rm -f /tmp/k.pem /tmp/c.pem
  chmod 600 "$PEM"
fi

if ! id "$FTP_USER" >/dev/null 2>&1; then
  useradd -m -d "/home/$FTP_USER" -s /usr/sbin/nologin "$FTP_USER"
  echo "$FTP_USER:$FTP_PASS" | chpasswd
fi
mkdir -p "/home/$FTP_USER/upload" /var/run/vsftpd/empty
# vsftpd refuses logins for users whose shell is not in /etc/shells.
grep -qx /usr/sbin/nologin /etc/shells 2>/dev/null || echo /usr/sbin/nologin >> /etc/shells
chown -R "$FTP_USER:$FTP_USER" "/home/$FTP_USER"

# NOTE: vsftpd exits 2 with NO diagnostic on an unrecognised option. In
# particular there is no ssl_tlsv1_2 setting; ssl_tlsv1 enables TLS 1.x.
CONF_FILE=/etc/vsftpd.conf
cat > "$CONF_FILE" <<CONF
listen=YES
listen_ipv6=NO
listen_port=${LISTEN_PORT}
anonymous_enable=NO
local_enable=YES
write_enable=YES
local_umask=022
dirmessage_enable=NO
use_localtime=YES
# vsftpd opens its log file with O_APPEND|O_CREAT, which fails on /dev/stdout
# and makes every connection answer "500 OOPS: failed to open vsftpd log file".
xferlog_enable=YES
xferlog_file=/var/log/vsftpd.log
connect_from_port_20=NO
chroot_local_user=YES
allow_writeable_chroot=YES
secure_chroot_dir=/var/run/vsftpd/empty
seccomp_sandbox=NO
idle_session_timeout=${IDLE_TIMEOUT}
pasv_enable=YES
pasv_min_port=${PASV_MIN}
pasv_max_port=${PASV_MAX}
pasv_address=${PASV_ADDRESS}
pasv_addr_resolve=NO
max_clients=20
max_per_ip=20
CONF

if [ "$MODE" != "plain" ]; then
  cat >> "$CONF_FILE" <<CONF
ssl_enable=YES
rsa_cert_file=${PEM}
rsa_private_key_file=${PEM}
force_local_data_ssl=YES
force_local_logins_ssl=YES
ssl_tlsv1=YES
ssl_sslv2=NO
ssl_sslv3=NO
require_ssl_reuse=${REQUIRE_SSL_REUSE}
ssl_ciphers=HIGH
CONF
fi

if [ "$MODE" = "implicit" ]; then
  echo "implicit_ssl=YES" >> "$CONF_FILE"
fi

echo "starting vsftpd in ${MODE} mode on port ${LISTEN_PORT} (pasv ${PASV_MIN}-${PASV_MAX})"
exec /usr/sbin/vsftpd "$CONF_FILE"
