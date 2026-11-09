# Legacy Config Bundle (for PG 9.1 - 9.6)

This directory contains the **legacy pg_exporter config bundle** for **PostgreSQL 9.1 - 9.6** (EOL).

- `pg_exporter.yml`: merged legacy config (ready to use)
- `config/`: separated collector definitions (source of truth)

The legacy bundle is maintained separately on `main`; it is not included in binary archives or RPM/DEB packages. Use the current bundle rather than the copy in an older release tag. In particular, PostgreSQL 9.1 needs the table-size query's zero-TOAST guard: a table without a TOAST relation must report a zero TOAST size instead of querying relation OID 0.

Download the maintained bundle independently of the exporter binary:

```bash
curl -fSL https://raw.githubusercontent.com/pgsty/pg_exporter/main/legacy/pg_exporter.yml -o pg_exporter-legacy.yml
pg_exporter --config=pg_exporter-legacy.yml --url='postgres://monitor@localhost:5432/postgres'
```

## Generate / Update

From the repository root:

```bash
make conf9
```

## Usage

Use the merged legacy config file:

```bash
PG_EXPORTER_CONFIG=legacy/pg_exporter.yml \
PG_EXPORTER_URL='postgres://user:pass@host:5432/postgres' \
pg_exporter --auto-discovery --exclude-database=template0,template1
```

Or load separated collectors directly (directory mode):

```bash
PG_EXPORTER_CONFIG=legacy/config \
PG_EXPORTER_URL='postgres://user:pass@host:5432/postgres' \
pg_exporter --auto-discovery --exclude-database=template0,template1
```

## Notes

- PostgreSQL **9.0 is not supported**.
- For PostgreSQL **10+**, use the default config in repo root: `pg_exporter.yml`.
