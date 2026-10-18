# Work scheduling simulation

Reproduce with `go run ./hack/sim` from the repository root (seeds 1, 2, 3, 4, 5; 40 binary-order trials per cell). Built with go1.26.7. Every number below is produced by that command; nothing is edited by hand except the discussion at the end, which is kept in `hack/sim/discussion.md`.

The simulator calls the production policy code (`work/sched.Decide`) and the estimator (`work/estimate`) on snapshots it builds, as the controller will. Baselines are in the same package with the same signature.

## Results

Mean ± standard deviation across seeds. A render deadline is missed when the job finishes after it; late hours sum the lateness of missed jobs. CI time to green runs from submission to the last shard passing. Utilization is busy slot time over owned capacity for the week (Monday 00:00 to Monday 00:00), including work later lost. Useful GPU hours are GPU time whose work was kept (finished units, and ML progress saved in a checkpoint); lost GPU hours are GPU time thrown away by failures and preemptions, plus ML restore time.

| policy          | render deadlines missed | missed %   | late hours    | CI p50 min | CI p95 min | CI over 15 min % | util cpu % | util gpu % | util ci %  | useful GPU h | lost GPU h | burst $   |
| --------------- | ----------------------- | ---------- | ------------- | ---------- | ---------- | ---------------- | ---------- | ---------- | ---------- | ------------ | ---------- | --------- |
| manual          | 43.0 ± 20.5             | 13.9 ± 6.1 | 107.8 ± 71.2  | 8.9 ± 0.1  | 17.3 ± 1.3 | 9.0 ± 1.3        | 47.2 ± 6.1 | 36.4 ± 6.3 | 17.3 ± 0.3 | 1935 ± 332   | 20.1 ± 5.3 | 0 ± 0     |
| fifo            | 46.6 ± 25.0             | 15.0 ± 7.7 | 140.3 ± 113.3 | 8.9 ± 0.1  | 17.1 ± 1.2 | 8.9 ± 1.3        | 47.1 ± 6.1 | 36.3 ± 6.2 | 17.3 ± 0.3 | 1935 ± 332   | 18.0 ± 4.9 | 0 ± 0     |
| edf             | 24.4 ± 8.8              | 7.9 ± 2.5  | 60.1 ± 37.2   | 8.9 ± 0.1  | 17.2 ± 1.3 | 9.1 ± 1.4        | 47.1 ± 6.1 | 36.3 ± 6.2 | 17.3 ± 0.3 | 1935 ± 332   | 18.0 ± 4.9 | 0 ± 0     |
| objective       | 24.0 ± 9.7              | 7.8 ± 2.9  | 61.8 ± 37.2   | 8.8 ± 0.1  | 15.5 ± 0.6 | 5.7 ± 0.6        | 47.2 ± 6.1 | 36.4 ± 6.3 | 17.3 ± 0.3 | 1935 ± 332   | 23.3 ± 7.4 | 0 ± 0     |
| objective+burst | 21.0 ± 8.9              | 6.8 ± 2.7  | 50.8 ± 31.8   | 8.8 ± 0.1  | 15.3 ± 0.7 | 5.4 ± 1.0        | 45.7 ± 5.9 | 34.2 ± 3.5 | 17.3 ± 0.3 | 1935 ± 332   | 23.6 ± 7.8 | 486 ± 289 |

Render jobs with deadlines per week: 305 on average. The spread across seeds is large because each seed is a different week (job counts, shot lengths, where heavy shots land); compare policies within a seed, below.

![late hours by policy](late_hours.svg)

### Paired against EDF, same seed

Difference from EDF on the same week (policy minus EDF): mean ± standard deviation of the per-seed difference, and on how many seeds the policy had fewer, equal or more late hours.

| policy                           | Δ missed    | Δ late hours | Δ CI p95 min | seeds with fewer / equal / more late hours |
| -------------------------------- | ----------- | ------------ | ------------ | ------------------------------------------ |
| manual                           | 18.6 ± 13.1 | 47.6 ± 47.4  | 0.1 ± 0.1    | 0 / 0 / 5                                  |
| fifo                             | 22.2 ± 18.4 | 80.1 ± 86.1  | -0.1 ± 0.1   | 0 / 0 / 5                                  |
| objective                        | -0.4 ± 5.2  | 1.7 ± 9.5    | -1.6 ± 0.8   | 2 / 0 / 3                                  |
| objective+burst                  | -3.4 ± 7.6  | -9.3 ± 14.8  | -1.8 ± 0.9   | 4 / 0 / 1                                  |
| objective, no information tier   | -0.6 ± 5.1  | 1.6 ± 9.8    | -1.7 ± 1.0   | 2 / 0 / 3                                  |
| objective, fair-share surplus    | -1.0 ± 3.7  | -1.1 ± 7.6   | -1.7 ± 0.8   | 3 / 0 / 2                                  |
| objective, DESIGN 9.9 as written | 13.6 ± 13.8 | 17.7 ± 28.9  | 0.1 ± 1.8    | 1 / 0 / 4                                  |

### By job kind

Missed deadlines and late hours per kind. ML runs carry a deadline of submission plus 36 hours.

| policy                           | dailies missed     | dailies late h | preview missed     | preview late h | delivery missed | delivery late h | ML missed       | ML late h |
| -------------------------------- | ------------------ | -------------- | ------------------ | -------------- | --------------- | --------------- | --------------- | --------- |
| manual                           | 16.6 ± 10.5 of 106 | 74.2 ± 59.3    | 26.0 ± 14.0 of 186 | 32.7 ± 18.3    | 0.4 ± 0.5 of 13 | 0.9 ± 1.7       | 0.0 ± 0.0 of 22 | 0.0 ± 0.0 |
| fifo                             | 12.6 ± 7.8 of 106  | 53.1 ± 34.7    | 33.8 ± 20.0 of 186 | 85.6 ± 79.8    | 0.2 ± 0.4 of 13 | 1.6 ± 3.6       | 0.0 ± 0.0 of 22 | 0.0 ± 0.0 |
| edf                              | 8.6 ± 5.1 of 106   | 34.0 ± 25.4    | 14.8 ± 4.0 of 186  | 19.6 ± 8.9     | 1.0 ± 1.0 of 13 | 6.6 ± 6.4       | 0.0 ± 0.0 of 22 | 0.0 ± 0.0 |
| objective                        | 8.4 ± 4.0 of 106   | 35.2 ± 24.9    | 14.6 ± 5.1 of 186  | 20.9 ± 10.7    | 1.0 ± 1.0 of 13 | 5.7 ± 5.3       | 0.0 ± 0.0 of 22 | 0.0 ± 0.0 |
| objective+burst                  | 6.2 ± 3.0 of 106   | 26.4 ± 19.9    | 14.0 ± 5.3 of 186  | 19.8 ± 10.2    | 0.8 ± 0.8 of 13 | 4.5 ± 4.2       | 0.0 ± 0.0 of 22 | 0.0 ± 0.0 |
| objective, no information tier   | 8.4 ± 4.0 of 106   | 35.2 ± 25.0    | 14.4 ± 4.6 of 186  | 20.9 ± 10.4    | 1.0 ± 1.0 of 13 | 5.6 ± 5.2       | 0.0 ± 0.0 of 22 | 0.0 ± 0.0 |
| objective, fair-share surplus    | 8.0 ± 3.6 of 106   | 32.4 ± 25.0    | 14.4 ± 4.3 of 186  | 21.3 ± 11.5    | 1.0 ± 1.0 of 13 | 5.4 ± 5.0       | 0.0 ± 0.0 of 22 | 0.0 ± 0.0 |
| objective, DESIGN 9.9 as written | 18.2 ± 13.1 of 106 | 48.7 ± 42.9    | 18.4 ± 8.2 of 186  | 23.9 ± 14.2    | 1.4 ± 1.7 of 13 | 5.3 ± 5.0       | 0.0 ± 0.0 of 22 | 0.0 ± 0.0 |

