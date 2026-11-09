package exporter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReloadUpdatesQueriesInPlace(t *testing.T) {
	originExporter := PgExporter
	t.Cleanup(func() { setCurrentExporter(originExporter) })

	originConfigPath := *configPath
	t.Cleanup(func() { *configPath = originConfigPath })

	// Seed exporter with an initial query set and a planned server.
	s := NewServer("postgresql://u:p@localhost:5432/postgres")
	s.beforeScrape = func(_ context.Context, s *Server) error { return nil }
	s.Planned = true
	s.queries = map[string]*Query{"old": makeGaugeQuery("old", 1)}
	s.Collectors = []*Collector{NewCollector(makeGaugeQuery("old", 1), s)}
	s.ResetStats()

	e := &Exporter{
		server:  s,
		servers: map[string]*Server{},
		queries: s.queries,
	}
	setCurrentExporter(e)

	// Write a new config and reload it.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "pg_exporter.yml")
	cfg := `
q_new:
  query: SELECT 1 AS value, 'db' AS datname
  metrics:
    - datname:
        usage: label
        description: db
    - value:
        usage: gauge
        description: value
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config failed: %v", err)
	}
	*configPath = cfgPath

	if err := Reload(); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	if _, ok := e.queries["q_new"]; !ok {
		t.Fatalf("expected new query to be loaded, got: %#v", e.queries)
	}
	if e.server.queries == nil || e.server.queries["q_new"] == nil {
		t.Fatalf("server queries not updated, got: %#v", e.server.queries)
	}
	if e.server.Planned {
		t.Fatalf("server should be marked unplanned after reload")
	}
	if e.server.Collectors != nil {
		t.Fatalf("server collectors should be cleared after reload")
	}
}
