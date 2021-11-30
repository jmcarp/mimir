// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/thanos-io/thanos/blob/2be2db77/pkg/compact/compact.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Thanos Authors.

package compactor

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/concurrency"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/compact/downsample"
	"github.com/thanos-io/thanos/pkg/errutil"
	"github.com/thanos-io/thanos/pkg/extprom"
	"github.com/thanos-io/thanos/pkg/objstore"
	"github.com/thanos-io/thanos/pkg/runutil"
	"go.uber.org/atomic"

	"github.com/grafana/mimir/pkg/storage/sharding"
	mimit_tsdb "github.com/grafana/mimir/pkg/storage/tsdb"
)

type ResolutionLevel int64

const (
	ResolutionLevelRaw = ResolutionLevel(downsample.ResLevel0)
	ResolutionLevel5m  = ResolutionLevel(downsample.ResLevel1)
	ResolutionLevel1h  = ResolutionLevel(downsample.ResLevel2)
)

type DeduplicateFilter interface {
	block.MetadataFilter

	// DuplicateIDs returns IDs of duplicate blocks generated by last call to Filter method.
	DuplicateIDs() []ulid.ULID
}

// Syncer synchronizes block metas from a bucket into a local directory.
// It sorts them into compaction groups based on equal label sets.
type Syncer struct {
	logger                   log.Logger
	bkt                      objstore.Bucket
	fetcher                  block.MetadataFetcher
	mtx                      sync.Mutex
	blocks                   map[ulid.ULID]*metadata.Meta
	partial                  map[ulid.ULID]error
	blockSyncConcurrency     int
	metrics                  *syncerMetrics
	deduplicateBlocksFilter  DeduplicateFilter
	ignoreDeletionMarkFilter *block.IgnoreDeletionMarkFilter
}

type syncerMetrics struct {
	garbageCollectedBlocks    prometheus.Counter
	garbageCollections        prometheus.Counter
	garbageCollectionFailures prometheus.Counter
	garbageCollectionDuration prometheus.Histogram
	blocksMarkedForDeletion   prometheus.Counter
}

func newSyncerMetrics(reg prometheus.Registerer, blocksMarkedForDeletion, garbageCollectedBlocks prometheus.Counter) *syncerMetrics {
	var m syncerMetrics

	m.garbageCollectedBlocks = garbageCollectedBlocks
	m.garbageCollections = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_compact_garbage_collection_total",
		Help: "Total number of garbage collection operations.",
	})
	m.garbageCollectionFailures = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_compact_garbage_collection_failures_total",
		Help: "Total number of failed garbage collection operations.",
	})
	m.garbageCollectionDuration = promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
		Name:    "thanos_compact_garbage_collection_duration_seconds",
		Help:    "Time it took to perform garbage collection iteration.",
		Buckets: []float64{0.01, 0.1, 0.3, 0.6, 1, 3, 6, 9, 20, 30, 60, 90, 120, 240, 360, 720},
	})

	m.blocksMarkedForDeletion = blocksMarkedForDeletion

	return &m
}

// NewMetaSyncer returns a new Syncer for the given Bucket and directory.
// Blocks must be at least as old as the sync delay for being considered.
func NewMetaSyncer(logger log.Logger, reg prometheus.Registerer, bkt objstore.Bucket, fetcher block.MetadataFetcher, deduplicateBlocksFilter DeduplicateFilter, ignoreDeletionMarkFilter *block.IgnoreDeletionMarkFilter, blocksMarkedForDeletion, garbageCollectedBlocks prometheus.Counter, blockSyncConcurrency int) (*Syncer, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	return &Syncer{
		logger:                   logger,
		bkt:                      bkt,
		fetcher:                  fetcher,
		blocks:                   map[ulid.ULID]*metadata.Meta{},
		metrics:                  newSyncerMetrics(reg, blocksMarkedForDeletion, garbageCollectedBlocks),
		deduplicateBlocksFilter:  deduplicateBlocksFilter,
		ignoreDeletionMarkFilter: ignoreDeletionMarkFilter,
		blockSyncConcurrency:     blockSyncConcurrency,
	}, nil
}

// SyncMetas synchronizes local state of block metas with what we have in the bucket.
func (s *Syncer) SyncMetas(ctx context.Context) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	metas, partial, err := s.fetcher.Fetch(ctx)
	if err != nil {
		return retry(err)
	}
	s.blocks = metas
	s.partial = partial
	return nil
}

// Partial returns partial blocks since last sync.
func (s *Syncer) Partial() map[ulid.ULID]error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	return s.partial
}

// Metas returns loaded metadata blocks since last sync.
func (s *Syncer) Metas() map[ulid.ULID]*metadata.Meta {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	return s.blocks
}

