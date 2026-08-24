FROM alpine:3.20

RUN apk add --no-cache openssh-server bash \
    && ssh-keygen -A \
    && { \
        echo "PermitRootLogin prohibit-password"; \
        cat /etc/ssh/sshd_config; \
    } > /etc/ssh/sshd_config.new \
    && mv /etc/ssh/sshd_config.new /etc/ssh/sshd_config

COPY docker.sh /usr/local/bin/docker
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /usr/local/bin/docker /entrypoint.sh

EXPOSE 22
ENTRYPOINT ["/entrypoint.sh"]
