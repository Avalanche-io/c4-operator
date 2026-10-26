# Results

What the end-to-end suite measured. [REPORT-v2.md](../REPORT-v2.md) has every
measurement and its method.

| what                    | result                                                                                                                                                                                                                                                                                                          |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| store load as pods grow | 64 pods reading one 28-object shot cost the store 42 GETs through the tiers (1.5 per object), against 224 when each node reads for itself and 1,792 when each pod does                                                                                                                                          |
| mounts                  | a warm mount in 2 ms; a cold one in 80 ms at the median, mostly from a peer in the same zone (0.8 to 1.0 s when nothing in the zone held the bytes); the next version as a delta layer                                                                                                                          |
| failure                 | a regional cache replica killed mid-transfer: all 32 pods finished, 7 transfers resumed from their offsets at another replica, none fell through to the store                                                                                                                                                   |
| isolation               | nine attempts by one project to read another's data refused; a revoked vendor grant took effect 0.1 s later                                                                                                                                                                                                     |
| monorepo CI             | 401 commits imported in 5.4 s; 16 test shards per change mount the next commit as a 3-file delta in 16 ms; which files changed between two commits in 13 to 221 ms, identical to `git diff`                                                                                                                     |
| deadlines               | 6 of 7 jobs met their deadlines on a contended farm while a training run yielded its GPUs at a checkpoint and resumed; in a five-week simulation, deadline ordering missed about 44% fewer render deadlines than manual priorities (as plain EDF does), and 51% fewer with burst capacity for about $486 a week |
| provenance              | a frame of a model-assisted denoise traces to the render it read, with the declared model, oversight and consent; changing one field of a stored record breaks its signature                                                                                                                                    |
| agents                  | an MCP client answered all 10 questions of the evaluation set from the live farm, each call in under 10 ms                                                                                                                                                                                                      |

How this compares with Anthropic's Scion, operation by operation, and
where C4 goes further, is in
[REPORT-v2.md](../REPORT-v2.md#comparison-with-scion).

![The queue: every job in run order with its progress, batches and slack, and the burst decision with its price](images/hud-queue.webp)

The HUD's documentation, its walkthroughs (five farms scheduled by the
operator's own policy, from one artist's home lab to a facility of four
rooms on four continents), its screenshots and the synthetic farm that
drives them live in the companion repository c4-hud, which will be
published as the documentation site. The operator embeds and serves the HUD
itself.
