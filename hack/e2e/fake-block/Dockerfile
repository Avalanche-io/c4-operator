# A stand-in block image for live runs without model spend: the real
# c4-agent and c4-unit, and a fake `claude` (print mode, the Stop hook run
# as Claude Code runs it). uid 65532, as images/block.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends python3 ca-certificates && rm -rf /var/lib/apt/lists/*
RUN useradd --create-home --home-dir /home/block --uid 65532 --user-group block \
 && mkdir -p /private/tmp && chmod 1777 /private/tmp
COPY c4-agent c4-unit claude /usr/local/bin/
USER 65532:65532
ENV HOME=/home/block
WORKDIR /home/block
ENTRYPOINT ["c4-agent", "block-pod"]
