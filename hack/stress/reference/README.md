# Scheduler stress grid

`go run ./hack/stress -quick` ran 39 grid points (24 Latin hypercube samples over 34 dimensions, plus 15 corners), 381 runs in all, in 4s with 4 workers. Every run calls the production policy (`sched.Decide`) or a baseline through `sched.Policy` on a simulated farm; the invariants below are checked on every decision of `objective` and `objective+burst`.

0 violations of invariants without a known cause, 88628 of known bugs.

## Invariants

| Invariant | Holds when | Points violating | Status |
|---|---|---|---|
| `starvation` | a job with ready work and nothing running for 30 minutes gets the next free slot it may use | 0 of 39 | holds |
| `starvation-over-deadline` | the starvation guard does not give the last free slot of a pool to a job with more than PreemptSlack of slack while a job on time in the plan needs it | 25 of 39 | **fails: known cause** |
| `work-conservation` | no slot is left idle while ready work that may use it waits, unless a gang holds it, a borrowing limit forbids it, or it is held for another project's guaranteed deadline need | 0 of 39 | holds |
| `blocked-rank` | a job whose units all wait on upstream work is never ranked ahead of a job with ready work | 30 of 39 | **fails: known cause** |
| `blocked-dispatch` | only runnable units are dispatched | 0 of 39 | holds |
| `overfill` | no pool is given more units than it has free slots | 0 of 39 | holds |
| `hold` | a held job gets no units; a hold-stopped job's running units are all stopped | 0 of 39 | holds |
| `borrow-limit` | no project goes past its nominal quota plus its borrowing limit | 0 of 39 | holds |
| `boost-quota` | a boost takes slots only within its own project's quota or from capacity no other project is guaranteed | 0 of 39 | holds |
| `lateness-unpriced` | a job planned late has LateBy > 0, so its lateness has a cost | 26 of 39 | **fails: known cause** |
| `eta-inconsistent` | a job at risk has an ETA after its deadline; a job on track has one before it | 26 of 39 | **fails: known cause** |
| `past-due-on-track` | an unfinished job past its deadline is reported at risk | 1 of 39 | **fails: known cause** |
| `burst-uneconomic` | an automatic burst closes the deadline and costs less than the lateness it prevents | 0 of 39 | holds |
| `burst-budget` | automatic bursts stay within each project's remaining daily budget | 0 of 39 | holds |
| `burst-unjustified` | a burst pool's request grows only by automatically approved slots, and never past its limit | 0 of 39 | holds |
| `churn-unit` | a unit is preempted at most MaxPreemptionsPerUnit times | 0 of 39 | holds |
| `churn-pool` | a pool sees at most PreemptPerWindow preemptions per PreemptWindow | 0 of 39 | holds |
| `churn-pingpong` | two jobs never take slots back and forth within an hour | 17 of 39 | **fails: known cause** |
| `determinism` | the same point and seed give the same decisions | 0 of 39 | holds |
| `metamorphic-capacity` | more owned capacity does not lower on-time completions beyond noise | 1 of 39 | **fails: known cause** |
| `metamorphic-deadline` | looser deadlines do not lower on-time completions beyond noise | 0 of 39 | holds |
| `metamorphic-budget` | a bigger burst budget does not lower on-time completions beyond noise | 0 of 39 | holds |
| `decide-latency` | one decision takes under a second at every farm size in the grid | 0 of 39 | holds |
| `dispatch-invalid` | dispatches name a known unit and a pool the job may use | 0 of 39 | holds |

The prediction scores (AtRisk against actual lateness) and Decide's cost by farm size are measured, not asserted; see below.

## Findings

Each failing invariant with up to five points, one example each, and the command that reruns the point (it prints every violation of the run). Add `-variant <name>` for metamorphic findings.

### `starvation-over-deadline`

Known bug: starvation() in decide.go runs before the deadline tier and releases a unit to any job idle for StarvationAfter, whatever its slack; on a small farm that unit takes a slot a job due within the hour needs, and once it has run MaxLostWork no preemption may take it back, so a long unit holds the slot for hours. Suggested fix: let the guard take only slots the deadline tier does not need (run it after need() for jobs with slack above PreemptSlack), or make guard-released units preemptible regardless of MaxLostWork (found by this grid).

- g001, day 0 22:48, `objective+burst`: p0-00058 guard gave the last slot of cpu to p0-00058 (slack 94h0m0s) while p0-00037, due Tue 03:58, holds 0 of the 0.9 slots it needs (3 times in the run)
  `go run ./hack/stress -point 'nodes=9,projects=4,quotaFill=0.974,quotaSkew=1.59,borrow=0.803,provisioning=19.6,burstCost=0.958,burstBudget=0.87,interruptions=0.0607,lateCost=583,load=1.1,days=0.484,arrivals=3,tightness=11.9,slackSpread=0.936,noDue=0.218,unitMin=2.09,spread=0.155,heavy=0.09,heavyMult=2.01,depth=4,width=2,expand=0.412,estErr=0.256,mlShare=0.621,ranks=1,ckptMin=51.8,failRate=0.0994,nodeFaults=0.295,boostRate=0.167,holdRate=0.18,moveRate=0.171,drain=0.5' -seed 1`
- g002, day 0 20:47, `objective+burst`: p1-00012 guard gave the last slot of cpu to p1-00012 (slack 38h0m0s) while p2-00033, due Mon 23:46, holds 0 of the 0.6 slots it needs (11 times in the run)
  `go run ./hack/stress -point 'nodes=3,projects=3,quotaSkew=1.9,borrow=-0.399,burstFrac=0.352,provisioning=20.9,burstCost=2.52,burstBudget=0.802,interruptions=0.0425,lateCost=358,load=1.71,days=0.301,arrivals=1,tightness=2.83,slackSpread=0.533,noDue=0.236,unitMin=10.3,spread=1.12,heavyMult=5.4,depth=4,width=3,expand=0.257,estErr=0.0261,mlShare=0.199,ranks=2,ckptMin=39.7,failRate=0.0667,nodeFaults=0.374,boostRate=0.141,holdRate=0.218,moveRate=0.136,drain=0.5' -seed 1`
