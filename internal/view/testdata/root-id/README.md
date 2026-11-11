# Root IDs

`TestRootIDIsTheDescriptionID` resolves each source here and checks that the
resolution's `RootID` is the description's C4 ID: the one c4 master's
`c4 id -q` reports for the file, a stale directory size or ID corrected by
the normaliser as `c4 id` corrects it. Every c4m here was made by a c4 CLI
or the c4m library, never by hand. `root-ids.txt` holds each source's name
and that ID.

| Source                 | Made by                                       | What it holds                                                                                        |
| ---------------------- | --------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `view`                 | `view.ScanDir` of the tree below, its records | the view's ID, resolved from `records/`; `a/` is known only by its ID and its size is null (a link)  |
| `view.c4m`             | that view's `Marshal`                         | the same tree as one c4m, `a/`'s size null                                                           |
| `full.c4m`             | c4 1.0.25 `c4 id tree`                        | the full description, dates and modes, no nulls                                                      |
| `chain.c4m`            | c4 master `c4 id tree2`                       | a scan chain: directory sizes and IDs null, a boundary, a refinement patch                           |
| `nested.c4m`           | the c4m library                               | `a/` with a null size and ID, its child listed                                                       |
| `stated-size.c4m`      | the c4m library                               | `a/` with its size stated (the size the library derives) and its ID null, its child listed           |
| `stale-size.c4m`       | the c4m library                               | `a/` stating a stale size, 999, over a child that derives 114: the derived size is the description's |
| `stale-id.c4m`         | the c4m library                               | `a/` stating every field, its ID stale (another listing's): the derived ID is the description's      |
| `stated-time.c4m`      | the c4m library                               | `a/` stating its own time, older than its child's: the time is kept, size and ID derived             |
| `dated-child.c4m`      | the c4m library                               | `a/` null throughout, its child dated: `a/`'s time comes from the child                              |
| `collapsed-stated.c4m` | the c4m library                               | `a/` null, holding `d/` known only by its ID (the view's `a/` record) with a stated size             |
| `collapsed-null.c4m`   | the c4m library                               | the same with `d/`'s size null                                                                       |

`stated-time.c4m` and `collapsed-null.c4m` are known gaps of the pinned c4m
library, held in the test's `rootIDGaps` until a pin move makes them right
(c4 f13056e and 50ba056).

The tree: `a/f.txt` holding `f` and a newline, `a/l`, a link to `f.txt`, and
`h.txt` holding `h` and a newline, every entry's time set to
2026-11-11T00:00 local with `touch -h -t 202611110000`. `tree2` is the same
without the link.
