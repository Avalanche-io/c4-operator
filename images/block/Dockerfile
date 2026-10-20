# A block's image: Claude Code with bash and git, and c4-agent and c4-unit
# for its Stop hook, its checkpoints and its safe points. The pod runs
# `c4-agent block-pod` (docs/agents/c4-agent.md, "Blocks in containers").
#
# It holds no credential. The sign-in comes from a credential lease mounted
# at $C4_LEASE_DIR (docs/agents/credential-leases.md), and the pod's own
# Claude Code store ($CLAUDE_CONFIG_DIR) is a volume.
#
# It runs as uid 65532, and fits PodSecurity "restricted" with
# runAsNonRoot, seccomp RuntimeDefault, all capabilities dropped and no
# privilege escalation. `make image-block` places linux c4-agent and c4-unit
# in the build context (bin/block-context).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash git ca-certificates curl && rm -rf /var/lib/apt/lists/*
RUN useradd --create-home --home-dir /home/block --uid 65532 --user-group block \
 # blocks run in /private/tmp, as on a macOS host, so a session that moves
 # between them keeps the same transcript folder
 && mkdir -p /private/tmp && chmod 1777 /private/tmp
USER 65532:65532
ENV HOME=/home/block PATH=/home/block/.local/bin:/usr/local/bin:/usr/bin:/bin
RUN curl -fsSL https://claude.ai/install.sh | bash && claude --version
COPY c4-agent c4-unit /usr/local/bin/
WORKDIR /home/block
ENTRYPOINT ["c4-agent", "block-pod"]