// GarbageCollect marks blocks for deletion from bucket if their data is available as part of a
// block with a higher compaction level.
// Call to SyncMetas function is required to populate duplicateIDs in duplicateBlocksFilter.
func (s *Syncer) GarbageCollect(ctx context.Context) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	begin := time.Now()

	// Ignore filter exists before deduplicate filter.
	deletionMarkMap := s.ignoreDeletionMarkFilter.DeletionMarkBlocks()
	duplicateIDs := s.deduplicateBlocksFilter.DuplicateIDs()

	// GarbageIDs contains the duplicateIDs, since these blocks can be replaced with other blocks.
	// We also remove ids present in deletionMarkMap since these blocks are already marked for deletion.
	garbageIDs := []ulid.ULID{}
	for _, id := range duplicateIDs {
		if _, exists := deletionMarkMap[id]; exists {
			continue
		}
		garbageIDs = append(garbageIDs, id)
	}

	for _, id := range garbageIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Spawn a new context so we always mark a block for deletion in full on shutdown.
		delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

		level.Info(s.logger).Log("msg", "marking outdated block for deletion", "block", id)
		err := block.MarkForDeletion(delCtx, s.logger, s.bkt, id, "outdated block", s.metrics.blocksMarkedForDeletion)
		cancel()
		if err != nil {
			s.metrics.garbageCollectionFailures.Inc()
			return retry(errors.Wrapf(err, "mark block %s for deletion", id))
		}

		// Immediately update our in-memory state so no further call to SyncMetas is needed
		// after running garbage collection.
		delete(s.blocks, id)
		s.metrics.garbageCollectedBlocks.Inc()
	}
	s.metrics.garbageCollections.Inc()
	s.metrics.garbageCollectionDuration.Observe(time.Since(begin).Seconds())
	return nil
}

// Grouper is responsible to group all known blocks into compaction Job which are safe to be
// compacted concurrently.
type Grouper interface {
	// Groups returns the compaction jobs for all blocks currently known to the syncer.
	// It creates all jobs from the scratch on every call.
	Groups(blocks map[ulid.ULID]*metadata.Meta) (res []*Job, err error)
}

// DefaultGroupKey returns a unique identifier for the group the block belongs to, based on
// the DefaultGrouper logic. It considers the downsampling resolution and the block's labels.
func DefaultGroupKey(meta metadata.Thanos) string {
	return defaultGroupKey(meta.Downsample.Resolution, labels.FromMap(meta.Labels))
}

func defaultGroupKey(res int64, lbls labels.Labels) string {
	return fmt.Sprintf("%d@%v", res, lbls.Hash())
}

// DefaultGrouper is the default grouper. It groups blocks based on downsample
// resolution and block's labels.
type DefaultGrouper struct {
	userID   string
	hashFunc metadata.HashFunc
}

// NewDefaultGrouper makes a new DefaultGrouper.
func NewDefaultGrouper(userID string, hashFunc metadata.HashFunc) *DefaultGrouper {
	return &DefaultGrouper{
		userID:   userID,
		hashFunc: hashFunc,
	}
}

// Groups implements Grouper.Groups.
func (g *DefaultGrouper) Groups(blocks map[ulid.ULID]*metadata.Meta) (res []*Job, err error) {
	groups := map[string]*Job{}
	for _, m := range blocks {
		groupKey := DefaultGroupKey(m.Thanos)
		job, ok := groups[groupKey]
		if !ok {
			lbls := labels.FromMap(m.Thanos.Labels)
			job = NewJob(
				g.userID,
				groupKey,
				lbls,
				m.Thanos.Downsample.Resolution,
				g.hashFunc,
				false, // No splitting.
				0,     // No splitting shards.
				"",    // No sharding.
			)
			groups[groupKey] = job
			res = append(res, job)
		}
		if err := job.AppendMeta(m); err != nil {
			return nil, errors.Wrap(err, "add compaction group")
		}
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].Key() < res[j].Key()
	})
	return res, nil
}

func minTime(metas []*metadata.Meta) time.Time {
	if len(metas) == 0 {
		return time.Time{}
	}

	minT := metas[0].MinTime
	for _, meta := range metas {
		if meta.MinTime < minT {
			minT = meta.MinTime
		}
	}

	return time.Unix(0, minT*int64(time.Millisecond)).UTC()
}

func maxTime(metas []*metadata.Meta) time.Time {
	if len(metas) == 0 {
		return time.Time{}
	}

	maxT := metas[0].MaxTime
	for _, meta := range metas {
		if meta.MaxTime > maxT {
			maxT = meta.MaxTime
		}
	}

	return time.Unix(0, maxT*int64(time.Millisecond)).UTC()
}

// Planner returns blocks to compact.
type Planner interface {
	// Plan returns a list of blocks that should be compacted into single one.
	// The blocks can be overlapping. The provided metadata has to be ordered by minTime.
	Plan(ctx context.Context, metasByMinTime []*metadata.Meta) ([]*metadata.Meta, error)
}

