// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package logical

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdcevent"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/changefeedbase"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/repstream/streampb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/lease"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/row"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/stats"
	"github.com/cockroachdb/cockroach/pkg/util/admission/admissionpb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
)

// A kvRowProcessor is a RowProcessor that bypasses SQL execution and directly
// writes KV pairs for replicated events.
type kvRowProcessor struct {
	decoder cdcevent.Decoder
	lastRow cdcevent.Row
	cfg     *execinfra.ServerConfig
	spec    execinfrapb.LogicalReplicationWriterSpec
	evalCtx *eval.Context

	dstBySrc map[descpb.ID]descpb.ID
	writers  map[descpb.ID]*kvTableWriter

	failureInjector
}

var _ BatchHandler = &kvRowProcessor{}

// newKVRowProcessor returns a RowProcessor that operates by writing KV pairs
// directly to the db, bypassing SQL Query execution.
func newKVRowProcessor(
	ctx context.Context,
	cfg *execinfra.ServerConfig,
	evalCtx *eval.Context,
	spec execinfrapb.LogicalReplicationWriterSpec,
	procConfigByDestID map[descpb.ID]sqlProcessorTableConfig,
) (*kvRowProcessor, error) {
	dstBySrc := make(map[descpb.ID]descpb.ID, len(procConfigByDestID))

	for dstID, s := range procConfigByDestID {
		dstBySrc[s.srcDesc.GetID()] = dstID
	}

	decoder, err := newCdcEventDecoder(ctx, procConfigByDestID, evalCtx.Settings)
	if err != nil {
		return nil, err
	}

	p := &kvRowProcessor{
		cfg:      cfg,
		spec:     spec,
		evalCtx:  evalCtx,
		dstBySrc: dstBySrc,
		writers:  make(map[descpb.ID]*kvTableWriter, len(procConfigByDestID)),
		decoder:  decoder,
	}
	return p, nil
}

func newCdcEventDecoder(
	ctx context.Context,
	procConfigByDestID map[descpb.ID]sqlProcessorTableConfig,
	settings *cluster.Settings,
) (cdcevent.Decoder, error) {
	cdcEventTargets := changefeedbase.Targets{}
	srcTablesBySrcID := make(map[descpb.ID]catalog.TableDescriptor, len(procConfigByDestID))

	for _, s := range procConfigByDestID {
		srcTablesBySrcID[s.srcDesc.GetID()] = s.srcDesc
		cdcEventTargets.Add(changefeedbase.Target{
			Type:              jobspb.ChangefeedTargetSpecification_EACH_FAMILY,
			DescID:            s.srcDesc.GetID(),
			StatementTimeName: changefeedbase.StatementTimeName(s.srcDesc.GetName()),
		})
	}

	prefixlessCodec := keys.SystemSQLCodec
	rfCache, err := cdcevent.NewFixedRowFetcherCache(
		ctx, prefixlessCodec, settings, cdcEventTargets, srcTablesBySrcID,
	)
	if err != nil {
		return nil, err
	}

	return cdcevent.NewEventDecoderWithCache(ctx, rfCache, false, false), nil
}

var originID1Options = &kvpb.WriteOptions{OriginID: 1}

func (p *kvRowProcessor) HandleBatch(
	ctx context.Context, batch []streampb.StreamEvent_KV,
) (batchStats, error) {
	ctx, sp := tracing.ChildSpan(ctx, "kvRowProcessor.HandleBatch")
	defer sp.Finish()

	if len(batch) == 1 {
		stats := batchStats{}
		if p.spec.Discard == jobspb.LogicalReplicationDetails_DiscardAllDeletes && len(batch[0].KeyValue.Value.RawBytes) == 0 {
			return stats, nil
		}
		s, err := p.processRow(ctx, nil, nil, batch[0].KeyValue, batch[0].PrevValue)
		if err != nil {
			return stats, err
		}
		stats.Add(s)
		return stats, err
	}
	// TODO(ssd,dt): There are two levels of batching we may care about: putting multiple
	// batches (each generated by 1 row) into a single transaction or putting multiple rows into
	// a single batch.
	//
	// In either case, the problem with batching is when the error indicates we need to refresh
	// our expected value -- We need some way to associate the refreshed value contained in the
	// error with the row that generated the error.
	//
	// We could forgo that completely and use the existing batch-splitting logic in the
	// logicalReplicationWriterProcessor: Any failure results in the batch being retried one row
	// at a time. If that original failure was an error requiring a value refresh, the
	// single-row batch it will fail again with the same error, but can be individually retried
	// by the code above.
	//
	// But, even then, since a LWW failure often means we are now processing duplicates, we may
	// want batch handling with a bit of hysteresis that prevents constantly building
	// multi-batch transactions that are likely to fail.
	return batchStats{}, errors.AssertionFailedf("TODO: multi-row transactions not supported by the kvRowProcessor")
}

