package exporter

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func progressQueries(t *testing.T) map[string]*Query {
	t.Helper()
	all := loadConfigDirForTest(t, "config")
	queries := make(map[string]*Query)
	for key, q := range all {
		switch q.Name {
		case "pg_vacuuming", "pg_indexing", "pg_clustering":
			queries[key] = q
		}
	}
	return queries
}

func TestProgressCollectorsStayClusterScoped(t *testing.T) {
	queries := progressQueries(t)
	for version := 120000; version <= 190000; version += 10000 {
		for _, forked := range []bool{false, true} {
			for _, recovery := range []bool{false, true} {
				s := NewServer("postgres://monitor@localhost/app", WithQueries(queries))
				s.Version, s.Forked, s.Recovery = version, forked, recovery
				s.Plan()
				want := 3
				if recovery || forked {
					want = 0
				}
				if len(s.Collectors) != want {
					t.Fatalf("PG %d forked=%v recovery=%v: %d collectors, want %d", version, forked, recovery, len(s.Collectors), want)
				}
			}
		}
	}
}

// PG_EXPORTER_TEST_DSN must point to a disposable PostgreSQL 12+ database,
// preferably as a pg_monitor member with TEMP privilege. Only temporary tables
// and views are created. The fixture includes a foreign database row with the
// same relation OID as a local table, exercising actual PostgreSQL OID lookup,
// and a foreign system catalog row, whose OID names the same table everywhere.
func TestProgressDatabaseScopeSQL(t *testing.T) {
	dsn := os.Getenv("PG_EXPORTER_TEST_DSN")
	if dsn == "" {
		t.Skip("set PG_EXPORTER_TEST_DSN for PostgreSQL integration checks")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := NewServer(dsn, WithQueries(progressQueries(t)), WithServerConnectTimeout(2000))
	s.DB = db
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	if s.Version < 120000 {
		t.Skip("progress collectors require PostgreSQL 12+")
	}
	fixture := `CREATE TEMP TABLE progress_local(i int);
CREATE TEMP VIEW progress_fixture AS
SELECT oid AS datid, datname, 123 AS pid, 'progress_local'::regclass::oid AS relid
FROM pg_database WHERE datname = current_database()
UNION ALL SELECT 0::oid, 'foreign_database', 456, 'progress_local'::regclass::oid
UNION ALL SELECT 0::oid, 'foreign_catalog', 789, 'pg_catalog.pg_attribute'::regclass::oid;
CREATE TEMP VIEW pg_stat_progress_vacuum AS
SELECT *, 'scanning heap'::text AS phase, 2 AS heap_blks_total, 1 AS heap_blks_scanned,
0 AS heap_blks_vacuumed, 1 AS indexes_total, 0 AS indexes_processed,
0 AS dead_tuple_bytes, 0 AS delay_time FROM progress_fixture;
CREATE TEMP VIEW pg_stat_progress_create_index AS
SELECT *, 2 AS blocks_total, 1 AS blocks_done, 2 AS tuples_total, 1 AS tuples_done,
2 AS partitions_total, 1 AS partitions_done, 2 AS lockers_total, 1 AS lockers_done FROM progress_fixture;
CREATE TEMP VIEW pg_stat_progress_cluster AS
SELECT *, 1 AS heap_tuples_scanned, 2 AS heap_blks_total, 1 AS heap_blks_scanned FROM progress_fixture;`
	if _, err := db.Exec(fixture); err != nil {
		t.Fatal(err)
	}
	var relationOID string
	if err := db.QueryRow("SELECT 'progress_local'::regclass::oid::text").Scan(&relationOID); err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(s)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) == 0 {
		t.Fatal("no progress metrics")
	}
	seen := map[string]bool{}
	for _, family := range families {
		if len(family.Metric) != 3 {
			t.Fatalf("%s: want local, foreign, and foreign catalog samples, got %d", family.GetName(), len(family.Metric))
		}
		for _, prefix := range []string{"pg_vacuuming_", "pg_indexing_", "pg_clustering_"} {
			if strings.HasPrefix(family.GetName(), prefix) {
				seen[prefix] = true
			}
		}
		identities := map[string]string{}
		for _, metric := range family.Metric {
			var datname, relname string
			for _, label := range metric.Label {
				switch label.GetName() {
				case "datname":
					datname = label.GetValue()
				case "relname":
					relname = label.GetValue()
				}
			}
			identities[datname] = relname
		}
		if len(identities) != 3 || identities[s.Database] != "progress_local" || identities["foreign_database"] != relationOID ||
			identities["foreign_catalog"] != "pg_attribute" {
			t.Fatalf("wrong local/foreign identity for %s: %v, foreign OID=%s", family.GetName(), identities, relationOID)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("missing progress collectors: %v", seen)
	}
}
