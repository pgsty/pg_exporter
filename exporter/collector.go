package exporter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

/* ================ Collector ================ */

type predicateCacheEntry struct {
	at   time.Time
	pass bool
}

type histogramMetricDescriptors struct {
	bucket *prometheus.Desc
	count  *prometheus.Desc
	sum    *prometheus.Desc
}

// histogramAccumulator holds one SQL result-set snapshot for one histogram
// column and one complete (dynamic) label tuple. Server const labels are fixed
// for the lifetime of a Collector, so they are implicitly part of this group.
type histogramAccumulator struct {
	column *Column
	labels []string
	counts []uint64 // non-cumulative counts; last element is the +Inf interval
	count  uint64
	sum    float64
}

// Collector holds runtime information of a Query running on a Server
// It is deeply coupled with Server. Besides, it can be a collector itself
type Collector struct {
	*Query
	Server *Server // It's a query, but holds a server

	// runtime information
	lock          sync.RWMutex                // scrape lock
	result        []prometheus.Metric         // cached metrics
	descriptors   map[string]*prometheus.Desc // scalar column name to descriptor, built on init
	histogramDesc map[string]histogramMetricDescriptors
	cacheHit      bool   // indicate last scrape was served from cache or real execution
	predicateSkip string // if nonempty, predicate query caused skip of this scrape
	err           error

	// predicate cache. Entry i caches PredicateQueries[i] if it has a positive TTL.
	predicateCache []predicateCacheEntry

	// stats
	lastScrape     time.Time     // SERVER's scrape start time (for cache window align)
	scrapeBegin    time.Time     // execution begin time
	scrapeDone     time.Time     // execution complete time
	scrapeDuration time.Duration // last real execution duration
}

// NewCollector will generate query instance from query, Injecting a server object
func NewCollector(q *Query, s *Server) *Collector {
	instance := &Collector{
		Query:  q,
		Server: s,
		result: make([]prometheus.Metric, 0),
	}
	if len(q.PredicateQueries) > 0 {
		instance.predicateCache = make([]predicateCacheEntry, len(q.PredicateQueries))
	}
	instance.makeDescMap()
	return instance
}

// Describe implement prometheus.Collector
func (q *Collector) Describe(ch chan<- *prometheus.Desc) {
	q.lock.Lock()
	defer q.lock.Unlock()
	q.sendDescriptors(ch)
}

// Collect implement prometheus.Collector
func (q *Collector) Collect(ch chan<- prometheus.Metric) {
	q.collect(context.Background(), ch)
}

func (q *Collector) collect(ctx context.Context, ch chan<- prometheus.Metric) {
	q.lock.Lock()
	defer q.lock.Unlock()
	if ctx.Err() != nil {
		return
	}
	q.scrapeBegin = time.Now()
	if q.cacheExpired() || q.Server.DisableCache {
		q.executeContext(ctx)
		q.cacheHit = false
		q.scrapeDone = time.Now()
		q.scrapeDuration = q.scrapeDone.Sub(q.scrapeBegin)
		q.lastScrape = q.Server.scrapeBegin
	} else { // serve from cache
		q.cacheHit = true
		q.scrapeDone = time.Now()
	}
	for _, metric := range q.result {
		select {
		case ch <- metric:
		case <-ctx.Done():
			return
		}
	}
}

// ResultSize report last scraped metric count
func (q *Collector) ResultSize() int {
	return len(q.result)
}

// Error wraps query error (including error in predicate query)
func (q *Collector) Error() error {
	return q.err
}

// PredicateSkip tells the last scrape skip due to predicate query and if so which predicate query caused the skip?
func (q *Collector) PredicateSkip() (bool, string) {
	return q.predicateSkip != "", q.predicateSkip
}

// Duration returns last scrape duration in float64 seconds
func (q *Collector) Duration() float64 {
	return q.scrapeDone.Sub(q.scrapeBegin).Seconds()
}

// CacheHit report whether last scrape was serve from cache
func (q *Collector) CacheHit() bool {
	return q.cacheHit
}

