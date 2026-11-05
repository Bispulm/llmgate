# Benchmark results, 02b9afd

# Run conditions

| | |
|---|---|
| Date (UTC) | 2026-11-05T21:44:07Z |
| Commit | 02b9afd |
| Go | go1.27.3 |
| OS | Darwin 25.6.0 |
| CPU | Apple M4 Pro |
| Cores | 14 |
| Memory | 24 GiB |
| Postgres and Redis | Compose containers (postgres:17-alpine, redis), through the Docker VM |
| Workload | concurrency 16, 20s measured after 5s warm-up, 200 streams for memory |
| Load average before | 1.65 |
| Other processes' CPU during the run | average 95%, peak 372%, sampled every 10 seconds |
| Contended | no (contended if the load average before the run exceeded a quarter of the cores, or other processes averaged more than one core) |

## Added latency, non-streaming

| Run | Requests | OK | Errors | Req/s | p50 ms | p95 ms | p99 ms | TTFB p50 ms | TTFB p95 ms | Added p50 | Added p95 | Added p99 | Added TTFB p50 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| direct | 1628709 | 1628709 | none | 81435 | 0.17 | 0.36 | 0.50 | 0.17 | 0.35 | +0.00 | +0.00 | +0.00 | +0.00 |
| dev | 537341 | 537341 | none | 26867 | 0.50 | 1.14 | 2.50 | 0.49 | 1.13 | +0.32 | +0.77 | +2.01 | +0.33 |
| tenant | 10806 | 10806 | none | 540 | 24.05 | 49.66 | 71.73 | 24.04 | 49.65 | +23.87 | +49.29 | +71.23 | +23.87 |

## Added latency and time to first byte, streaming

| Run | Requests | OK | Errors | Req/s | p50 ms | p95 ms | p99 ms | TTFB p50 ms | TTFB p95 ms | Added p50 | Added p95 | Added p99 | Added TTFB p50 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| direct-stream | 314538 | 314538 | none | 15727 | 0.94 | 1.69 | 2.49 | 0.51 | 1.14 | +0.00 | +0.00 | +0.00 | +0.00 |
| dev-stream | 137562 | 137562 | none | 6878 | 1.85 | 4.65 | 13.82 | 1.26 | 2.61 | +0.92 | +2.96 | +11.33 | +0.75 |
| tenant-stream | 16507 | 16507 | none | 825 | 18.54 | 24.00 | 27.57 | 14.86 | 19.38 | +17.60 | +22.31 | +25.08 | +14.35 |

The difference between development and tenant mode is the cost of authentication, team limits, and accounting.

## Saturation

Throughput and latency as concurrency rises, 10 seconds each after 2 seconds of warm-up. One team's admissions take turns on its budget row, so eight teams show how tenant mode scales across teams. For eight teams, latency is the worst team's.

| Run | Req/s | p50 ms | p99 ms | Errors |
|---|---|---|---|---|
| dev-c1 | 6519 | 0.145 | 0.346 | 0 |
| dev-c4 | 18094 | 0.189 | 0.51 | 0 |
| dev-c16 | 28340 | 0.513 | 1.488 | 0 |
| dev-c64 | 39704 | 1.268 | 5.127 | 0 |
| dev-c256 | 53146 | 4.123 | 14.88 | 0 |
| tenant-c1 | 226 | 4.354 | 5.703 | 0 |
| tenant-c4 | 693 | 5.651 | 8.116 | 0 |
| tenant-c16 | 927 | 16.564 | 25.082 | 0 |
| tenant-c64 | 998 | 62.978 | 77.806 | 0 |
| tenant-c256 | 898 | 281.266 | 323.527 | 0 |
| 8 teams, 8 in flight | 1165 | 6.811 | 9.071 | 0 |
| 8 teams, 32 in flight | 2021 | 15.65 | 23.417 | 0 |
| 8 teams, 128 in flight | 2059 | 61.974 | 77.005 | 0 |

## Failover before commit

Every request first fails on a route answering 500, then succeeds on the next. Compared with going straight to the working route:

| Run | Requests | OK | Errors | Req/s | p50 ms | p95 ms | p99 ms | TTFB p50 ms | TTFB p95 ms | Added p50 | Added p95 | Added p99 | Added TTFB p50 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| dev | 537341 | 537341 | none | 26867 | 0.50 | 1.14 | 2.50 | 0.49 | 1.13 | +0.00 | +0.00 | +0.00 | +0.00 |
| dev-failover | 551177 | 551177 | none | 27559 | 0.53 | 1.15 | 1.54 | 0.52 | 1.14 | +0.03 | +0.01 | -0.96 | +0.03 |

## Memory per active stream

| | Idle | 200 streams | Per stream |
|---|---|---|---|
| Go heap in use | 2.9 MiB | 17.6 MiB | 75.1 KiB |
| Goroutines | 11 | 810 | 4.0 |

## Fairness under a saturated team

The provider allows 8 attempts at once. A noisy team keeps 64 requests in flight, and a quiet team 2. The quiet team alone, then beside the noisy one:

