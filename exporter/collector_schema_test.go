package exporter

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestCollectorMissingLabelColumnFailsAtomically(t *testing.T) {
	tests := []struct {
		name   string
		values [][]driver.Value
	}{
		{
			name:   "non-empty result",
			values: [][]driver.Value{{"db", float64(1)}},
		},
		{
			name: "empty result",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := histogramTestQuery(
				"test_missing_label",
				[]string{"datname"},
				map[string]*Column{
					"value": {Name: "value", Usage: GAUGE},
				},
				[]string{"value"},
			)
			collector := newHistogramTestCollector(t, query, func() driver.Rows {
				return &histogramTestRows{
					columns: []string{"wrong_label", "value"},
					values:  tt.values,
				}
			}, nil)

			// A failed refresh must discard an earlier successful snapshot.
			collector.result = []prometheus.Metric{
				prometheus.MustNewConstMetric(
					collector.descriptors["value"],
					prometheus.GaugeValue,
					1,
					"old",
				),
			}
			collector.scrapeBegin = time.Now()
			collector.execute()

			if err := collector.Error(); err == nil ||
				!strings.Contains(err.Error(), "missing label column") ||
				!strings.Contains(err.Error(), "datname") {
				t.Fatalf("missing label error = %v", err)
			}
			if got := collector.ResultSize(); got != 0 {
				t.Fatalf("missing label published %d metrics", got)
			}

			ch := make(chan prometheus.Metric, 1)
			collector.sendMetrics(ch)
			if got := len(ch); got != 0 {
				t.Fatalf("missing label sent %d cached metrics", got)
			}
		})
	}
}

func TestServerNonFatalMissingLabelDoesNotStopOtherCollectors(t *testing.T) {
	rowsFactory := func() driver.Rows {
		return &histogramTestRows{
			columns: []string{"datname", "value"},
			values:  [][]driver.Value{{"db", float64(1)}},
		}
	}

	badQuery := histogramTestQuery(
		"test_bad_label",
		[]string{"missing_label"},
		map[string]*Column{
			"value": {Name: "value", Usage: GAUGE},
		},
		[]string{"value"},
	)
	badCollector := newHistogramTestCollector(t, badQuery, rowsFactory, nil)

	goodQuery := histogramTestQuery(
		"test_good_label",
		[]string{"datname"},
		map[string]*Column{
			"value": {Name: "value", Usage: GAUGE},
		},
		[]string{"value"},
	)
	server := badCollector.Server
	goodCollector := NewCollector(goodQuery, server)
	server.Collectors = []*Collector{badCollector, goodCollector}
	server.scrapeBegin = time.Now()
	server.ResetStats()

	ch := make(chan prometheus.Metric, 2)
	server.collectNonFatalQueries(context.Background(), ch)

	if got := len(ch); got != 1 {
		t.Fatalf("non-fatal collectors emitted %d metrics, want only the good collector", got)
	}
	if badCollector.Error() == nil {
		t.Fatal("bad collector did not report the missing label")
	}
	if got := badCollector.ResultSize(); got != 0 {
		t.Fatalf("bad collector retained %d metrics", got)
	}
	if err := goodCollector.Error(); err != nil {
		t.Fatalf("good collector failed after non-fatal error: %v", err)
	}
	if got := goodCollector.ResultSize(); got != 1 {
		t.Fatalf("good collector emitted %d metrics, want 1", got)
	}
	if got := server.queryScrapeErrorCount[badCollector.Name]; got != 1 {
		t.Fatalf("bad collector error count = %v, want 1", got)
	}
	if got := server.queryScrapeErrorCount[goodCollector.Name]; got != 0 {
		t.Fatalf("good collector error count = %v, want 0", got)
	}
	if got := server.queryScrapeTotalCount[goodCollector.Name]; got != 1 {
		t.Fatalf("good collector scrape count = %v, want 1", got)
	}
}

func TestCollectorDuplicateLabelTupleIsolatedFromOtherCollectors(t *testing.T) {
	dupQuery := histogramTestQuery(
		"test_dup_label",
		[]string{"datname"},
		map[string]*Column{
			"value": {Name: "value", Usage: GAUGE},
		},
		[]string{"value"},
	)
	dupCollector := newHistogramTestCollector(t, dupQuery, func() driver.Rows {
		return &histogramTestRows{
			columns: []string{"datname", "value"},
			values: [][]driver.Value{
				{"same_db", float64(1)},
				{"same_db", float64(2)},
			},
		}
	}, nil)

	goodQuery := histogramTestQuery(
		"test_healthy",
		[]string{"datname"},
		map[string]*Column{
			"value": {Name: "value", Usage: GAUGE},
		},
		[]string{"value"},
	)
	goodCollector := newHistogramTestCollector(t, goodQuery, func() driver.Rows {
		return &histogramTestRows{
			columns: []string{"datname", "value"},
			values:  [][]driver.Value{{"ok_db", float64(42)}},
		}
	}, nil)

	server := dupCollector.Server
	server.beforeScrape = func(_ context.Context, s *Server) error {
		s.UP = true
		return nil
	}
	server.Planned = true
	server.Collectors = []*Collector{dupCollector, goodCollector}
	server.ResetStats()

	reg := prometheus.NewRegistry()
	reg.MustRegister(server)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Registry.Gather failed despite non-fatal duplicate row isolation: %v", err)
	}
	if dupCollector.Error() == nil || !strings.Contains(dupCollector.Error().Error(), "duplicate label set") {
		t.Fatalf("dupCollector error = %v, want duplicate label set error", dupCollector.Error())
	}
	if dupCollector.ResultSize() != 0 {
		t.Fatalf("dupCollector ResultSize = %d, want 0", dupCollector.ResultSize())
	}
	if len(families) != 1 || families[0].GetName() != "test_healthy_value" {
		t.Fatalf("unexpected gathered families: %+v", families)
	}
}

func TestCollectorDirectCollectZeroTTLRunsWithoutServerScrapeBegin(t *testing.T) {
	q := histogramTestQuery(
		"test_zero_ttl",
		[]string{"datname"},
		map[string]*Column{
			"value": {Name: "value", Usage: GAUGE},
		},
		[]string{"value"},
	)
	q.TTL = 0
	c := newHistogramTestCollector(t, q, func() driver.Rows {
		return &histogramTestRows{
			columns: []string{"datname", "value"},
			values:  [][]driver.Value{{"db", float64(7)}},
		}
	}, nil)
	// Leave c.Server.scrapeBegin zero and call Collect directly twice.
	for i := 0; i < 2; i++ {
		ch := make(chan prometheus.Metric, 2)
		c.Collect(ch)
		if c.CacheHit() {
			t.Fatalf("pass %d unexpectedly hit cache when TTL=0", i)
		}
		if len(ch) != 1 {
			t.Fatalf("pass %d emitted %d metrics, want 1", i, len(ch))
		}
	}
}
