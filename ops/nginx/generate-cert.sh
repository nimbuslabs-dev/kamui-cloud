#!/bin/sh
set -eu
umask 077

dir=${1:?Provide the TLS directory}
ip=${2:-127.0.0.1}
valid_ip() {
    awk -v ip="$ip" 'BEGIN {
        n = split(ip, a, "[.]")
        if (n != 4) exit 1
        for (i = 1; i <= 4; i++)
            if (a[i] !~ /^(0|[1-9][0-9]?[0-9]?)$/ || a[i] > 255) exit 1
        exit !(ip == "127.0.0.1" || a[1] == 10 ||
            (a[1] == 172 && a[2] >= 16 && a[2] <= 31) ||
            (a[1] == 192 && a[2] == 168))
    }'
}
if ! valid_ip; then
    echo "KAMUI_BIND_IP must be 127.0.0.1 or one private LAN IPv4 address" >&2
    exit 1
fi
mkdir -p "$dir/serving"
if ! mkdir "$dir/.tls-lock"; then
    echo "TLS initialization is already running or left a lock at $dir/.tls-lock" >&2
    exit 1
fi
tmp="$dir/.tls-lock"
trap 'find "$tmp" -type f -exec rm -f {} +; rmdir "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

if [ ! -e "$dir/ca.pem" ] && [ ! -e "$dir/ca.key" ]; then
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -noenc -sha256 \
        -days 1825 -subj '/CN=Kamui Cloud Local CA' \
        -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
        -addext 'keyUsage=critical,keyCertSign,cRLSign' \
        -keyout "$tmp/ca.key" -out "$tmp/ca.pem"
    mv "$tmp/ca.key" "$dir/ca.key"
    mv "$tmp/ca.pem" "$dir/ca.pem"
fi
if [ ! -s "$dir/ca.pem" ] || [ ! -s "$dir/ca.key" ]; then
    echo "TLS certificate authority is incomplete. Restore its certificate and key from backup." >&2
    exit 1
fi

leaf="$dir/serving/server.pem"
key="$dir/serving/server.key"
if [ -s "$leaf" ] && [ -s "$key" ] &&
    openssl x509 -in "$leaf" -checkend 2592000 -noout >/dev/null 2>&1 &&
    openssl verify -CAfile "$dir/ca.pem" -verify_ip "$ip" "$leaf" >/dev/null 2>&1 &&
    [ "$(openssl x509 -in "$leaf" -pubkey -noout)" = "$(openssl pkey -in "$key" -pubout)" ]; then
    exit 0
fi

san='DNS:localhost,IP:127.0.0.1'
if [ "$ip" != 127.0.0.1 ]; then san="$san,IP:$ip"; fi
printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=%s\n' "$san" > "$tmp/leaf.ext"
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -noenc \
    -subj '/CN=localhost' -keyout "$tmp/server.key" -out "$tmp/server.csr"
openssl x509 -req -sha256 -days 365 -in "$tmp/server.csr" -extfile "$tmp/leaf.ext" \
    -CA "$dir/ca.pem" -CAkey "$dir/ca.key" -out "$tmp/server.pem"
openssl verify -CAfile "$dir/ca.pem" -verify_ip "$ip" "$tmp/server.pem"
mv "$tmp/server.key" "$key"
mv "$tmp/server.pem" "$leaf"
