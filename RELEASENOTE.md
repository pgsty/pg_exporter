# Release Note


------

## 1.0.1

- Add dockerhub images: [pgsty/pg_exporter](https://hub.docker.com/r/pgsty/pg_exporter)
- Bump go dependencies to the latest version, build with go 1.24.5
- Disable `pg_tsdb_hypertable` collector by default, since `timescaledb` catalog is changed.

https://github.com/pgsty/pg_exporter/releases/tag/v1.0.1

------

## 1.0.0

Add PostgreSQL 18 metrics support

- new collector branch `pg_wal_18`:
  - remove `write`, `sync`, `write_time`, `sync_time` metrics
  - move to `pg_stat_io`
- new collector branch `pg_checkpointer_18`:
  - new metric `num_done`
  - new metric `slru_written`
- new collector branch `pg_db_18`:
  - new metric `parallel_workers_to_launch`
  - new metric `parallel_workers_launched`
- new collector branch `pg_table_18`:
  - `table_parallel_workers_to_launch`
  - `table_parallel_workers_launched`
- new collector branch `pg_io_18`:
  - new series about WAL statistics
  - new metric `read_bytes`
  - new metric `write_bytes`
  - new metric `extend_bytes`
  - remove `op_bytes` due to fixed value
- new collector branch `pg_vacuuming_18`
  - new metric `delay_time` 

https://github.com/pgsty/pg_exporter/releases/tag/v1.0.0


------

## 0.9.0

**Default Collectors**

* new metrics collector for `timescaledb` hypertable
* new metrics collector for `citus` dist node
* new metrics collector for `pg_wait_sampling` wait event profile
* `pg_slot` overhaul: Add 16/17 pg_replication_slot metrics
  * allow `pg_slot` collector run on replica since 16/17
* refactor `pg_wait` collector to agg from all processes
* restrict pg_clustering, pg_indexing, pg_vacuuming run on primary
* mark all `reset_time` as `GAUGE` rather than `COUNTER`
* fix `pg_recovery_prefetch_skip_fpw` type from `GAUGE` to `COUNTER`
* fix `pg_recv.state` type from `LABEL` to `GAUGE`
* Format collector in compact mode
* new default metric `pg_exporter_build_info` / `pgbouncer_exporter_build_info`
* add `server_encoding` to `pg_meta` collector
* add 12 new setting metrics to `pg_setting` collector
  - wal_block_size
  - segment_size
  - wal_segment_size
  - wal_level
  - wal_log_hints
  - work_mem
  - hugepage_count
  - hugepage_status
  - max_wal_size
  - min_wal_size
  - max_slot_wal_keep_size

**Exporter Codebase**

* normalize collector branch name with min pg ver suffix
* Add license file to binary packages
* move `pgsty/pg_exporter` repo to `pgsty/pg_exporter`
* refactor `server.go` to reduce `Compatible` and `PostgresPrecheck` complexity
* rename metrics collector with extra number prefix for better sorting
* bump dependencies to the latest version
* execute fatal collectors ahead of all non-fatal collectors, and fail fast

https://github.com/pgsty/pg_exporter/releases/tag/v0.9.0



------

## 0.8.1

* Bump dependencies to the latest version
* [Bump golang.org/x/net from 0.35.0 to 0.36.0 #67](https://github.com/pgsty/pg_exporter/pull/67)
* Update docker images building tags

https://github.com/pgsty/pg_exporter/releases/tag/0.9.0




------

## 0.8.0

* add pgBouncer 1.24 new metrics support (stat, pool, database)
* fix: `310-pg_size.yml` fails if log dir not set properly https://github.com/pgsty/pg_exporter/issues/64 by [@Süleyman Vurucu](https://github.com/suikast42)
* Build with the latest go 1.24 and bump all the dependencies
* Refactor logging with the standard `log/slog` instead of `go-kit`
* Full Changelog**: https://github.com/pgsty/pg_exporter/compare/v0.7.1...v0.8.0

https://github.com/pgsty/pg_exporter/releases/tag/v0.8.0



------

## 0.7.1

Routine update with dependabot

* feat: support specifying configuration as Reader by @ringerc in https://github.com/pgsty/pg_exporter/pull/62
* Bump golang.org/x/crypto from 0.21.0 to 0.31.0 by @dependabot in https://github.com/pgsty/pg_exporter/pull/63
* Fix some typos
* Full Changelog**: https://github.com/pgsty/pg_exporter/compare/v0.7.0...v0.7.1

https://github.com/pgsty/pg_exporter/releases/tag/v0.7.1



------

## 0.7.0

Refactor codebase for the latest go version.

- [PostgreSQL 17 Metrics Support](https://github.com/pgsty/pg_exporter/issues/53) by @Vonng
- [pg_exporter: predicate queries feature](https://github.com/pgsty/pg_exporter/pull/47) by [@ringerc](https://github.com/ringerc)
- [Do a clean build in the dockerfile](https://github.com/pgsty/pg_exporter/pull/54) by [@ringerc](https://github.com/ringerc) by [@ringerc](https://github.com/ringerc)
- [pg_exporter: don't panic after "bind: address already in use"](https://github.com/pgsty/pg_exporter/pull/46) by [@ringerc](https://github.com/ringerc)
- [pg_exporter: fix /stat endpoint formatting](https://github.com/pgsty/pg_exporter/pull/48) by [@ringerc](https://github.com/ringerc)
- [pg_exporter: omit default query properties on yaml export](https://github.com/pgsty/pg_exporter/pull/49) by [@ringerc](https://github.com/ringerc)
- [Exclude template DBs from discovery and schema-qualify discovery query](https://github.com/pgsty/pg_exporter/pull/50) by [@ringerc](https://github.com/ringerc)
- [Fix some typos and some metric description mistakes](https://github.com/pgsty/pg_exporter/pull/51) by [@ringerc](https://github.com/ringerc)
- [Switch from unmaintained lib/pq driver to pgx with stdlib wrapper](https://github.com/pgsty/pg_exporter/pull/52) by [@ringerc](https://github.com/ringerc)

https://github.com/pgsty/pg_exporter/releases/tag/v0.7.0


------

## 0.6.0

- Security Enhancement: Fix [security](https://github.com/pgsty/pg_exporter/security/dependabot?q=is%3Aclosed)
  dependent-bot issue
- Add pg16 collectors
- Add `arm64` & `aarch64` packages
- Remove the `monitor` schema requirement for `pg_query` collectors (you have to ensure it with search_path or just
  install `pg_stat_statements` in the default `public` schema)
- Fix pgbouncer version parsing message level from info to debug
- Fix `pg_table_10_12` collector missing `relid` issue.

- [Recognize the files with yml suffix in config directory](https://github.com/pgsty/pg_exporter/pull/34) by [@Japin Li](https://github.com/japinli)
- [Support PostgreSQL 15 and higher](https://github.com/pgsty/pg_exporter/pull/35) by [@Japin Li](https://github.com/japinli)
- [Fix connect-timeout propagation](https://github.com/pgsty/pg_exporter/pull/37/files) by [@mouchar](https://github.com/mouchar)

https://github.com/pgsty/pg_exporter/releases/tag/v0.6.0



------

## 0.5.0

**Exporter Enhancement**

- Build rpm & deb with `nfpm`
- Add `column.default`, replace when metric value is NULL
- Add `column.scale`, multiply scale factor when metric value is float/int (e.g µs to second)
- Fix `/stat` endpoint output
- Add docker container [`pgsty/pg_exporter`](https://hub.docker.com/r/pgsty/pg_exporter)

**Metrics Collector**

- scale bgwriter & pg_wal time unit to second
- remove pg_class collector and move it to pg_table & pg_inex
- add pg_class metrics to pg_table
- add pg_class metrics to pg_index
- enable pg_table_size by default
- scale pg_query pg_db pg_bgwriter pg_ssl pgbouncer_stat time metrics to second

https://github.com/pgsty/pg_exporter/releases/tag/v0.5.0






------

## 0.4.1

- update default collectors
    - omit citus & timescaledb schemas on object monitoring
    - avoid duplicate pg_statio tuples
    - support pgbouncer v1.16
    - bug fix: `pg_repl` collector overlap on pg 12
- new parameter: `-T` `connect-timeout` `PG_EXPORTER_CONNECT_TIMEOUT`
  this can be useful when monitoring remote Postgres instances.
- now `pg_exporter.yaml` are renamed as `pg_exporter.yml` in rpm package.

https://github.com/pgsty/pg_exporter/releases/tag/v0.4.1






------

## 0.4.0

- Add PG 14 support
- Default metrics configuration overhaul. (BUT you can still use the old configuration)
- add `auto-discovery` , `include-database` and `exclude-database` option
- Add multiple database monitoring implementations (with `auto-discovery = on`)

https://github.com/pgsty/pg_exporter/releases/tag/v0.4.0





------

## 0.3.2

- fix shadow DSN corner case
- fix typo & docs

https://github.com/pgsty/pg_exporter/releases/tag/v0.3.2





------

## 0.3.1

fix default configuration problems (especially for versions lower than 13)

- setting `primary_conninfo` not exists until PG13
- add `funcid` label to `pg_func` collector to avoid func name duplicate label
- fix version string to `pg_exporter`

https://github.com/pgsty/pg_exporter/releases/tag/v0.3.1






------

## 0.3.0

https://github.com/pgsty/pg_exporter/releases/tag/v0.3.0

- Change default configuration, Support PostgreSQL 13 new metrics (`pg_slru`, `pg_shmem`, `pg_query13`,`pg_backup`,
  etc...)
- Add a series of new REST APIs for health / recovery status check
- Add a dummy server with fake `pg_up 0` metric, which serves before PgExporter is initialized.
- Add `sslmode=disable` to URL if `sslmode` is not given
- fix typos and bugs

------

## 0.2.0

- add yum package and linux service definition
- add a 'skip' flag into query config
- fix `pgbouncer_up` metrics
- add conf reload support

https://github.com/pgsty/pg_exporter/releases/tag/v0.2.0






------

## 0.1.2

* fix pgbouncer_up metrics
* add dynamic configuration reload
* remove 'shard' related logic
* add a 'bulky' mode to default settings

https://github.com/pgsty/pg_exporter/releases/tag/v0.1.2




------

## 0.1.1

Fix the bug that pg_exporter will hang during start-up if any query is failed.

https://github.com/pgsty/pg_exporter/releases/tag/v0.1.1



------

## 0.1.0

It works, looks good to me.

https://github.com/pgsty/pg_exporter/releases/tag/v0.1.0


------

## 0.0.4

Tested in real world production environment with 200+ nodes for about 2 weeks. Looks good !

https://github.com/pgsty/pg_exporter/releases/tag/v0.0.4



------

## 0.0.3

v0.0.3 Release, Tested in Production Environment

This version is already tested in a production environment.

This project is still under rapid evolution, I would say if you want use it in production , try with caution.

https://github.com/pgsty/pg_exporter/releases/tag/v0.0.3



------

## 0.0.2

It's ok to try now

https://github.com/pgsty/pg_exporter/releases/tag/v0.0.2



------

## 0.0.1

Add pgbouncer mode

https://github.com/pgsty/pg_exporter/releases/tag/v0.0.1