- g003, day 0 23:52, `objective+burst`: p2-00031 guard gave the last slot of gpu-burst to p2-00031 (slack 100000h0m0s) while p3-00027, due Tue 00:17, holds 0 of the 4.1 slots it needs (1 times in the run)
  `go run ./hack/stress -point 'nodes=12,gpuFrac=0.0558,projects=9,quotaFill=0.719,quotaSkew=0.221,borrow=0.705,burstFrac=0.601,provisioning=1.7,burstCost=2.15,burstBudget=0.408,interruptions=0.281,lateCost=67.9,load=0.608,days=0.352,arrivals=1,tightness=0.502,slackSpread=1.12,noDue=0.193,unitMin=18.1,spread=0.188,heavyMult=8.21,depth=4,width=1,expand=0.119,estErr=0.22,mlShare=0.0892,ranks=4,ckptMin=18.3,failRate=0.0709,nodeFaults=0.452,boostRate=0.114,holdRate=0.236,moveRate=0.0305,drain=0.5' -seed 1`
- g004, day 0 15:50, `objective+burst`: p1-00010 guard gave the last slot of cpu to p1-00010 (slack 2h0m0s) while p1-00010, due Mon 18:35, holds 1 of the 1.1 slots it needs (1 times in the run)
  `go run ./hack/stress -point 'nodes=10,gpuFrac=0.0417,projects=10,quotaFill=0.831,quotaSkew=1.28,borrow=-0.248,burstFrac=0.271,provisioning=9.58,burstCost=1.98,burstBudget=0.336,interruptions=0.193,lateCost=17.5,load=0.925,days=0.38,arrivals=2,tightness=1.29,slackSpread=0.105,noDue=0.0577,unitMin=5.42,spread=0.379,heavy=0.374,heavyMult=4.9,depth=4,width=1,expand=0.48,estErr=0.0973,mlShare=0.229,ranks=2,ckptMin=14.4,nodeFaults=0.17,boostRate=0.0287,holdRate=0.244,moveRate=0.24,drain=0.5' -seed 1`
- g005, day 0 19:32, `objective+burst`: p7-00070 guard gave the last slot of gpu to p7-00070 (slack 15h0m0s) while p5-00031, due Mon 22:50, holds 0 of the 2.8 slots it needs (23 times in the run)
  `go run ./hack/stress -point 'nodes=16,gpuFrac=0.234,projects=10,quotaFill=0.924,quotaSkew=1.54,borrow=-0.137,burstFrac=0.309,provisioning=29.5,burstCost=2.63,burstBudget=0.531,interruptions=0.179,lateCost=230,load=1.19,days=0.471,arrivals=1,tightness=9.53,slackSpread=0.873,noDue=0.075,unitMin=6.68,spread=0.641,heavy=0.339,heavyMult=6.65,depth=1,width=2,expand=0.351,estErr=0.556,mlShare=0.673,ranks=5,ckptMin=9.75,nodeFaults=0.0743,boostRate=0.0793,moveRate=0.285,drain=0.5' -seed 1`
- and 41 more in `violations.csv`

### `blocked-rank`

Known bug: rank() in decide.go sorts jobs with R>0 by (running+incoming)/R; a job whose units all wait upstream has ratio 0 and sorts first (found by hud-scenarios; the fix is on the scenarios branch).

- g001, day 0 04:37, `objective+burst`: p2-00053 blocked, ranked 1, ahead of p2-00050 with ready work at rank 4 (4109 times in the run)
  `go run ./hack/stress -point 'nodes=9,projects=4,quotaFill=0.974,quotaSkew=1.59,borrow=0.803,provisioning=19.6,burstCost=0.958,burstBudget=0.87,interruptions=0.0607,lateCost=583,load=1.1,days=0.484,arrivals=3,tightness=11.9,slackSpread=0.936,noDue=0.218,unitMin=2.09,spread=0.155,heavy=0.09,heavyMult=2.01,depth=4,width=2,expand=0.412,estErr=0.256,mlShare=0.621,ranks=1,ckptMin=51.8,failRate=0.0994,nodeFaults=0.295,boostRate=0.167,holdRate=0.18,moveRate=0.171,drain=0.5' -seed 1`
- g002, day 0 19:55, `objective+burst`: p1-00014 blocked, ranked 3, ahead of p1-00011 with ready work at rank 9 (1050 times in the run)
  `go run ./hack/stress -point 'nodes=3,projects=3,quotaSkew=1.9,borrow=-0.399,burstFrac=0.352,provisioning=20.9,burstCost=2.52,burstBudget=0.802,interruptions=0.0425,lateCost=358,load=1.71,days=0.301,arrivals=1,tightness=2.83,slackSpread=0.533,noDue=0.236,unitMin=10.3,spread=1.12,heavyMult=5.4,depth=4,width=3,expand=0.257,estErr=0.0261,mlShare=0.199,ranks=2,ckptMin=39.7,failRate=0.0667,nodeFaults=0.374,boostRate=0.141,holdRate=0.218,moveRate=0.136,drain=0.5' -seed 1`
