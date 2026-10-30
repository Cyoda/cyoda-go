# Time reads

- Business time (entity stamps, transition times, scheduled due times) comes from the injected `spi.Clock`, never `time.Now()` or the database clock.
- A read that measures real time (timeout, lease, heartbeat, auth validity, metrics, throttling) carries `// realtime: <reason>` on its line or the line above (`-- realtime:` in `.sql`).
- The database clock is for liveness and the production commit stamp (`cyoda_stamp`, `cyoda_consistency_time`) only, and is the production fallback when no business clock is injected (`plugins/postgres/entity_store.go`, `plugins/postgres/platform_clock.go`), marked `// realtime:`.
- `internal/platformclock/time_reads_test.go` enforces this on Go time reads in the root module and the three in-tree plugins, and on SQL time functions in postgres Go strings and in postgres migrations from 000017 on.