func (p *kvRowProcessor) BatchSize() int {
	return 1
}

func (p *kvRowProcessor) processRow(
	ctx context.Context,
	txn isql.Txn,
	b *kv.Batch,
	keyValue roachpb.KeyValue,
	prevValue roachpb.Value,
) (batchStats, error) {
	var err error
	keyValue.Key, err = keys.StripTenantPrefix(keyValue.Key)
	if err != nil {
		return batchStats{}, errors.Wrap(err, "stripping tenant prefix")
	}

	row, err := p.decoder.DecodeKV(ctx, keyValue, cdcevent.CurrentRow, keyValue.Value.Timestamp, false)
	if err != nil {
		p.lastRow = cdcevent.Row{}
		return batchStats{}, errors.Wrap(err, "decoding KeyValue")
	}
	dstTableID, ok := p.dstBySrc[row.TableID]
	if !ok {
		return batchStats{}, errors.AssertionFailedf("replication configuration missing for table %d / %q", row.TableID, row.TableName)
	}

	p.lastRow = row

	if err = p.injectFailure(); err != nil {
		return batchStats{}, err
	}

	if txn == nil {
		var s batchStats
		if err := p.processOneRow(ctx, dstTableID, row, keyValue, prevValue, &s, 0); err != nil {
			return batchStats{}, err
		}
		return s, nil
	}

	return batchStats{}, p.addToBatch(ctx, txn.KV(), b, dstTableID, row, keyValue, prevValue)
}

func (p *kvRowProcessor) ReportMutations(refresher *stats.Refresher) {
	for _, w := range p.writers {
		if w.unreportedMutations > 0 && w.leased != nil {
			if desc := w.leased.Underlying(); desc != nil {
				refresher.NotifyMutation(desc.(catalog.TableDescriptor), w.unreportedMutations)
				w.unreportedMutations = 0
			}
		}
	}
}

// ReleaseLeases releases all held leases.
func (p *kvRowProcessor) ReleaseLeases(ctx context.Context) {
	for _, w := range p.writers {
		if w.leased != nil {
			w.leased.Release(ctx)
			w.leased = nil
		}
	}
}

// maxRefreshCount is the maximum number of times we will retry a KV batch that has failed with a
// ConditionFailedError with HadNewerOriginTimetamp=true.
const maxRefreshCount = 10

func makeKVBatch(lowPri bool, txn *kv.Txn) *kv.Batch {
	b := txn.NewBatch()
	b.Header.WriteOptions = originID1Options
	if lowPri {
		b.AdmissionHeader.Priority = int32(admissionpb.BulkLowPri)
	} else {
		b.AdmissionHeader.Priority = int32(admissionpb.NormalPri)
	}
	b.AdmissionHeader.Source = kvpb.AdmissionHeader_FROM_SQL
	return b
}