### By show

Late hours of each show's render jobs (fairness across projects).

| policy                           | showA late h | showB late h | showC late h |
| -------------------------------- | ------------ | ------------ | ------------ |
| manual                           | 27.5 ± 18.8  | 44.9 ± 35.4  | 35.4 ± 32.4  |
| fifo                             | 62.3 ± 48.3  | 47.2 ± 35.7  | 30.9 ± 34.2  |
| edf                              | 23.0 ± 17.8  | 26.4 ± 18.5  | 10.7 ± 10.5  |
| objective                        | 25.4 ± 19.2  | 21.5 ± 18.6  | 15.0 ± 14.9  |
| objective+burst                  | 20.0 ± 21.0  | 20.8 ± 17.4  | 10.0 ± 7.9   |
| objective, no information tier   | 25.2 ± 19.3  | 21.5 ± 18.6  | 15.0 ± 15.0  |
| objective, fair-share surplus    | 24.0 ± 19.6  | 21.3 ± 19.5  | 13.7 ± 13.4  |
| objective, DESIGN 9.9 as written | 31.7 ± 22.5  | 27.5 ± 27.1  | 18.6 ± 19.4  |

### Preemption, lost work and burst

Preemptions counts units stopped (yields and kills). Lost CPU hours include task failures, which every policy suffers alike (failures are drawn per unit and attempt from the seed, not from the policy). Bursts needing approval counts distinct jobs for which the policy found a burst that would restore the deadline but was over the project's budget or cost more than the lateness it prevents; the simulation never approves those.

| policy                           | preemptions | by reason (mean)                                           | lost CPU h | ML useful GPU h | ML lost GPU h | burst slot-hours | burst $   | bursts needing approval |
| -------------------------------- | ----------- | ---------------------------------------------------------- | ---------- | --------------- | ------------- | ---------------- | --------- | ----------------------- |
| manual                           | 7 ± 1       | wrangler-suspend 7                                         | 198 ± 37   | 378 ± 95        | 2.3 ± 0.9     | 0 ± 0            | 0 ± 0     | 0.0 ± 0.0               |
| fifo                             | 0 ± 0       | none                                                       | 184 ± 22   | 378 ± 95        | 0.4 ± 0.4     | 0 ± 0            | 0 ± 0     | 0.0 ± 0.0               |
| edf                              | 0 ± 0       | none                                                       | 184 ± 22   | 378 ± 95        | 0.4 ± 0.4     | 0 ± 0            | 0 ± 0     | 0.0 ± 0.0               |
| objective                        | 566 ± 62    | guard-unit 7, reclaim-nominal 255, restore-feasibility 304 | 217 ± 32   | 378 ± 95        | 0.4 ± 0.4     | 0 ± 0            | 0 ± 0     | 4.2 ± 2.2               |
| objective+burst                  | 572 ± 60    | guard-unit 6, reclaim-nominal 263, restore-feasibility 302 | 217 ± 32   | 378 ± 95        | 0.4 ± 0.4     | 764 ± 436        | 486 ± 289 | 0.0 ± 0.0               |
| objective, no information tier   | 581 ± 59    | guard-unit 7, reclaim-nominal 250, restore-feasibility 324 | 216 ± 33   | 378 ± 95        | 0.4 ± 0.4     | 0 ± 0            | 0 ± 0     | 4.2 ± 1.3               |
| objective, fair-share surplus    | 515 ± 80    | guard-unit 6, reclaim-nominal 217, restore-feasibility 292 | 212 ± 30   | 378 ± 95        | 0.4 ± 0.4     | 0 ± 0            | 0 ± 0     | 5.0 ± 2.0               |
| objective, DESIGN 9.9 as written | 598 ± 85    | reclaim-nominal 211, restore-feasibility 387               | 219 ± 33   | 378 ± 95        | 0.4 ± 0.4     | 0 ± 0            | 0 ± 0     | 54.4 ± 31.2             |

### Ablations

The objective policy with parts of DESIGN.md section 9.9 restored as written (see the discussion for what changed and why). Same columns as the main table.

| policy                           | render deadlines missed | missed %   | late hours  | CI p50 min | CI p95 min | CI over 15 min % | util cpu % | util gpu % | util ci %  | useful GPU h | lost GPU h | burst $ |
| -------------------------------- | ----------------------- | ---------- | ----------- | ---------- | ---------- | ---------------- | ---------- | ---------- | ---------- | ------------ | ---------- | ------- |
| objective                        | 24.0 ± 9.7              | 7.8 ± 2.9  | 61.8 ± 37.2 | 8.8 ± 0.1  | 15.5 ± 0.6 | 5.7 ± 0.6        | 47.2 ± 6.1 | 36.4 ± 6.3 | 17.3 ± 0.3 | 1935 ± 332   | 23.3 ± 7.4 | 0 ± 0   |
| objective, no information tier   | 23.8 ± 9.2              | 7.7 ± 2.7  | 61.8 ± 37.0 | 8.8 ± 0.1  | 15.5 ± 0.8 | 5.7 ± 1.1        | 47.2 ± 6.1 | 36.4 ± 6.3 | 17.3 ± 0.3 | 1935 ± 332   | 23.0 ± 6.7 | 0 ± 0   |
| objective, fair-share surplus    | 23.4 ± 8.6              | 7.6 ± 2.5  | 59.1 ± 38.3 | 8.8 ± 0.1  | 15.4 ± 0.7 | 5.6 ± 0.9        | 47.2 ± 6.1 | 36.4 ± 6.3 | 17.3 ± 0.3 | 1935 ± 332   | 23.4 ± 6.8 | 0 ± 0   |
| objective, DESIGN 9.9 as written | 38.0 ± 21.0             | 12.2 ± 6.2 | 77.8 ± 58.0 | 8.8 ± 0.1  | 17.3 ± 1.2 | 9.0 ± 2.0        | 47.2 ± 6.1 | 36.4 ± 6.3 | 17.3 ± 0.3 | 1935 ± 332   | 24.1 ± 6.2 | 0 ± 0   |

## Sensitivity: half the GPUs

The main farm's GPU pool runs at about a third of capacity, so ML runs and GPU renders rarely contend there. This scenario replays the same weeks (same seeds, same jobs) with 16 owned GPUs instead of 32 and every GPU quota halved. Everything else is unchanged.

