# Benchmark — stage2-harness-check

`2026-09-08 01:07 UTC` · code `b35b9a6` (uncommitted changes)

## Configuration

| | |
|---|---|
| Command | `loadgen -mode=charge -n=600 -c=100 -dupes=1 -key=sk_test_f9bc15e2ac7c63d9ca68260c900edde0c0f2429f8aa1b5e4,sk_test_f6431efb2d0cdc65d86ffda4e3aa8d3716372f5172b2aba0,sk_test_1d4f07f160d912c4e85fb67634eb648aef964bdf19e299fb,sk_test_240961706645bf10eb0a701514a772860fd6efc9abc9fea8,sk_test_2e31edf5f3149e5c01ec59604a000bb9e507b8e6597db679,sk_test_aa2270fd7badce19f37c7dffd20e6373c0891b50ff91f812,sk_test_1f54f77404186ea13051e573214e7a350dbcd206ed38afd3,sk_test_767f08ff26d14aaf72ae8a7b0daafdd1db7683f13422abfa,sk_test_51fa55ac829ef4cc4cc0c67fe5d2f663723ff76e186e6122,sk_test_f654ab8285d6f954e886b4c90d330bb8d2900d9f610eeebd,sk_test_57d2c5459ca60c633f3d28060a4795b7df11ed867440af75,sk_test_f1c4a25110c763cf0b6114a2d1e5f91d5ec1f42b310482f0,sk_test_20fabead28f4e57c7ab22b2674388189b6e86682b6e2d2a0,sk_test_2ffc87d525cb5743ecca59dd1c07aeabb7764feee2e5c2b9,sk_test_e256e824a997ff1aeb34fb4baca4b24fd802aa4b6558a8f4,sk_test_8357afcce80d077d65934f6bc8cd940c9f37bd94f86fa473,sk_test_993d2159a8accdc92ca3d8cbc36c742200c643b1f6a8f4a6,sk_test_cfed507bcaaaefdd5eebc68b596655e078b2656fbd30b728,sk_test_0b143e22cdb1d93180294112f233b4205586a665cfae23a3,sk_test_f21cb219d413d44e44737088caf5827533cd60a1d375c3c9,sk_test_bad2611dac9c846f3ecf593df9c5dc7d342e543d0548ee86,sk_test_1354732d92a9b8f502d056351c04642e88600cee9c3c254f,sk_test_533a94419b24e30ba47b65f7cf8cd4a631d372f7a0814ba4,sk_test_c11100332dc6a728d7310341f24d5ccc3e89ff815db79528` |
| Dataset | 55370 charges, 100402 ledger entries, 27 merchants |
| Hardware | 8 cores, load generator on the same machine |
| DB_MAX_CONNS | 50 |
| Acquirer | simulator, ~24ms p50, 95/3/1.5/0.5 success/decline/timeout/error |

## Result

```
──────────────────────────────────────────────────
  mode           charge
  requests       600 in 11.989s
  throughput     50 req/s
  latency        p50 849ms   p95 2.189s   p99 2.189s
  ok             586
  replayed       0   (idempotent hits — these did NOT charge again)
  in-flight 409  0   (concurrent duplicate, correctly refused)
  distinct idempotency keys sent: 600
  → charges in the database must equal that number, not 600.
  rate limited   0
  failed         14
──────────────────────────────────────────────────
```

## System under load

| | Peak |
|---|---|
| Postgres CPU | 209% |
| API resident memory | 44.7 MB |
| Active DB connections | 51 |
| Queries **waiting on a lock** | 46 |
| Longest running query | 1606 ms |

## Most expensive statements

- pg_stat_statements not enabled