func (p *kvRowProcessor) processOneRow(
	ctx context.Context,
	dstTableID descpb.ID,
	row cdcevent.Row,
	k roachpb.KeyValue,
	prevValue roachpb.Value,
	s *batchStats,
	refreshCount int,
) error {
	if err := p.cfg.DB.KV().Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		txn.SetBufferedWritesEnabled(false)
		b := makeKVBatch(useLowPriority.Get(&p.cfg.Settings.SV), txn)

		if err := p.addToBatch(ctx, txn, b, dstTableID, row, k, prevValue); err != nil {
			return err
		}
		return txn.CommitInBatch(ctx, b)
	}); err != nil {
		if condErr := (*kvpb.ConditionFailedError)(nil); errors.As(err, &condErr) {
			// If OriginTimestampOlderThan is set, then this was the LWW
			// loser. We ignore the error and move onto the next row row we have
			// to process.
			if condErr.OriginTimestampOlderThan.IsSet() {
				s.kvWriteTooOld++
				return nil
			}
			// If HadNewerOriginTimestamp is true, it implies that the row we
			// are processing was the LWW winner but the previous value from the
			// rangefeed event doesn't represent what was on disk. In this case,
			// we use the ActualValue returned in the error and retry the
			// request. We don't do this via the retry queue because we want to
			// do it without delay since we are likely to succeed on the first
			// retry unless the key has a high rate of cross-cluster writes.
			if condErr.HadNewerOriginTimestamp {
				// We limit the number of times we hit this in a row, but we
				// don't expect to hit this many times in a row. Any
				// intervening write that would invalidate our refreshed
				// expected value is likely to have been written at a higher
				// MVCC timestamp and thus we would get a LWW failure rather
				// than another refresh attempt.
				//
				// However, this does protect us against the job just
				// running forever in the case of some bug in the above
				// reasoning or our ConditionalPut code.
				if refreshCount > maxRefreshCount {
					return errors.Wrapf(err, "max refresh count (%d) reached", maxRefreshCount)
				}
				s.kvWriteValueRefreshes++
				var refreshedValue roachpb.Value
				if condErr.ActualValue != nil {
					refreshedValue = *condErr.ActualValue
				}
				return p.processOneRow(ctx, dstTableID, row, k, refreshedValue, s, refreshCount+1)
			}

			// Since the conditional error is not a timestamp error, that means it
			// must be a uniqueness constraint validation. In the future, if the
			// conditional failure indicates which key failed, we should ensure the
			// key belongs to a unique index.
			//
			// Overall, converting this to a unique violation is low risk. A
			// condition validation failure is persistent condition and sending the
			// row to the DLQ allows the job to continue processing.
			//
			// TODO(jeffswenson): ideally this would share an implementaiton with
			// mkFastPathUniqueCheckErr.
			return pgerror.Newf(pgcode.UniqueViolation, "duplicate key value violates unique constraint: %s", condErr.String())
		}
		return err
	}
	return nil
}

func (p *kvRowProcessor) addToBatch(
	ctx context.Context,
	txn *kv.Txn,
	b *kv.Batch,
	dstTableID descpb.ID,
	row cdcevent.Row,
	keyValue roachpb.KeyValue,
	prevValue roachpb.Value,
) error {
	w, err := p.getWriter(ctx, dstTableID, txn.ProvisionalCommitTimestamp())
	if err != nil {
		return err
	}
	// This batch should only commit if it can do so prior to the expiration of
	// the lease of the descriptor used to encode it.
	if err := txn.UpdateDeadline(ctx, w.leased.Expiration(ctx)); err != nil {
		return err
	}

	prevRow, err := p.decoder.DecodeKV(ctx, roachpb.KeyValue{
		Key:   keyValue.Key,
		Value: prevValue,
	}, cdcevent.PrevRow, prevValue.Timestamp, false)
	if err != nil {
		return err
	}

	if row.IsDeleted() {
		if err := w.deleteRow(ctx, b, prevRow, row); err != nil {
			return err
		}
	} else {
		if prevValue.IsPresent() {
			if err := w.updateRow(ctx, b, prevRow, row); err != nil {
				return err
			}
		} else {
			if err := w.insertRow(ctx, b, row); err != nil {
				return err
			}
		}
	}

	// Note that we should report this mutation to sql stats refresh later.
	w.unreportedMutations++

	return nil
}

// GetLastRow implements the RowProcessor interface.
func (p *kvRowProcessor) GetLastRow() cdcevent.Row {
	return p.lastRow
}