| policy          | render missed | render late h | GPU render missed | GPU render late h | ML missed | util gpu %  | ML useful GPU h | ML lost GPU h | lost GPU h | preemptions | burst $   |
| --------------- | ------------- | ------------- | ----------------- | ----------------- | --------- | ----------- | --------------- | ------------- | ---------- | ----------- | --------- |
| manual          | 68.8 ± 23.7   | 476.3 ± 596.1 | 28.6 ± 23.7       | 375.5 ± 621.8     | 5.4 ± 7.9 | 72.7 ± 12.3 | 378 ± 95        | 3.8 ± 2.7     | 21.8 ± 3.7 | 10 ± 6      | 0 ± 0     |
| fifo            | 82.0 ± 30.0   | 610.0 ± 645.9 | 39.8 ± 15.9       | 480.0 ± 694.0     | 2.2 ± 4.9 | 72.6 ± 12.3 | 378 ± 95        | 0.4 ± 0.4     | 18.0 ± 4.9 | 0 ± 0       | 0 ± 0     |
| edf             | 49.2 ± 18.8   | 381.6 ± 574.9 | 27.6 ± 19.9       | 328.5 ± 591.0     | 4.4 ± 8.8 | 72.6 ± 12.3 | 378 ± 95        | 0.4 ± 0.4     | 18.0 ± 4.9 | 0 ± 0       | 0 ± 0     |
| objective       | 39.0 ± 10.4   | 258.9 ± 260.8 | 16.6 ± 7.3        | 204.1 ± 277.9     | 0.6 ± 1.3 | 72.6 ± 12.0 | 378 ± 95        | 2.3 ± 0.8     | 23.1 ± 5.7 | 516 ± 64    | 0 ± 0     |
| objective+burst | 22.0 ± 8.6    | 51.9 ± 30.4   | 1.4 ± 1.1         | 2.6 ± 4.7         | 0.0 ± 0.0 | 49.8 ± 4.9  | 378 ± 95        | 1.7 ± 1.2     | 25.2 ± 6.0 | 541 ± 56    | 976 ± 406 |

## Binary against sequential order

One shot at a time on 16 slots, rendered in binary or in sequential order: 40 trials for each frame count [120 240 480] and each heavy-region multiplier [1 2 3 5 8] (1 is the control, with no heavy region). At 99 evenly spaced moments of the render the estimated remaining time, max(W/P, t_path), is compared with the truth. The `sequential-mean` arm uses the usual render-manager estimate (mean of completed frames times frames left) instead of the estimate package.

### Error of the remaining-time estimate

Mean of |estimated - true| / true remaining time, over all frame counts, at the given fraction of the render's elapsed time; then the mean over the whole render and the mean signed error (negative: underestimate).

| multiplier | arm             | at 10% | at 25% | at 50% | at 75% | whole render | signed |
| ---------- | --------------- | ------ | ------ | ------ | ------ | ------------ | ------ |
| 1x         | binary          | 0.083  | 0.064  | 0.097  | 0.186  | 0.150        | -0.078 |
| 1x         | sequential      | 0.135  | 0.155  | 0.177  | 0.225  | 0.211        | -0.084 |
| 1x         | sequential-mean | 0.181  | 0.118  | 0.135  | 0.212  | 0.187        | -0.098 |
| 2x         | binary          | 0.112  | 0.103  | 0.144  | 0.186  | 0.196        | -0.101 |
| 2x         | sequential      | 0.234  | 0.365  | 0.385  | 0.235  | 0.317        | -0.004 |
| 2x         | sequential-mean | 0.255  | 0.258  | 0.278  | 0.206  | 0.262        | -0.044 |
| 3x         | binary          | 0.185  | 0.135  | 0.190  | 0.213  | 0.244        | -0.111 |
| 3x         | sequential      | 0.338  | 0.511  | 0.672  | 0.328  | 0.464        | +0.046 |
| 3x         | sequential-mean | 0.340  | 0.361  | 0.418  | 0.360  | 0.383        | -0.044 |
| 5x         | binary          | 0.308  | 0.220  | 0.220  | 0.253  | 0.330        | -0.124 |
| 5x         | sequential      | 0.443  | 0.939  | 1.239  | 0.465  | 0.784        | +0.340 |
| 5x         | sequential-mean | 0.462  | 0.527  | 0.734  | 0.659  | 0.640        | +0.118 |
| 8x         | binary          | 0.441  | 0.303  | 0.205  | 0.238  | 0.390        | -0.114 |
| 8x         | sequential      | 0.574  | 0.887  | 1.707  | 0.801  | 1.063        | +0.542 |
| 8x         | sequential-mean | 0.591  | 0.663  | 0.713  | 0.794  | 0.777        | +0.061 |

Relative error grows near the end of every render because the true remainder approaches zero; the chart therefore shows the absolute error as a fraction of the render, averaged over trials with a heavy region of 3x or more.

![remaining-time error against elapsed time](order_error.svg)

### Heavy region flagged, and slack settled

A heavy region is flagged when the estimator reports a run of frames above 3x the reference cost that overlaps the true region (a running frame counts once its elapsed time alone is above the threshold). Times are fractions of the render. False flags are trials in which a region not overlapping the true one was flagged at some moment. The deadline is set so the true slack is 50% of the render; the slack estimate is within the band when its error is at most 10% of the true slack, and has settled when it stays in the band from then on (trials that never settle count as 1.00).

| multiplier | arm             | heavy region flagged | false flags | slack within band, share of render | slack settled, median | slack settled, p90 | settled before the end |
| ---------- | --------------- | -------------------- | ----------- | ---------------------------------- | --------------------- | ------------------ | ---------------------- |
| 1x         | binary          | n/a                  | 0%          | 65%                                | 0.52                  | 0.87               | 98%                    |
| 1x         | sequential      | n/a                  | 6%          | 42%                                | 0.84                  | 0.91               | 100%                   |
| 1x         | sequential-mean | n/a                  | n/a         | 46%                                | 0.79                  | 0.89               | 100%                   |
| 2x         | binary          | 54%, median at 0.34  | 0%          | 52%                                | 0.76                  | 1.00               | 88%                    |
| 2x         | sequential      | 76%, median at 0.50  | 3%          | 33%                                | 0.85                  | 0.91               | 98%                    |
| 2x         | sequential-mean | n/a                  | n/a         | 36%                                | 0.74                  | 0.86               | 98%                    |
| 3x         | binary          | 100%, median at 0.15 | 0%          | 39%                                | 0.82                  | 1.00               | 73%                    |
| 3x         | sequential      | 100%, median at 0.46 | 3%          | 28%                                | 0.85                  | 0.92               | 93%                    |
| 3x         | sequential-mean | n/a                  | n/a         | 26%                                | 0.83                  | 0.95               | 92%                    |
| 5x         | binary          | 100%, median at 0.11 | 0%          | 28%                                | 0.85                  | 1.00               | 66%                    |
| 5x         | sequential      | 100%, median at 0.35 | 0%          | 26%                                | 0.86                  | 1.00               | 82%                    |
| 5x         | sequential-mean | n/a                  | n/a         | 17%                                | 0.92                  | 1.00               | 79%                    |
| 8x         | binary          | 100%, median at 0.08 | 0%          | 25%                                | 0.96                  | 1.00               | 54%                    |
| 8x         | sequential      | 100%, median at 0.30 | 0%          | 20%                                | 0.90                  | 1.00               | 68%                    |
| 8x         | sequential-mean | n/a                  | n/a         | 13%                                | 0.96                  | 1.00               | 64%                    |

