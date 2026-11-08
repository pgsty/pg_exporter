package exporter

import (
	"bytes"
	"fmt"
	htmltmpl "html/template"
	"slices"
	texttmpl "text/template"
	"time"

	"go.yaml.in/yaml/v4"
)

/* ================ Query ================ */

// Query hold the information of how to fetch metric and parse them
type Query struct {
	Name             string           `yaml:"name,omitempty"`              // actual query name, used as metric prefix
	Desc             string           `yaml:"desc,omitempty"`              // description of this metric query
	SQL              string           `yaml:"query"`                       // SQL command to fetch metrics
	PredicateQueries []PredicateQuery `yaml:"predicate_queries,omitempty"` // SQL command to filter metrics
	Branch           string           `yaml:"-"`                           // branch name, top layer key of config file

	// control query behaviour
	Tags       []string `yaml:"tags,omitempty"`        // tags are used for execution control
	TTL        float64  `yaml:"ttl,omitempty"`         // caching ttl in seconds
	Timeout    float64  `yaml:"timeout,omitempty"`     // query execution timeout in seconds
	Priority   int      `yaml:"priority,omitempty"`    // execution priority, from 1 to 999
	MinVersion int      `yaml:"min_version,omitempty"` // minimal supported version, include
	MaxVersion int      `yaml:"max_version,omitempty"` // maximal supported version, not include
	Fatal      bool     `yaml:"fatal,omitempty"`       // if query marked fatal fail, entire scrape will fail
	Skip       bool     `yaml:"skip,omitempty"`        // if query marked skip, it will be omit while loading

	Metrics []map[string]*Column `yaml:"metrics"` // metric definition list

	// metrics parsing auxiliaries
	Path        string             `yaml:"-"` // where am I from ?
	Columns     map[string]*Column `yaml:"-"` // column map
	ColumnNames []string           `yaml:"-"` // column names in origin orders
	LabelNames  []string           `yaml:"-"` // column (name) that used as label, sequences matters
	MetricNames []string           `yaml:"-"` // column (name) that used as metric
}

// A PredicateQuery is a query that returns a 1-column resultset that's used to decide whether
// to run the main query.
type PredicateQuery struct {
	Name string  `yaml:"name,omitempty"`  // predicate query name, only used for logging
	SQL  string  `yaml:"predicate_query"` // SQL command to return a predicate
	TTL  float64 `yaml:"ttl,omitempty"`   // How long to cache results for
}

var queryTemplate, _ = texttmpl.New("Query").Parse(`##
# SYNOPSIS
#       {{ .Name }}{{ if ne .Name .Branch }}.{{ .Branch }}{{ end }}_*
#
# DESCRIPTION
#       {{ with .Desc }}{{ . }}{{ else }}N/A{{ end }}
#
# OPTIONS
#       Tags       [{{ range $i, $e := .Tags }}{{ if $i }}, {{ end }}{{ $e }}{{ end }}]
#       TTL        {{ .TTL }}
#       Priority   {{ .Priority }}
#       Timeout    {{ .TimeoutDuration }}
#       Fatal      {{ .Fatal }}
#       Version    {{ if ne .MinVersion 0 }}{{ .MinVersion }}{{ else }}lower{{ end }} ~ {{ if ne .MaxVersion 0 }}{{ .MaxVersion }}{{ else }}higher{{ end }}
#       Source     {{ .Path }}
#
# METRICS
{{- range .ColumnList }}
#       {{ .Name }} ({{ .Usage }})
#           {{ with .Desc }}{{ . }}{{ else }}N/A{{ end }}{{ if .Bucket }}
#           Bucket {{ .Bucket }}{{ end }}{{ end }}
#
{{.MarshalYAML -}}
`)