- g003, day 0 17:32, `objective+burst`: p1-00014 blocked, ranked 1, ahead of p1-00028 with ready work at rank 2 (662 times in the run)
  `go run ./hack/stress -point 'nodes=12,gpuFrac=0.0558,projects=9,quotaFill=0.719,quotaSkew=0.221,borrow=0.705,burstFrac=0.601,provisioning=1.7,burstCost=2.15,burstBudget=0.408,interruptions=0.281,lateCost=67.9,load=0.608,days=0.352,arrivals=1,tightness=0.502,slackSpread=1.12,noDue=0.193,unitMin=18.1,spread=0.188,heavyMult=8.21,depth=4,width=1,expand=0.119,estErr=0.22,mlShare=0.0892,ranks=4,ckptMin=18.3,failRate=0.0709,nodeFaults=0.452,boostRate=0.114,holdRate=0.236,moveRate=0.0305,drain=0.5' -seed 1`
- g004, day 0 04:52, `objective+burst`: p3-00007 blocked, ranked 1, ahead of p3-00006 with ready work at rank 3 (1069 times in the run)
  `go run ./hack/stress -point 'nodes=10,gpuFrac=0.0417,projects=10,quotaFill=0.831,quotaSkew=1.28,borrow=-0.248,burstFrac=0.271,provisioning=9.58,burstCost=1.98,burstBudget=0.336,interruptions=0.193,lateCost=17.5,load=0.925,days=0.38,arrivals=2,tightness=1.29,slackSpread=0.105,noDue=0.0577,unitMin=5.42,spread=0.379,heavy=0.374,heavyMult=4.9,depth=4,width=1,expand=0.48,estErr=0.0973,mlShare=0.229,ranks=2,ckptMin=14.4,nodeFaults=0.17,boostRate=0.0287,holdRate=0.244,moveRate=0.24,drain=0.5' -seed 1`
- g006, day 0 00:44, `objective+burst`: p1-00003 blocked, ranked 2, ahead of p1-00001 with ready work at rank 3 (1143 times in the run)
  `go run ./hack/stress -point 'nodes=4,projects=6,quotaFill=0.663,quotaSkew=1.83,borrow=0.356,burstFrac=0.175,provisioning=6.31,burstCost=0.719,burstBudget=0.638,interruptions=0.0651,lateCost=284,load=1.79,days=0.46,arrivals=2,tightness=5.7,slackSpread=0.402,noDue=0.172,unitMin=27.1,spread=1.27,heavy=0.0685,heavyMult=7.93,depth=2,width=2,expand=0.473,estErr=0.342,mlShare=0.418,ranks=8,ckptMin=48.4,failRate=0.0295,nodeFaults=0.291,boostRate=0.235,holdRate=0.0391,moveRate=0.123,drain=0.5' -seed 1`
- and 55 more in `violations.csv`

### `lateness-unpriced`

Known bug: planFluid in plan.go: a job Moore-Hodgson evicts from the on-time set takes its lateness from its own EDF position, which can end before its deadline, so lateBy is 0, the lateness it would cost is $0, and auto-burst never fires for it (found by hud-scenarios; the fix is on the scenarios branch).

- g000, day 0 02:49, `objective+burst`: p6-00001 at risk with LateBy 0 (lateness priced at $0 against 9/h); p6-00001 will miss Mon 05:57 by about 5m at current capacity. No burst closes it: no burst capacity is left. (102 times in the run)
  `go run ./hack/stress -point 'nodes=2,gpuFrac=0.2,projects=11,quotaFill=0.894,quotaSkew=0.36,borrow=0.608,burstFrac=0.93,provisioning=11.8,burstCost=0.347,burstBudget=0.935,lateCost=13.1,load=0.841,days=0.277,arrivals=2,tightness=4.31,slackSpread=0.698,unitMin=84.7,spread=0.745,heavy=0.272,heavyMult=8.66,depth=1,width=2,estErr=0.585,mlShare=0.318,ranks=2,ckptMin=58.1,failRate=0.0363,boostRate=0.295,holdRate=0.0516,moveRate=0.298,drain=0.5' -seed 1`
- g002, day 0 22:06, `objective+burst`: p2-00033 at risk with LateBy 0 (lateness priced at $0 against 394/h); p2-00033 will miss Mon 23:46 by about 5m at current capacity. No burst closes it: no burst capacity is left. (208 times in the run)
  `go run ./hack/stress -point 'nodes=3,projects=3,quotaSkew=1.9,borrow=-0.399,burstFrac=0.352,provisioning=20.9,burstCost=2.52,burstBudget=0.802,interruptions=0.0425,lateCost=358,load=1.71,days=0.301,arrivals=1,tightness=2.83,slackSpread=0.533,noDue=0.236,unitMin=10.3,spread=1.12,heavyMult=5.4,depth=4,width=3,expand=0.257,estErr=0.0261,mlShare=0.199,ranks=2,ckptMin=39.7,failRate=0.0667,nodeFaults=0.374,boostRate=0.141,holdRate=0.218,moveRate=0.136,drain=0.5' -seed 1`
- g003, day 0 18:33, `objective+burst`: p4-00005 at risk with LateBy 0 (lateness priced at $0 against 90/h); p4-00005 will miss Mon 18:52 by about 5m at current capacity. No burst closes it: no burst capacity is left. (52 times in the run)
  `go run ./hack/stress -point 'nodes=12,gpuFrac=0.0558,projects=9,quotaFill=0.719,quotaSkew=0.221,borrow=0.705,burstFrac=0.601,provisioning=1.7,burstCost=2.15,burstBudget=0.408,interruptions=0.281,lateCost=67.9,load=0.608,days=0.352,arrivals=1,tightness=0.502,slackSpread=1.12,noDue=0.193,unitMin=18.1,spread=0.188,heavyMult=8.21,depth=4,width=1,expand=0.119,estErr=0.22,mlShare=0.0892,ranks=4,ckptMin=18.3,failRate=0.0709,nodeFaults=0.452,boostRate=0.114,holdRate=0.236,moveRate=0.0305,drain=0.5' -seed 1`