### Makespan

The order also changes when the shot finishes on a fixed number of slots. Binary order leaves frames from every part of the shot, heavy ones included, to its last levels, so the last wave can hold long frames that start late; sequential order finishes with whatever lies at the end of the shot. Ratio of binary to sequential makespan for the same shot.

| multiplier | mean ratio | median | p90   | binary more than 5% longer |
| ---------- | ---------- | ------ | ----- | -------------------------- |
| 1x         | 1.002      | 1.002  | 1.021 | 2%                         |
| 2x         | 1.037      | 1.026  | 1.100 | 24%                        |
| 3x         | 1.064      | 1.053  | 1.156 | 54%                        |
| 5x         | 1.114      | 1.083  | 1.231 | 77%                        |
| 8x         | 1.134      | 1.123  | 1.244 | 81%                        |

### Calibration of W90

Share of moments at which the true remaining work was at or below W90. A calibrated 90th percentile covers about 90%; the pass band is 87 to 93%. As built, W90 is W + 1.28 Sigma. Calibrated, it is W plus the conformal 90% quantile of scores from finished shots in the same stratum (`estimate.Calibration`): 200 history shots per frame count and multiplier, drawn like the experiment's from a separate seed, with the same mix of multipliers. Strata are what the estimator sees: the share of frames finished, the heavy multiple measured on finished frames, and a running frame that has outlasted it.

| multiplier | binary, as built | binary, calibrated | sequential, as built | sequential, calibrated |
| ---------- | ---------------- | ------------------ | -------------------- | ---------------------- |
| 1x         | 88%              | 92%                | 90%                  | 99%                    |
| 2x         | 85%              | 92%                | 83%                  | 96%                    |
| 3x         | 83%              | 93%                | 72%                  | 92%                    |
| 5x         | 71%              | 89%                | 70%                  | 86%                    |
| 8x         | 68%              | 85%                | 65%                  | 76%                    |

Binary order, by the share of frames finished: calibrated (as built), and the number of moments.

| multiplier | whole render      | under 1/16        | 1/16 to 1/4       | 1/4 to 1/2       | 1/2 to 3/4       | 3/4 on           |
| ---------- | ----------------- | ----------------- | ----------------- | ---------------- | ---------------- | ---------------- |
| 1x         | 92% (88%) n=11880 | 100% (88%) n=1042 | 100% (93%) n=2128 | 90% (91%) n=2853 | 87% (88%) n=2863 | 89% (82%) n=2994 |
| 2x         | 92% (85%) n=11880 | 99% (69%) n=974   | 97% (88%) n=2073  | 92% (91%) n=2782 | 91% (90%) n=2790 | 89% (80%) n=3261 |
| 3x         | 93% (83%) n=11880 | 97% (41%) n=854   | 94% (75%) n=2040  | 92% (92%) n=2696 | 93% (92%) n=2704 | 91% (85%) n=3586 |
| 5x         | 89% (71%) n=11880 | 83% (28%) n=654   | 88% (48%) n=1880  | 90% (76%) n=2609 | 88% (85%) n=2588 | 90% (77%) n=4149 |
| 8x         | 85% (68%) n=11880 | 53% (12%) n=474   | 67% (29%) n=1689  | 87% (68%) n=2554 | 89% (81%) n=2523 | 91% (80%) n=4640 |

Binary order, moments at which the multiple is observable: every moment of a plain shot, and the moments of a heavy shot after a frame of its heavy region has finished.

| multiplier | whole render      | under 1/16        | 1/16 to 1/4       | 1/4 to 1/2       | 1/2 to 3/4       | 3/4 on           |
| ---------- | ----------------- | ----------------- | ----------------- | ---------------- | ---------------- | ---------------- |
| 1x         | 92% (88%) n=11880 | 100% (88%) n=1042 | 100% (93%) n=2128 | 90% (91%) n=2853 | 87% (88%) n=2863 | 89% (82%) n=2994 |
| 2x         | 92% (88%) n=10715 | 100% (92%) n=128  | 97% (93%) n=1754  | 92% (91%) n=2782 | 91% (90%) n=2790 | 89% (80%) n=3261 |
| 3x         | 92% (90%) n=10363 | 100% (97%) n=33   | 96% (95%) n=1359  | 92% (92%) n=2681 | 93% (92%) n=2704 | 91% (85%) n=3586 |
| 5x         | 89% (83%) n=9966  | 100% (50%) n=2    | 92% (93%) n=909   | 89% (85%) n=2320 | 88% (85%) n=2586 | 90% (77%) n=4149 |
| 8x         | 90% (84%) n=9523  |                   | 77% (82%) n=600   | 90% (90%) n=1915 | 90% (86%) n=2376 | 91% (80%) n=4632 |

Binary order, the other moments: a heavy shot whose heavy frames are all still running or queued. Until one finishes, a 3x, a 5x and an 8x region produce the same observations (the same plain frames finish at the same times, and the heavy frames have run equally long), while the 8x shot has roughly twice the remaining work of the 3x one. No W90 computed from those observations can cover both at 87 to 93%; calibration covers 90% of the mix.

| multiplier | whole render     | under 1/16      | 1/16 to 1/4     | 1/4 to 1/2      | 1/2 to 3/4     | 3/4 on      |
| ---------- | ---------------- | --------------- | --------------- | --------------- | -------------- | ----------- |
| 1x         |                  |                 |                 |                 |                |             |
| 2x         | 98% (65%) n=1165 | 99% (66%) n=846 | 96% (65%) n=319 |                 |                |             |
| 3x         | 94% (38%) n=1517 | 97% (39%) n=821 | 92% (35%) n=681 | 100% (87%) n=15 |                |             |
| 5x         | 86% (13%) n=1914 | 83% (27%) n=652 | 85% (5%) n=971  | 96% (5%) n=289  | 0% (0%) n=2    |             |
| 8x         | 65% (3%) n=2357  | 53% (12%) n=474 | 62% (0%) n=1089 | 78% (2%) n=639  | 77% (0%) n=147 | 0% (0%) n=8 |

Binary order, by stratum (what the estimator sees), strata with at least 300 moments; 4179 moments fall in smaller strata.