// Run any predicate queries for this query. Return true only if all predicate queries pass.
// As a side effect sets predicateSkip to the first predicate query that failed, using
// the predicate query name if specified otherwise the index.
func (q *Collector) executePredicateQueries(ctx context.Context) bool {
	for i, predicateQuery := range q.PredicateQueries {
		predicateQueryName := predicateQuery.Name
		if predicateQueryName == "" {
			predicateQueryName = fmt.Sprintf("%d", i)
		}
		q.predicateSkip = predicateQueryName

		msgPrefix := fmt.Sprintf("predicate query [%s] for query [%s] @ server [%s]", predicateQueryName, q.Name, q.Server.Database)

		// Optional predicate cache (independent of main query cache).
		if predicateQuery.TTL > 0 && len(q.predicateCache) == len(q.PredicateQueries) {
			entry := q.predicateCache[i]
			if !entry.at.IsZero() {
				ttl := time.Duration(predicateQuery.TTL * float64(time.Second))
				if q.scrapeBegin.Sub(entry.at) < ttl {
					logDebugf("%s served from predicate cache (ttl=%vs, pass=%v)", msgPrefix, predicateQuery.TTL, entry.pass)
					if entry.pass {
						continue
					}
					// cached skip
					return false
				}
			}
		}

		// Execute the predicate query.
		logDebugf("%s executing predicate query", msgPrefix)
		rows, err := q.Server.QueryContext(ctx, predicateQuery.SQL)
		if err != nil {
			// If a predicate query fails that's treated as a skip, and the err
			// flag is set so Fatal will be respected if set.
			if errors.Is(err, context.DeadlineExceeded) { // timeout
				q.err = fmt.Errorf("%s timeout after %v: %w", msgPrefix, time.Since(q.scrapeBegin), err)
			} else {
				q.err = fmt.Errorf("%s failed: %w", msgPrefix, err)
			}
			return false
		}

		// The predicate passes if it returns exactly one row with one column
		// that is a boolean true.
		colTypes, err := rows.ColumnTypes()
		if err != nil {
			q.err = fmt.Errorf("%s failed to get column types: %w", msgPrefix, err)
			_ = rows.Close()
			return false
		}
		if len(colTypes) != 1 {
			q.err = fmt.Errorf("%s failed because it returned %d columns, expected 1", msgPrefix, len(colTypes))
			_ = rows.Close()
			return false
		}
		typeName := strings.ToUpper(colTypes[0].DatabaseTypeName())
		if typeName != "BOOL" && typeName != "BOOLEAN" {
			q.err = fmt.Errorf("%s failed because it returned a column of type %s, expect bool. consider a cast(colname as boolean) or colname::boolean in the query", msgPrefix, colTypes[0].DatabaseTypeName())
			_ = rows.Close()
			return false
		}

		firstRow := true
		predicatePass := sql.NullBool{}
		for rows.Next() {
			if !firstRow {
				q.err = fmt.Errorf("%s failed because it returned more than one row", msgPrefix)
				_ = rows.Close()
				return false
			}
			firstRow = false
			err = rows.Scan(&predicatePass)
			if err != nil {
				q.err = fmt.Errorf("%s failed scanning in expected 1-row 1-column nullable boolean result: %w", msgPrefix, err)
				_ = rows.Close()
				return false
			}
		}
		if err = rows.Err(); err != nil {
			q.err = fmt.Errorf("%s failed while iterating rows: %w", msgPrefix, err)
			_ = rows.Close()
			return false
		}
		if err = rows.Close(); err != nil {
			q.err = fmt.Errorf("%s failed closing rows: %w", msgPrefix, err)
			return false
		}
		pass := predicatePass.Valid && predicatePass.Bool
		if predicateQuery.TTL > 0 && len(q.predicateCache) == len(q.PredicateQueries) {
			q.predicateCache[i] = predicateCacheEntry{at: q.scrapeBegin, pass: pass}
		}
		if !pass {
			// successfully executed predicate query requested a skip
			logDebugf("%s returned false, null or zero rows, skipping query", msgPrefix)
			return false
		}
		logDebugf("%s returned true", msgPrefix)
	}
	// If we get here, all predicate queries passed.
	q.predicateSkip = ""
	return true
}

// execute will run this query to registered server, result and err are registered
func (q *Collector) execute() {
	q.executeContext(context.Background())
}

