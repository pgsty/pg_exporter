package exporter

import (
	"reflect"
	"testing"
)

func TestConfigYAMLCompatibility(t *testing.T) {
	const config = `
base: &base
  query: "SELECT \n  'on' AS state, 1 AS value\n"
  fatal: yes
  tags: [primary, on]
  future_option: ignored
  metrics:
    - state: {usage: LABEL, description: on}
    - value: {usage: GAUGE, default: 0, scale: 1}
derived:
  <<: *base
  query: SELECT 'off' AS state, 2 AS value
`
	queries, err := ParseConfig([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("got %d queries, want 2", len(queries))
	}
	wantSQL := map[string]string{
		"base":    "SELECT \n  'on' AS state, 1 AS value\n",
		"derived": "SELECT 'off' AS state, 2 AS value",
	}
	for branch, sql := range wantSQL {
		t.Run(branch, func(t *testing.T) {
			q := queries[branch]
			if q == nil {
				t.Fatalf("missing query %q", branch)
			}
			if q.SQL != sql || !q.Fatal || !reflect.DeepEqual(q.Tags, []string{"primary", "on"}) {
				t.Fatalf("YAML merge or scalar conversion changed: %#v", q)
			}
			if q.Columns["state"].Desc != "on" || q.Columns["value"].Default != "0" || q.Columns["value"].Scale != "1" {
				t.Fatalf("YAML string conversion changed: %#v", q.Metrics)
			}
			// The YAML shown by diagnostic routes must remain valid input.
			roundTrip, err := ParseConfig([]byte(q.MarshalYAML()))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(roundTrip[branch], q) {
				t.Fatalf("query changed after YAML round trip: got %#v, want %#v", roundTrip[branch], q)
			}
		})
	}
}

func TestConfigYAMLRejectsDuplicateKeys(t *testing.T) {
	const config = `
sample:
  query: SELECT 1 AS value
  query: SELECT 2 AS value
  metrics:
    - value: {usage: GAUGE}
`
	if _, err := ParseConfig([]byte(config)); err == nil {
		t.Fatal("duplicate YAML mapping keys must be rejected")
	}
}

func TestConfigYAMLReadsFirstDocument(t *testing.T) {
	const config = `
first:
  query: SELECT 1 AS value
  metrics:
    - value: {usage: GAUGE}
---
second:
  query: SELECT 2 AS value
  metrics:
    - value: {usage: GAUGE}
`
	queries, err := ParseConfig([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || queries["first"] == nil || queries["first"].SQL != "SELECT 1 AS value" {
		t.Fatalf("expected only the first YAML document, got %#v", queries)
	}
}