| share finished / heavy multiple measured / running frame past it | moments | as built | calibrated |
| ---------------------------------------------------------------- | ------- | -------- | ---------- |
| under 1/16 / measured none / running past it none                | 3419    | 57%      | 90%        |
| under 1/16 / measured none / running past it 1.5-3x              | 549     | 40%      | 92%        |
| 1/16 to 1/4 / measured none / running past it none               | 2736    | 78%      | 90%        |
| 1/16 to 1/4 / measured none / running past it 1.5-3x             | 2512    | 47%      | 89%        |
| 1/16 to 1/4 / measured none / running past it 3-6x               | 902     | 13%      | 91%        |
| 1/16 to 1/4 / measured mild / running past it none               | 433     | 93%      | 91%        |
| 1/16 to 1/4 / measured mild / running past it 1.5-3x             | 316     | 97%      | 95%        |
| 1/16 to 1/4 / measured 3-3.75x / running past it none            | 721     | 96%      | 94%        |
| 1/16 to 1/4 / measured 3-3.75x / running past it 3-6x            | 431     | 92%      | 90%        |
| 1/16 to 1/4 / measured 3.75-4.7x / running past it 3-6x          | 429     | 93%      | 93%        |
| 1/4 to 1/2 / measured none / running past it none                | 2729    | 90%      | 88%        |
| 1/4 to 1/2 / measured none / running past it 1.5-3x              | 688     | 81%      | 88%        |
| 1/4 to 1/2 / measured none / running past it 3-6x                | 838     | 11%      | 88%        |
| 1/4 to 1/2 / measured mild / running past it none                | 997     | 91%      | 90%        |
| 1/4 to 1/2 / measured mild / running past it 1.5-3x              | 577     | 91%      | 92%        |
| 1/4 to 1/2 / measured 3-3.75x / running past it none             | 2124    | 93%      | 93%        |
| 1/4 to 1/2 / measured 3-3.75x / running past it 3-6x             | 860     | 88%      | 88%        |
| 1/4 to 1/2 / measured 3.75-4.7x / running past it none           | 356     | 87%      | 88%        |
| 1/4 to 1/2 / measured 3.75-4.7x / running past it 3-6x           | 848     | 80%      | 86%        |
| 1/4 to 1/2 / measured 4.7-5.9x / running past it 3-6x            | 515     | 91%      | 92%        |
| 1/4 to 1/2 / measured 4.7-5.9x / running past it 6x+             | 449     | 90%      | 90%        |
| 1/4 to 1/2 / measured 5.9-7.3x / running past it 6x+             | 541     | 88%      | 94%        |
| 1/4 to 1/2 / measured 7.3-9.2x / running past it 6x+             | 925     | 96%      | 94%        |
| 1/2 to 3/4 / measured none / running past it none                | 2601    | 88%      | 87%        |
| 1/2 to 3/4 / measured none / running past it 1.5-3x              | 326     | 88%      | 87%        |
| 1/2 to 3/4 / measured mild / running past it none                | 979     | 89%      | 91%        |
| 1/2 to 3/4 / measured mild / running past it 1.5-3x              | 500     | 85%      | 89%        |
| 1/2 to 3/4 / measured 3-3.75x / running past it none             | 2643    | 91%      | 92%        |
| 1/2 to 3/4 / measured 3-3.75x / running past it 3-6x             | 843     | 92%      | 91%        |
| 1/2 to 3/4 / measured 3.75-4.7x / running past it none           | 471     | 87%      | 89%        |
| 1/2 to 3/4 / measured 3.75-4.7x / running past it 3-6x           | 908     | 82%      | 87%        |
| 1/2 to 3/4 / measured 4.7-5.9x / running past it none            | 336     | 87%      | 88%        |
| 1/2 to 3/4 / measured 4.7-5.9x / running past it 3-6x            | 596     | 85%      | 91%        |
| 1/2 to 3/4 / measured 4.7-5.9x / running past it 6x+             | 484     | 87%      | 89%        |
| 1/2 to 3/4 / measured 5.9-7.3x / running past it 6x+             | 657     | 81%      | 88%        |
| 1/2 to 3/4 / measured 7.3-9.2x / running past it 6x+             | 1124    | 91%      | 95%        |
| 3/4 on / measured none / running past it none                    | 2733    | 82%      | 89%        |
| 3/4 on / measured mild / running past it none                    | 972     | 81%      | 91%        |
| 3/4 on / measured mild / running past it 1.5-3x                  | 476     | 78%      | 84%        |
| 3/4 on / measured 3-3.75x / running past it none                 | 3614    | 81%      | 89%        |
| 3/4 on / measured 3-3.75x / running past it 3-6x                 | 1179    | 87%      | 91%        |
| 3/4 on / measured 3.75-4.7x / running past it none               | 664     | 79%      | 90%        |
| 3/4 on / measured 3.75-4.7x / running past it 3-6x               | 1322    | 72%      | 88%        |
| 3/4 on / measured 4.7-5.9x / running past it none                | 456     | 73%      | 89%        |
| 3/4 on / measured 4.7-5.9x / running past it 3-6x                | 958     | 79%      | 92%        |
| 3/4 on / measured 4.7-5.9x / running past it 6x+                 | 809     | 89%      | 94%        |
| 3/4 on / measured 5.9-7.3x / running past it none                | 312     | 77%      | 87%        |
| 3/4 on / measured 5.9-7.3x / running past it 6x+                 | 1167    | 79%      | 90%        |
| 3/4 on / measured 7.3-9.2x / running past it none                | 428     | 81%      | 94%        |
| 3/4 on / measured 7.3-9.2x / running past it 6x+                 | 2289    | 81%      | 93%        |
| 3/4 on / measured 9.2x+ / running past it 6x+                    | 479     | 87%      | 93%        |

Acceptance (binary, calibrated). Standard errors cluster the moments by trial.

- Strata the estimator can see, with at least 300 moments, covered at 87 to 93% or within two standard errors of 90%: 46 of 51 (two standard errors leave about 1 in 20 out by chance). fail: 1/16 to 1/4 / measured mild / running past it 1.5-3x 94.9% (se 2.4); 1/16 to 1/4 / measured 3-3.75x / running past it none 94.3% (se 1.9); 1/4 to 1/2 / measured 5.9-7.3x / running past it 6x+ 94.3% (se 1.9); 1/4 to 1/2 / measured 7.3-9.2x / running past it 6x+ 94.5% (se 2.0); 1/2 to 3/4 / measured 7.3-9.2x / running past it 6x+ 94.8% (se 2.3).
- Plain, 3x, 5x and 8x at 87 to 93%, observable moments, every band from 1/4 finished on: pass.
- The same from 1/16 finished on: fail: 1x 1/16 to 1/4 99.8% (se 0.2); 3x 1/16 to 1/4 95.7% (se 1.3); 8x 1/16 to 1/4 77.3% (se 3.7).
- As first written, every band and every moment, including those at which the multiple cannot be observed: fail: 1x under 1/16 99.7% (se 0.2); 1x 1/16 to 1/4 99.8% (se 0.2); 3x under 1/16 96.7% (se 0.7); 3x 1/16 to 1/4 94.4% (se 1.0); 5x under 1/16 83.2% (se 1.5); 8x under 1/16 53.4% (se 2.3); 8x 1/16 to 1/4 67.2% (se 2.8); 8x 1/4 to 1/2 86.8% (se 1.9).

## Every parameter

### Farm

| pool     | slots    | kind  | provisioning | per slot-hour |
| -------- | -------- | ----- | ------------ | ------------- |
| cpu      | 240      | owned |              |               |
| gpu      | 32       | owned |              |               |
| ci       | 32       | owned |              |               |
| cpu-spot | 0 to 160 | burst | 7m0s         | $0.60         |
| cpu-od   | 0 to 80  | burst | 4m0s         | $1.43         |
| gpu-spot | 0 to 24  | burst | 8m0s         | $0.80         |
| gpu-od   | 0 to 16  | burst | 5m0s         | $1.86         |

A CPU slot is a 32 vCPU render node; a GPU slot is one 48 GB class GPU; a CI slot is an 8 vCPU runner. Burst prices are approximate public list prices for comparable instances (c7i.8xlarge, one GPU of a g6e.xlarge), spot at about 40 to 45 percent of on-demand, not quoted from a live price list. Provisioning includes boot, image pull and C4 cache pre-warm. Spot interruptions are not modeled. Render jobs may use their pool type's owned and burst pools; CI uses only the CI pool; ML runs use only the owned GPU pool. The baselines see the burst pools but never request them.

