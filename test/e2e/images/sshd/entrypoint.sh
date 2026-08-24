#!/bin/sh
# Generates (once) an SSH keypair shared with the test-runner over a named volume,
# authorizes the public half for root login, and starts sshd in the foreground.
set -eu

mkdir -p /root/.ssh /shared
chmod 700 /root/.ssh

if [ ! -f /shared/id_ed25519 ]; then
    ssh-keygen -t ed25519 -N "" -f /shared/id_ed25519 -C ramify-e2e
fi

cp /shared/id_ed25519.pub /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
chmod 644 /shared/id_ed25519.pub
chmod 600 /shared/id_ed25519
touch /shared/ready

exec /usr/sbin/sshd -D -e
