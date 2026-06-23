# portitor — a git-over-SSH gateway hosting bare repos gated by the pre-receive
# checks, forwarding accepted feature branches to a real upstream.
#
# Build:  docker build -t portitor .
# Run:    docker run -d --name portitor -p 2222:22 \
#             -e AGENT_AUTHORIZED_KEY="$(cat agent_push_key.pub)" \
#             -v portitor-repos:/srv/git portitor
# Provision a repo:
#         docker exec portitor portitor init-repo \
#             --bare /srv/git/myrepo.git --default main \
#             --upstream https://github.com/you/myrepo.git \
#             --config /srv/git/myrepo.portitor.json

# --- build ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/portitor ./cmd/portitor

# --- runtime: minimal git-over-ssh server ---
FROM alpine:3.20
RUN apk add --no-cache git openssh-server tini \
    && adduser -D -s /bin/sh git \
    && mkdir -p /srv/git /home/git/.ssh \
    && chown -R git:git /srv/git /home/git \
    && chmod 700 /home/git/.ssh \
    # key-only sshd
    && sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && sed -i 's/^#\?PubkeyAuthentication.*/PubkeyAuthentication yes/' /etc/ssh/sshd_config \
    && sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config

COPY --from=build /out/portitor /usr/local/bin/portitor
COPY deploy/entrypoint.sh /usr/local/bin/portitor-entrypoint
RUN chmod +x /usr/local/bin/portitor-entrypoint

EXPOSE 22
VOLUME ["/srv/git"]
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/portitor-entrypoint"]
