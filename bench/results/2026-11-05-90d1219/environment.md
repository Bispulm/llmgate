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