// Compactor provides compaction against an underlying storage of time series data.
// This is similar to tsdb.Compactor just without Plan method.
// TODO(bwplotka): Split the Planner from Compactor on upstream as well, so we can import it.
type Compactor interface {
	// Write persists a Block into a directory.
	// No Block is written when resulting Block has 0 samples, and returns empty ulid.ULID{}.
	Write(dest string, b tsdb.BlockReader, mint, maxt int64, parent *tsdb.BlockMeta) (ulid.ULID, error)

	// Compact runs compaction against the provided directories. Must
	// only be called concurrently with results of Plan().
	// Can optionally pass a list of already open blocks,
	// to avoid having to reopen them.
	// When resulting Block has 0 samples
	//  * No block is written.
	//  * The source dirs are marked Deletable.
	//  * Returns empty ulid.ULID{}.
	Compact(dest string, dirs []string, open []*tsdb.Block) (ulid.ULID, error)

	// CompactWithSplitting merges and splits the input blocks into shardCount number of output blocks,
	// and returns slice of block IDs. Position of returned block ID in the result slice corresponds to the shard index.
	// If given output block has no series, corresponding block ID will be zero ULID value.
	CompactWithSplitting(dest string, dirs []string, open []*tsdb.Block, shardCount uint64) (result []ulid.ULID, _ error)
}

