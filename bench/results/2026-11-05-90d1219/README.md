# Benchmark results, 90d1219

# Run conditions

| | |
|---|---|
| Date (UTC) | 2026-11-05T21:29:38Z |
| Commit | 90d1219 |
| Go | go1.27.3 |
| OS | Darwin 25.6.0 |
| CPU | Apple M4 Pro |
| Cores | 14 |
| Memory | 24 GiB |
| Postgres and Redis | Compose containers (postgres:17-alpine, redis), through the Docker VM |
| Workload | concurrency 16, 20s measured after 5s warm-up, 200 streams for memory |
| Load average before | 1.75 |
| Other processes' CPU during the run | average 82%, peak 272%, sampled every 10 seconds |
| Contended | no (contended if the load average before the run exceeded a quarter of the cores, or other processes averaged more than one core) |

## Added latency, non-streaming

| Run | Requests | OK | Errors | Req/s | p50 ms | p95 ms | p99 ms | TTFB p50 ms | TTFB p95 ms | Added p50 | Added p95 | Added p99 | Added TTFB p50 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| direct | 1611926 | 1611926 | none | 80596 | 0.18 | 0.36 | 0.50 | 0.17 | 0.35 | +0.00 | +0.00 | +0.00 | +0.00 |
| dev | 557951 | 557951 | none | 27898 | 0.52 | 1.13 | 1.50 | 0.52 | 1.12 | +0.35 | +0.76 | +1.00 | +0.35 |
| tenant | 20337 | 20337 | none | 1017 | 15.01 | 19.95 | 22.18 | 15.01 | 19.95 | +14.83 | +19.59 | +21.69 | +14.84 |

## Added latency and time to first byte, streaming

| Run | Requests | OK | Errors | Req/s | p50 ms | p95 ms | p99 ms | TTFB p50 ms | TTFB p95 ms | Added p50 | Added p95 | Added p99 | Added TTFB p50 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| direct-stream | 318767 | 318767 | none | 15938 | 0.92 | 1.75 | 2.41 | 0.55 | 1.18 | +0.00 | +0.00 | +0.00 | +0.00 |
| dev-stream | 174765 | 174765 | none | 8738 | 1.75 | 2.96 | 3.71 | 1.22 | 2.25 | +0.82 | +1.21 | +1.31 | +0.66 |
| tenant-stream | 20116 | 20116 | none | 1006 | 15.74 | 17.68 | 19.64 | 12.50 | 14.18 | +14.82 | +15.92 | +17.23 | +11.95 |

The difference between development and tenant mode is the cost of authentication, team limits, and accounting.

## Saturation

Throughput and latency as concurrency rises, 10 seconds each after 2 seconds of warm-up. One team's admissions take turns on its budget row, so eight teams show how tenant mode scales across teams. For eight teams, latency is the worst team's.

| Run | Req/s | p50 ms | p99 ms | Errors |
|---|---|---|---|---|
| dev-c1 | 4273 | 0.131 | 5.486 | 0 |
| dev-c4 | 14897 | 0.232 | 0.681 | 0 |
| dev-c16 | 27963 | 0.523 | 1.474 | 0 |
| dev-c64 | 40553 | 1.241 | 4.998 | 0 |
| dev-c256 | 50982 | 4.302 | 15.926 | 0 |
| tenant-c1 | 264 | 3.725 | 5.219 | 0 |
| tenant-c4 | 899 | 4.4 | 5.559 | 0 |
| tenant-c16 | 1043 | 15.14 | 19.71 | 0 |
| tenant-c64 | 993 | 63.619 | 73.877 | 0 |
| tenant-c256 | 914 | 278.456 | 300.482 | 0 |
| 8 teams, 8 in flight | 1165 | 6.754 | 9.972 | 0 |
| 8 teams, 32 in flight | 2044 | 15.402 | 25.047 | 0 |
| 8 teams, 128 in flight | 2013 | 62.918 | 83.744 | 0 |

## Failover before commit

Every request first fails on a route answering 500, then succeeds on the next. Compared with going straight to the working route:

| Run | Requests | OK | Errors | Req/s | p50 ms | p95 ms | p99 ms | TTFB p50 ms | TTFB p95 ms | Added p50 | Added p95 | Added p99 | Added TTFB p50 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| dev | 557951 | 557951 | none | 27898 | 0.52 | 1.13 | 1.50 | 0.52 | 1.12 | +0.00 | +0.00 | +0.00 | +0.00 |
| dev-failover | 558021 | 558021 | none | 27901 | 0.50 | 1.07 | 1.77 | 0.50 | 1.06 | -0.02 | -0.06 | +0.28 | -0.02 |

## Memory per active stream

**These memory numbers are wrong, and are kept only as a record.** The idle reading was taken straight after the saturation runs, while the heap still held their garbage and their connections' goroutines hadn't exited. The script was fixed to restart the gateway before measuring, and the run at `2026-11-05-02b9afd` has valid numbers.

| | Idle | 200 streams | Per stream |
|---|---|---|---|
| Go heap in use | 20.8 MiB | 19.8 MiB | -4.9 KiB |
| Goroutines | 522 | 923 | 2.0 |

## Fairness under a saturated team

The provider allows 8 attempts at once. A noisy team keeps 64 requests in flight, and a quiet team 2. The quiet team alone, then beside the noisy one:

