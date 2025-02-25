// SPDX-License-Identifier: AGPL-3.0-only

package continuoustest

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
)

const (
	writeInterval = 20 * time.Second
	metricName    = "mimir_continuous_test_sine_wave"
)

type WriteReadSeriesTestConfig struct {
	NumSeries   int
	MaxQueryAge time.Duration
}

func (cfg *WriteReadSeriesTestConfig) RegisterFlags(f *flag.FlagSet) {
	f.IntVar(&cfg.NumSeries, "tests.write-read-series-test.num-series", 10000, "Number of series used for the test.")
	f.DurationVar(&cfg.MaxQueryAge, "tests.write-read-series-test.max-query-age", 7*24*time.Hour, "How back in the past metrics can be queried at most.")
}

type WriteReadSeriesTest struct {
	name    string
	cfg     WriteReadSeriesTestConfig
	client  MimirClient
	logger  log.Logger
	metrics *TestMetrics

	lastWrittenTimestamp time.Time
	queryMinTime         time.Time
	queryMaxTime         time.Time
}

func NewWriteReadSeriesTest(cfg WriteReadSeriesTestConfig, client MimirClient, logger log.Logger, reg prometheus.Registerer) *WriteReadSeriesTest {
	const name = "write-read-series"

	return &WriteReadSeriesTest{
		name:    name,
		cfg:     cfg,
		client:  client,
		logger:  log.With(logger, "test", name),
		metrics: NewTestMetrics(name, reg),
	}
}

// Name implements Test.
func (t *WriteReadSeriesTest) Name() string {
	return t.name
}

// Init implements Test.
func (t *WriteReadSeriesTest) Init() error {
	// TODO Here we should populate lastWrittenTimestamp, queryMinTime, queryMaxTime after querying Mimir to get data previously written.
	return nil
}

// Run implements Test.
func (t *WriteReadSeriesTest) Run(ctx context.Context, now time.Time) {
	// Write series for each expected timestamp until now.
	for timestamp := t.nextWriteTimestamp(now); !timestamp.After(now); timestamp = t.nextWriteTimestamp(now) {
		statusCode, err := t.client.WriteSeries(ctx, generateSineWaveSeries(metricName, timestamp, t.cfg.NumSeries))

		t.metrics.writesTotal.Inc()
		if statusCode/100 != 2 {
			t.metrics.writesFailedTotal.WithLabelValues(strconv.Itoa(statusCode)).Inc()
			level.Warn(t.logger).Log("msg", "Failed to remote write series", "num_series", t.cfg.NumSeries, "timestamp", timestamp.String(), "status_code", statusCode, "err", err)
		} else {
			level.Debug(t.logger).Log("msg", "Remote write series succeeded", "num_series", t.cfg.NumSeries, "timestamp", timestamp.String())
		}

		// If the write request failed because of a 4xx error, retrying the request isn't expected to succeed.
		// The series may have been not written at all or partially written (eg. we hit some limit).
		// We keep writing the next interval, but we reset the query timestamp because we can't reliably
		// assert on query results due to possible gaps.
		if statusCode/100 == 4 {
			t.lastWrittenTimestamp = timestamp
			t.queryMinTime = time.Time{}
			t.queryMaxTime = time.Time{}
			continue
		}

		// If the write request failed because of a network or 5xx error, we'll retry to write series
		// in the next test run.
		if statusCode/100 != 2 || err != nil {
			break
		}

		// The write request succeeded.
		t.lastWrittenTimestamp = timestamp
		t.queryMaxTime = timestamp
		if t.queryMinTime.IsZero() {
			t.queryMinTime = timestamp
		}
	}

	queryRanges, queryInstants := t.getQueryTimeRanges(now)
	for _, timeRange := range queryRanges {
		t.runRangeQueryAndVerifyResult(ctx, timeRange[0], timeRange[1])
	}
	for _, ts := range queryInstants {
		t.runInstantQueryAndVerifyResult(ctx, ts)
	}
}