// runCompactionJob plans and runs a single compaction against the provided job. The compacted result
// is uploaded into the bucket the blocks were retrieved from.
func (c *BucketCompactor) runCompactionJob(ctx context.Context, job *Job) (shouldRerun bool, compIDs []ulid.ULID, rerr error) {
	jobBeginTime := time.Now()

	jobLogger := log.With(c.logger, "groupKey", job.Key())
	subDir := filepath.Join(c.compactDir, job.Key())

	defer func() {
		elapsed := time.Since(jobBeginTime)
		level.Info(jobLogger).Log("msg", "compaction job finished", "success", rerr == nil, "duration", elapsed, "duration_ms", elapsed.Milliseconds())

		// Leave the compact directory for inspection if it is a halt error
		// or if it is not then so that possibly we would not have to download everything again.
		if rerr != nil {
			return
		}
		if err := os.RemoveAll(subDir); err != nil {
			level.Error(jobLogger).Log("msg", "failed to remove compaction group work directory", "path", subDir, "err", err)
		}
	}()

	if err := os.MkdirAll(subDir, 0750); err != nil {
		return false, nil, errors.Wrap(err, "create compaction job dir")
	}

	toCompact, err := c.planner.Plan(ctx, job.metasByMinTime)
	if err != nil {
		return false, nil, errors.Wrap(err, "plan compaction")
	}
	if len(toCompact) == 0 {
		// Nothing to do.
		return false, nil, nil
	}

	// The planner returned some blocks to compact, so we can enrich the logger
	// with the min/max time between all blocks to compact.
	jobLogger = log.With(jobLogger, "minTime", minTime(toCompact).String(), "maxTime", maxTime(toCompact).String())

	level.Info(jobLogger).Log("msg", "compaction available and planned; downloading blocks", "blocks", len(toCompact), "plan", fmt.Sprintf("%v", toCompact))

	// Once we have a plan we need to download the actual data.
	begin := time.Now()

	toCompactDirs := make([]string, len(toCompact))
	for ix := range toCompact {
		toCompactDirs[ix] = filepath.Join(subDir, toCompact[ix].ULID.String())
	}

	err = concurrency.ForEach(ctx, convertSliceOfMetasToSliceOfInterfaces(toCompact), len(toCompact), func(ctx context.Context, job interface{}) error {
		meta := job.(*metadata.Meta)

		// Must be same as in toCompactDirs.
		bdir := filepath.Join(subDir, meta.ULID.String())

		if err := block.Download(ctx, jobLogger, c.bkt, meta.ULID, bdir); err != nil {
			return retry(errors.Wrapf(err, "download block %s", meta.ULID))
		}

		// Ensure all input blocks are valid.
		stats, err := block.GatherIndexHealthStats(jobLogger, filepath.Join(bdir, block.IndexFilename), meta.MinTime, meta.MaxTime)
		if err != nil {
			return errors.Wrapf(err, "gather index issues for block %s", bdir)
		}

		if err := stats.CriticalErr(); err != nil {
			return halt(errors.Wrapf(err, "block with not healthy index found %s; Compaction level %v; Labels: %v", bdir, meta.Compaction.Level, meta.Thanos.Labels))
		}

		if err := stats.OutOfOrderChunksErr(); err != nil {
			return outOfOrderChunkError(errors.Wrapf(err, "blocks with out-of-order chunks are dropped from compaction:  %s", bdir), meta.ULID)
		}

		if err := stats.Issue347OutsideChunksErr(); err != nil {
			return issue347Error(errors.Wrapf(err, "invalid, but reparable block %s", bdir), meta.ULID)
		}

		if err := stats.PrometheusIssue5372Err(); err != nil {
			return errors.Wrapf(err, "block id %s", meta.ULID)
		}
		return nil
	})

	if err != nil {
		return false, nil, err
	}

	elapsed := time.Since(begin)
	level.Info(jobLogger).Log("msg", "downloaded and verified blocks; compacting blocks", "blocks", len(toCompact), "plan", fmt.Sprintf("%v", toCompactDirs), "duration", elapsed, "duration_ms", elapsed.Milliseconds())

	begin = time.Now()

	if job.UseSplitting() {
		compIDs, err = c.comp.CompactWithSplitting(subDir, toCompactDirs, nil, uint64(job.SplittingShards()))
	} else {
		var compID ulid.ULID
		compID, err = c.comp.Compact(subDir, toCompactDirs, nil)
		compIDs = append(compIDs, compID)
	}
	if err != nil {
		return false, nil, halt(errors.Wrapf(err, "compact blocks %v", toCompactDirs))
	}

	if !hasNonZeroULIDs(compIDs) {
		// Prometheus compactor found that the compacted block would have no samples.
		level.Info(jobLogger).Log("msg", "compacted block would have no samples, deleting source blocks", "blocks", fmt.Sprintf("%v", toCompactDirs))
		for _, meta := range toCompact {
			if meta.Stats.NumSamples == 0 {
				if err := deleteBlock(c.bkt, meta.ULID, filepath.Join(subDir, meta.ULID.String()), jobLogger, c.metrics.blocksMarkedForDeletion); err != nil {
					level.Warn(jobLogger).Log("msg", "failed to mark for deletion an empty block found during compaction", "block", meta.ULID, "err", err)
				}
			}
		}
		// Even though this block was empty, there may be more work to do.
		return true, nil, nil
	}

	elapsed = time.Since(begin)
	level.Info(jobLogger).Log("msg", "compacted blocks", "new", fmt.Sprintf("%v", compIDs), "blocks", fmt.Sprintf("%v", toCompactDirs), "duration", elapsed, "duration_ms", elapsed.Milliseconds())

	uploadBegin := time.Now()
	uploadedBlocks := atomic.NewInt64(0)

	err = concurrency.ForEach(ctx, convertSliceOfUlidsToSliceOfInterfaces(compIDs), len(compIDs), func(ctx context.Context, j interface{}) error {
		shardID := j.(ulidWithIndex).index
		compID := j.(ulidWithIndex).ulid

		// Skip if it's an empty block.
		if compID == (ulid.ULID{}) {
			if job.UseSplitting() {
				level.Info(jobLogger).Log("msg", "compaction produced an empty block", "shard_id", sharding.FormatShardIDLabelValue(uint64(shardID), uint64(job.SplittingShards())))
			} else {
				level.Info(jobLogger).Log("msg", "compaction produced an empty block")
			}
			return nil
		}

		uploadedBlocks.Inc()

		bdir := filepath.Join(subDir, compID.String())
		index := filepath.Join(bdir, block.IndexFilename)

		// When splitting is enabled, we need to inject the shard ID as external label.
		newLabels := job.Labels().Map()
		if job.UseSplitting() {
			newLabels[mimit_tsdb.CompactorShardIDExternalLabel] = sharding.FormatShardIDLabelValue(uint64(shardID), uint64(job.SplittingShards()))
		}

		newMeta, err := metadata.InjectThanos(jobLogger, bdir, metadata.Thanos{
			Labels:       newLabels,
			Downsample:   metadata.ThanosDownsample{Resolution: job.Resolution()},
			Source:       metadata.CompactorSource,
			SegmentFiles: block.GetSegmentFiles(bdir),
		}, nil)
		if err != nil {
			return errors.Wrapf(err, "failed to finalize the block %s", bdir)
		}

		if err = os.Remove(filepath.Join(bdir, "tombstones")); err != nil {
			return errors.Wrap(err, "remove tombstones")
		}

		// Ensure the output block is valid.
		if err := block.VerifyIndex(jobLogger, index, newMeta.MinTime, newMeta.MaxTime); err != nil {
			return halt(errors.Wrapf(err, "invalid result block %s", bdir))
		}

		begin = time.Now()

		if err := block.Upload(ctx, jobLogger, c.bkt, bdir, job.hashFunc); err != nil {
			return retry(errors.Wrapf(err, "upload of %s failed", compID))
		}

		elapsed = time.Since(begin)
		level.Info(jobLogger).Log("msg", "uploaded block", "result_block", compID, "duration", elapsed, "duration_ms", elapsed.Milliseconds(), "external_labels", labels.FromMap(newLabels))
		return nil
	})

	if err != nil {
		return false, nil, err
	}

	elapsed = time.Since(uploadBegin)
	level.Info(jobLogger).Log("msg", "uploaded all blocks", "blocks", uploadedBlocks, "duration", elapsed, "duration_ms", elapsed.Milliseconds())

	// Mark for deletion the blocks we just compacted from the job and bucket so they do not get included
	// into the next planning cycle.
	// Eventually the block we just uploaded should get synced into the job again (including sync-delay).
	for _, meta := range toCompact {
		if err := deleteBlock(c.bkt, meta.ULID, filepath.Join(subDir, meta.ULID.String()), jobLogger, c.metrics.blocksMarkedForDeletion); err != nil {
			return false, nil, retry(errors.Wrapf(err, "mark old block for deletion from bucket"))
		}
		c.metrics.garbageCollectedBlocks.Inc()
	}

	return true, compIDs, nil
}

