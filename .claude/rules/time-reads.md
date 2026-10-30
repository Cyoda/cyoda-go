# Time reads

- Business time (entity stamps, transition times, scheduled due times) comes from the injected `spi.Clock`, never `time.Now()` or the database clock.
- A read that measures real time (timeout, lease, heartbeat, auth validity, metrics, throttling) carries `// realtime: <reason>` on its line or the line above (`-- realtime:` in `.sql`).
- The database clock is for liveness and the production commit stamp (`cyoda_stamp`, `cyoda_consistency_time`) only.
- `internal/platformclock/time_reads_test.go` enforces this on the root module and the three in-tree plugins.
