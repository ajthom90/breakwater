
---

## Reviewer correction and verification (fix round, 2026-07-30)

### S5-F1's root cause as published above was WRONG — corrected here

My original diagnosis ("agent and server chunk splitting diverge for ~15% of
payloads", "PLAN's bit-identical invariant is false") was **incorrect**. The
implementer pushed back with a better explanation and it is right.

**Actual root cause:** kopia's `contentIDTracker` stores content IDs in a
`map[content.ID]bool` (`repo/object/object_writer.go:36-50`), so
`repo.VerifyObject` returns an **unordered set** with Go's randomized map
iteration order. `TestS3F8` compared `pkg`'s stream-ordered sequence against that
unordered set, so it failed whenever the random iteration happened to differ at
index 0. The CDC boundaries were bit-identical all along.

**Verified decisively:** re-running my own seeded probe against the *unmodified*
commit `9ab2831`, all six previously-"diverging" seeds (11, 16, 26, 28, 36, 39)
matched in **both sorted-set and sequence** comparison on that run — the same
seeds, same code, different result across runs. Same-input nondeterminism proves
the ordering hypothesis and refutes mine. Evidence I had already collected should
have pointed here: identical chunk *counts* with a differing element 0 is the
signature of a permuted set, and I wrote it off as coincidence instead of testing
set equality. That was the analytical error.

**What my finding did get right:** the test was genuinely broken, the suite was
not reliably green, and the seeded reproducer isolated the exact inputs — which
is what made the real root cause findable. S5-F2 (unreproducible "all green"
claim) stands unchanged.

### Fix verification (`aee9a10`)

- New `Vault.ObjectDataContentIDs` returns **stream order** by walking the
  indirect index (direct single-content objects short-circuit).
- `TestS3F8` plus the new `TestS5F1_SeededSplitterSequenceIdentity` (seeds 1–40,
  including all six) assert exact sequence equality **and** that no chunk exceeds
  `MaxPutContentBytes`; `TestS5F1_VerifyObjectOrderIsNotStreamOrder` pins the
  map-order property itself so the trap cannot be re-entered.
- Splitter and `ChunkAndID` code is **unchanged** — correctly, since it was never
  at fault.
- **Mutation check:** reverting the new tests to use `VerifyObject` instead of
  `ObjectDataContentIDs` makes both fail — the tests genuinely depend on stream
  ordering rather than passing vacuously.
- Contract tests green at `-count=10`; full server suite green under `-race`.

**S5-F1 and S5-F2 closed.** The `PLAN` "bit-identical IDs" invariant holds and is
now locked by a deterministic test.

### Standing lesson for later milestones

`repo.VerifyObject` returns an unordered set. Any future code that needs chunk
order (restore verification, replication re-chunking, image manifests) must use
`ObjectDataContentIDs`, never `VerifyObject`. The prune mark phase is unaffected
— it only needs set membership.