var htmlTemplate, _ = htmltmpl.New("Query").Parse(`
<div style="border-style: solid; padding-left: 20px; padding-bottom: 10px;">

<h2>{{ .Name }}</h2>
<p>{{ .Desc }}</p>
{{ if len .PredicateQueries }}
<h4>Predicate queries</h4>
<table style="border-style: dotted;">
<thead><tr><th>Name</th> <th>SQL</th> <th>Cache TTL</th></tr></thead>
<tbody>
{{ range .PredicateQueries }}
<tr><td>{{ .Name }}</td><td><code>{{ .SQL }}</code></td><td>{{if ne .TTL 0}}{{ .TTL }}s{{else}}<i>not cached</i>{{end}}</td></tr>
{{ end }}
</tbody></table>
{{ end }}
<h4>Query</h4>
<code><pre>{{ .SQL }}</pre></code>

<h4>Attribution</h4>
<code><table style="border-style: dotted;"><tbody>
<tr><td>Branch   </td> <td> {{ .Branch }} </td></tr>
<tr><td>TTL      </td> <td> {{ .TTL }} </td></tr>
<tr><td>Priority </td> <td> {{ .Priority }} </td></tr>
<tr><td>Timeout  </td> <td> {{ .TimeoutDuration }} </td></tr>
<tr><td>Fatal    </td> <td> {{ .Fatal }} </td></tr>
<tr><td>Version  </td> <td> {{if ne .MinVersion 0}}{{ .MinVersion }}{{else}}lower{{end}} ~ {{if ne .MaxVersion 0}}{{ .MaxVersion }}{{else}}higher{{end}} </td></tr>
<tr><td>Tags     </td> <td> {{ .Tags }} </td></tr>
<tr><td>Source   </td> <td> {{ .Path }} </td></tr>
</tbody></table></code>

<h4>Columns</h4>
<code><table align="left" style="border-style: dotted;"><thead><tr><th>Name</th> <th>Usage</th> <th>Rename</th> <th>Bucket</th> <th>Scale</th> <th>Default</th> <th>Description</th></tr></thead>
<tbody>{{ range .ColumnList }}<tr><td>{{ .Name }}</td><td>{{ .Usage }}</td><td>{{ .Rename }}</td><td>{{ .Bucket }}</td><td>{{ .Scale }}</td><td>{{ .Default }}</td><td>{{ .Desc }}</td></tr>{{ end }}
</tbody></table></code>

<h4>Metrics</h4>
<code><table align="left" style="border-style: dotted;"><thead><tr><th>Name</th> <th>Usage</th> <th>Desc</th></tr></thead><tbody>
{{ range .MetricList }}<tr><td>{{ .Name }}</td><td>{{ .Column.Usage }}</td><td>{{ .Column.Desc }}</td></tr>{{ end }}
</tbody></table></code>
</div>
`)

// MarshalYAML will turn query into YAML format
func (q *Query) MarshalYAML() string {
	v := make(map[string]Query, 1)
	v[q.Branch] = *q
	// The classic API keeps four-space indentation and disables line wrapping.
	buf, err := yaml.Marshal(v)
	if err != nil {
		msg := fmt.Sprintf("fail to marshall query yaml: %s", err.Error())
		logError(msg)
		return msg
	}
	return string(buf)
}

// Explain will turn query into text format
func (q *Query) Explain() string {
	buf := new(bytes.Buffer)
	err := queryTemplate.Execute(buf, q)
	if err != nil {
		msg := fmt.Sprintf("fail to explain query: %s", err.Error())
		logError(msg)
		return msg
	}
	return buf.String()
}

// HTML will turn Query into HTML format
func (q *Query) HTML() string {
	buf := new(bytes.Buffer)
	err := htmlTemplate.Execute(buf, q)
	if err != nil {
		msg := fmt.Sprintf("fail to generate query html: %s", err.Error())
		logError(msg)
		return msg
	}
	return buf.String()
}

// HasTag tells whether this query have specific tag
// since only few tags is provided, we don't really need a map here
func (q *Query) HasTag(tag string) bool {
	return slices.Contains(q.Tags, tag)
}

// ColumnList return ordered column list
func (q *Query) ColumnList() (res []*Column) {
	res = make([]*Column, len(q.ColumnNames))
	for i, colName := range q.ColumnNames {
		res[i] = q.Columns[colName]
	}
	return
}

// LabelList returns a list of label column names
func (q *Query) LabelList() []string {
	labelNames := make([]string, len(q.LabelNames))
	for i, labelName := range q.LabelNames {
		labelColumn := q.Columns[labelName]
		if labelColumn.Rename != "" {
			labelNames[i] = labelColumn.Rename
		} else {
			labelNames[i] = labelColumn.Name
		}
	}
	return labelNames
}

// MetricList returns a list of MetricDesc generated by this query
func (q *Query) MetricList() (res []*MetricDesc) {
	res = make([]*MetricDesc, len(q.MetricNames))
	for i, metricName := range q.MetricNames {
		column := q.Columns[metricName]
		res[i] = column.MetricDesc(q.Name, q.LabelList())
	}
	return
}

// HasHistogram reports whether this query defines at least one logical
// Histogram metric. Histogram components are derived later by the collector.
func (q *Query) HasHistogram() bool {
	for _, metricName := range q.MetricNames {
		if column := q.Columns[metricName]; column != nil && column.IsHistogram() {
			return true
		}
	}
	return false
}

// TimeoutDuration will turn timeout settings into time.Duration
func (q *Query) TimeoutDuration() time.Duration {
	return time.Duration(float64(time.Second) * q.Timeout)
}