func convertSliceOfMetasToSliceOfInterfaces(input []*metadata.Meta) []interface{} {
	result := make([]interface{}, len(input))
	for ix := range input {
		result[ix] = input[ix]
	}
	return result
}

func convertSliceOfUlidsToSliceOfInterfaces(input []ulid.ULID) []interface{} {
	result := make([]interface{}, len(input))
	for ix := range input {
		result[ix] = ulidWithIndex{index: ix, ulid: input[ix]}
	}
	return result
}

type ulidWithIndex struct {
	index int
	ulid  ulid.ULID
}

// Issue347Error is a type wrapper for errors that should invoke repair process for broken block.
type Issue347Error struct {
	err error

	id ulid.ULID
}

func issue347Error(err error, brokenBlock ulid.ULID) Issue347Error {
	return Issue347Error{err: err, id: brokenBlock}
}

func (e Issue347Error) Error() string {
	return e.err.Error()
}

// IsIssue347Error returns true if the base error is a Issue347Error.
func IsIssue347Error(err error) bool {
	_, ok := errors.Cause(err).(Issue347Error)
	return ok
}

// OutOfOrderChunkError is a type wrapper for OOO chunk error from validating block index.
type OutOfOrderChunksError struct {
	err error
	id  ulid.ULID
}

func (e OutOfOrderChunksError) Error() string {
	return e.err.Error()
}

func outOfOrderChunkError(err error, brokenBlock ulid.ULID) OutOfOrderChunksError {
	return OutOfOrderChunksError{err: err, id: brokenBlock}
}

// IsOutOfOrderChunk returns true if the base error is a OutOfOrderChunkError.
func IsOutOfOrderChunkError(err error) bool {
	_, ok := errors.Cause(err).(OutOfOrderChunksError)
	return ok
}

// HaltError is a type wrapper for errors that should halt any further progress on compactions.
type HaltError struct {
	err error
}

func halt(err error) HaltError {
	return HaltError{err: err}
}

func (e HaltError) Error() string {
	return e.err.Error()
}

// IsHaltError returns true if the base error is a HaltError.
// If a multierror is passed, any halt error will return true.
func IsHaltError(err error) bool {
	if multiErr, ok := errors.Cause(err).(errutil.NonNilMultiError); ok {
		for _, err := range multiErr {
			if _, ok := errors.Cause(err).(HaltError); ok {
				return true
			}
		}
		return false
	}

	_, ok := errors.Cause(err).(HaltError)
	return ok
}

// RetryError is a type wrapper for errors that should trigger warning log and retry whole compaction loop, but aborting
// current compaction further progress.
type RetryError struct {
	err error
}

func retry(err error) error {
	if IsHaltError(err) {
		return err
	}
	return RetryError{err: err}
}

func (e RetryError) Error() string {
	return e.err.Error()
}

// IsRetryError returns true if the base error is a RetryError.
// If a multierror is passed, all errors must be retriable.
func IsRetryError(err error) bool {
	if multiErr, ok := errors.Cause(err).(errutil.NonNilMultiError); ok {
		for _, err := range multiErr {
			if _, ok := errors.Cause(err).(RetryError); !ok {
				return false
			}
		}
		return true
	}

	_, ok := errors.Cause(err).(RetryError)
	return ok
}

// RepairIssue347 repairs the https://github.com/prometheus/tsdb/issues/347 issue when having issue347Error.
func RepairIssue347(ctx context.Context, logger log.Logger, bkt objstore.Bucket, blocksMarkedForDeletion prometheus.Counter, issue347Err error) error {
	ie, ok := errors.Cause(issue347Err).(Issue347Error)
	if !ok {
		return errors.Errorf("Given error is not an issue347 error: %v", issue347Err)
	}

	level.Info(logger).Log("msg", "Repairing block broken by https://github.com/prometheus/tsdb/issues/347", "id", ie.id, "err", issue347Err)

	tmpdir, err := ioutil.TempDir("", fmt.Sprintf("repair-issue-347-id-%s-", ie.id))
	if err != nil {
		return err
	}

	defer func() {
		if err := os.RemoveAll(tmpdir); err != nil {
			level.Warn(logger).Log("msg", "failed to remote tmpdir", "err", err, "tmpdir", tmpdir)
		}
	}()

	bdir := filepath.Join(tmpdir, ie.id.String())
	if err := block.Download(ctx, logger, bkt, ie.id, bdir); err != nil {
		return retry(errors.Wrapf(err, "download block %s", ie.id))
	}

	meta, err := metadata.ReadFromDir(bdir)
	if err != nil {
		return errors.Wrapf(err, "read meta from %s", bdir)
	}

	resid, err := block.Repair(logger, tmpdir, ie.id, metadata.CompactorRepairSource, block.IgnoreIssue347OutsideChunk)
	if err != nil {
		return errors.Wrapf(err, "repair failed for block %s", ie.id)
	}

	// Verify repaired id before uploading it.
	if err := block.VerifyIndex(logger, filepath.Join(tmpdir, resid.String(), block.IndexFilename), meta.MinTime, meta.MaxTime); err != nil {
		return errors.Wrapf(err, "repaired block is invalid %s", resid)
	}

	level.Info(logger).Log("msg", "uploading repaired block", "newID", resid)
	if err = block.Upload(ctx, logger, bkt, filepath.Join(tmpdir, resid.String()), metadata.NoneFunc); err != nil {
		return retry(errors.Wrapf(err, "upload of %s failed", resid))
	}

	level.Info(logger).Log("msg", "deleting broken block", "id", ie.id)

	// Spawn a new context so we always mark a block for deletion in full on shutdown.
	delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// TODO(bplotka): Issue with this will introduce overlap that will halt compactor. Automate that (fix duplicate overlaps caused by this).
	if err := block.MarkForDeletion(delCtx, logger, bkt, ie.id, "source of repaired block", blocksMarkedForDeletion); err != nil {
		return errors.Wrapf(err, "marking old block %s for deletion has failed", ie.id)
	}
	return nil
}

