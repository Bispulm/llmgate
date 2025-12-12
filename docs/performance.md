# Measuring performance

What the gateway adds is measured, not estimated. The method keeps everything the same except the thing being measured, and records enough about each run to tell a clean measurement from a contended one.

## Running it

```bash
make dev-up          # Postgres and Redis
./scripts/bench.sh   # About five minutes
```

Results go to `bench/results/<date>-<commit>/`: one JSON file per run, the conditions of the run, a CPU profile of tenant mode, and a `README.md` with the tables. Settings are `BENCH_CONCURRENCY` (16), `BENCH_DURATION` (20s), `BENCH_WARMUP` (5s), and `BENCH_STREAMS` (200).

## What's compared

Every run uses the same mock, payload, concurrency, duration, and warm-up. Only where the requests go changes.

| Run | Path |
|---|---|
| `direct` | Straight to the mock |
| `dev` | Through the gateway in development mode: validation, routing, and the adapter, with no authentication store, limits, or accounting |
| `tenant` | Through the gateway in tenant mode: key lookup, team limits, reservation, dispatch, and settlement in Postgres |

The difference between `direct` and `dev` is what proxying costs. The difference between `dev` and `tenant` is what authentication, limits, and accounting cost. Each comparison is run for non-streaming requests, and for streams, where time to first byte matters most.

The script also measures:

| Measurement | How |
|---|---|
| Failover latency | Every request first fails on a route answering 500, then succeeds on the next, compared with going straight to the working route |
| Memory and goroutines per active stream | The gateway's own heap and goroutine counts, idle and with 200 slow streams open |
| Fairness | A provider allowing 8 attempts at once, a noisy team keeping 64 requests in flight, and a quiet team keeping 2: the quiet team's latency alone, then beside the noisy one |
| Where tenant mode spends CPU | A 10-second CPU profile during the tenant run |
| Liability accuracy | Every attempt's reservation against its charge, how many were charged conservatively or above their reservation, and whether every budget agrees with the ledger |

## The harness

`llmgate-bench` sends requests from a fixed number of workers for a fixed time, after a warm-up whose requests are sent but not counted. For each response it records the time to the first byte of the body, which for a stream is its first event, and the time to read the whole response. It reports nearest-rank percentiles, throughput, and errors by status, as JSON. `llmgate-bench compare` puts several runs in a table, with what each adds to the first. The API key comes from `LLMGATE_BENCH_KEY` and never appears in the output.

Three things to keep in mind when reading the numbers:

- **It's closed-loop.** Each worker waits for a response before sending the next request, so when the gateway slows down, fewer requests are sent. That understates tail latency under overload compared with a fixed arrival rate. The saturation sweep shows where throughput stops growing, which is where this matters.
- **Failed requests are counted, not timed.** Errors are reported by status, and only successful responses go into the percentiles. Any run with errors says so in its table.
- **"Added" latency is a difference of percentiles**, such as the gateway's p99 minus the mock's p99, not the percentile of a per-request difference.

The CPU profile is taken during the tenant-mode run only, which costs that run a little.

## Conditions

Each run records the commit, the Go version, the operating system, the CPU, its cores and memory, how Postgres and Redis were run, the workload, the load average before it started, and the CPU other processes used during it, sampled every 10 seconds. The benchmark loads the machine itself, so the load average during the run says nothing about other work. A run is labeled **contended** if the load average before it exceeded a quarter of the cores, or other processes averaged more than one core, and its report says so at the top. A contended run isn't presented as a measurement of the gateway.

Postgres and Redis run in containers, which on macOS means inside a VM, so tenant mode's numbers include that network hop. They are a property of this setup, not of a production deployment.

## Published results

Two uncontended runs on 2026-11-05, on an Apple M4 Pro with 14 cores and 24 GiB, macOS, Go 1.27.3, with Postgres and Redis in Compose containers inside the Docker VM. The full reports, conditions, and raw JSON are in [`bench/results/2026-11-05-90d1219`](../bench/results/2026-11-05-90d1219/README.md) and [`bench/results/2026-11-05-02b9afd`](../bench/results/2026-11-05-02b9afd/README.md). Earlier, contended runs aren't published.

| What | Run 90d1219 | Run 02b9afd |
|---|---|---|
| Development mode, added latency, non-streaming, 16 in flight | +0.35 ms p50, +1.00 ms p99 | +0.32 ms p50, +2.01 ms p99 |
| Development mode, added time to first byte, streaming | +0.66 ms p50 | +0.75 ms p50 |
| Development mode, throughput at 256 in flight | 50,982 req/s | 53,146 req/s |
| Tenant mode, one team, 1 in flight | 3.7 ms p50, 264 req/s | 4.4 ms p50, 226 req/s |
| Tenant mode, one team, 16 in flight | 1,017 req/s, 15.0 ms p50 | 540 to 927 req/s, 16.6 to 24.1 ms p50 |
| Tenant mode, eight teams, 32 in flight | 2,044 req/s | 2,021 req/s |
| Failover before commit, added latency, when the failing route answers at once | +0.0 ms p50, +0.3 ms p99 | +0.0 ms p50, within noise at p99 |
| Memory per active stream | not valid in this run | 75 KiB of heap, 4 goroutines |
| A quiet team beside a saturating one, added p99 | +0.84 ms | +0.69 ms |
| Attempts settled, charged conservatively, over their reservation, budgets disagreeing with the ledger | 171,176, 0, 0, 0 | 149,579, 0, 0, 0 |
| Charges as a share of reservations | 19% | 19% |

What the numbers say:

- **Proxying adds about a third of a millisecond at the median**, and one to two milliseconds at p99, against a mock that answers in 0.17 ms. The mock is far faster than any real provider, so in practice this is a small fraction of a request.
- **Tenant mode's cost is Postgres round trips.** Each request reserves, marks dispatched, and settles, and here each round trip crosses into a VM. One team's admissions take turns on its budget row, so a single team levels off around 1,000 requests a second on this setup, and adding concurrency past that only adds latency. More teams admit in parallel, up to about 2,000 a second here, where Postgres in the VM becomes the limit.
- **Tenant mode varies between runs**, from 540 to 1,017 requests a second for one team at 16 in flight, even with the machine otherwise quiet. Development mode doesn't vary much, so the variance comes from Postgres and the VM it runs in.
- **Reservations are conservative.** Charges came to about a fifth of what was reserved, because the bound assumes every byte of the prompt is a token and the whole output limit is used. No attempt cost more than its reservation.
- **Fairness holds.** A team keeping 64 requests in flight waits in its own queue, about 300 ms here, while a team with 2 in flight sees under a millisecond of difference.