func (q *Collector) executeContext(ctx context.Context) {
	// A failed refresh must never publish a prefix of the new result or retain an
	// old snapshot. Build the complete scrape locally and publish it only after
	// rows, scalar metrics, and all histogram groups have been validated.
	pending := q.result[:0]
	q.result = nil
	q.err = nil
	q.predicateSkip = ""
	var rows *sql.Rows
	var err error

	if q.Timeout != 0 { // if timeout is provided, use context
		logDebugf("query [%s] @ server [%s] executing begin with time limit: %v", q.Name, q.Server.Database, q.TimeoutDuration())
		// A started bounded refresh (its predicates and main query share this one
		// deadline) finishes even if the request ends: a departing client must not
		// turn a healthy refresh into a cached failure shared by every scraper. No
		// further collector starts once the request ends. Only unbounded queries
		// (negative timeout) follow the request budget.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), q.TimeoutDuration())
		defer cancel()
	} else {
		logDebugf("query [%s] @ server [%s] executing begin", q.Name, q.Server.Database)
	}

	// check predicate queries if any
	if predicatePass := q.executePredicateQueries(ctx); !predicatePass {
		// predicateSkip and err if appropriate were set as side effects
		return
	}

	// main query execution
	rows, err = q.Server.QueryContext(ctx, q.SQL)

	// error handling: if query failed because of timeout or error, record and return
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) { // timeout
			q.err = fmt.Errorf("query [%s] timeout after %v: %w", q.Name, time.Since(q.scrapeBegin), err)
		} else {
			q.err = fmt.Errorf("query [%s] failed: %w", q.Name, err)
		}
		return
	}

	// close rows
	defer func(rows *sql.Rows) { _ = rows.Close() }(rows)

	// parsing meta:  fetch column metadata for dynamic name lookup
	columnNames, err := rows.Columns()
	if err != nil {
		q.err = fmt.Errorf("query [%s] fail retriving rows meta: %w", q.Name, err)
		return
	}
	columnIndexes := make(map[string]int, len(columnNames)) // column name to index
	for i, n := range columnNames {
		columnIndexes[n] = i
	}
	nColumn := len(columnNames)
	colData := make([]interface{}, nColumn)
	colArgs := make([]interface{}, nColumn)
	for i := range colData {
		colArgs[i] = &colData[i]
	}
	if len(columnNames) != len(q.Columns) { // warn if column count not match
		logWarnf("query [%s] column count not match, result %d ≠ config %d", q.Name, len(columnNames), len(q.Columns))
	}
	// A missing label invalidates the identity of every resulting series, so fail
	// this collector instead of substituting an empty label value.
	for _, labelName := range q.LabelNames {
		if _, found := columnIndexes[labelName]; !found {
			q.err = fmt.Errorf("query [%s] missing label column %s.%s in result", q.Name, q.Name, labelName)
			return
		}
	}
	// A missing scalar column preserves the historic warn-and-skip behavior. A
	// missing histogram column is an error because silently emitting no groups is
	// indistinguishable from a valid empty population.
	for _, metricName := range q.MetricNames {
		column := q.Columns[metricName]
		if column != nil && column.IsHistogram() {
			if _, found := columnIndexes[metricName]; !found {
				q.err = fmt.Errorf("query [%s] missing histogram column %s.%s in result", q.Name, q.Name, metricName)
				return
			}
		}
	}

	histograms := make(map[string]map[string]*histogramAccumulator)

	// scan loop: for each row, extract labels from all label columns, then generate a new metric for each metric column
	for rows.Next() {
		if q.err = ctx.Err(); q.err != nil {
			return
		}
		err = rows.Scan(colArgs...)
		if err != nil {
			q.err = fmt.Errorf("fail scanning rows: %w", err)
			return
		}

		// get labels, sequence matters, empty string for null labels
		labels := make([]string, len(q.LabelNames))
		for i, labelName := range q.LabelNames {
			labels[i] = castString(colData[columnIndexes[labelName]])
		}

		// get metrics, warn if column not exist
		for _, metricName := range q.MetricNames {
			if dataIndex, found := columnIndexes[metricName]; found { // the metric column is found in result
				column := q.Columns[metricName]
				if column.IsHistogram() {
					value, present, castErr := castHistogramFloat64(colData[dataIndex], column)
					if castErr != nil {
						q.err = fmt.Errorf("query [%s] histogram column %s.%s: %w", q.Name, q.Name, metricName, castErr)
						return
					}
					if !present {
						continue
					}

					groups := histograms[metricName]
					if groups == nil {
						groups = make(map[string]*histogramAccumulator)
						histograms[metricName] = groups
					}
					labelKey := encodeLabelTuple(labels)
					accumulator := groups[labelKey]
					if accumulator == nil {
						accumulator = &histogramAccumulator{
							column: column,
							labels: append([]string(nil), labels...),
							counts: make([]uint64, len(column.Bucket)+1),
						}
						groups[labelKey] = accumulator
					}
					bucketIndex := sort.SearchFloat64s(column.Bucket, value)
					accumulator.counts[bucketIndex]++
					accumulator.count++
					accumulator.sum += value
					if math.IsNaN(accumulator.sum) || math.IsInf(accumulator.sum, 0) {
						q.err = fmt.Errorf("query [%s] histogram column %s.%s: non-finite observation sum", q.Name, q.Name, metricName)
						return
					}
					continue
				}

				metric, metricErr := prometheus.NewConstMetric(
					q.descriptors[metricName], // always find desc & column via name
					column.PrometheusValueType(),
					castFloat64(colData[dataIndex], column),
					labels...,
				)
				if metricErr != nil {
					q.err = fmt.Errorf("query [%s] failed building metric %s.%s: %w", q.Name, q.Name, metricName, metricErr)
					return
				}
				pending = append(pending, metric)
			} else {
				logWarnf("missing metric column %s.%s in result", q.Name, metricName)
			}
		}
	}
	if err = rows.Err(); err != nil {
		q.err = fmt.Errorf("query [%s] failed while iterating rows: %w", q.Name, err)
		return
	}

	// Emit histogram groups in config metric order and collision-safe label-key
	// order. Each group is bucket(s), +Inf, count, then sum.
	for _, metricName := range q.MetricNames {
		column := q.Columns[metricName]
		if column == nil || !column.IsHistogram() {
			continue
		}
		groups := histograms[metricName]
		groupKeys := make([]string, 0, len(groups))
		for groupKey := range groups {
			groupKeys = append(groupKeys, groupKey)
		}
		sort.Strings(groupKeys)
		for _, groupKey := range groupKeys {
			accumulator := groups[groupKey]
			metrics, metricErr := q.histogramMetrics(metricName, accumulator)
			if metricErr != nil {
				q.err = fmt.Errorf("query [%s] failed building histogram %s.%s: %w", q.Name, q.Name, metricName, metricErr)
				return
			}
			pending = append(pending, metrics...)
		}
	}

	if q.err = ctx.Err(); q.err != nil {
		return
	}
	q.result = pending
	q.err = nil
	logDebugf("query [%s] executing complete in %v, metrics count: %d",
		q.Name, time.Since(q.scrapeBegin), len(q.result))
}

