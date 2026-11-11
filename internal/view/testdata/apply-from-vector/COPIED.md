# A copy, to be replaced

This folder is a byte-for-byte copy of c4's shared conformance vector
`c4m/testdata/apply-from-vector` at c4 commit
5f313dbed7c6ef315da1f7706cdcbf5c9cafae8e (2026-11-09); its own README.md
is c4's. `TestThePatchReadersMeetTheSharedVector` reads it, projected into view
space: `ApplyPatchFrom` on every case, the resolver on the one whose
target is its base, and `TestThePatchReadersAgree` the two against each
other on all of them.

It is kept here only until c4's shared vector also carries the restated-entry
case (an entry restated with another mode and date is a write, never a
removal), which the test builds for itself meanwhile. When it does, read the
vector from the c4 module instead and delete this folder.