| project | nominal quota (borrowing unlimited) | burst budget per day | lateness cost per hour | manual priority bonus |
| ------- | ----------------------------------- | -------------------- | ---------------------- | --------------------- |
| showA   | cpu 110, gpu 8                      | $2000                | $600                   | 4                     |
| showB   | cpu 80, gpu 6                       | $1200                | $450                   | 2                     |
| showC   | cpu 50, gpu 4                       | $800                 | $300                   | 0                     |
| rnd     | gpu 14                              | $0                   | $0                     | 0                     |
| eng     | ci 32                               | $0                   | $0                     | 0                     |

Manual baseline priorities by job type: ci 90, dailies 60, delivery 70, ml 30, preview 50, plus the show bonus. Quotas apply only to the objective policy; the baselines have none.

### Workload

```
Days: 7
DailiesPerEvening: map[showA:9 showB:7 showC:5]
DailiesSubmitMean: 19
DailiesSubmitSD: 1.3
DailiesSubmitLo: 15
DailiesSubmitHi: 23.5
PreviewPerHour: map[showA:2 showB:1.5 showC:1]
PreviewCost: 0.35
PreviewTurn: 3
DeliveriesPerShow: map[showA:6 showB:4 showC:3]
DeliveryCost: 1.6
DeliveryFrames: 150
FramesMedian: 100
FramesSigma: 0.5
FramesMin: 24
FramesMax: 400
CPUFrameMin: 45
GPUFrameMin: 12
JobSigma: 0.6
FrameSigma: 0.2
TrendAmp: 0.15
HeavyProb: 0.4
HeavyLenLo: 0.08
HeavyLenHi: 0.25
HeavyMults: [2 3 5 8]
GPUShare: 0.25
DeliveryGPUShare: 0.2
ExpandShare: 0.3
DeliveryExpandShare: 0.4
EnvCost: 0.2
VolumesCost: 0.3
PassSigma: 0.1
VolumesProbLo: 0.3
VolumesProbHi: 0.9
UsdExportSec: 45
UsdExportSigma: 0.3
RenderFailProb: 0.02
CIRatePerHour: 30
CIBuildMin: 3
CIBuildSigma: 0.3
CIShardsBase: 4
CIShardsPoisson: 10
CITargets: 400
CITargetMin: 1.5
CITargetSigma: 0.6
CIRunSigma: 0.15
CIFailProb: 0.02
CISLO: 15
CIMergeDayFactor: 1.3
MLWeekdayRuns: 4
MLWeekendRuns: 2
MLGPUs: [1 2 4 8]
MLGPUWeights: [0.2 0.35 0.35 0.1]
MLHoursMedian: 5
MLHoursSigma: 0.5
MLCkptMinLo: 5
MLCkptMinHi: 15
MLRestoreMin: 3
MLHazardPerHour: 0.005
MLDeadlineHours: 36
MLDeclaredSigma: 0.15
```

