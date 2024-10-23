#!/bin/bash

set -e

# install wireguard
{{ if eq .Type "aws" }}
amazon-linux-extras install -y epel
rwfile="/etc/yum.repos.d/wireguard.repo"
rwurl="https://copr.fedorainfracloud.org/coprs/jdoss/wireguard/repo/epel-7/jdoss-wireguard-epel-7.repo"
# Download it
sudo wget --output-document="$rwfile" "$rwurl"
# Install it
sudo yum install -y wireguard-dkms wireguard-tools
{{ else }}
    dnf install -y epel-release
    dnf install wireguard-tools -y
{{ end }}



if ! grep -q "net.ipv4.ip_forward = 1" /etc/sysctl.conf >/dev/null; then
    echo "net.ipv4.ip_forward = 1" >> /etc/sysctl.conf
fi
sysctl -p

# generate wireguard keys
mkdir -p /etc/wireguard
cd /etc/wireguard

if ! [ -f privatekey ]; then
    wg genkey | tee privatekey
fi

if ! [ -f publickey ]; then
    cat privatekey | wg pubkey > publickey
fi

privatekey=$(cat privatekey)
publickey=$(cat publickey)

# configure wireguard
cat <<EOF > /etc/wireguard/wg0.conf
[Interface]
Address = {{ .ServerWgIp }}/32
PrivateKey = $privatekey
ListenPort = {{ .WgPort }}

[Peer]
PublicKey = {{ .ClientPublicKey }}
AllowedIPs = {{ .ClientWgIp }}/32
EOF

systemctl enable wg-quick@wg0
systemctl restart wg-quick@wg0

# configure iptables
yum install -y iptables-services
systemctl enable iptables
iptables -t nat -I POSTROUTING 1 -s {{ .ClientWgIp }}/32 -o eth0 -j MASQUERADE
service iptables save

####################### OUTPUT #######################

printf "{{ .OutputSeparator }}"

cat << _EOF
{
    "ServerWgPublicKey": "$publickey"
}
_EOF
