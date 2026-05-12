FROM alpine:3.20

ARG TARGETARCH

COPY zpinit-linux-${TARGETARCH} /usr/local/bin/zpinit
COPY zpctl-linux-${TARGETARCH} /usr/local/bin/zpctl

# bash and curl are playground conveniences (`docker exec -it zp bash`,
# `curl http://localhost` from inside a service). They cost ~3 MB; pull
# them only if you're using the image directly. If you only need the
# binaries (`COPY --from=ghcr.io/0ploy/zpinit /usr/local/bin/zpinit …`)
# you never pay for them — the COPY --from sees just the two binaries.
RUN apk add --no-cache bash curl \
 && chmod +x /usr/local/bin/zpinit /usr/local/bin/zpctl

# Doubles as a binary-delivery layer (`COPY --from=ghcr.io/0ploy/zpinit`)
# and a playground image: `docker run -it ghcr.io/0ploy/zpinit` starts
# zpinit as PID 1 with zero services so you can `docker exec` in,
# install software, drop service files into /etc/zpinit/services/,
# and `zpctl reread` to bring them up.
ENTRYPOINT ["zpinit"]
