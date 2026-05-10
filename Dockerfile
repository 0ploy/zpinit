FROM alpine:3.20

ARG TARGETARCH

COPY zpinit-linux-${TARGETARCH} /usr/local/bin/zpinit
COPY zpctl-linux-${TARGETARCH} /usr/local/bin/zpctl

RUN chmod +x /usr/local/bin/zpinit /usr/local/bin/zpctl

CMD ["sh"]