func deleteBlock(bkt objstore.Bucket, id ulid.ULID, bdir string, logger log.Logger, blocksMarkedForDeletion prometheus.Counter) error {
	if err := os.RemoveAll(bdir); err != nil {
		return errors.Wrapf(err, "remove old block dir %s", id)
	}

	// Spawn a new context so we always mark a block for deletion in full on shutdown.
	delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	level.Info(logger).Log("msg", "marking compacted block for deletion", "old_block", id)
	if err := block.MarkForDeletion(delCtx, logger, bkt, id, "source of compacted block", blocksMarkedForDeletion); err != nil {
		return errors.Wrapf(err, "mark block %s for deletion from bucket", id)
	}
	return nil
}

// BucketCompactorMetrics holds the metrics tracked by BucketCompactor.
type BucketCompactorMetrics struct {
	groupCompactionRunsStarted   prometheus.Counter
	groupCompactionRunsCompleted prometheus.Counter
	groupCompactionRunsFailed    prometheus.Counter
	groupCompactions             prometheus.Counter
	garbageCollectedBlocks       prometheus.Counter
	blocksMarkedForDeletion      prometheus.Counter
	blocksMarkedForNoCompact     prometheus.Counter
}

// NewBucketCompactorMetrics makes a new BucketCompactorMetrics.
func NewBucketCompactorMetrics(blocksMarkedForDeletion, garbageCollectedBlocks prometheus.Counter, reg prometheus.Registerer) *BucketCompactorMetrics {
	return &BucketCompactorMetrics{
		groupCompactionRunsStarted: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_compactor_group_compaction_runs_started_total",
			Help: "Total number of group compaction attempts.",
		}),
		groupCompactionRunsCompleted: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_compactor_group_compaction_runs_completed_total",
			Help: "Total number of group completed compaction runs. This also includes compactor group runs that resulted with no compaction.",
		}),
		groupCompactionRunsFailed: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_compactor_group_compactions_failures_total",
			Help: "Total number of failed group compactions.",
		}),
		groupCompactions: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_compactor_group_compactions_total",
			Help: "Total number of group compaction attempts that resulted in new block(s).",
		}),
		blocksMarkedForDeletion: blocksMarkedForDeletion,
		blocksMarkedForNoCompact: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name:        "cortex_compactor_blocks_marked_for_no_compaction_total",
			Help:        "Total number of blocks that were marked for no-compaction.",
			ConstLabels: prometheus.Labels{"reason": metadata.OutOfOrderChunksNoCompactReason},
		}),
		garbageCollectedBlocks: garbageCollectedBlocks,
	}
}

type ownCompactionJobFunc func(job *Job) (bool, error)

// ownAllJobs is a ownCompactionJobFunc that always return true.
var ownAllJobs = func(job *Job) (bool, error) {
	return true, nil
}

// BucketCompactor compacts blocks in a bucket.
type BucketCompactor struct {
	logger                         log.Logger
	sy                             *Syncer
	grouper                        Grouper
	comp                           Compactor
	planner                        Planner
	compactDir                     string
	bkt                            objstore.Bucket
	concurrency                    int
	skipBlocksWithOutOfOrderChunks bool
	ownJob                         ownCompactionJobFunc
	sortJobs                       jobsOrderFunc
	metrics                        *BucketCompactorMetrics
}

// NewBucketCompactor creates a new bucket compactor.
func NewBucketCompactor(
	logger log.Logger,
	sy *Syncer,
	grouper Grouper,
	planner Planner,
	comp Compactor,
	compactDir string,
	bkt objstore.Bucket,
	concurrency int,
	skipBlocksWithOutOfOrderChunks bool,
	ownJob ownCompactionJobFunc,
	sortJobs jobsOrderFunc,
	metrics *BucketCompactorMetrics,
) (*BucketCompactor, error) {
	if concurrency <= 0 {
		return nil, errors.Errorf("invalid concurrency level (%d), concurrency level must be > 0", concurrency)
	}
	return &BucketCompactor{
		logger:                         logger,
		sy:                             sy,
		grouper:                        grouper,
		planner:                        planner,
		comp:                           comp,
		compactDir:                     compactDir,
		bkt:                            bkt,
		concurrency:                    concurrency,
		skipBlocksWithOutOfOrderChunks: skipBlocksWithOutOfOrderChunks,
		ownJob:                         ownJob,
		sortJobs:                       sortJobs,
		metrics:                        metrics,
	}, nil
}

