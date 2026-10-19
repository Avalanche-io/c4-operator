# The lane map (a synthetic plan in the lane-map layout, for tests)

## The lanes

| Chapter          | Lane                                     | Spec section | Families (no-site)  | Rules | M2 lane                  |
| ---------------- | ---------------------------------------- | ------------ | ------------------- | ----- | ------------------------ |
| 1 words          | **C1.0** `modules` *(merged `abc1234`)*  | §1           | multi-file loading  | —     | M2-1: after C1.1, C1.2   |
|                  | **C1.1**                                 | §2           | LEX 3, LIT 2        | 5     |                          |
|                  | **C1.2** *(chartered 2026-10-01; docs/plan/charters/M1-C1.2-shapes.md)* | §3 | SHP 9 | 9 (+1 site) | |
| 2 numbers        | **C2.1**                                 | §4           | INT 6               | 6     | M2-2: after C2.3         |
|                  | **C2.2**                                 | §5           | FLT 12              | 12    |                          |
|                  | **C2.3**                                 | §6           | CNV 7, MIX 2        | 9     |                          |
| 3 stores         | **C3.1**                                 | §7           | IDX 4               | 4     | M2-3: after C3.1 … C3.3  |
|                  | **C3.2**                                 | §7           | STO 5               | 5     |                          |
|                  | **C3.3**                                 | §7           | EFF 2               | 2     |                          |
| 4 kernels        | **C4.1**                                 | §8           | KRN 8               | 8     |                          |
|                  | **C4.2**                                 | §8           | ATM 6               | 6     |                          |
|                  |                                          |              | **nine lanes**      | **66**|                          |

**C1.0 is new, ruled on a date, and it lands before C1.2 starts.**

## The five merges

1. **CNV + MIX come after both INT and FLT.** A conversion row needs both.
2. **So C2.3 precedes C4.1.**

## Run order

```
C2.1 → C2.2 → C2.3 → C4.1
     → C3.1 → C4.2
```

with **C1.1, C3.2** as fillers that can run beside any of them.

**Before any of it: M1-F1 `foundation`.** Chartered with its seam ruled. It starts after M1-B2 merges, since the checker is B2's.

**The hard orderings:**

- **C4.1, C4.2 cannot start before C2.2 merges.** A kernel needs floats.
- **C4.2 follows C3 and C4.1**: atomics reason over stores.
- **C3.3 follows C3.2**, and **C4.1 starts when C1.0's entry and C2.1's operators have landed.**
- **C9.9 follows C2.1**: a lane the plan does not list.
- **The cross-cutting lanes run after M1-F1 merges and before the fan-out.**

## Lanes that exist because a lane needed them, and their edges

| Lane | What it covers | Edges naming it |
| ---- | -------------- | --------------- |
| **F1** `foundation` *(merged 2026-10-02)* | the vocabulary sealed | — |
| **Z1** `size-phase` | **DEFERRED TO M2 by the owner's decision.** A big phase. | 3 |
| **A3** `lane-map` *(merged `def5678`)* | a check that was deferred to M2 in part | — |
