#!/usr/bin/env bash
# Test-only credentials. Never use these private keys outside tests.
set -euo pipefail
cd "$(dirname "$0")"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
openssl ecparam -name prime256v1 -genkey -noout -out ca-key.pem
openssl req -new -x509 -key ca-key.pem -subj /CN=grpc-lite-client-ca -days 7300 -out ca.pem
openssl ecparam -name prime256v1 -genkey -noout -out client-key.pem
openssl req -new -key client-key.pem -subj /CN=grpc-lite-client -out "$work/client.csr"
touch "$work/index"
echo 01 > "$work/serial"
cat > "$work/ca.cnf" <<CONFIG
[ca]
default_ca = local
[local]
database = $work/index
serial = $work/serial
new_certs_dir = $work
certificate = ca.pem
private_key = ca-key.pem
default_md = sha256
policy = policy
unique_subject = no
[policy]
commonName = supplied
[client]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = clientAuth
CONFIG
openssl ca -batch -notext -config "$work/ca.cnf" -extensions client -in "$work/client.csr" -startdate 20200101000000Z -enddate 20460101000000Z -out client.pem
openssl ca -batch -notext -config "$work/ca.cnf" -extensions client -in "$work/client.csr" -startdate 20200101000000Z -enddate 20210101000000Z -out expired-client.pem
rm ca-key.pem
