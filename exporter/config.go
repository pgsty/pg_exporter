package exporter

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// GetConfig will try load config from target path
func GetConfig() (res string) {
	// priority: cli-args > env  > default settings (check exist)
	if res = *configPath; res != "" {
		logInfof("retrieve config path %s from command line", res)
		return res
	}
	if res = os.Getenv("PG_EXPORTER_CONFIG"); res != "" {
		logInfof("retrieve config path %s from PG_EXPORTER_CONFIG", res)
		return res
	}

	candidate := []string{"pg_exporter.yml", "/etc/pg_exporter.yml", "/etc/pg_exporter"}
	for _, res = range candidate {
		if _, err := os.Stat(res); err == nil { // default1 exist
			logInfof("fallback on default config path: %s", res)
			return res
		}
	}
	return ""
}

// ParseConfig turn config content into Query struct
func ParseConfig(content []byte) (queries map[string]*Query, err error) {
	queries = make(map[string]*Query)
	// Keep the classic API's empty-document and trailing-content handling.
	if err = yaml.Unmarshal(content, &queries); err != nil {
		return nil, fmt.Errorf("malformed config: %w", err)
	}

	// parse additional fields
	for branch, query := range queries {
		if query == nil {
			return nil, fmt.Errorf("query %q is null", branch)
		}
		query.Branch = branch
		if query.Name == "" {
			query.Name = branch
		}
		if strings.TrimSpace(query.SQL) == "" {
			return nil, fmt.Errorf("query %q has empty SQL", branch)
		}
		if query.TTL < 0 {
			return nil, fmt.Errorf("query %q has negative ttl: %v", branch, query.TTL)
		}
		if !validDurationSeconds(query.TTL) || !validDurationSeconds(query.Timeout) {
			return nil, fmt.Errorf("query %q ttl and timeout must be finite and fit in time.Duration", branch)
		}
		for i, pq := range query.PredicateQueries {
			if strings.TrimSpace(pq.SQL) == "" {
				return nil, fmt.Errorf("query %q has empty predicate_query at index %d", branch, i)
			}
			if pq.TTL < 0 {
				return nil, fmt.Errorf("query %q has negative predicate_queries[%d].ttl: %v", branch, i, pq.TTL)
			}
			if !validDurationSeconds(pq.TTL) {
				return nil, fmt.Errorf("query %q predicate_queries[%d].ttl must be finite and fit in time.Duration", branch, i)
			}
		}
		if len(query.Metrics) == 0 {
			return nil, fmt.Errorf("query %q has no metrics definition", branch)
		}
		// parse query column info
		columns := make(map[string]*Column, len(query.Metrics))
		var allColumns, labelColumns, metricColumns []string
		for _, colMap := range query.Metrics {
			if len(colMap) == 0 {
				return nil, fmt.Errorf("query %q has an empty metrics entry", branch)
			}
			if len(colMap) != 1 {
				return nil, fmt.Errorf("query %q has invalid metrics entry with %d columns, expect exactly 1", branch, len(colMap))
			}
			for colName, column := range colMap { // one-entry map
				if column == nil {
					return nil, fmt.Errorf("query %q has null column definition for %q", branch, colName)
				}
				if column.Name == "" {
					column.Name = colName
				}
				usage := strings.ToUpper(strings.TrimSpace(column.Usage))
				if usage == "" {
					return nil, fmt.Errorf("query %q column %q has empty usage", branch, colName)
				}
				if _, isValid := ColumnUsage[usage]; !isValid {
					return nil, fmt.Errorf("query %q column %q has unsupported usage: %s", branch, colName, column.Usage)
				}
				column.Usage = usage
				if err := column.parseNumbers(); err != nil {
					return nil, fmt.Errorf("query %q column %q: %w", branch, colName, err)
				}
				switch column.Usage {
				case LABEL:
					labelColumns = append(labelColumns, column.Name)
				case GAUGE, COUNTER, HISTOGRAM:
					if column.IsHistogram() {
						if err := validateHistogramBuckets(column.Bucket); err != nil {
							return nil, fmt.Errorf("query %q column %q: %w", branch, colName, err)
						}
					}
					metricColumns = append(metricColumns, column.Name)
				}
				allColumns = append(allColumns, column.Name)
				if _, exists := columns[column.Name]; exists {
					return nil, fmt.Errorf("query %q has duplicate column name %q", branch, column.Name)
				}
				columns[column.Name] = column
			}
		}
		if len(metricColumns) == 0 {
			return nil, fmt.Errorf("query %q defines no GAUGE/COUNTER/HISTOGRAM columns", branch)
		}
		query.Columns, query.ColumnNames, query.LabelNames, query.MetricNames = columns, allColumns, labelColumns, metricColumns
		hasHistogram := query.HasHistogram()

		// Validate prometheus label names and metric names. This prevents panics at scrape time.
		seenLabels := make(map[string]bool, len(query.LabelNames))
		for _, labelColName := range query.LabelNames {
			c := query.Columns[labelColName]
			if c == nil {
				return nil, fmt.Errorf("query %q missing label column %q", branch, labelColName)
			}
			lbl := c.Name
			if c.Rename != "" {
				lbl = c.Rename
			}
			if err := validatePromLabelName(lbl); err != nil {
				return nil, fmt.Errorf("query %q label %q: %w", branch, lbl, err)
			}
			if hasHistogram && lbl == "le" {
				return nil, fmt.Errorf("query %q label %q conflicts with generated Histogram bucket label %q", branch, lbl, "le")
			}
			if seenLabels[lbl] {
				return nil, fmt.Errorf("query %q has duplicate label name %q", branch, lbl)
			}
			seenLabels[lbl] = true
		}

		// Reserve every logical base name and every emitted family name. Reserving
		// Histogram bases as well keeps post-rename names unambiguous even though
		// version 1 emits only the derived _bucket, _count, and _sum families.
		seenMetrics := make(map[string]string, len(query.MetricNames)*4)
		for _, metricColName := range query.MetricNames {
			c := query.Columns[metricColName]
			if c == nil {
				return nil, fmt.Errorf("query %q missing metric column %q", branch, metricColName)
			}
			suffix := c.Name
			if c.Rename != "" {
				suffix = c.Rename
			}
			metricName := fmt.Sprintf("%s_%s", query.Name, suffix)
			if err := validatePromMetricName(metricName); err != nil {
				return nil, fmt.Errorf("query %q metric %q: %w", branch, metricName, err)
			}

			familyNames := []string{metricName}
			if c.IsHistogram() {
				familyNames = append(familyNames,
					metricName+"_bucket",
					metricName+"_count",
					metricName+"_sum",
				)
			}
			for _, familyName := range familyNames {
				if err := validatePromMetricName(familyName); err != nil {
					return nil, fmt.Errorf("query %q metric %q derived family %q: %w", branch, metricName, familyName, err)
				}
				if previous, exists := seenMetrics[familyName]; exists {
					return nil, fmt.Errorf("query %q metric %q family %q conflicts with metric %q", branch, metricName, familyName, previous)
				}
				seenMetrics[familyName] = metricName
			}
		}
	}
	return
}