- g004, day 0 04:43, `objective+burst`: p3-00005 at risk with LateBy 0 (lateness priced at $0 against 13/h); p3-00005 will miss Mon 04:51 by about 5m at current capacity. No burst closes it: no burst capacity is left. (2 times in the run)
  `go run ./hack/stress -point 'nodes=10,gpuFrac=0.0417,projects=10,quotaFill=0.831,quotaSkew=1.28,borrow=-0.248,burstFrac=0.271,provisioning=9.58,burstCost=1.98,burstBudget=0.336,interruptions=0.193,lateCost=17.5,load=0.925,days=0.38,arrivals=2,tightness=1.29,slackSpread=0.105,noDue=0.0577,unitMin=5.42,spread=0.379,heavy=0.374,heavyMult=4.9,depth=4,width=1,expand=0.48,estErr=0.0973,mlShare=0.229,ranks=2,ckptMin=14.4,nodeFaults=0.17,boostRate=0.0287,holdRate=0.244,moveRate=0.24,drain=0.5' -seed 1`
- g005, day 0 19:04, `objective+burst`: p1-00055 at risk with LateBy 0 (lateness priced at $0 against 233/h); p1-00055 will miss Mon 22:22 by about 5m at current capacity. No burst closes it: no burst capacity is left. (610 times in the run)
  `go run ./hack/stress -point 'nodes=16,gpuFrac=0.234,projects=10,quotaFill=0.924,quotaSkew=1.54,borrow=-0.137,burstFrac=0.309,provisioning=29.5,burstCost=2.63,burstBudget=0.531,interruptions=0.179,lateCost=230,load=1.19,days=0.471,arrivals=1,tightness=9.53,slackSpread=0.873,noDue=0.075,unitMin=6.68,spread=0.641,heavy=0.339,heavyMult=6.65,depth=1,width=2,expand=0.351,estErr=0.556,mlShare=0.673,ranks=5,ckptMin=9.75,nodeFaults=0.0743,boostRate=0.0793,moveRate=0.285,drain=0.5' -seed 1`
- and 46 more in `violations.csv`

### `eta-inconsistent`

Known bug: the same cause as lateness-unpriced: an evicted job's ETA90 is its own EDF position, before its deadline, while AtRisk is true.

- g000, day 0 02:49, `objective+burst`: p6-00001 at risk but ETA90 Mon 05:01 is before its deadline Mon 05:57 (102 times in the run)
  `go run ./hack/stress -point 'nodes=2,gpuFrac=0.2,projects=11,quotaFill=0.894,quotaSkew=0.36,borrow=0.608,burstFrac=0.93,provisioning=11.8,burstCost=0.347,burstBudget=0.935,lateCost=13.1,load=0.841,days=0.277,arrivals=2,tightness=4.31,slackSpread=0.698,unitMin=84.7,spread=0.745,heavy=0.272,heavyMult=8.66,depth=1,width=2,estErr=0.585,mlShare=0.318,ranks=2,ckptMin=58.1,failRate=0.0363,boostRate=0.295,holdRate=0.0516,moveRate=0.298,drain=0.5' -seed 1`
- g002, day 0 22:06, `objective+burst`: p2-00033 at risk but ETA90 Mon 23:17 is before its deadline Mon 23:46 (208 times in the run)
  `go run ./hack/stress -point 'nodes=3,projects=3,quotaSkew=1.9,borrow=-0.399,burstFrac=0.352,provisioning=20.9,burstCost=2.52,burstBudget=0.802,interruptions=0.0425,lateCost=358,load=1.71,days=0.301,arrivals=1,tightness=2.83,slackSpread=0.533,noDue=0.236,unitMin=10.3,spread=1.12,heavyMult=5.4,depth=4,width=3,expand=0.257,estErr=0.0261,mlShare=0.199,ranks=2,ckptMin=39.7,failRate=0.0667,nodeFaults=0.374,boostRate=0.141,holdRate=0.218,moveRate=0.136,drain=0.5' -seed 1`
- g003, day 0 18:33, `objective+burst`: p4-00005 at risk but ETA90 Mon 18:48 is before its deadline Mon 18:52 (50 times in the run)
  `go run ./hack/stress -point 'nodes=12,gpuFrac=0.0558,projects=9,quotaFill=0.719,quotaSkew=0.221,borrow=0.705,burstFrac=0.601,provisioning=1.7,burstCost=2.15,burstBudget=0.408,interruptions=0.281,lateCost=67.9,load=0.608,days=0.352,arrivals=1,tightness=0.502,slackSpread=1.12,noDue=0.193,unitMin=18.1,spread=0.188,heavyMult=8.21,depth=4,width=1,expand=0.119,estErr=0.22,mlShare=0.0892,ranks=4,ckptMin=18.3,failRate=0.0709,nodeFaults=0.452,boostRate=0.114,holdRate=0.236,moveRate=0.0305,drain=0.5' -seed 1`
- g004, day 0 04:43, `objective+burst`: p3-00005 at risk but ETA90 Mon 04:49 is before its deadline Mon 04:51 (2 times in the run)
  `go run ./hack/stress -point 'nodes=10,gpuFrac=0.0417,projects=10,quotaFill=0.831,quotaSkew=1.28,borrow=-0.248,burstFrac=0.271,provisioning=9.58,burstCost=1.98,burstBudget=0.336,interruptions=0.193,lateCost=17.5,load=0.925,days=0.38,arrivals=2,tightness=1.29,slackSpread=0.105,noDue=0.0577,unitMin=5.42,spread=0.379,heavy=0.374,heavyMult=4.9,depth=4,width=1,expand=0.48,estErr=0.0973,mlShare=0.229,ranks=2,ckptMin=14.4,nodeFaults=0.17,boostRate=0.0287,holdRate=0.244,moveRate=0.24,drain=0.5' -seed 1`
