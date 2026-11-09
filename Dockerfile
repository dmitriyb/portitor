# portitor — a git-over-SSH gateway hosting bare repos gated by the pre-receive
# checks, forwarding accepted feature branches to a real upstream.
#
# Build:  docker build -t portitor .
# Run:    deploy/run.sh --config-dir <dir with repos.d/> --keys <role *.pub,...>
#         It bakes the config into a derived image (used by ID) and copies the
#         GitHub PAT into the created container at /run/secrets/gh_token, so no host
#         path is mounted and the PAT is never an env var or an image layer.
# Provision a repo (its config at /etc/portitor/repos.d/myrepo.json, baked by run.sh;
# config lives in the registry, the single identity the gate + pr API both read):
#         docker exec -u git portitor portitor add-repo \
#             --repo myrepo --upstream https://github.com/you/myrepo.git

# --- build ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY cli ./cli
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/portitor ./cmd/portitor

# --- runtime: minimal git-over-ssh server (+ gh for the action API) ---
FROM alpine:3.20
RUN apk add --no-cache git openssh-server tini github-cli \
    && adduser -D -s /bin/sh git \
    # adduser -D leaves the account locked (! in shadow); sshd refuses pubkey
    # login to a locked account ("invalid user"). Mark it valid-but-passwordless.
    && sed -i 's/^git:!/git:*/' /etc/shadow \
    && mkdir -p /srv/git /home/git/.ssh \
    # deploy/run.sh copies the PAT here into the created container before start
    && mkdir -m 0700 /run/secrets \
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
