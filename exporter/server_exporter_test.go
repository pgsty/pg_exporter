package exporter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func makeGaugeQuery(name string, priority int, tags ...string) *Query {
	return &Query{
		Name:     name,
		Branch:   name,
		SQL:      "SELECT 'db' AS datname, 1 AS value",
		Tags:     tags,
		Priority: priority,
		Columns: map[string]*Column{
			"datname": {Name: "datname", Usage: LABEL, Desc: "database"},
			"value":   {Name: "value", Usage: GAUGE, Desc: "value"},
		},
		ColumnNames: []string{"datname", "value"},
		LabelNames:  []string{"datname"},
		MetricNames: []string{"value"},
	}
}

func TestParseSemver(t *testing.T) {
	if got := ParseSemver("1.2.3"); got != 10203 {
		t.Fatalf("ParseSemver 1.2.3 = %d, want 10203", got)
	}
	if got := ParseSemver("PgBouncer 1.22.1"); got != 12201 {
		t.Fatalf("ParseSemver pgbouncer string = %d, want 12201", got)
	}
	if got := ParseSemver("invalid"); got != 0 {
		t.Fatalf("ParseSemver invalid = %d, want 0", got)
	}
}

func TestNewServerAndBasics(t *testing.T) {
	labels := prometheus.Labels{"k": "v"}
	q := map[string]*Query{"q": makeGaugeQuery("q", 1)}

	s := NewServer(
		"postgresql://user:pass@localhost:5432/postgres?sslmode=disable",
		WithConstLabel(labels),
		WithCachePolicy(true),
		WithQueries(q),
		WithServerTags([]string{"tag1"}),
		WithServerConnectTimeout(250),
	)

	if s.PgbouncerMode {
		t.Fatal("postgres database should not trigger pgbouncer mode")
	}
	if !s.DisableCache {
		t.Fatal("WithCachePolicy(true) not applied")
	}
	if s.ConnectTimeout != 250 {
		t.Fatalf("ConnectTimeout = %d, want 250", s.ConnectTimeout)
	}
	if !s.HasTag("tag1") {
		t.Fatal("WithServerTags not applied")
	}
	if got := s.GetConnectTimeout(); got != 250*time.Millisecond {
		t.Fatalf("GetConnectTimeout = %v, want 250ms", got)
	}
	if got := s.Name(); got != "postgres" {
		t.Fatalf("Name = %s, want postgres", got)
	}

	s.Database = ""
	if got := s.Name(); !strings.Contains(got, "postgresql://") {
		t.Fatalf("Name with empty database should return redacted dsn, got: %s", got)
	}

	s2 := NewServer("postgresql://user:pass@localhost:5432/pgbouncer")
	if !s2.PgbouncerMode {
		t.Fatal("pgbouncer database should trigger pgbouncer mode")
	}
}

func TestServerCompatible(t *testing.T) {
	s := NewServer("postgresql://user:pass@localhost:5432/postgres")
	s.Version = 160000
	s.Recovery = false
	s.Database = "postgres"
	s.Username = "monitor"
	s.Extensions = map[string]bool{"pg_stat_statements": true}
	s.Namespaces = map[string]bool{"public": true}
	s.Tags = []string{"foo"}

	tests := []struct {
		name string
		q    *Query
		ok   bool
	}{
		{name: "skip", q: &Query{Name: "q", Skip: true}, ok: false},
		{name: "pgbouncer tag mismatch", q: &Query{Name: "q", Tags: []string{"pgbouncer"}}, ok: false},
		{name: "min version", q: &Query{Name: "q", MinVersion: 170000}, ok: false},
		{name: "max version", q: &Query{Name: "q", MaxVersion: 160000}, ok: false},
		{name: "extension exists", q: &Query{Name: "q", Tags: []string{"extension:pg_stat_statements"}}, ok: true},
		{name: "extension missing", q: &Query{Name: "q", Tags: []string{"extension:missing"}}, ok: false},
		{name: "schema exists", q: &Query{Name: "q", Tags: []string{"schema:public"}}, ok: true},
		{name: "schema missing", q: &Query{Name: "q", Tags: []string{"schema:private"}}, ok: false},
		{name: "dbname mismatch", q: &Query{Name: "q", Tags: []string{"dbname:other"}}, ok: false},
		{name: "username match", q: &Query{Name: "q", Tags: []string{"username:monitor"}}, ok: true},
		{name: "username mismatch", q: &Query{Name: "q", Tags: []string{"username:other"}}, ok: false},
		{name: "forbidden not tag", q: &Query{Name: "q", Tags: []string{"not:foo"}}, ok: false},
		{name: "server tag match", q: &Query{Name: "q", Tags: []string{"foo"}}, ok: true},
	}

	for _, tt := range tests {
		got, _ := s.Compatible(tt.q)
		if got != tt.ok {
			t.Fatalf("%s compatible = %v, want %v", tt.name, got, tt.ok)
		}
	}

	s.Forked = true
	if ok, _ := s.Compatible(&Query{Name: "q", Tags: []string{"cluster"}}); ok {
		t.Fatal("cluster query should not run on forked server")
	}
	s.Forked = false

	s.Recovery = true
	if ok, _ := s.Compatible(&Query{Name: "q", Tags: []string{"primary"}}); ok {
		t.Fatal("primary query should not run on recovery server")
	}
	if ok, _ := s.Compatible(&Query{Name: "q", Tags: []string{"replica"}}); !ok {
		t.Fatal("replica query should run on recovery server")
	}
}