- g005, day 0 19:04, `objective+burst`: p1-00055 at risk but ETA90 Mon 21:37 is before its deadline Mon 22:22 (610 times in the run)
  `go run ./hack/stress -point 'nodes=16,gpuFrac=0.234,projects=10,quotaFill=0.924,quotaSkew=1.54,borrow=-0.137,burstFrac=0.309,provisioning=29.5,burstCost=2.63,burstBudget=0.531,interruptions=0.179,lateCost=230,load=1.19,days=0.471,arrivals=1,tightness=9.53,slackSpread=0.873,noDue=0.075,unitMin=6.68,spread=0.641,heavy=0.339,heavyMult=6.65,depth=1,width=2,expand=0.351,estErr=0.556,mlShare=0.673,ranks=5,ckptMin=9.75,nodeFaults=0.0743,boostRate=0.0793,moveRate=0.285,drain=0.5' -seed 1`
- and 46 more in `violations.csv`

### `past-due-on-track`

Known bug: groups() in plan.go plans only jobs with at least one pool in the snapshot; a job none of whose pools exists gets no plan, so AtRisk stays false even past its deadline, LateBy is 0 and no burst is sized for it. Suggested fix: mark such deadline jobs at risk (planOK false, lateBy the time to the deadline's end of remaining work at zero capacity, or past it) (found by this grid).

- empty-farm, day 0 20:17, `objective+burst`: p2-00023 past its deadline Mon 20:16 and reported on track (914 times in the run)
  `go run ./hack/stress -point 'gpuFrac=0.15,projects=3,quotaFill=0.8,quotaSkew=0.5,borrow=-1,provisioning=6,burstCost=1,burstBudget=0.3,lateCost=400,load=1,days=0.3,arrivals=1,tightness=2,slackSpread=0.4,noDue=0.1,unitMin=15,spread=0.5,heavy=0.3,heavyMult=4,depth=2,width=2,expand=0.3,estErr=0.2,mlShare=0.4,ranks=4,ckptMin=15,failRate=0.02,drain=0.5' -seed 1`
- and 1 more in `violations.csv`

### `churn-pingpong`

Known bug: take() in preempt.go checks aboveShare (running - outgoing > share) and then removes a whole unit, so a victim holding ceil(share) slots drops below its fractional share; the deadline tier rounds R up (needSlots), so a job released to ceil(share) is at once a victim, and two jobs whose shares are each under a slot take one slot back and forth on every decision, losing the work each time. Suggested fix: take a unit only if running - outgoing - its slots >= ceil(share - 1e-9) (found by this grid).

- g001, day 1 05:22, `objective+burst`: p0-00057 lost a unit to p3-00003, which lost one to it 23m0s earlier (23 times in the run)
  `go run ./hack/stress -point 'nodes=9,projects=4,quotaFill=0.974,quotaSkew=1.59,borrow=0.803,provisioning=19.6,burstCost=0.958,burstBudget=0.87,interruptions=0.0607,lateCost=583,load=1.1,days=0.484,arrivals=3,tightness=11.9,slackSpread=0.936,noDue=0.218,unitMin=2.09,spread=0.155,heavy=0.09,heavyMult=2.01,depth=4,width=2,expand=0.412,estErr=0.256,mlShare=0.621,ranks=1,ckptMin=51.8,failRate=0.0994,nodeFaults=0.295,boostRate=0.167,holdRate=0.18,moveRate=0.171,drain=0.5' -seed 1`
- g002, day 0 23:17, `objective+burst`: p2-00036 lost a unit to p0-00022, which lost one to it 1m0s earlier (1 times in the run)
  `go run ./hack/stress -point 'nodes=3,projects=3,quotaSkew=1.9,borrow=-0.399,burstFrac=0.352,provisioning=20.9,burstCost=2.52,burstBudget=0.802,interruptions=0.0425,lateCost=358,load=1.71,days=0.301,arrivals=1,tightness=2.83,slackSpread=0.533,noDue=0.236,unitMin=10.3,spread=1.12,heavyMult=5.4,depth=4,width=3,expand=0.257,estErr=0.0261,mlShare=0.199,ranks=2,ckptMin=39.7,failRate=0.0667,nodeFaults=0.374,boostRate=0.141,holdRate=0.218,moveRate=0.136,drain=0.5' -seed 1`
- g010, day 0 21:56, `objective+burst`: p2-00014 lost a unit to p1-00010, which lost one to it 39m0s earlier (6 times in the run)
  `go run ./hack/stress -point 'nodes=5,gpuFrac=0.151,projects=7,quotaFill=0.581,quotaSkew=1.73,borrow=0.194,burstFrac=0.25,provisioning=18.8,burstCost=1.66,burstBudget=0.163,interruptions=0.299,lateCost=41,load=1.76,days=0.414,arrivals=1,tightness=1.75,slackSpread=0.238,noDue=0.047,unitMin=4.85,spread=0.97,heavy=0.16,heavyMult=9.31,depth=1,width=3,expand=0.515,estErr=0.378,mlShare=0.442,ranks=6,ckptMin=30.4,failRate=0.0783,nodeFaults=0.0956,boostRate=0.125,holdRate=0.0693,moveRate=0.144,drain=0.5' -seed 1`
- g013, day 0 06:56, `objective+burst`: p0-00016 lost a unit to p0-00017, which lost one to it 0s earlier (2 times in the run)
  `go run ./hack/stress -point 'nodes=2,gpuFrac=0.256,projects=7,quotaFill=0.835,quotaSkew=1.22,borrow=0.502,burstFrac=0.567,provisioning=27.5,burstCost=0.855,interruptions=0.134,lateCost=208,load=0.37,days=0.303,tightness=5.3,slackSpread=0.258,noDue=0.127,unitMin=2.39,spread=0.84,heavy=0.547,heavyMult=4.43,depth=3,width=2,expand=0.331,estErr=0.0156,mlShare=0.109,ranks=5,ckptMin=11.9,failRate=0.00934,nodeFaults=0.246,boostRate=0.0886,holdRate=0.292,moveRate=0.258,drain=0.5' -seed 1`
- g016, day 0 02:09, `objective+burst`: p2-00031 lost a unit to p1-00043, which lost one to it 16m0s earlier (19 times in the run)
  `go run ./hack/stress -point 'nodes=17,gpuFrac=0.112,projects=3,quotaFill=0.381,quotaSkew=1.39,borrow=0.277,burstFrac=1,provisioning=7.2,burstCost=0.669,burstBudget=0.734,interruptions=0.121,lateCost=1450,load=0.742,days=0.498,tightness=7.38,slackSpread=0.965,noDue=0.212,unitMin=9.09,spread=0.405,heavy=0.176,heavyMult=7.14,depth=2,width=3,expand=0.535,estErr=0.117,mlShare=0.737,ranks=5,ckptMin=35,failRate=0.0569,nodeFaults=0.378,boostRate=0.0393,holdRate=0.273,moveRate=0.231,drain=0.5' -seed 1`
- and 28 more in `violations.csv`

### `metamorphic-capacity`

- g020, day 0 00:00, `objective+burst`:  on time 2 of 18 with the capacity variant, 6 in the base (tolerance 2); repro with -variant capacity; 19 of the variant's approval requests priced lateness at $0 (base 0): caused by lateness-unpriced
  `go run ./hack/stress -point 'nodes=1,gpuFrac=0.0734,projects=1,quotaFill=0.372,quotaSkew=1.98,borrow=0.679,burstFrac=0.9,provisioning=25.3,burstCost=1.05,burstBudget=0.775,interruptions=0.242,lateCost=506,load=1.95,days=0.373,tightness=0.591,slackSpread=0.801,noDue=0.118,unitMin=4.03,spread=0.578,heavy=0.555,heavyMult=6.95,depth=2,width=2,expand=0.242,estErr=0.244,mlShare=0.71,ranks=3,ckptMin=23.1,failRate=0.0479,nodeFaults=0.488,boostRate=0.219,holdRate=0.129,moveRate=0.0436,drain=0.5' -seed 1`

## Policy against baselines

The same workloads under each policy. `objective` is `sched.Decide` without burst requests, `objective+burst` with them; the baselines never burst. Wins and losses count points where `objective+burst` finished more jobs on time than the baseline, or fewer, by more than 2% of the point's deadline jobs. Baselines ignore quotas, so on points with tight borrowing limits they use capacity the policy is not allowed to lend.

| Policy | Mean on time | Median on time | Late hours | Lateness cost | Burst spend | Owned use | Preemptions | objective+burst wins / ties / losses |
|---|---|---|---|---|---|---|---|---|
| `objective+burst` | 73.9% | 93.1% | 1313 | $452277 | $1577 | 34% | 1476 | - |
| `objective` | 68.6% | 91.6% | 1752 | $600302 | $0 | 35% | 1300 | 5 / 34 / 0 |
| `edf` | 74.2% | 95.0% | 1244 | $459716 | $0 | 40% | 30 | 5 / 30 / 4 |
| `fifo` | 67.1% | 84.6% | 1914 | $779464 | $0 | 39% | 23 | 11 / 27 / 1 |
| `fair-share` | 69.1% | 88.5% | 1633 | $697289 | $0 | 39% | 21 | 9 / 27 / 3 |
| `static-priority` | 70.1% | 83.9% | 1855 | $884961 | $0 | 39% | 23 | 10 / 24 / 5 |

On-time points of `objective+burst` minus the best baseline at each point, mean by region (positive: the policy is ahead):

| Load \ tightness | 0.5-1 | 1-2 | 2-4 | 4-8 | 8-12 |
|---|---|---|---|---|---|
| 0.3-0.65 | +9.1 (1) | +0.0 (1) | -2.5 (2) | -3.8 (1) | |
| 0.65-1 | -60.0 (1) | +0.0 (1) | | +0.0 (2) | +0.0 (1) |
| 1-1.4 | | -1.5 (2) | +0.0 (1) | | +0.0 (2) |
| 1.4-1.7 | -9.1 (1) | | +1.7 (2) | +0.0 (1) | |
| 1.7-2 | +21.8 (2) | -8.8 (1) | -66.7 (1) | +0.0 (1) | |

Points where `objective+burst` trails the best baseline by more than 10 points:

- g002 (3 nodes, load 1.7, tight 2.8, dailies): 33.3% on time against 100.0% for `edf`. `go run ./hack/stress -point 'nodes=3,projects=3,quotaSkew=1.9,borrow=-0.399,burstFrac=0.352,provisioning=20.9,burstCost=2.52,burstBudget=0.802,interruptions=0.0425,lateCost=358,load=1.71,days=0.301,arrivals=1,tightness=2.83,slackSpread=0.533,noDue=0.236,unitMin=10.3,spread=1.12,heavyMult=5.4,depth=4,width=3,expand=0.257,estErr=0.0261,mlShare=0.199,ranks=2,ckptMin=39.7,failRate=0.0667,nodeFaults=0.374,boostRate=0.141,holdRate=0.218,moveRate=0.136,drain=0.5' -seed 1`
- g015 (2 nodes, load 0.96, tight 0.76, dailies): 0.0% on time against 60.0% for `static-priority`. `go run ./hack/stress -point 'nodes=2,gpuFrac=0.125,projects=8,quotaFill=0.491,quotaSkew=0.727,borrow=0.0136,provisioning=24.6,burstCost=0.54,burstBudget=0.699,interruptions=0.102,lateCost=20.6,load=0.965,days=0.342,arrivals=1,tightness=0.763,slackSpread=0.368,noDue=0.0317,unitMin=3.65,spread=0.876,heavy=0.393,heavyMult=4,depth=1,width=1,expand=0.307,estErr=0.37,mlShare=0.136,ranks=4,ckptMin=38.1,failRate=0.0144,nodeFaults=0.436,boostRate=0.0541,holdRate=0.12,drain=0.5' -seed 1`
- quota-skew (8 nodes, load 1.4, tight 2, dailies): 85.7% on time against 100.0% for `edf`. `go run ./hack/stress -point 'nodes=8,gpuFrac=0.15,projects=8,quotaFill=1,quotaSkew=2,borrow=0.1,burstFrac=0.5,provisioning=6,burstCost=1,burstBudget=0.3,lateCost=400,load=1.4,days=0.3,arrivals=1,tightness=2,slackSpread=0.4,noDue=0.1,unitMin=15,spread=0.5,heavy=0.3,heavyMult=4,depth=2,width=2,expand=0.3,estErr=0.2,mlShare=0.4,ranks=4,ckptMin=15,failRate=0.02,drain=0.5' -seed 1`
- zero-budget (8 nodes, load 1.6, tight 1, dailies): 55.2% on time against 65.5% for `edf`. `go run ./hack/stress -point 'nodes=8,gpuFrac=0.15,projects=3,quotaFill=0.8,quotaSkew=0.5,borrow=-1,burstFrac=0.5,provisioning=6,burstCost=1,lateCost=2000,load=1.6,days=0.3,arrivals=1,tightness=1,slackSpread=0.4,noDue=0.1,unitMin=15,spread=0.5,heavy=0.3,heavyMult=4,depth=2,width=2,expand=0.3,estErr=0.2,mlShare=0.4,ranks=4,ckptMin=15,failRate=0.02,drain=0.5' -seed 1`

The large losses come from `starvation-over-deadline` (see Findings): on small farms the starvation guard gives slots to jobs with a day or more of slack ahead of jobs due within hours, and a guard unit that has run past MaxLostWork cannot be preempted. The rest are quota points: the baselines ignore quotas, and where nominal quotas and borrowing limits are small the policy may not use capacity they use; at the extreme (a small farm whose nominal quotas and borrowing limits all round to 0) nothing may run at all, as Kueue's rules require.

## Heatmaps

Means over the Latin hypercube points in each cell (corners excluded); the number of points is under each value. Hover a cell for its range.

![On time, objective+burst (%)](ontime-load-tightness.svg)

![On time, objective+burst (%)](ontime-nodes-load.svg)

![On time, objective+burst minus best baseline (points)](delta-load-tightness.svg)

![Burst spend per owned node-day ($)](burst-budget.svg)

![Invariants failing without a known cause (per point)](violations-nodes-load.svg)

![Invariants failing without a known cause (per point)](violations-load-tightness.svg)

## Feasibility predictions

Each deadline job's `AtRisk` under `objective+burst`, at its first decision with work ready and again halfway to its deadline, against whether it finished late.

| When | At risk and late | At risk, on time | On track, late | On track, on time | Precision | Recall |
|---|---|---|---|---|---|---|
| first decision | 173 | 32 | 108 | 634 | 84.4% | 61.6% |
| halfway | 190 | 25 | 73 | 107 | 88.4% | 72.2% |

## Decide's cost by farm size

One `sched.Decide` call on the busiest snapshot of a short overloaded run (load 1.3, eight projects) at each size, averaged over repeated calls.

| Owned nodes | Jobs | Running units | Runnable units | Time per call | Allocated per call | Allocations per call |
|---|---|---|---|---|---|---|
| 3 | 14 | 5 | 16 | 16µs | 17 KiB | 265 |
| 30 | 20 | 45 | 429 | 28µs | 32 KiB | 408 |
| 300 | 117 | 434 | 1895 | 173µs | 188 KiB | 1656 |

The slowest single decision in the grid's runs took 20ms.

## Points

On time per policy (jobs on time of jobs with a deadline), and failing invariants.

| Point | Farm and load | objective+burst | objective | edf | fifo | fair-share | static-priority | Failing |
|---|---|---|---|---|---|---|---|---|
| all-blocked | 8 nodes, load 1, tight 2, dailies | 4/39 | 4/39 | 4/39 | 4/39 | 4/39 | 4/39 | blocked-rank*, eta-inconsistent*, lateness-unpriced* |
| all-past-due | 8 nodes, load 1, tight 2, dailies | 0/35 | 0/35 | 0/35 | 0/35 | 0/35 | 0/35 | blocked-rank* |
| cloud-only | 0 nodes, load 1, tight 2, dailies | 13/17 | 0/14 | 0/14 | 0/14 | 0/14 | 0/14 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| deep-dag | 8 nodes, load 1, tight 2, dailies | 55/57 | 54/57 | 56/57 | 54/57 | 56/57 | 55/57 | blocked-rank*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| empty-farm | 0 nodes, load 1, tight 2, dailies | 0/14 | 0/14 | 0/14 | 0/14 | 0/14 | 0/14 | blocked-rank*, past-due-on-track* |
| g000 | 2 nodes, load 0.84, tight 4.3, crunch | 3/3 | 3/3 | 3/3 | 3/3 | 3/3 | 3/3 | eta-inconsistent*, lateness-unpriced* |
| g001 | 9 nodes, load 1.1, tight 12, sun | 56/56 | 56/56 | 56/56 | 47/56 | 47/56 | 47/56 | blocked-rank*, churn-pingpong*, starvation-over-deadline* |
| g002 | 3 nodes, load 1.7, tight 2.8, dailies | 11/33 | 11/33 | 32/32 | 10/37 | 24/32 | 13/30 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g003 | 12 nodes, load 0.61, tight 0.5, dailies | 18/33 | 17/33 | 15/33 | 13/33 | 15/33 | 14/33 | blocked-rank*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g004 | 10 nodes, load 0.93, tight 1.3, crunch | 24/24 | 24/24 | 24/24 | 19/24 | 23/24 | 24/24 | blocked-rank*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g005 | 16 nodes, load 1.2, tight 9.5, dailies | 65/66 | 65/66 | 65/66 | 57/66 | 57/66 | 50/66 | eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g006 | 4 nodes, load 1.8, tight 5.7, crunch | 10/10 | 10/10 | 10/10 | 10/10 | 10/10 | 10/10 | blocked-rank* |
| g007 | 6 nodes, load 1.1, tight 1.1, sun | 24/34 | 24/34 | 25/34 | 23/34 | 23/34 | 23/34 | blocked-rank*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g008 | 2 nodes, load 0.68, tight 8.1, steady | 1/1 | 1/1 | 1/1 | 1/1 | 1/1 | 1/1 |  |
| g009 | 22 nodes, load 1.6, tight 3.3, sun | 31/33 | 31/33 | 29/32 | 29/32 | 29/32 | 29/32 | blocked-rank*, eta-inconsistent*, lateness-unpriced* |
| g010 | 5 nodes, load 1.8, tight 1.8, dailies | 9/16 | 5/14 | 9/19 | 8/18 | 8/18 | 13/20 | churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g011 | 7 nodes, load 0.52, tight 2.5, crunch | 18/20 | 18/20 | 19/20 | 19/20 | 18/20 | 18/20 | blocked-rank*, eta-inconsistent*, lateness-unpriced* |
| g012 | 8 nodes, load 0.5, tight 3.7, steady | 4/4 | 4/4 | 4/4 | 4/4 | 4/4 | 4/4 |  |
| g013 | 2 nodes, load 0.37, tight 5.3, steady | 25/26 | 25/26 | 26/26 | 25/26 | 25/26 | 25/26 | blocked-rank*, churn-pingpong*, starvation-over-deadline* |
| g014 | 1 nodes, load 1.4, tight 2.1, crunch | 1/1 | 0/0 | 2/2 | 2/2 | 2/2 | 2/2 | blocked-rank*, starvation-over-deadline* |
| g015 | 2 nodes, load 0.96, tight 0.76, dailies | 0/5 | 0/5 | 2/5 | 1/5 | 0/5 | 3/5 |  |
| g016 | 17 nodes, load 0.74, tight 7.4, steady | 37/39 | 37/39 | 37/39 | 37/39 | 37/39 | 37/39 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g017 | 13 nodes, load 1.5, tight 0.66, sun | 9/55 | 9/55 | 10/55 | 9/55 | 12/55 | 14/55 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g018 | 5 nodes, load 1.3, tight 1.1, crunch | 8/10 | 8/10 | 8/10 | 5/10 | 5/10 | 5/10 | blocked-rank*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g019 | 1 nodes, load 0.44, tight 1.6, sun | 2/2 | 2/2 | 2/2 | 2/2 | 2/2 | 2/2 | blocked-rank* |
| g020 | 1 nodes, load 1.9, tight 0.59, steady | 6/18 | 0/18 | 0/18 | 0/18 | 0/18 | 0/18 | blocked-rank*, eta-inconsistent*, lateness-unpriced*, metamorphic-capacity*, starvation-over-deadline* |
| g021 | 3 nodes, load 1.4, tight 6.8, sun | 42/42 | 42/42 | 42/42 | 42/42 | 42/42 | 42/42 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g022 | 21 nodes, load 1.9, tight 0.86, dailies | 19/39 | 14/39 | 15/39 | 9/38 | 9/38 | 14/38 | churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| g023 | 4 nodes, load 1.3, tight 2.1, steady | 12/17 | 12/17 | 12/17 | 11/17 | 12/17 | 11/17 | blocked-rank*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| nudge-storm | 8 nodes, load 1, tight 2, dailies | 27/29 | 27/29 | 27/29 | 26/29 | 26/29 | 27/29 | blocked-rank*, churn-pingpong*, starvation-over-deadline* |
| one-node | 1 nodes, load 1, tight 2, dailies | 6/6 | 6/6 | 6/6 | 6/6 | 6/6 | 6/6 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| one-node-gpu | 1 nodes, load 1, tight 2, dailies | 7/7 | 7/7 | 7/7 | 7/7 | 7/7 | 7/7 | churn-pingpong* |
| overload-2x-tight | 8 nodes, load 2, tight 0.8, dailies | 13/24 | 13/24 | 14/25 | 8/26 | 12/25 | 13/25 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| oversize-gang | 8 nodes, load 1, tight 2, dailies | 29/30 | 29/30 | 29/30 | 29/30 | 29/30 | 29/30 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced* |
| quota-skew | 8 nodes, load 1.4, tight 2, dailies | 12/14 | 8/13 | 23/23 | 21/23 | 21/23 | 21/23 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| single-huge-job | 8 nodes, load 1.5, tight 2, dailies | 1/1 | 1/1 | 1/1 | 1/1 | 1/1 | 1/1 |  |
| spot-storm | 8 nodes, load 1.5, tight 2, dailies | 25/26 | 25/26 | 26/26 | 22/26 | 23/26 | 21/26 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| zero-budget | 8 nodes, load 1.6, tight 1, dailies | 16/29 | 16/29 | 19/29 | 17/29 | 15/29 | 14/29 | blocked-rank*, churn-pingpong*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |
| zero-late-cost | 8 nodes, load 1.6, tight 1, dailies | 23/29 | 23/29 | 24/29 | 23/29 | 24/29 | 23/29 | blocked-rank*, eta-inconsistent*, lateness-unpriced*, starvation-over-deadline* |

`*` marks an invariant whose findings at the point all have a known cause. Every point's full definition is in `violations.csv` (for points with findings) or printed by `-point <name>` for corners.
