package exporter

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type closeTrackingConnector struct {
	closed *atomic.Bool
}

func (c closeTrackingConnector) Connect(context.Context) (driver.Conn, error) {
	return &closeTrackingConn{closed: c.closed}, nil
}

func (c closeTrackingConnector) Driver() driver.Driver {
	return closeTrackingDriver{}
}

type closeTrackingDriver struct{}

func (closeTrackingDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("close tracking driver must be opened through its connector")
}

type closeTrackingConn struct {
	closed *atomic.Bool
}

func (c *closeTrackingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (c *closeTrackingConn) Close() error {
	c.closed.Store(true)
	return nil
}

func (c *closeTrackingConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

func (c *closeTrackingConn) Ping(context.Context) error {
	return nil
}

func readGauge(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(gauge)
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather gauge metric: %v", err)
	}
	if len(families) != 1 || len(families[0].GetMetric()) != 1 || families[0].GetMetric()[0].GetGauge() == nil {
		t.Fatalf("gathered metric is not a single gauge: %v", families)
	}
	return families[0].GetMetric()[0].GetGauge().GetValue()
}

func makeCachedCollectorForServer(s *Server, name string, val float64) *Collector {
	q := makeGaugeQuery(name, 1)
	c := NewCollector(q, s)
	c.TTL = 3600
	c.lastScrape = time.Now()
	metric := prometheus.MustNewConstMetric(c.descriptors["value"], prometheus.GaugeValue, val, "db")
	c.result = []prometheus.Metric{metric}
	c.err = nil
	return c
}

func TestExporterCollectAndInternalMetrics(t *testing.T) {
	primary := NewServer("postgresql://u:p@localhost:5432/postgres")
	primary.beforeScrape = func(_ context.Context, s *Server) error {
		s.UP = true
		s.Version = 160000
		s.Recovery = false
		return nil
	}
	primary.Planned = true
	primary.Collectors = []*Collector{makeCachedCollectorForServer(primary, "q_primary", 1)}
	primary.ResetStats()

	extra := NewServer("postgresql://u:p@localhost:5432/otherdb")
	extra.Forked = true
	extra.beforeScrape = func(_ context.Context, s *Server) error {
		s.UP = true
		s.Version = 160000
		s.Recovery = false
		return nil
	}
	extra.Planned = true
	extra.Collectors = []*Collector{makeCachedCollectorForServer(extra, "q_extra", 2)}
	extra.ResetStats()

	e := &Exporter{
		server:  primary,
		servers: map[string]*Server{"otherdb": extra},
	}
	e.setupInternalMetrics()

	ch := make(chan prometheus.Metric, 256)
	e.Collect(ch)

	if got := readGauge(t, e.up); got != 1 {
		t.Fatalf("pg_up = %v, want 1", got)
	}
	if e.Status() != "unknown" {
		t.Fatalf("scrape changed health without a probe: %s", e.Status())
	}
}

func TestExporterRecoveryMetricRetainsLastKnownRoleWhenTargetGoesDown(t *testing.T) {
	primary := NewServer("postgresql://u:p@localhost:5432/postgres")
	scrape := 0
	primary.beforeScrape = func(_ context.Context, s *Server) error {
		scrape++
		switch scrape {
		case 1:
			s.UP = true
			s.Version = 160000
			s.Recovery = true
			return nil
		case 2:
			return errors.New("database unavailable")
		default:
			s.UP = true
			s.Version = 160000
			s.Recovery = false
			return nil
		}
	}
	primary.Planned = true
	primary.ResetStats()

	e := &Exporter{server: primary, servers: map[string]*Server{}}
	e.setupInternalMetrics()

	e.Collect(make(chan prometheus.Metric, 64))
	if got := readGauge(t, e.recovery); got != 1 {
		t.Fatalf("recovery metric after replica scrape = %v, want 1", got)
	}

	e.Collect(make(chan prometheus.Metric, 64))
	if !primary.Recovery {
		t.Fatal("test fixture should retain the server's last known recovery state")
	}
	if got := readGauge(t, e.recovery); got != 1 {
		t.Fatalf("recovery metric after failed scrape = %v, want last known value 1", got)
	}

	e.Collect(make(chan prometheus.Metric, 64))
	if got := readGauge(t, e.recovery); got != 0 {
		t.Fatalf("recovery metric after reconnecting as primary = %v, want 0", got)
	}
}

func TestNewExporterFailFastClosesPrimaryServer(t *testing.T) {
	closed := &atomic.Bool{}
	db := sql.OpenDB(closeTrackingConnector{closed: closed})
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("open close-tracking database connection: %v", err)
	}

	checkErr := errors.New("connectivity check failed")
	server := NewServer("postgresql://u:p@localhost:5432/postgres")
	server.DB = db
	server.beforeScrape = func(context.Context, *Server) error { return checkErr }
	serverFactory := func(string, ...ServerOpt) *Server { return server }

	exporter, err := newExporterWithServerFactory(
		"postgresql://u:p@localhost:5432/postgres",
		serverFactory,
		WithFailFast(true),
	)
	if exporter != nil {
		t.Fatal("NewExporter should return a nil exporter when fail-fast check fails")
	}
	if !errors.Is(err, checkErr) {
		t.Fatalf("NewExporter error = %v, want wrapped connectivity error", err)
	}
	if !closed.Load() {
		t.Fatal("NewExporter did not close the primary server database after fail-fast check failure")
	}
}