// Compact runs compaction over bucket.
// If maxCompactionTime is positive then after this time no more new compactions are started.
func (c *BucketCompactor) Compact(ctx context.Context, maxCompactionTime time.Duration) (rerr error) {
	defer func() {
		// Do not remove the compactDir if an error has occurred
		// because potentially on the next run we would not have to download
		// everything again.
		if rerr != nil {
			return
		}
		if err := os.RemoveAll(c.compactDir); err != nil {
			level.Error(c.logger).Log("msg", "failed to remove compaction work directory", "path", c.compactDir, "err", err)
		}
	}()

	var maxCompactionTimeChan <-chan time.Time
	if maxCompactionTime > 0 {
		maxCompactionTimeChan = time.After(maxCompactionTime)
	}

	// Loop over bucket and compact until there's no work left.
	for {
		var (
			wg                     sync.WaitGroup
			workCtx, workCtxCancel = context.WithCancel(ctx)
			jobChan                = make(chan *Job)
			errChan                = make(chan error, c.concurrency)
			finishedAllJobs        = true
			mtx                    sync.Mutex
		)
		defer workCtxCancel()

		// Set up workers who will compact the jobs when the jobs are ready.
		// They will compact available jobs until they encounter an error, after which they will stop.
		for i := 0; i < c.concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for g := range jobChan {
					// Ensure the job is still owned by the current compactor instance.
					// If not, we shouldn't run it because another compactor instance may already
					// process it (or will do it soon).
					if ok, err := c.ownJob(g); err != nil {
						level.Info(c.logger).Log("msg", "skipped compaction because unable to check whether the job is owned by the compactor instance", "groupKey", g.Key(), "err", err)
						continue
					} else if !ok {
						level.Info(c.logger).Log("msg", "skipped compaction because job is not owned by the compactor instance anymore", "groupKey", g.Key())
						continue
					}

					c.metrics.groupCompactionRunsStarted.Inc()

					shouldRerunJob, compactedBlockIDs, err := c.runCompactionJob(workCtx, g)
					if err == nil {
						c.metrics.groupCompactionRunsCompleted.Inc()
						if hasNonZeroULIDs(compactedBlockIDs) {
							c.metrics.groupCompactions.Inc()
						}

						if shouldRerunJob {
							mtx.Lock()
							finishedAllJobs = false
							mtx.Unlock()
						}
						continue
					}

					// At this point the compaction has failed.
					c.metrics.groupCompactionRunsFailed.Inc()

					if IsIssue347Error(err) {
						if err := RepairIssue347(workCtx, c.logger, c.bkt, c.sy.metrics.blocksMarkedForDeletion, err); err == nil {
							mtx.Lock()
							finishedAllJobs = false
							mtx.Unlock()
							continue
						}
					}
					// If block has out of order chunk and it has been configured to skip it,
					// then we can mark the block for no compaction so that the next compaction run
					// will skip it.
					if IsOutOfOrderChunkError(err) && c.skipBlocksWithOutOfOrderChunks {
						if err := block.MarkForNoCompact(
							ctx,
							c.logger,
							c.bkt,
							err.(OutOfOrderChunksError).id,
							metadata.OutOfOrderChunksNoCompactReason,
							"OutofOrderChunk: marking block with out-of-order series/chunks to as no compact to unblock compaction", c.metrics.blocksMarkedForNoCompact); err == nil {
							mtx.Lock()
							finishedAllJobs = false
							mtx.Unlock()
							continue
						}
					}
					errChan <- errors.Wrapf(err, "group %s", g.Key())
					return
				}
			}()
		}

		level.Info(c.logger).Log("msg", "start sync of metas")
		if err := c.sy.SyncMetas(ctx); err != nil {
			return errors.Wrap(err, "sync")
		}

		level.Info(c.logger).Log("msg", "start of GC")
		// Blocks that were compacted are garbage collected after each Compaction.
		// However if compactor crashes we need to resolve those on startup.
		if err := c.sy.GarbageCollect(ctx); err != nil {
			return errors.Wrap(err, "garbage")
		}

		jobs, err := c.grouper.Groups(c.sy.Metas())
		if err != nil {
			return errors.Wrap(err, "build compaction jobs")
		}

		// There is another check just before we start processing the job, but we can avoid sending it
		// to the goroutine in the first place.
		jobs, err = c.filterOwnJobs(jobs)
		if err != nil {
			return err
		}

		// Sort jobs based on the configured ordering algorithm.
		jobs = c.sortJobs(jobs)

		ignoreDirs := []string{}
		for _, gr := range jobs {
			for _, grID := range gr.IDs() {
				ignoreDirs = append(ignoreDirs, filepath.Join(gr.Key(), grID.String()))
			}
		}

		if err := runutil.DeleteAll(c.compactDir, ignoreDirs...); err != nil {
			level.Warn(c.logger).Log("msg", "failed deleting non-compaction job directories/files, some disk space usage might have leaked. Continuing", "err", err, "dir", c.compactDir)
		}

		level.Info(c.logger).Log("msg", "start of compactions")

		maxCompactionTimeReached := false
		// Send all jobs found during this pass to the compaction workers.
		var jobErrs errutil.MultiError
	jobLoop:
		for _, g := range jobs {
			select {
			case jobErr := <-errChan:
				jobErrs.Add(jobErr)
				break jobLoop
			case jobChan <- g:
			case <-maxCompactionTimeChan:
				maxCompactionTimeReached = true
				level.Info(c.logger).Log("msg", "max compaction time reached, no more compactions will be started")
				break jobLoop
			}
		}
		close(jobChan)
		wg.Wait()

		// Collect any other error reported by the workers, or any error reported
		// while we were waiting for the last batch of jobs to run the compaction.
		close(errChan)
		for jobErr := range errChan {
			jobErrs.Add(jobErr)
		}

		workCtxCancel()
		if len(jobErrs) > 0 {
			return jobErrs.Err()
		}

		if maxCompactionTimeReached || finishedAllJobs {
			break
		}
	}
	level.Info(c.logger).Log("msg", "compaction iterations done")
	return nil
}

