#!/bin/sh
set -eu

FTP_USER="${FTP_USER:-ftpuser}"
FTP_PASS="${FTP_PASS:-ftppass}"
PASV_MIN="${PASV_MIN:-30010}"
PASV_MAX="${PASV_MAX:-30019}"
PASV_ADDRESS="${PASV_ADDRESS:-127.0.0.1}"
MAX_CLIENTS="${MAX_CLIENTS:-20}"
if ! id "$FTP_USER" >/dev/null 2>&1; then
  adduser -D -h "/home/$FTP_USER" "$FTP_USER"
fi
mkdir -p "/home/$FTP_USER/upload"
chown -R "$FTP_USER:$FTP_USER" "/home/$FTP_USER"

# A virtual user mapped onto the system account, so no system password is needed.
# pure-pw will not create its passwd file, so it has to exist first.
mkdir -p /etc/pure-ftpd
PASSWD=/etc/pure-ftpd/pureftpd.passwd
touch "$PASSWD"
( echo "$FTP_PASS"; echo "$FTP_PASS" ) | \
  pure-pw useradd "$FTP_USER" -f "$PASSWD" -u "$FTP_USER" -d "/home/$FTP_USER" >/dev/null
pure-pw mkdb /etc/pure-ftpd/pureftpd.pdb -f "$PASSWD"

# Only short options: this build does not accept the long forms.
set -- \
  -l puredb:/etc/pure-ftpd/pureftpd.pdb \
  -E -j -A \
  -p "${PASV_MIN}:${PASV_MAX}" \
  -P "${PASV_ADDRESS}" \
  -c "${MAX_CLIENTS}" -C "${MAX_CLIENTS}"

echo "starting pure-ftpd (pasv ${PASV_MIN}-${PASV_MAX})"
exec pure-ftpd "$@"