func TestNewExporterNonFailFastKeepsPrimaryServerOpen(t *testing.T) {
	closed := &atomic.Bool{}
	db := sql.OpenDB(closeTrackingConnector{closed: closed})
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("open close-tracking database connection: %v", err)
	}

	checkErr := errors.New("connectivity check failed")
	server := NewServer("postgresql://u:p@localhost:5432/postgres")
	server.DB = db
	server.beforeScrape = func(context.Context, *Server) error { return checkErr }
	serverFactory := func(string, ...ServerOpt) *Server { return server }

	exporter, err := newExporterWithServerFactory(
		"postgresql://u:p@localhost:5432/postgres",
		serverFactory,
		WithFailFast(false),
	)
	if err != nil {
		t.Fatalf("NewExporter returned an error with fail-fast disabled: %v", err)
	}
	if exporter == nil {
		t.Fatal("NewExporter returned a nil exporter with fail-fast disabled")
	}
	if closed.Load() {
		t.Fatal("NewExporter closed the primary server database with fail-fast disabled")
	}

	exporter.Close()
	if !closed.Load() {
		t.Fatal("Exporter.Close did not close the primary server database")
	}
}

func TestExporterDescribeAndCloseNoPanic(t *testing.T) {
	s := NewServer("postgresql://u:p@localhost:5432/postgres")
	s.beforeScrape = func(_ context.Context, s *Server) error {
		s.UP = true
		return nil
	}
	s.Planned = true
	s.Collectors = []*Collector{makeCachedCollectorForServer(s, "q", 1)}
	s.ResetStats()

	e := &Exporter{
		server:  s,
		servers: map[string]*Server{},
	}
	e.setupInternalMetrics()

	descCh := make(chan *prometheus.Desc, 32)
	e.Describe(descCh)
	if len(descCh) != 0 {
		t.Fatalf("Describe should not emit descriptors for a dynamic/unchecked exporter, got %d", len(descCh))
	}

	// server DB pointers are nil in this synthetic test; Close should not panic.
	e.Close()
}

func TestDisableIntroSuppressesInternalMetrics(t *testing.T) {
	s := NewServer("postgresql://u:p@localhost:5432/postgres")
	s.beforeScrape = func(_ context.Context, s *Server) error {
		s.UP = true
		s.Version = 160000
		s.Recovery = false
		return nil
	}
	s.Planned = true
	s.Collectors = []*Collector{makeCachedCollectorForServer(s, "q", 1)}
	s.ResetStats()

	e := &Exporter{
		server:       s,
		servers:      map[string]*Server{},
		disableIntro: true,
	}
	e.setupInternalMetrics()

	r := prometheus.NewRegistry()
	if err := r.Register(e); err != nil {
		t.Fatalf("register exporter failed: %v", err)
	}
	mfs, err := r.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	found := map[string]bool{}
	for _, mf := range mfs {
		found[mf.GetName()] = true
	}
	if !found["q_value"] {
		t.Fatalf("expected query metric q_value to be present, got: %#v", found)
	}
	// Default internal metrics namespace for postgres is "pg".
	if found["pg_up"] || found["pg_version"] || found["pg_in_recovery"] || found["pg_exporter_build_info"] || found["pg_exporter_up"] {
		t.Fatalf("disable-intro should suppress internal metrics, got: %#v", found)
	}
}

func TestServerIntrospectionHelpers(t *testing.T) {
	s := NewServer("postgresql://u:p@localhost:5432/postgres")
	c := makeCachedCollectorForServer(s, "q", 1)
	s.Collectors = []*Collector{c}
	s.ResetStats()

	if s.Error() != nil {
		t.Fatalf("new server Error should be nil, got %v", s.Error())
	}
	if got := s.Duration(); got != 0 {
		t.Fatalf("new server Duration = %v, want 0", got)
	}
	if got := s.Uptime(); got < 0 {
		t.Fatalf("Uptime should be non-negative, got %v", got)
	}

	if got := c.ResultSize(); got != 1 {
		t.Fatalf("collector ResultSize = %d, want 1", got)
	}
	if skip, _ := c.PredicateSkip(); skip {
		t.Fatal("collector PredicateSkip should be false by default")
	}
	if got := c.Duration(); got != 0 {
		t.Fatalf("collector Duration = %v, want 0", got)
	}

	if exp := s.Explain(); exp == "" {
		t.Fatal("Explain should not be empty")
	}
	if html := s.ExplainHTML(); html == "" {
		t.Fatal("ExplainHTML should not be empty")
	}
}