// histogramMetrics materializes a snapshot histogram as ordinary Gauge series.
// It deliberately does not use prometheus.Histogram: SQL refreshes are
// independent snapshots whose bucket/count/sum values may decrease.
func (q *Collector) histogramMetrics(metricName string, accumulator *histogramAccumulator) ([]prometheus.Metric, error) {
	descriptors, found := q.histogramDesc[metricName]
	if !found {
		return nil, fmt.Errorf("missing descriptors")
	}

	result := make([]prometheus.Metric, 0, len(accumulator.column.Bucket)+3)
	bucketLabels := make([]string, len(accumulator.labels)+1)
	copy(bucketLabels, accumulator.labels)
	var cumulative uint64
	for i, upperBound := range accumulator.column.Bucket {
		cumulative += accumulator.counts[i]
		bucketLabels[len(bucketLabels)-1] = strconv.FormatFloat(upperBound, 'g', -1, 64)
		metric, err := prometheus.NewConstMetric(descriptors.bucket, prometheus.GaugeValue, float64(cumulative), bucketLabels...)
		if err != nil {
			return nil, err
		}
		result = append(result, metric)
	}

	cumulative += accumulator.counts[len(accumulator.column.Bucket)]
	bucketLabels[len(bucketLabels)-1] = "+Inf"
	metric, err := prometheus.NewConstMetric(descriptors.bucket, prometheus.GaugeValue, float64(cumulative), bucketLabels...)
	if err != nil {
		return nil, err
	}
	result = append(result, metric)

	metric, err = prometheus.NewConstMetric(descriptors.count, prometheus.GaugeValue, float64(accumulator.count), accumulator.labels...)
	if err != nil {
		return nil, err
	}
	result = append(result, metric)

	metric, err = prometheus.NewConstMetric(descriptors.sum, prometheus.GaugeValue, accumulator.sum, accumulator.labels...)
	if err != nil {
		return nil, err
	}
	result = append(result, metric)
	return result, nil
}