// getQueryTimeRanges returns the start/end time ranges to use to run test range queries,
// and the timestamps to use to run test instant queries.
func (t *WriteReadSeriesTest) getQueryTimeRanges(now time.Time) (ranges [][2]time.Time, instants []time.Time) {
	// The min and max allowed query timestamps are zero if there's no successfully written data yet.
	if t.queryMinTime.IsZero() || t.queryMaxTime.IsZero() {
		level.Info(t.logger).Log("msg", "Skipped queries because there's no valid time range to query")
		return nil, nil
	}

	// Honor the configured max age.
	adjustedQueryMinTime := maxTime(t.queryMinTime, now.Add(-t.cfg.MaxQueryAge))
	if t.queryMaxTime.Before(adjustedQueryMinTime) {
		level.Info(t.logger).Log("msg", "Skipped queries because there's no valid time range to query after honoring configured max query age", "min_valid_time", t.queryMinTime, "max_valid_time", t.queryMaxTime, "max_query_age", t.cfg.MaxQueryAge)
		return
	}

	// Last 1h.
	if t.queryMaxTime.After(now.Add(-1 * time.Hour)) {
		ranges = append(ranges, [2]time.Time{
			maxTime(adjustedQueryMinTime, now.Add(-1*time.Hour)),
			minTime(t.queryMaxTime, now),
		})
		instants = append(instants, minTime(t.queryMaxTime, now))
	}

	// Last 24h (only if the actual time range is not already covered by "Last 1h").
	if t.queryMaxTime.After(now.Add(-24*time.Hour)) && adjustedQueryMinTime.Before(now.Add(-1*time.Hour)) {
		ranges = append(ranges, [2]time.Time{
			maxTime(adjustedQueryMinTime, now.Add(-24*time.Hour)),
			minTime(t.queryMaxTime, now),
		})
		instants = append(instants, maxTime(adjustedQueryMinTime, now.Add(-24*time.Hour)))
	}

	// From last 23h to last 24h.
	if adjustedQueryMinTime.Before(now.Add(-23*time.Hour)) && t.queryMaxTime.After(now.Add(-23*time.Hour)) {
		ranges = append(ranges, [2]time.Time{
			maxTime(adjustedQueryMinTime, now.Add(-24*time.Hour)),
			minTime(t.queryMaxTime, now.Add(-23*time.Hour)),
		})
	}

	// A random time range.
	randMinTime := randTime(adjustedQueryMinTime, t.queryMaxTime)
	ranges = append(ranges, [2]time.Time{randMinTime, randTime(randMinTime, t.queryMaxTime)})
	instants = append(instants, randMinTime)

	return ranges, instants
}

func (t *WriteReadSeriesTest) runRangeQueryAndVerifyResult(ctx context.Context, start, end time.Time) {
	// We align start, end and step to write interval in order to avoid any false positives
	// when checking results correctness. The min/max query time is always aligned.
	start = maxTime(t.queryMinTime, alignTimestampToInterval(start, writeInterval))
	end = minTime(t.queryMaxTime, alignTimestampToInterval(end, writeInterval))
	if end.Before(start) {
		return
	}

	step := getQueryStep(start, end, writeInterval)
	query := fmt.Sprintf("sum(%s)", metricName)

	logger := log.With(t.logger, "query", query, "start", start.UnixMilli(), "end", end.UnixMilli(), "step", step)
	level.Debug(logger).Log("msg", "Running range query")

	t.metrics.queriesTotal.Inc()
	matrix, err := t.client.QueryRange(ctx, query, start, end, step)
	if err != nil {
		t.metrics.queriesFailedTotal.Inc()
		level.Warn(logger).Log("msg", "Failed to execute range query", "err", err)
		return
	}

	t.metrics.queryResultChecksTotal.Inc()
	err = verifySineWaveSamplesSum(matrix, t.cfg.NumSeries, step)
	if err != nil {
		t.metrics.queryResultChecksFailedTotal.Inc()
		level.Warn(logger).Log("msg", "Range query result check failed", "err", err)
		return
	}
}

func (t *WriteReadSeriesTest) runInstantQueryAndVerifyResult(ctx context.Context, ts time.Time) {
	// We align the query timestamp to write interval in order to avoid any false positives
	// when checking results correctness. The min/max query time is always aligned.
	ts = maxTime(t.queryMinTime, alignTimestampToInterval(ts, writeInterval))
	if t.queryMaxTime.Before(ts) {
		return
	}

	query := fmt.Sprintf("sum(%s)", metricName)

	logger := log.With(t.logger, "query", query, "ts", ts.UnixMilli())
	level.Debug(logger).Log("msg", "Running instant query")

	t.metrics.queriesTotal.Inc()
	vector, err := t.client.Query(ctx, query, ts)
	if err != nil {
		t.metrics.queriesFailedTotal.Inc()
		level.Warn(logger).Log("msg", "Failed to execute instant query", "err", err)
		return
	}

	// Convert the vector to matrix to reuse the same results comparison utility.
	matrix := make(model.Matrix, 0, len(vector))
	for _, entry := range vector {
		matrix = append(matrix, &model.SampleStream{
			Metric: entry.Metric,
			Values: []model.SamplePair{{
				Timestamp: entry.Timestamp,
				Value:     entry.Value,
			}},
		})
	}

	t.metrics.queryResultChecksTotal.Inc()
	err = verifySineWaveSamplesSum(matrix, t.cfg.NumSeries, 0)
	if err != nil {
		t.metrics.queryResultChecksFailedTotal.Inc()
		level.Warn(logger).Log("msg", "Instant query result check failed", "err", err)
		return
	}
}

func (t *WriteReadSeriesTest) nextWriteTimestamp(now time.Time) time.Time {
	if t.lastWrittenTimestamp.IsZero() {
		return alignTimestampToInterval(now, writeInterval)
	}

	return t.lastWrittenTimestamp.Add(writeInterval)
}
