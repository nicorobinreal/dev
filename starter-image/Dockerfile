# ghcr.io/nicorobinreal/dev-starter: root-only Debian with Node LTS, Go,
# Claude Code, Codex, OpenCode and dev-sshd-keys. gh, sshd and the Docker CLI
# come from the features in .devcontainer/devcontainer.json. Build with
# `devcontainer build` (see .devcontainer/devcontainer.json), multi-arch.
FROM debian:trixie-slim

LABEL org.opencontainers.image.source="https://github.com/nicorobinreal/dev" \
      org.opencontainers.image.description="Base image of the dev starter devcontainer"

ARG TARGETARCH
ARG NODE_VERSION=26.13.1
ARG GO_VERSION=1.27.4

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    PATH=/root/go/bin:/usr/local/go/bin:$PATH

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash-completion ca-certificates curl git gnupg jq less lsof make \
      nano openssh-client procps ripgrep unzip vim-tiny wget xz-utils \
      build-essential python3 \
 && rm -rf /var/lib/apt/lists/*

# Node LTS from nodejs.org (checksum-verified); npm's global bin is /usr/local/bin.
RUN case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      amd64) arch=x64 ;; arm64) arch=arm64 ;; *) echo "unsupported arch" >&2; exit 1 ;; \
    esac \
 && file="node-v${NODE_VERSION}-linux-${arch}.tar.xz" \
 && cd /tmp \
 && curl -fsSLO "https://nodejs.org/dist/v${NODE_VERSION}/${file}" \
 && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt" | grep " ${file}\$" | sha256sum -c - \
 && tar -xJf "${file}" -C /usr/local --strip-components=1 --no-same-owner \
 && rm -f "${file}" /usr/local/CHANGELOG.md /usr/local/LICENSE /usr/local/README.md \
 && node --version && npm --version

# Go from go.dev (checksum-verified), linked into /usr/local/bin so SSH login
# shells (which reset PATH) find it too.
RUN arch="${TARGETARCH:-$(dpkg --print-architecture)}" \
 && file="go${GO_VERSION}.linux-${arch}.tar.gz" \
 && cd /tmp \
 && curl -fsSLO "https://dl.google.com/go/${file}" \
 && echo "$(curl -fsSL "https://dl.google.com/go/${file}.sha256")  ${file}" | sha256sum -c - \
 && tar -xzf "${file}" -C /usr/local \
 && rm -f "${file}" \
 && ln -s /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/bin/ \
 && go version

# Agent harnesses.
RUN npm install -g @anthropic-ai/claude-code @openai/codex opencode-ai \
 && npm cache clean --force \
 && claude --version && codex --version && opencode --version

COPY rootfs/ /

# Harnesses run without approval prompts (the VM is the security boundary);
# history survives rebuilds via HISTFILE on the history volume.
RUN chmod 755 /usr/local/bin/dev-sshd-keys \
 && printf '%s\n' \
      '' \
      '# dev starter' \
      "alias claude='claude --dangerously-skip-permissions'" \
      "alias codex='codex --dangerously-bypass-approvals-and-sandbox'" \
      "PROMPT_COMMAND='history -a'" \
      >> /root/.bashrc \
 && echo 'CLAUDE_CONFIG_DIR=/root/.claude-auth' >> /etc/environment \
 && echo 'export PATH="/root/go/bin:$PATH"' > /etc/profile.d/dev-go.sh

WORKDIR /root
