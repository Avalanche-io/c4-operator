# Diagnostic bundles

A diagnostic bundle is one file you can attach to a bug report. It holds
the farm's scheduling decisions, redacted, so a maintainer can replay what
the farm decided without learning anything about your work. Nothing is sent
anywhere: you make the file, read it, and choose where it goes.

```sh
c4 farm diagnose --show     # what the bundle holds, and nothing written
c4 farm diagnose            # writes c4-diagnose-<ID>.tgz in this folder
```

Making one takes the farm admin role, or, on a farm made with `c4 farm up`,
its owner.

## What a bundle holds

A bundle is a gzipped tar. Its first entry, `README.txt`, says in plain words
what the bundle holds and never holds, the key epoch, whether redaction kept
the decisions faithful, and the bundle's C4 ID. `--show` prints it, and every
other entry with the fields it holds, before anything is written.

| entry                | what it holds                                                                            |
| -------------------- | ---------------------------------------------------------------------------------------- |
| `manifest.c4m`       | every entry with its size and C4 ID; the bundle's ID is this manifest's                   |
| `build.json`         | versions, commit, operating system and architecture of each component                     |
| `farm.json`          | the farm's shape: pools, projects, sites and links, as pseudonyms with counts             |
| `snapshots/NNN.json` | the scheduler's input at each decision kept, oldest first                                 |
| `events.jsonl`       | the transition log's closed fields: sequence, relative time, kind, and pseudonyms         |
| `counters.jsonl`     | the farm's counters over the same window                                                  |
| `redaction.json`     | the rules applied, counts of pseudonyms by kind, and the faithfulness result per snapshot |

## What a bundle never holds

- **No names.** Projects, shots, jobs, people, hosts, pools and sites appear
  as pseudonyms made under your farm's own key. Without that key they cannot
  be turned back into names. A farm admin can rotate the key, after which
  later pseudonyms do not link to earlier ones.
- **No paths, file names or contents.** No scene, image, log line, command
  line, environment value or parameter value is copied in.
- **No clock times.** Every time is relative, within the bundle's window.

If any step of making a bundle fails, no bundle is written: never a partly
redacted one. A farm that refuses to make one says why, in plain words, and
writes nothing.

## The window

`--window 30m` keeps the decisions within 30 minutes of the latest. By
default a bundle holds what the farm keeps. A farm keeps a ring of its recent
decisions only when its manager runs with `--telemetry-ring`; without it, a
bundle holds the present moment alone, and the bundle line says so.

## Sending nothing, guaranteed

This release sends no telemetry at all: a bundle is a file you make and move
yourself. To guarantee the same of any later release, set `C4_TELEMETRY=off`
in the manager's environment. It holds every telemetry setting off, whatever
else is configured, and a bundle can still be made by hand.
