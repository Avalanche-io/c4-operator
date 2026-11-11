# Root IDs

`TestRootIDIsTheDescriptionID` resolves each source here and checks the
resolution's `RootID` against the description's C4 ID that c4 master's
`c4 id -q` reports (for a changeset, for the state it resolves to): equal,
or, for the sources kept as recorded below, still different. Every c4m here was made by a c4 CLI
or the c4m library, never by hand. `root-ids.txt` holds each source's name
and that ID.

| Source                       | Made by                                                                                                  | What it holds                                                                                          |
| ---------------------------- | -------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `view`                       | `view.ScanDir` of the tree below, its records                                                            | the view's ID, resolved from `records/`; `a/` is known only by its ID and its size is null (a link)    |
| `view.c4m`                   | that view's `Marshal`                                                                                    | the same tree as one c4m, `a/`'s size null                                                             |
| `full.c4m`                   | c4 1.0.25 `c4 id tree`                                                                                   | the full description, dates and modes, no nulls                                                        |
| `chain.c4m`                  | c4 master `c4 id tree2`                                                                                  | a scan chain: directory sizes and IDs null, a boundary, a refinement patch                             |
| `nested.c4m`                 | the c4m library                                                                                          | `a/` with a null size and ID, its child listed                                                         |
| `stated-size.c4m`            | the c4m library                                                                                          | `a/` with its size stated (the size the library derives) and its ID null, its child listed             |
| `stale-size.c4m`             | the c4m library                                                                                          | `a/` stating a stale size, 999, over a child that derives 114: the derived size is the description's   |
| `stale-id.c4m`               | the c4m library                                                                                          | `a/` stating every field, its ID stale (another listing's): the derived ID is the description's        |
| `stated-time.c4m`            | the c4m library                                                                                          | `a/` stating its own time, older than its child's: the time is kept, size and ID derived               |
| `dated-child.c4m`            | the c4m library                                                                                          | `a/` null throughout, its child dated: `a/`'s time comes from the child                                |
| `collapsed-stated.c4m`       | the c4m library                                                                                          | `a/` null, holding `d/` known only by its ID (the view's `a/` record) with a stated size               |
| `collapsed-null.c4m`         | the c4m library                                                                                          | the same with `d/`'s size null                                                                         |
| `empty-id.c4m`               | the c4m library                                                                                          | `a/` known by the empty listing's ID, its size null: an empty directory, size 0                        |
| `empty-unknown.c4m`          | the c4m library                                                                                          | `a/` null holding `e/`, which has neither an ID nor children                                           |
| `collapsed-sized-record.c4m` | the c4m library                                                                                          | `a/` known only by its ID, size null, its record (`f.txt`, sized) in `records/`: never read for the ID |
| `seq.c4m`                    | c4 master `c4 id` of three frames and a file, collapsed by the library's `DetectSequences`               | a top-level sequence, hashed as its own line; its ID list is in `records/`                             |
| `changeset-file.c4m`         | the c4m library's changeset over a stored base                                                           | `h.txt` replaced; the expected ID is `c4 id -q` of the state it resolves to                            |
| `changeset-dirline.c4m`      | the same                                                                                                 | `a/`'s line restated with a time, no children                                                          |
| `chain-two.c4m`              | two of the library's changesets over the empty tree, concatenated as gitimport's `ChainFile.Append` does | `h.txt` added, then `k.txt`; the expected ID is `c4 id -q` of the state after both                     |
| `changeset-empty-base.c4m`   | the library's changeset over the empty tree                                                              | `h.txt` added to nothing                                                                               |
| `deepen-collapsed.c4m`       | the same, over the view's root record                                                                    | `a/`, known to the base only by its ID, deepened with a new `g.txt`: pending a format ruling           |

Eleven sources are kept as recorded and differ from `c4 id -q`: `nested`,
`stated-size`, `stale-size`, `stale-id`, `stated-time`, `dated-child`,
`collapsed-stated`, `collapsed-null`, `empty-unknown`, `changeset-file`,
and `changeset-dirline`. Each has a top-level directory line
that is stale, or null where its listed children would derive it. `c4 id -q`
re-derives it; RootID keeps it as stated, because c4's normaliser would
also re-derive a directory a changeset deepens from a partial listing, and
the farm's own changesets restate a changed directory with its new size and
ID over only its changed child. The test holds them in `rootIDAsRecorded`
until c4 rules on partial listings and RootID can be exactly `-q`.
`deepen-collapsed.c4m` has no expected ID (`-` in `root-ids.txt`): when a patch
deepens a directory its base knows only by its ID, the description lists that
directory's children only in part, and whether its ID is derived from the
part, kept, or nulled is a format question awaiting a ruling.

The tree: `a/f.txt` holding `f` and a newline, `a/l`, a link to `f.txt`, and
`h.txt` holding `h` and a newline, every entry's time set to
2026-11-11T00:00 local with `touch -h -t 202611110000`. `tree2` is the same
without the link.
