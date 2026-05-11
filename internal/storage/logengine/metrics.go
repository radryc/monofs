package logengine

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	logengineQueriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "logengine",
		Name:      "queries_total",
		Help:      "Total telemetry queries executed by the MonoFS logengine, by signal.",
	}, []string{"signal"})

	logengineQueryDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "monofs",
		Subsystem: "logengine",
		Name:      "query_duration_seconds",
		Help:      "End-to-end telemetry query duration in the MonoFS logengine, by signal.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"signal"})

	logengineQueryStageDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "monofs",
		Subsystem: "logengine",
		Name:      "query_stage_duration_seconds",
		Help:      "Query-stage duration for chunk listing, manifest pruning, and parquet opens, by signal and stage.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"signal", "stage"})

	logengineQueryChunksListedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "logengine",
		Name:      "query_chunks_listed_total",
		Help:      "Total chunk IDs listed for logengine queries before manifest pruning, by signal.",
	}, []string{"signal"})

	logengineQueryChunksPrunedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "logengine",
		Name:      "query_chunks_pruned_total",
		Help:      "Total chunks skipped during manifest pruning, by signal and prune reason.",
	}, []string{"signal", "reason"})

	logengineQueryParquetOpensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "logengine",
		Name:      "query_parquet_opens_total",
		Help:      "Total parquet open attempts performed by logengine queries, by signal and result.",
	}, []string{"signal", "result"})

	logengineQueryReturnedRecordsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "monofs",
		Subsystem: "logengine",
		Name:      "query_returned_records_total",
		Help:      "Total query records returned after filtering and limits, by signal.",
	}, []string{"signal"})
)

type queryPathObserver struct {
	signal  string
	started time.Time
}

func beginQueryPathObservation(signal Signal) *queryPathObserver {
	label := string(signal)
	logengineQueriesTotal.WithLabelValues(label).Inc()
	return &queryPathObserver{signal: label, started: time.Now()}
}

func (o *queryPathObserver) finish() {
	logengineQueryDurationSeconds.WithLabelValues(o.signal).Observe(time.Since(o.started).Seconds())
}

func (o *queryPathObserver) observeStage(stage string, started time.Time) {
	logengineQueryStageDurationSeconds.WithLabelValues(o.signal, stage).Observe(time.Since(started).Seconds())
}

func (o *queryPathObserver) addChunksListed(count int) {
	if count <= 0 {
		return
	}
	logengineQueryChunksListedTotal.WithLabelValues(o.signal).Add(float64(count))
}

func (o *queryPathObserver) addChunksPruned(reason string, count int) {
	if count <= 0 {
		return
	}
	logengineQueryChunksPrunedTotal.WithLabelValues(o.signal, reason).Add(float64(count))
}

func (o *queryPathObserver) addParquetOpen(result string) {
	logengineQueryParquetOpensTotal.WithLabelValues(o.signal, result).Inc()
}

func (o *queryPathObserver) addReturnedRecords(count int) {
	if count <= 0 {
		return
	}
	logengineQueryReturnedRecordsTotal.WithLabelValues(o.signal).Add(float64(count))
}