| Run | Requests | OK | Errors | Req/s | p50 ms | p95 ms | p99 ms | TTFB p50 ms | TTFB p95 ms | Added p50 | Added p95 | Added p99 | Added TTFB p50 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| quiet-alone | 1444 | 1444 | none | 72 | 27.73 | 28.57 | 29.16 | 27.73 | 28.56 | +0.00 | +0.00 | +0.00 | +0.00 |
| quiet-beside-noisy | 1423 | 1423 | none | 71 | 28.15 | 29.10 | 30.00 | 28.14 | 29.09 | +0.41 | +0.53 | +0.84 | +0.41 |
| noisy | 4298 | 4298 | none | 215 | 303.48 | 313.51 | 316.54 | 303.48 | 313.48 | +275.75 | +284.94 | +287.39 | +275.75 |

## Where tenant mode spends CPU

A 10-second CPU profile of the gateway during the non-streaming tenant run, top 25 functions:

```
File: llmgate
Type: cpu
Time: 2026-11-05 15:33:10 CST
Duration: 10.10s, Total samples = 4.42s (43.74%)
Showing nodes accounting for 4.29s, 97.06% of 4.42s total
Dropped 56 nodes (cum <= 0.02s)
Showing top 25 nodes out of 190
      flat  flat%   sum%        cum   cum%
     1.59s 35.97% 35.97%      1.59s 35.97%  syscall.rawsyscalln
     1.36s 30.77% 66.74%      1.36s 30.77%  runtime.kevent
     0.65s 14.71% 81.45%      0.65s 14.71%  runtime.pthread_cond_signal
     0.53s 11.99% 93.44%      0.53s 11.99%  runtime.pthread_cond_wait
     0.11s  2.49% 95.93%      0.11s  2.49%  runtime.usleep
     0.03s  0.68% 96.61%      0.03s  0.68%  runtime.pthread_kill
     0.01s  0.23% 96.83%      0.03s  0.68%  runtime.scanblock
     0.01s  0.23% 97.06%      0.03s  0.68%  runtime.stealWork
         0     0% 97.06%      0.17s  3.85%  _.goready.func1
         0     0% 97.06%      0.33s  7.47%  bufio.(*Reader).Peek
         0     0% 97.06%      0.33s  7.47%  bufio.(*Reader).fill
         0     0% 97.06%      0.26s  5.88%  bufio.(*Writer).Flush
         0     0% 97.06%      0.43s  9.73%  github.com/Bispulm/llmgate/internal/accounting.(*Ledger).Finish
         0     0% 97.06%      0.25s  5.66%  github.com/Bispulm/llmgate/internal/accounting.(*Ledger).Finish.func1
         0     0% 97.06%      0.10s  2.26%  github.com/Bispulm/llmgate/internal/accounting.(*Ledger).MarkDispatched
         0     0% 97.06%      0.15s  3.39%  github.com/Bispulm/llmgate/internal/accounting.(*Ledger).Reserve
         0     0% 97.06%      0.15s  3.39%  github.com/Bispulm/llmgate/internal/accounting.(*Ledger).reserveInOneStatement
         0     0% 97.06%      0.14s  3.17%  github.com/Bispulm/llmgate/internal/accounting.(*LogRecorder).RecordAttempt
         0     0% 97.06%      0.08s  1.81%  github.com/Bispulm/llmgate/internal/accounting.lockAttempt
         0     0% 97.06%      0.17s  3.85%  github.com/Bispulm/llmgate/internal/accounting.settle
         0     0% 97.06%      1.36s 30.77%  github.com/Bispulm/llmgate/internal/api.(*Server).ServeHTTP
         0     0% 97.06%      0.22s  4.98%  github.com/Bispulm/llmgate/internal/api.(*Server).admitTeam
         0     0% 97.06%      0.81s 18.33%  github.com/Bispulm/llmgate/internal/api.(*Server).attempt
         0     0% 97.06%      0.68s 15.38%  github.com/Bispulm/llmgate/internal/api.(*Server).attempt.func1
         0     0% 97.06%      1.06s 23.98%  github.com/Bispulm/llmgate/internal/api.(*Server).attemptWithSlot
```

## Liability and settlement

Every attempt from the tenant-mode runs, by final state. Reserved is what was held before each attempt, and charged is what it cost.

```
   team_id    |   state    | attempts | reserved_micros | charged_micros | conservative | over_reservation 
--------------+------------+----------+-----------------+----------------+--------------+------------------
 bench        | reconciled |    99355 |       138600225 |       26825850 |            0 |                0
 bench-noisy  | reconciled |     5434 |         7580430 |         489060 |            0 |                0
 bench-quiet  | reconciled |     3583 |         4998285 |         322470 |            0 |                0
 bench-team-1 | reconciled |     7846 |        10945170 |        2118420 |            0 |                0
 bench-team-2 | reconciled |     7833 |        10927035 |        2114910 |            0 |                0
 bench-team-3 | reconciled |     7844 |        10942380 |        2117880 |            0 |                0
 bench-team-4 | reconciled |     7851 |        10952145 |        2119770 |            0 |                0
 bench-team-5 | reconciled |     7849 |        10949355 |        2119230 |            0 |                0
 bench-team-6 | reconciled |     7859 |        10963305 |        2121930 |            0 |                0
 bench-team-7 | reconciled |     7855 |        10957725 |        2120850 |            0 |                0
 bench-team-8 | reconciled |     7867 |        10974465 |        2124090 |            0 |                0
(11 rows)

```

Budget periods whose totals disagree with the ledger: 0.
