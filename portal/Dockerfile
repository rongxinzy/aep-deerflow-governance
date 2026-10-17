FROM debian:13.6-slim
RUN set -eux; \
    printf 'deb http://mirrors.aliyun.com/debian/ bookworm main\ndeb http://mirrors.aliyun.com/debian-security/ bookworm-security main\n' > /etc/apt/sources.list; \
    rm -f /etc/apt/sources.list.d/*.sources; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates; \
    rm -rf /var/lib/apt/lists/*
COPY de-portal /de-portal
USER 65532:65532
ENTRYPOINT ["/de-portal"]