func (c *BucketCompactor) filterOwnJobs(jobs []*Job) ([]*Job, error) {
	for ix := 0; ix < len(jobs); {
		// Skip any job which doesn't belong to this compactor instance.
		if ok, err := c.ownJob(jobs[ix]); err != nil {
			return nil, errors.Wrap(err, "ownJob")
		} else if !ok {
			jobs = append(jobs[:ix], jobs[ix+1:]...)
		} else {
			ix++
		}
	}
	return jobs, nil
}

var _ block.MetadataFilter = &NoCompactionMarkFilter{}

// NoCompactionMarkFilter is a block.Fetcher filter that finds all blocks with no-compact marker files, and optionally
// removes them from synced metas.
type NoCompactionMarkFilter struct {
	logger                log.Logger
	bkt                   objstore.InstrumentedBucketReader
	noCompactMarkedMap    map[ulid.ULID]*metadata.NoCompactMark
	concurrency           int
	removeNoCompactBlocks bool
}

// NewNoCompactionMarkFilter creates NoCompactionMarkFilter.
func NewNoCompactionMarkFilter(logger log.Logger, bkt objstore.InstrumentedBucketReader, concurrency int, removeNoCompactBlocks bool) *NoCompactionMarkFilter {
	return &NoCompactionMarkFilter{
		logger:                logger,
		bkt:                   bkt,
		concurrency:           concurrency,
		removeNoCompactBlocks: removeNoCompactBlocks,
	}
}

// NoCompactMarkedBlocks returns block ids that were marked for no compaction.
func (f *NoCompactionMarkFilter) NoCompactMarkedBlocks() map[ulid.ULID]*metadata.NoCompactMark {
	return f.noCompactMarkedMap
}

// Filter finds blocks that should not be compacted, and fills f.noCompactMarkedMap. If f.removeNoCompactBlocks is true,
// blocks are also removed from metas. (Thanos version of the filter doesn't do removal).
func (f *NoCompactionMarkFilter) Filter(ctx context.Context, metas map[ulid.ULID]*metadata.Meta, synced *extprom.TxGaugeVec) error {
	f.noCompactMarkedMap = make(map[ulid.ULID]*metadata.NoCompactMark)

	ids := make([]interface{}, 0, len(metas))
	for id := range metas {
		ids = append(ids, id)
	}

	var mtx sync.Mutex // for accessing f.noCompactMarkedMap and metas from goroutines.
	err := concurrency.ForEach(ctx, ids, f.concurrency, func(ctx context.Context, job interface{}) error {
		id := job.(ulid.ULID)
		m := &metadata.NoCompactMark{}
		// TODO(bwplotka): Hook up bucket cache here + reset API so we don't introduce API calls .
		if err := metadata.ReadMarker(ctx, f.logger, f.bkt, id.String(), m); err != nil {
			if errors.Cause(err) == metadata.ErrorMarkerNotFound {
				return nil
			}

			if errors.Cause(err) == metadata.ErrorUnmarshalMarker {
				level.Warn(f.logger).Log("msg", "found partial no-compact-mark.json; if we will see it happening often for the same block, consider manually deleting no-compact-mark.json from the object storage", "block", id, "err", err)
				return nil
			}

			return err
		}

		mtx.Lock()
		f.noCompactMarkedMap[id] = m
		if f.removeNoCompactBlocks {
			delete(metas, id)
		}
		mtx.Unlock()

		synced.WithLabelValues(block.MarkedForNoCompactionMeta).Inc()
		return nil
	})

	return errors.Wrap(err, "filter blocks marked for no compaction")
}

func hasNonZeroULIDs(ids []ulid.ULID) bool {
	for _, id := range ids {
		if id != (ulid.ULID{}) {
			return true
		}
	}

	return false
}