Render jobs whose frames are generators export the frame's USD stage (a Solaris USD export, 45 s median, on the job's pool) and expand into a deep environment layer (0.2x the frame's cost), a deep volumes layer with a per-job probability (0.3x), and a beauty layer that holds out against both, so it depends on both. CI pipelines are a build that discovers 4 + Poisson(10) shards from a fixed monorepo of 400 test targets, each with its own duration and failure rate; shards run in order of failure rate. Failures happen per attempt (render and CI 2%, the fourth attempt always succeeds; ML runs at 0.5% per hour of progress) and are drawn from the seed and the unit, so every policy sees the same failures. ML runs checkpoint at a per-run interval; a yield waits for the next checkpoint and loses only restore time, a kill loses the work since the last checkpoint.

The estimator's history priors are built from 2,400 render jobs and 3,000 CI pipelines drawn from the same generator with a seed no experiment uses, so they describe the population a job comes from, not the job. An ML run's prior is its declared length (steps times historical step time), off by a lognormal factor with sigma 0.15.

### Policy settings

```
StarvationAfter: 30m0s
StarvationDeferMax: 2h0m0s
TieBand: 0.1
InfoUnitsPerJob: 0
PreemptSlack: 2h0m0s
PreemptWindow: 10m0s
PreemptPerWindow: 8
MaxPreemptionsPerUnit: 2
MaxLostWork: 15m0s
ScaleDownAfter: 10m0s
Burst: true
MinRemaining: 5m0s
TailAware: true
Planner: fluid-edf
NoInfoTier: false
Surplus: least-slack
SlackBand: 1h0m0s
RateSlack: 24h0m0s
WranglerEvery: 2h0m0s
WranglerMargin: 1h0m0s
WranglerUnbump: 3h0m0s
WranglerStartAlarm: 4h0m0s
```

Simulation: a decision at most every 10 s after a change and at least every 60 s; a job's estimate is refreshed on change or every 300 s while it runs. The Manual wrangler reviews every 2 hours.

### Binary-order experiment

```
Frames: [120 240 480]
Mults: [1 2 3 5 8]
Slots: 16
BaseMin: 20
JobSigma: 0.4
FrameSigma: 0.2
TrendAmp: 0.15
HeavyLenLo: 0.08
HeavyLenHi: 0.25
Trials: 40
SlackFrac: 0.5
SettleFrac: 0.1
GridPoints: 99
Seed: 45477
HeavyFactor: 3
PriorSamples: 20000
CalTrials: 200
```

## Discussion

### Summary

- On render deadlines the objective policy without burst ties EDF: per seed, 0.4 ± 5.2 fewer missed deadlines and 1.7 ± 9.5 more late hours, with fewer late hours than EDF on 2 of 5 seeds. It does not beat EDF there. Both are far ahead of FIFO and of the Manual baseline (about 19 to 23 fewer missed deadlines a week).
- With burst, the objective policy has fewer late hours than EDF on 4 of 5 seeds (9.3 ± 14.8 fewer per week, and 3.4 ± 7.6 fewer missed deadlines) for $486 ± 289 a week of cloud capacity, with no burst waiting for a person's approval. At the projects' lateness prices ($300 to $600 an hour) the hours saved are worth several times the spend, but the per-seed spread is wide.
- CI is where the policy helps without spending: p95 time to green falls from 17.2 to 15.5 minutes, and pipelines over the 15-minute SLO from 9.1% to 5.7%. Turning the information tier off (builds that discover shards are released ahead of other pipelines' spare shards) changes little now (15.5 minutes, 5.7%).
- DESIGN.md section 9.9 as written does clearly worse than EDF: 13.6 ± 13.8 more missed deadlines a week. The policy that was measured differs from it in the ways listed below, each one found by tracing a specific late job in this simulator. DESIGN.md has not been updated to match; it should be.
- When GPUs are scarce (the half-GPU scenario), the objective policy meets ML deadlines that EDF and Manual miss (0.6 against 4.4 and 5.4 a week) by having training runs yield at checkpoints, and without burst it also misses fewer render deadlines than EDF (39.0 against 49.2 a week) with fewer render late hours (259 against 382), though the late hours are dominated by one or two weeks. With burst it cuts GPU render late hours from 204 to 2.6 a week for about $976.
- Binary order flags heavy regions much earlier than sequential order (median at 8 to 15 percent of the render against 30 to 46 percent) and keeps the remaining-time error lower for most of the render. It costs makespan when shots have heavy regions (6 to 13 percent longer on average at 3x to 8x), and its slack estimate settles near the end of the render later than sequential order's.

### How the measured policy differs from DESIGN.md 9.9, and why

1. **Required slots are planned per pool group, not per job.** DESIGN.md sets R = W90/T for each job on its own. That treats every job as if the farm's future were its alone. On Wednesday night of seed 1 the Friday deliveries each claimed a steady share of the farm while that night's dailies needed all of it, and because no single job looked infeasible against the whole pool, the overload rule never fired: water-filling by running/R spread the shortage over every job and all of them finished slightly late. The measured policy plans each group of pools as one earliest-deadline-first fluid schedule, uses Moore-Hodgson to pick the set of jobs that can all be on time (the classic algorithm that minimizes the number of late jobs, which is what the overload rule asks for), and sets each job's R to what that schedule gives it now. A job due on Friday gets R = 0 on Wednesday night if it is on time after the dailies finish. The ablation "DESIGN 9.9 as written" keeps the per-job rule.
2. **R leaves room for the last wave.** With R = W90/T a job holding exactly R slots starts its longest units too late (list scheduling finishes within W/R plus the longest unit). R is computed against T minus the critical path. In seed 1 this alone cut dailies misses from 20 to 9 before the change above.
3. **Spare capacity goes by least planned slack, with fair share as the tiebreaker.** DESIGN.md gives spare capacity to projects by fair share first. The project's requirements say "the jobs most at risk get capacity first, with fair share across projects as the tiebreaker", and that is what was built. The difference is small here (ablation: fair share first had 2.7 fewer late hours, within the seed spread).
4. **Quota is held back only for deadline need.** An earlier version reserved a project's unused nominal quota whenever it had runnable work, so a show's Friday deliveries blocked another show's urgent dailies from borrowing. Kueue lends all unused quota and takes it back by preemption; the measured policy holds back only what a project needs now for jobs below their required rate.
5. **Preemption moves allocations toward the plan and never kills more than 15 minutes of work.** A victim must still hold its planned share, rounded up to whole slots, without the unit; the job gaining slots must be at least a whole slot short of min(R, share); and a unit that would lose more than 15 minutes (time since its last checkpoint plus restore cost) is not a victim. The one-slot band between the two rules keeps two jobs from taking a unit back and forth: before it, jobs whose shares were fractions of a slot traded one slot on every decision, and on seed 1 the policy lost about 330 CPU hours more than EDF to killed frames. It preempts about 570 units a week; lost CPU hours are 217 against EDF's 184, and lost GPU hours 23 against 18, out of about 1,935 useful GPU hours.
6. **Estimator.** A key with no completed unit in the job now uses history for that key before the job's median over other keys: a beauty pass just revealed by a generator was being predicted from 45-second generator evaluations, putting a 150 slot-hour job at 2 slot-hours. Running units that have run longer than their prediction are entered as censored observations (duration at least elapsed, assumed 1.25 times elapsed, the same overrun rule DESIGN.md uses for a running unit's remainder). Without this, binary order sampled frames inside an 8x region within minutes, but the estimator learned nothing until those 4.6-hour frames finished.

The simulation also caught a nondeterminism bug: summing region errors over a Go map made results differ in the last digit between runs, enough to flip decisions. Two full runs now produce byte-identical CSVs.

### Where a baseline does as well or better, and why

- **EDF on render deadlines.** On this workload the binding constraint is the nightly crunch, where most work shares one deadline (08:00), and earliest deadline first already serves it in the right order. The objective policy adds quotas, preemption and fair-share tie-breaking, which cost a little work and do not change the order much. The by-show table shows no fairness gain from quotas either: EDF's late hours per show are as even as the objective policy's.
- **EDF under GPU overload, on late hours (no longer).** The objective policy trailed EDF here until two fixes found by the stress grid (`hack/stress`): a training gang larger than its project's GPU quota had free GPUs held back for it that it could never use, and a gang starving for a slot drained the pool before the deadline tier, stopping deadline renders for hours. Now a gang that cannot fit its project's guarantee holds nothing back, and a starving gang drains the pool only after deadline work is served. Moore-Hodgson still minimizes the number of late jobs rather than late hours, so the few jobs it drops can still be very late in a heavy week.
- **Lost work.** EDF, FIFO and Manual preempt almost nothing, so they lose only what failures lose.
- **Manual** is a competent wrangler with Deadline-style static priorities: deliveries above dailies above previews. Those priorities put Friday deliveries ahead of the dailies on Wednesday and Thursday nights, and the two-hourly review cannot recover a job that fell behind early in the night. That, not a straw-man rule set, is why it trails EDF.

### Binary order

- **Detection.** Every 3x, 5x and 8x region was flagged in both orders; binary order flagged it at a median of 15, 11 and 8 percent of the render, sequential order at 46, 35 and 30 percent. Binary order produced no false flags; sequential order produced them in up to 6 percent of trials (a one-sided estimate near a heavy edge). At 2x, below the 3x threshold, flags are expected to be rare and are.
- **Error.** Over the whole render the mean relative error of remaining time was 0.15 (binary), 0.21 (sequential with the same estimator) and 0.19 (sequential with the render-manager mean) on shots without a heavy region, and 0.39, 1.06 and 0.78 on 8x shots. The slack estimate was within 10% of the true slack for 65% of the render under binary order against 42% and 46% for the sequential arms on plain shots, and 25% against 20% and 13% on 8x shots.
- **Sequential with the estimator is worse than the plain mean in heavy shots.** Once sequential order enters a heavy region, the estimator's one-sided rule (median of the three nearest frames) assumes the rest of the shot is as heavy as the last frames, which overestimates by 54% on average at 8x. The plain mean dilutes the heavy frames instead. This is a weakness of the one-sided fallback, visible only in sequential order.
- **Makespan.** Binary order leaves frames from everywhere in the shot, heavy ones included, to its finest levels, so the last wave on a fixed slot count can hold long frames that start late. Binary renders were longer than sequential ones by 6%, 11% and 13% on average at 3x, 5x and 8x (no difference on plain shots). Starting the longest known frames of each level first would likely recover most of this; it was not tried.
- **Settling.** For heavy shots the binary estimate settles inside the 10% band later in the render, or not at all before the end, more often than sequential (at 8x: 54% of trials settle before the end against 68%). Near the end, heavy frames still running make the remainder lumpy under binary order, while sequential order has left its heavy region behind.
- **Calibration.** As built (W + 1.28 Sigma), W90 covered the true remaining work 88% of the time on plain shots, close to the intended 90%, but only 68% to 83% on shots with a heavy region of 3x or more: the error bars are too narrow when part of the cost has not been sampled yet. Calibrated against history shots in strata the estimator can see (`estimate.Calibration`), binary-order W90 covers 87 to 93% for plain, 3x, 5x and 8x shots in every band from a quarter of the frames finished on, once a frame of the heavy region has finished. Before that, a 3x and an 8x region look the same to any estimator, and calibration covers 90% of the mix: plain and 3x shots are over-covered and 8x shots under-covered in the first quarter of the frames. Sequential order reveals a heavy region only when it reaches it, so the same strata calibrate it less well (75% at 8x).
- Both orders underestimate remaining time by about 8% on plain shots: max(W/P, t_path) leaves out the tail of a list schedule, the same effect item 2 above corrects for in R.

### ETA bands and suggested deadlines (out/eta.md)

- **The band a person is shown must be calibrated on the finish, not only on the work.** Recorded at each job's first plan, at binary order's information points and hourly, over five seeds, and compared with the actual finish: the band built from the scheduler's ETAs (Graham's bound since sched's tail hybrid), with its share of the critical path replaced by half of it, covered 45/50/53/55% of finishes at P50/P80/P90/P95. Calibrating W (as `estimate.Calibration` does) raised that only to 49/56/60/64%. The plan assumes each job gets its planned share of a fluid schedule, and under the nightly crunch that is optimistic: dailies' own ETA50 and ETA90 cover 40% and 69% of their finishes, deliveries' 24% and 37%. A conformal step on the finish itself (`forecast.FinishCalibration`: the log of actual remaining time over the shown median, per class and stage, learned from an earlier week) brings the band to 50/79/89/94%. It holds at every stage, from nothing finished (51/79/88/93%) to the last quarter (52/80/90/95%), and for every kind within a few points, except deliveries at 44/73/86/95%.
- **The isolated-job rule had to change for single-wave jobs.** W/P plus half the longest unit was unbiased for render jobs with many units per slot, but a CI pipeline or a short preview runs every unit in one wave. Taking back half the critical path halved their remaining time, so the band's floor is now the whole critical path.
- **Generous suggestions reach 95% on time; 0.95 is the lowest quantile that does.** Previews, whose workload deadline is three hours after submission, are on time 93% of the time with a median padding of 3.95 times their run time. When the person instead guesses "an hour after submission", previews are on time 66% of the time and ML runs miss 21.6 deadlines a week. A suggestion is the earliest deadline the job meets at the policy quantile without making any job that is on time now late. Set once at 0.8 (the earlier default), previews are on time 86% of the time at a padding of 3.09. Set once at 0.95, 95% at 6.85; at 0.98, 97% at 9.96. Moved later only when at risk (0.95, `forecast.Suggestion` with up to two moves), 99% at 18.25, with 0.80 at-risk moves per preview suggestion. ML runs are on time in every generous arm, at a padding of 0.87 to 1.36, and 94% at 0.8. The band stops at P95, so the 0.98 arm extrapolates the tail past P95 (normal in log time-left); `forecast.Band.At` returns P95 for any quantile above 0.925, so a 0.98 policy in the queue today would suggest what 0.95 does. The earlier rule, revising either way when the band narrowed, gave 79% at a padding of 1.71 (17e6ab1) and is no longer in `forecast`.
- **A person tightening to the next session changes nothing on the main workload.** The generous deadline of a daytime preview is already before the next 08:00, so no preview was tightened; 12% of ML suggestions were, and every ML run was still on time.
- **No collateral effect on the main workload.** Across the arms dailies miss 3.2 to 3.8 a week, deliveries 0.4, and burst spend is $3,649 to $4,050; median preview turnaround is 0.65 to 0.72 hours. Previews are small and daytime capacity is ample, so the deadline changes what is promised, not when the work is done.
- **Under contention, suggestions cost dailies less than the workload's deadlines do.** The `contention` variant (out/eta.md section 3) makes previews about three times the work, half again as frequent in the day, and adds previews from 17:00 to 22:00 while dailies are submitted: 350 previews a week instead of 186. With the workload's three-hour deadlines, 56% of previews are on time and dailies miss 7.8 a week with 43.0 late hours (3.2 and 13.6 on the main workload). Guessing "an hour after submission" puts 15% of previews on time and leaves dailies at 7.6 and 42.1: here the false urgency made previews later, not dailies, and burst spend fell from $7,748 to $6,373. Suggested deadlines put 84% (0.8), 99% (0.95), 99% (0.98) and 100% (at risk) of previews on time, and dailies miss 4.6, 6.4, 4.2 and 6.0 a week with 27.8 to 41.3 late hours. Burst spend is $9,937 at 0.8, $9,119 at 0.95, $8,735 when moved at risk and $7,346 at 0.98, the only suggested arm below the workload's deadlines. These collateral figures moved by up to one missed daily a week between two runs of the same arms (before and after sched's tail hybrid, which changes no decision but does change the ETAs the suggestions are computed from), so differences between the suggested arms are within that noise; the gap to the workload's deadlines (7.8) is larger than it at 0.8 and 0.98 and about equal to it at 0.95. A suggestion rejects any deadline under which a job that is on time now becomes at risk, so at the moment it is set a preview's deadline never asks the scheduler to make an on-time daily late.
- **Tightening to the session puts previews on the dailies' deadline.** Under contention a person moving the 0.95 suggestion to the next 08:00 session tightens 58% of previews, onto the dailies' own deadline. Previews stay 98% on time; dailies miss 6.6 a week with 38.1 late hours (6.4 and 41.3 without the tightening, within the noise above) and burst spend is $9,271. An earlier run had the tightening cost about one missed daily a week; with this noise the measurement does not settle whether it does. Deliveries late hours are 7.1 a week with the workload's deadlines and 3.5 to 4.5 in every other arm, from 0.6 against 0.2 to 0.4 missed deliveries a week: a handful of deliveries over five seeds.
- **Cost.** A suggestion set once runs the policy 14 times, 15 with the person's tightening. Moving it when at risk checks the shown band at each information point and searches again when at risk: 31 policy runs per preview suggestion on the main workload and 23 under contention. An ML run is one unit, so it has no information point before it finishes, so their suggestions are never checked or moved.