/* ================ Collector Auxiliary ================ */

// makeDescMap will generate descriptor map from Query
func (q *Collector) makeDescMap() {
	descriptors := make(map[string]*prometheus.Desc)
	histogramDescriptors := make(map[string]histogramMetricDescriptors)

	// rename label name if label column have rename option
	labelNames := make([]string, len(q.LabelNames))
	for i, labelName := range q.LabelNames {
		labelColumn := q.Columns[labelName]
		if labelColumn.Rename != "" {
			labelNames[i] = labelColumn.Rename
		} else {
			labelNames[i] = labelColumn.Name
		}
	}

	// rename metric if metric column have a rename option
	for _, metricName := range q.MetricNames {
		metricColumn := q.Columns[metricName] // always found
		prometheusName := fmt.Sprintf("%s_%s", q.Name, metricColumn.Name)
		if metricColumn.Rename != "" {
			prometheusName = fmt.Sprintf("%s_%s", q.Name, metricColumn.Rename)
		}
		if metricColumn.IsHistogram() {
			bucketLabelNames := append(append([]string(nil), labelNames...), "le")
			histogramDescriptors[metricName] = histogramMetricDescriptors{
				bucket: prometheus.NewDesc(
					prometheusName+"_bucket", histogramHelp(metricColumn.Desc, "bucket"), bucketLabelNames, q.Server.labels,
				),
				count: prometheus.NewDesc(
					prometheusName+"_count", histogramHelp(metricColumn.Desc, "count"), labelNames, q.Server.labels,
				),
				sum: prometheus.NewDesc(
					prometheusName+"_sum", histogramHelp(metricColumn.Desc, "sum"), labelNames, q.Server.labels,
				),
			}
			continue
		}
		descriptors[metricColumn.Name] = prometheus.NewDesc(
			prometheusName, metricColumn.Desc, labelNames, q.Server.labels,
		)
	}
	q.descriptors = descriptors
	q.histogramDesc = histogramDescriptors
}

func (q *Collector) sendDescriptors(ch chan<- *prometheus.Desc) {
	for _, desc := range q.descriptors {
		ch <- desc
	}
	for _, descriptors := range q.histogramDesc {
		ch <- descriptors.bucket
		ch <- descriptors.count
		ch <- descriptors.sum
	}
}

func histogramHelp(description, component string) string {
	suffix := map[string]string{
		"bucket": "cumulative bucket",
		"count":  "observation count",
		"sum":    "observation sum",
	}[component]
	if suffix == "" {
		suffix = component
	}
	if description == "" {
		return suffix
	}
	return fmt.Sprintf("%s (%s)", description, suffix)
}

// cacheExpired report whether this instance needs actual execution
// Note you have to use Server.scrapeBegin as "now", and set that timestamp as
func (q *Collector) cacheExpired() bool {
	return q.Server.scrapeBegin.Sub(q.lastScrape) > time.Duration(q.TTL*float64(time.Second))
}

func (q *Collector) cacheTTL() float64 {
	return q.TTL - q.Server.scrapeBegin.Sub(q.lastScrape).Seconds()
}

// sendMetrics will send cached result to ch
func (q *Collector) sendMetrics(ch chan<- prometheus.Metric) {
	for _, metric := range q.result {
		ch <- metric
	}
}