func TestPlanResetAndCollectCached(t *testing.T) {
	s := NewServer("postgresql://user:pass@localhost:5432/postgres")
	s.Version = 160000
	s.Database = "postgres"
	s.Username = "monitor"
	s.Namespaces = map[string]bool{"public": true}
	s.Extensions = map[string]bool{}

	q1 := makeGaugeQuery("q1", 20)
	q2 := makeGaugeQuery("q2", 10)
	s.queries = map[string]*Query{"q1": q1, "q2": q2}

	s.Plan()
	if !s.Planned {
		t.Fatal("Plan should mark server planned")
	}
	if len(s.Collectors) != 2 {
		t.Fatalf("collector count = %d, want 2", len(s.Collectors))
	}
	if s.Collectors[0].Name != "q2" || s.Collectors[1].Name != "q1" {
		t.Fatalf("collectors should be sorted by priority, got %s then %s", s.Collectors[0].Name, s.Collectors[1].Name)
	}

	// Build one cached metric for q2, so Collect path does not need DB.
	c := s.Collectors[0]
	metric := prometheus.MustNewConstMetric(c.descriptors["value"], prometheus.GaugeValue, 1, "db")
	c.result = []prometheus.Metric{metric}
	c.TTL = 3600
	c.lastScrape = time.Now()
	c.err = nil

	s.beforeScrape = func(_ context.Context, s *Server) error {
		s.UP = true
		return nil
	}
	s.Collectors = []*Collector{c}
	s.ResetStats()
	s.Planned = true

	ch := make(chan prometheus.Metric, 10)
	s.Collect(ch)

	if !s.UP {
		t.Fatal("Collect should keep server UP for successful cached query")
	}
	if s.totalCount != 1 {
		t.Fatalf("totalCount = %v, want 1", s.totalCount)
	}
	if s.queryScrapeTotalCount[c.Name] != 1 {
		t.Fatalf("queryScrapeTotalCount = %v, want 1", s.queryScrapeTotalCount[c.Name])
	}
	if s.queryScrapeMetricCount[c.Name] != 1 {
		t.Fatalf("queryScrapeMetricCount = %v, want 1", s.queryScrapeMetricCount[c.Name])
	}
	if s.queryScrapeHitCount[c.Name] != 1 {
		t.Fatalf("queryScrapeHitCount = %v, want 1", s.queryScrapeHitCount[c.Name])
	}
}

func TestExporterServerLifecycleHelpers(t *testing.T) {
	e := &Exporter{
		dsn:     "postgresql://user:pass@localhost:5432/postgres?sslmode=disable",
		servers: map[string]*Server{},
		queries: map[string]*Query{"q": makeGaugeQuery("q", 1)},
	}

	e.CreateServer("db1")
	if len(e.servers) != 1 {
		t.Fatalf("CreateServer count = %d, want 1", len(e.servers))
	}
	snapshot := e.IterateServer()
	if len(snapshot) != 1 || snapshot[0] == nil {
		t.Fatalf("IterateServer snapshot invalid: %#v", snapshot)
	}
	if !snapshot[0].Forked {
		t.Fatal("CreateServer should mark new server as Forked")
	}

	e.RemoveServer("db1")
	if len(e.servers) != 0 {
		t.Fatalf("RemoveServer count = %d, want 0", len(e.servers))
	}
}