// SetSyntheticFailurePercent implements the RowProcessor interface.
func (p *kvRowProcessor) SetSyntheticFailurePercent(rate uint32) {
	p.rate = rate
}

func (p *kvRowProcessor) Close(ctx context.Context) {
	p.ReleaseLeases(ctx)
}

func (p *kvRowProcessor) getWriter(
	ctx context.Context, id descpb.ID, ts hlc.Timestamp,
) (*kvTableWriter, error) {
	w, ok := p.writers[id]
	if ok {
		if w.leased != nil {
			// If the lease is still valid, just use the writer.
			if w.leased.Expiration(ctx).After(ts) {
				return w, nil
			}
			// The lease is invalid; we'll be getting a new one so release this one.
			w.leased.Release(ctx)
			w.leased = nil
		}
	}

	l, err := p.cfg.LeaseManager.(*lease.Manager).Acquire(ctx, ts, id)
	if err != nil {
		return nil, err
	}

	// If the new lease just so happened to be the same version, we can just swap
	// the lease in the existing writer.
	if ok && l.Underlying().GetVersion() == w.v {
		w.leased = l
		return w, nil
	}

	// New lease and desc version; make a new writer.
	w, err = newKVTableWriter(ctx, l, p.evalCtx)
	if err != nil {
		return nil, err
	}

	p.writers[id] = w
	return w, nil
}

// kvTableWriter writes row changes for a specific table using a leased desc for
// that table to a passed batch. An instance of a kvTableWriter should not be
// used past the expiration of the lease used to construct it, and batches that
// it populates should commit no later than the expiration of said lease.
type kvTableWriter struct {
	leased           lease.LeasedDescriptor
	v                descpb.DescriptorVersion
	newVals, oldVals []tree.Datum
	ru               row.Updater
	ri               row.Inserter
	rd               row.Deleter

	// Mutations to the table this writer wraps that should be reported to sql
	// stats at some point.
	unreportedMutations int
}

func newKVTableWriter(
	ctx context.Context, leased lease.LeasedDescriptor, evalCtx *eval.Context,
) (*kvTableWriter, error) {

	tableDesc := leased.Underlying().(catalog.TableDescriptor)

	// TODO(dt): figure out the right sets of columns here and in fillNew/fillOld.
	writeCols, err := writeableColunms(ctx, tableDesc)
	if err != nil {
		return nil, err
	}
	readCols := writeCols

	// TODO(dt): pass these some sort fo flag to have them use versions of CPut
	// or a new LWW KV API. For now they're not detecting/handling conflicts.
	ri, err := row.MakeInserter(evalCtx.Codec, tableDesc, nil /* uniqueWithTombstoneIndexes */, writeCols, evalCtx.SessionData(), &evalCtx.Settings.SV, nil /* metrics */)
	if err != nil {
		return nil, err
	}
	rd := row.MakeDeleter(evalCtx.Codec, tableDesc, nil /* lockedIndexes */, readCols, evalCtx.SessionData(), &evalCtx.Settings.SV, nil /* metrics */)
	ru, err := row.MakeUpdater(
		evalCtx.Codec, tableDesc, nil /* uniqueWithTombstoneIndexes */, nil /* lockedIndexes */, readCols, writeCols, row.UpdaterDefault, evalCtx.SessionData(), &evalCtx.Settings.SV, nil, /* metrics */
	)
	if err != nil {
		return nil, err
	}

	return &kvTableWriter{
		leased:  leased,
		v:       leased.Underlying().GetVersion(),
		oldVals: make([]tree.Datum, len(readCols)),
		newVals: make([]tree.Datum, len(writeCols)),
		ri:      ri,
		rd:      rd,
		ru:      ru,
	}, nil
}