### What the simulation does not show

- The workload is synthetic. Parameters were chosen to look like a mid-size studio, not fitted to a real farm's logs.
- The main scenario's GPU pool runs at about 36% utilization, so ML and GPU rendering seldom contend there; ML lost GPU hours under the objective policy equal EDF's, so no training run yielded in it. The half-GPU scenario is the only evidence for the yield-at-checkpoint behavior, and its late-hour figures are dominated by one or two seeds (the standard deviations exceed the means).
- Spot interruptions, inter-job dependencies (deadline propagation up a dependency graph), pod start-up time and cache warm-up on owned nodes are not modeled. Burst suggestions that need approval are never approved. Quotas apply only to the objective policy.
- Decisions are coalesced to at most one every 10 seconds; the controller design coalesces to one per second.
- Five seeds are few for metrics with this much week-to-week spread. The paired comparisons are the fairer reading.

### Calibration and changes after seeing results

- The CI pool was sized at 24 runners in the initial run. Every policy then had a CI p95 of about 60 minutes, which did not match the intended peak load of about 90%, so the pool was raised to 32. No other workload, farm or baseline parameter was changed after results were seen.
- The objective policy and the estimator were changed after results were seen, as listed above, and each change is in the code with its reason. The ablation rows keep the earlier rules runnable (`sched.Config`: `Planner`, `TailAware`, `Surplus`, `NoInfoTier`), so the effect of the main changes can be checked with the same command.
- The binary-order experiment's parameters were set before it was run and were not changed.