func validDurationSeconds(seconds float64) bool {
	// Negative finite timeouts disable the deadline and are never converted.
	// The float64 representation of MaxInt64 rounds up, so equality is unsafe.
	return !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds < float64(math.MaxInt64)/float64(time.Second)
}

// validateHistogramBuckets validates the finite inclusive upper boundaries
// configured for a snapshot Histogram. The +Inf bucket is generated at
// collection time and must not appear in configuration.
func validateHistogramBuckets(buckets []float64) error {
	if len(buckets) == 0 {
		return fmt.Errorf("HISTOGRAM requires at least one bucket")
	}
	for i, bucket := range buckets {
		if math.IsNaN(bucket) || math.IsInf(bucket, 0) {
			return fmt.Errorf("HISTOGRAM bucket[%d] must be finite, got %v", i, bucket)
		}
		if i > 0 && bucket <= buckets[i-1] {
			return fmt.Errorf("HISTOGRAM buckets must be strictly increasing: bucket[%d]=%v follows %v", i, bucket, buckets[i-1])
		}
	}
	return nil
}

func FinalizeQueries(queries map[string]*Query, source string) error {
	for branch, q := range queries {
		if q == nil {
			return fmt.Errorf("query %q is null", branch)
		}
		q.Path = source
		// If timeout is not set, set to 100ms by default.
		// If timeout is set to a negative number, set to 0 (disabled).
		if q.Timeout == 0 {
			q.Timeout = 0.1
		}
		if q.Timeout < 0 {
			q.Timeout = 0
		}
	}
	return nil
}

