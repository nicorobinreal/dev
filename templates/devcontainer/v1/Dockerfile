# dev starter v1
# The base image holds Debian, Node LTS, Go, gh, sshd, the Docker CLI,
# Claude Code, Codex, OpenCode and dev-sshd-keys. DEV_STARTER_IMAGE (passed
# from the VM's environment by devcontainer.json) overrides it; leave it unset
# to use the published image.
ARG DEV_STARTER_IMAGE
FROM ${DEV_STARTER_IMAGE:-ghcr.io/nicorobinreal/dev-starter:v1}

# Project-specific lines go below.