| Run | Requests | OK | Errors | Req/s | p50 ms | p95 ms | p99 ms | TTFB p50 ms | TTFB p95 ms | Added p50 | Added p95 | Added p99 | Added TTFB p50 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| quiet-alone | 1432 | 1432 | none | 72 | 28.00 | 28.77 | 29.56 | 27.99 | 28.76 | +0.00 | +0.00 | +0.00 | +0.00 |
| quiet-beside-noisy | 1419 | 1419 | none | 71 | 28.22 | 29.19 | 30.25 | 28.22 | 29.18 | +0.22 | +0.42 | +0.69 | +0.22 |
| noisy | 4277 | 4277 | none | 214 | 304.28 | 313.58 | 315.63 | 304.27 | 313.57 | +276.28 | +284.81 | +286.07 | +276.28 |

## Where tenant mode spends CPU

A 10-second CPU profile of the gateway during the non-streaming tenant run, top 25 functions:

```
File: llmgate
Type: cpu
Time: 2026-11-05 15:47:43 CST
Duration: 10.11s, Total samples = 2.66s (26.30%)
Showing nodes accounting for 2.45s, 92.11% of 2.66s total
Dropped 88 nodes (cum <= 0.01s)
Showing top 25 nodes out of 184
      flat  flat%   sum%        cum   cum%
     0.94s 35.34% 35.34%      0.94s 35.34%  syscall.rawsyscalln
     0.72s 27.07% 62.41%      0.72s 27.07%  runtime.kevent
     0.40s 15.04% 77.44%      0.40s 15.04%  runtime.pthread_cond_signal
     0.33s 12.41% 89.85%      0.33s 12.41%  runtime.pthread_cond_wait
     0.02s  0.75% 90.60%      0.02s  0.75%  internal/runtime/atomic.(*Int32).Load
     0.02s  0.75% 91.35%      0.02s  0.75%  runtime.(*m).clearAllpSnapshot
     0.01s  0.38% 91.73%      0.05s  1.88%  github.com/jackc/pgx/v5.(*dbTx).QueryRow
     0.01s  0.38% 92.11%      0.02s  0.75%  runtime.scanObjectsSmall
         0     0% 92.11%      0.13s  4.89%  _.goready.func1
         0     0% 92.11%      0.08s  3.01%  bufio.(*Reader).Peek
         0     0% 92.11%      0.08s  3.01%  bufio.(*Reader).fill
         0     0% 92.11%      0.16s  6.02%  bufio.(*Writer).Flush
         0     0% 92.11%      0.22s  8.27%  github.com/Bispulm/llmgate/internal/accounting.(*Ledger).Finish
         0     0% 92.11%      0.17s  6.39%  github.com/Bispulm/llmgate/internal/accounting.(*Ledger).Finish.func1
         0     0% 92.11%      0.10s  3.76%  github.com/Bispulm/llmgate/internal/accounting.(*Ledger).MarkDispatched
         0     0% 92.11%      0.11s  4.14%  github.com/Bispulm/llmgate/internal/accounting.(*Ledger).Reserve
         0     0% 92.11%      0.10s  3.76%  github.com/Bispulm/llmgate/internal/accounting.(*Ledger).reserveInOneStatement
         0     0% 92.11%      0.10s  3.76%  github.com/Bispulm/llmgate/internal/accounting.(*LogRecorder).RecordAttempt
         0     0% 92.11%      0.05s  1.88%  github.com/Bispulm/llmgate/internal/accounting.lockAttempt
         0     0% 92.11%      0.12s  4.51%  github.com/Bispulm/llmgate/internal/accounting.settle
         0     0% 92.11%      0.93s 34.96%  github.com/Bispulm/llmgate/internal/api.(*Server).ServeHTTP
         0     0% 92.11%      0.15s  5.64%  github.com/Bispulm/llmgate/internal/api.(*Server).admitTeam
         0     0% 92.11%      0.53s 19.92%  github.com/Bispulm/llmgate/internal/api.(*Server).attempt
         0     0% 92.11%      0.43s 16.17%  github.com/Bispulm/llmgate/internal/api.(*Server).attempt.func1
         0     0% 92.11%      0.74s 27.82%  github.com/Bispulm/llmgate/internal/api.(*Server).attemptWithSlot
```

## Liability and settlement

Every attempt from the tenant-mode runs, by final state. Reserved is what was held before each attempt, and charged is what it cost.

```
   team_id    |   state    | attempts | reserved_micros | charged_micros | conservative | over_reservation 
--------------+------------+----------+-----------------+----------------+--------------+------------------
 bench        | reconciled |    77520 |       108140400 |       20930400 |            0 |                0
 bench-noisy  | reconciled |     5422 |         7563690 |         487980 |            0 |                0
 bench-quiet  | reconciled |     3565 |         4973175 |         320850 |            0 |                0
 bench-team-1 | reconciled |     7868 |        10975860 |        2124360 |            0 |                0
 bench-team-2 | reconciled |     7872 |        10981440 |        2125440 |            0 |                0
 bench-team-3 | reconciled |     7898 |        11017710 |        2132460 |            0 |                0
 bench-team-4 | reconciled |     7881 |        10993995 |        2127870 |            0 |                0
 bench-team-5 | reconciled |     7898 |        11017710 |        2132460 |            0 |                0
 bench-team-6 | reconciled |     7869 |        10977255 |        2124630 |            0 |                0
 bench-team-7 | reconciled |     7905 |        11027475 |        2134350 |            0 |                0
 bench-team-8 | reconciled |     7881 |        10993995 |        2127870 |            0 |                0
(11 rows)

```

Budget periods whose totals disagree with the ledger: 0.