// writeableColumns are 'writable' in the sense that they are stored on disk in the primary index.
//
// We assume this is all public non-virtual columns and all virtual columns stored in the primary
// indexes value or key.
//
// NOTE: We don't handle virtual columns that are stored in secondary index keys or values here
// because we assume tables with such columns were disallowed earlier. This function will need to be
// updated if that restriction is relaxed.
func writeableColunms(ctx context.Context, td catalog.TableDescriptor) ([]catalog.Column, error) {
	// TODO(ssd): Validate with SQL foundations that this is correct.
	keyColumnIDs := td.GetPrimaryIndex().CollectKeyColumnIDs()

	pubCols := td.PublicColumns()
	ret := make([]catalog.Column, 0, len(pubCols))
	for _, c := range pubCols {
		if c.IsComputed() && c.IsVirtual() && !keyColumnIDs.Contains(c.GetID()) {
			continue
		}
		ret = append(ret, c)
	}
	return ret, nil
}

func (p *kvTableWriter) insertRow(ctx context.Context, b *kv.Batch, after cdcevent.Row) error {
	if err := p.fillNew(after); err != nil {
		return err
	}

	var ph row.PartialIndexUpdateHelper
	// TODO(dt): support partial indexes.
	var vh row.VectorIndexUpdateHelper
	// TODO(mw5h, drewk): support vector indexes.
	oth := row.OriginTimestampCPutHelper{
		OriginTimestamp: after.MvccTimestamp,
		// TODO(ssd): We should choose this based by comparing the cluster IDs of the source
		// and destination clusters.
		// ShouldWinTie: true,
	}
	return p.ri.InsertRow(ctx, &row.KVBatchAdapter{Batch: b}, p.newVals, ph, vh, oth, row.CPutOp, false /* traceKV */)
}

func (p *kvTableWriter) updateRow(
	ctx context.Context, b *kv.Batch, before, after cdcevent.Row,
) error {
	if err := p.fillOld(before); err != nil {
		return err
	}
	if err := p.fillNew(after); err != nil {
		return err
	}

	var ph row.PartialIndexUpdateHelper
	// TODO(dt): support partial indexes.
	var vh row.VectorIndexUpdateHelper
	// TODO(mw5h, drewk): support vector indexes.
	oth := row.OriginTimestampCPutHelper{
		OriginTimestamp: after.MvccTimestamp,
		// TODO(ssd): We should choose this based by comparing the cluster IDs of the source
		// and destination clusters.
		// ShouldWinTie: true,
	}
	_, err := p.ru.UpdateRow(
		ctx, b, p.oldVals, p.newVals, ph, vh, oth, false, /* mustValidateOldPKValues */
		false, /* traceKV */
	)
	return err
}

func (p *kvTableWriter) deleteRow(
	ctx context.Context, b *kv.Batch, before, after cdcevent.Row,
) error {
	if err := p.fillOld(before); err != nil {
		return err
	}

	var ph row.PartialIndexUpdateHelper
	// TODO(dt): support partial indexes.
	var vh row.VectorIndexUpdateHelper
	// TODO(mw5h, drewk): support vector indexes.
	oth := row.OriginTimestampCPutHelper{
		PreviousWasDeleted: before.IsDeleted(),
		OriginTimestamp:    after.MvccTimestamp,
		// TODO(ssd): We should choose this based by comparing the cluster IDs of the source
		// and destination clusters.
		// ShouldWinTie: true,
	}

	return p.rd.DeleteRow(
		ctx, b, p.oldVals, ph, vh, oth, false /* mustValidateOldPKValues */, false, /* traceKV */
	)
}

func (p *kvTableWriter) fillOld(vals cdcevent.Row) error {
	p.oldVals = p.oldVals[:0]
	if err := vals.ForAllColumns().Datum(func(d tree.Datum, col cdcevent.ResultColumn) error {
		// TODO(dt): add indirection from col ID to offset.
		p.oldVals = append(p.oldVals, d)
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (p *kvTableWriter) fillNew(vals cdcevent.Row) error {
	p.newVals = p.newVals[:0]
	if err := vals.ForAllColumns().Datum(func(d tree.Datum, col cdcevent.ResultColumn) error {
		p.newVals = append(p.newVals, d)
		return nil
	}); err != nil {
		return err
	}
	return nil
}