// ParseQuery generate a single query from config string
func ParseQuery(config string) (*Query, error) {
	queries, err := ParseConfig([]byte(config))
	if err != nil {
		return nil, err
	}
	if len(queries) == 0 {
		return nil, fmt.Errorf("no query definition found")
	}
	if len(queries) > 1 {
		return nil, fmt.Errorf("multiple query definition found")
	}
	if err := FinalizeQueries(queries, "<inline>"); err != nil {
		return nil, err
	}
	for _, q := range queries {
		return q, nil // return the only query instance
	}
	return nil, fmt.Errorf("no query definition found")
}

// LoadConfig will read single conf file or read multiple conf file if a dir is given
// conf file in a dir will be load in alphabetic order, query with same name will overwrite predecessor
func LoadConfig(configPath string) (queries map[string]*Query, err error) {
	stat, err := os.Stat(configPath)
	if err != nil {
		return nil, fmt.Errorf("invalid config path: %s: %w", configPath, err)
	}
	if stat.IsDir() { // iterate conf files (non-recursive) if a dir is given
		files, err := os.ReadDir(configPath)
		if err != nil {
			return nil, fmt.Errorf("fail reading config dir: %s: %w", configPath, err)
		}

		logDebugf("load config from dir: %s", configPath)
		confFiles := make([]string, 0)
		for _, conf := range files {
			if conf.IsDir() {
				continue // skip subdirectories
			}
			if !(strings.HasSuffix(conf.Name(), ".yaml") || strings.HasSuffix(conf.Name(), ".yml")) {
				continue // skip non-yaml files
			}
			confFiles = append(confFiles, filepath.Join(configPath, conf.Name()))
		}

		// make global config map and assign priority according to config file alphabetic orders
		// priority is an integer range from 1 to 999, where 1 - 99 is reserved for user
		queries = make(map[string]*Query)
		var queryCount, configCount int
		var firstErr error
		for _, confPath := range confFiles {
			if singleQueries, err := LoadConfig(confPath); err != nil {
				logWarnf("skip config %s due to error: %s", confPath, err.Error())
				if firstErr == nil {
					firstErr = err
				}
			} else {
				configCount++
				for name, query := range singleQueries {
					queryCount++
					if query.Priority == 0 { // set to config rank if not manually set
						query.Priority = 100 + configCount
					}
					queries[name] = query // so the later one will overwrite former one
				}
			}
		}
		if len(confFiles) > 0 && len(queries) == 0 {
			if firstErr != nil {
				return nil, fmt.Errorf("no valid queries loaded from config dir %s (%d yaml files), first error: %w", configPath, len(confFiles), firstErr)
			}
			return nil, fmt.Errorf("no queries loaded from config dir %s (%d yaml files)", configPath, len(confFiles))
		}
		logDebugf("load %d of %d queries from %d config files", len(queries), queryCount, configCount)
		return queries, nil
	}

	// single file case: recursive exit condition
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("fail reading config file %s: %w", configPath, err)
	}
	queries, err = ParseConfig(content)
	if err != nil {
		return nil, err
	}
	if err := FinalizeQueries(queries, stat.Name()); err != nil {
		return nil, err
	}
	logDebugf("load %d queries from %s", len(queries), configPath)
	return queries, nil

}
