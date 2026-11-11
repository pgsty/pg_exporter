package exporter

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func makeSampleQuery() *Query {
	return &Query{
		Name:    "sample",
		Branch:  "sample_branch",
		Desc:    "sample query",
		Tags:    []string{"tag1", "tag2"},
		Timeout: 1.5,
		Columns: map[string]*Column{
			"datname": {
				Name:   "datname",
				Usage:  LABEL,
				Rename: "db",
				Desc:   "database name",
			},
			"value": {
				Name:   "value",
				Usage:  GAUGE,
				Rename: "val",
				Desc:   "metric value",
			},
		},
		ColumnNames: []string{"datname", "value"},
		LabelNames:  []string{"datname"},
		MetricNames: []string{"value"},
		Metrics: []map[string]*Column{
			{"datname": {Name: "datname", Usage: LABEL, Rename: "db", Desc: "database name"}},
			{"value": {Name: "value", Usage: GAUGE, Rename: "val", Desc: "metric value"}},
		},
	}
}

func TestColumnPrometheusValueType(t *testing.T) {
	cGauge := &Column{Name: "g", Usage: GAUGE}
	if got := cGauge.PrometheusValueType(); got != prometheus.GaugeValue {
		t.Fatalf("gauge type = %v, want GaugeValue", got)
	}

	cCounter := &Column{Name: "c", Usage: COUNTER}
	if got := cCounter.PrometheusValueType(); got != prometheus.CounterValue {
		t.Fatalf("counter type = %v, want CounterValue", got)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("PrometheusValueType should panic for non-value usage")
		}
	}()
	_ = (&Column{Name: "x", Usage: LABEL}).PrometheusValueType()
}

func TestColumnAndMetricDescString(t *testing.T) {
	c := &Column{Name: "value", Usage: GAUGE, Desc: "desc"}
	if !strings.Contains(c.String(), "value") {
		t.Fatalf("column string does not contain name: %s", c.String())
	}

	md := c.MetricDesc("sample", []string{"db"})
	if !strings.Contains(md.Name, "sample_value") {
		t.Fatalf("metric desc name = %s", md.Name)
	}
	if !strings.Contains(md.Name, "{db}") {
		t.Fatalf("metric desc should contain labels signature, got %s", md.Name)
	}
	if !strings.Contains(md.String(), "desc") {
		t.Fatalf("metric desc string = %s", md.String())
	}
}

func TestQueryHelpersAndRender(t *testing.T) {
	q := makeSampleQuery()

	if !q.HasTag("tag1") || q.HasTag("missing") {
		t.Fatalf("HasTag result unexpected for tags %v", q.Tags)
	}

	cols := q.ColumnList()
	if len(cols) != 2 || cols[0].Name != "datname" || cols[1].Name != "value" {
		t.Fatalf("ColumnList order unexpected: %#v", cols)
	}

	labels := q.LabelList()
	if len(labels) != 1 || labels[0] != "db" {
		t.Fatalf("LabelList = %#v, want [db]", labels)
	}

	metrics := q.MetricList()
	if len(metrics) != 1 {
		t.Fatalf("MetricList len = %d, want 1", len(metrics))
	}
	if !strings.Contains(metrics[0].Name, "sample_val") {
		t.Fatalf("MetricList name = %s", metrics[0].Name)
	}

	if got := q.TimeoutDuration(); got != 1500*time.Millisecond {
		t.Fatalf("TimeoutDuration = %v, want 1500ms", got)
	}

	yaml := q.MarshalYAML()
	if !strings.Contains(yaml, "sample_branch:") {
		t.Fatalf("MarshalYAML missing branch key: %s", yaml)
	}

	explain := q.Explain()
	if !strings.Contains(explain, "SYNOPSIS") {
		t.Fatalf("Explain output unexpected: %s", explain)
	}

	html := q.HTML()
	if !strings.Contains(html, "<h2>sample</h2>") {
		t.Fatalf("HTML output unexpected: %s", html)
	}

	q.PredicateQueries = []PredicateQuery{
		{Name: "p_uncached", SQL: "SELECT true", TTL: 0},
		{Name: "p_cached", SQL: "SELECT true", TTL: 15.5},
	}
	htmlWithPred := q.HTML()
	if !strings.Contains(htmlWithPred, "<i>not cached</i>") || !strings.Contains(htmlWithPred, "15.5s") {
		t.Fatalf("HTML predicate rendering failed: %s", htmlWithPred)
	}
}

func TestColumnParseNumbersRejectsNonFinite(t *testing.T) {
	for _, bad := range []string{"NaN", "+Inf", "-Inf", "Inf"} {
		colScale := &Column{Name: "v", Usage: GAUGE, Scale: bad}
		if err := colScale.parseNumbers(); err == nil {
			t.Fatalf("expected non-finite scale %q to fail validation", bad)
		}
		colDefault := &Column{Name: "v", Usage: GAUGE, Default: bad}
		if err := colDefault.parseNumbers(); err == nil {
			t.Fatalf("expected non-finite default %q to fail validation", bad)
		}
	}
}
