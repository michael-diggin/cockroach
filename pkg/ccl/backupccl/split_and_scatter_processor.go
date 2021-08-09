// Copyright 2020 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package backupccl

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
)

type splitAndScatterer interface {
	// split issues a split request at the given key, which may be rewritten to
	// the RESTORE keyspace.
	split(ctx context.Context, codec keys.SQLCodec, splitKey roachpb.Key) error
	// scatter issues a scatter request at the given key. It returns the node ID
	// of where the range was scattered to.
	scatter(ctx context.Context, codec keys.SQLCodec, scatterKey roachpb.Key) (roachpb.NodeID, error)
}

type noopSplitAndScatterer struct{}

var _ splitAndScatterer = noopSplitAndScatterer{}

// split implements splitAndScatterer.
func (n noopSplitAndScatterer) split(_ context.Context, _ keys.SQLCodec, _ roachpb.Key) error {
	return nil
}

// scatter implements splitAndScatterer.
func (n noopSplitAndScatterer) scatter(
	_ context.Context, _ keys.SQLCodec, _ roachpb.Key,
) (roachpb.NodeID, error) {
	return 0, nil
}

// dbSplitAndScatter is the production implementation of this processor's
// scatterer. It actually issues the split and scatter requests against the KV
// layer.
type dbSplitAndScatterer struct {
	db *kv.DB
	kr *storageccl.KeyRewriter
}

var _ splitAndScatterer = dbSplitAndScatterer{}

func makeSplitAndScatterer(db *kv.DB, kr *storageccl.KeyRewriter) splitAndScatterer {
	return dbSplitAndScatterer{db: db, kr: kr}
}

// split implements splitAndScatterer.
func (s dbSplitAndScatterer) split(
	ctx context.Context, codec keys.SQLCodec, splitKey roachpb.Key,
) error {
	if s.kr == nil {
		return errors.AssertionFailedf("KeyRewriter was not set when expected to be")
	}
	if s.db == nil {
		return errors.AssertionFailedf("split and scatterer's database was not set when expected")
	}

	expirationTime := s.db.Clock().Now().Add(time.Hour.Nanoseconds(), 0)
	newSplitKey, err := rewriteBackupSpanKey(s.kr, splitKey)
	if err != nil {
		return err
	}
	log.VEventf(ctx, 1, "presplitting new key %+v", newSplitKey)
	if err := s.db.AdminSplit(ctx, newSplitKey, expirationTime); err != nil {
		return errors.Wrapf(err, "splitting key %s", newSplitKey)
	}

	return nil
}

// scatter implements splitAndScatterer.
func (s dbSplitAndScatterer) scatter(
	ctx context.Context, codec keys.SQLCodec, scatterKey roachpb.Key,
) (roachpb.NodeID, error) {
	if s.kr == nil {
		return 0, errors.AssertionFailedf("KeyRewriter was not set when expected to be")
	}
	if s.db == nil {
		return 0, errors.AssertionFailedf("split and scatterer's database was not set when expected")
	}

	newScatterKey, err := rewriteBackupSpanKey(s.kr, scatterKey)
	if err != nil {
		return 0, err
	}

	log.VEventf(ctx, 1, "scattering new key %+v", newScatterKey)
	req := &roachpb.AdminScatterRequest{
		RequestHeader: roachpb.RequestHeaderFromSpan(roachpb.Span{
			Key:    newScatterKey,
			EndKey: newScatterKey.Next(),
		}),
		// This is a bit of a hack, but it seems to be an effective one (see #36665
		// for graphs). As of the commit that added this, scatter is not very good
		// at actually balancing leases. This is likely for two reasons: 1) there's
		// almost certainly some regression in scatter's behavior, it used to work
		// much better and 2) scatter has to operate by balancing leases for all
		// ranges in a cluster, but in RESTORE, we really just want it to be
		// balancing the span being restored into.
		RandomizeLeases: true,
	}

	res, pErr := kv.SendWrapped(ctx, s.db.NonTransactionalSender(), req)
	if pErr != nil {
		// TODO(pbardea): Unfortunately, Scatter is still too unreliable to
		// fail the RESTORE when Scatter fails. I'm uncomfortable that
		// this could break entirely and not start failing the tests,
		// but on the bright side, it doesn't affect correctness, only
		// throughput.
		log.Errorf(ctx, "failed to scatter span [%s,%s): %+v",
			newScatterKey, newScatterKey.Next(), pErr.GoError())
		return 0, nil
	}

	return s.findDestination(res.(*roachpb.AdminScatterResponse)), nil
}

// findDestination returns the node ID of the node of the destination of the
// AdminScatter request. If the destination cannot be found, 0 is returned.
func (s dbSplitAndScatterer) findDestination(res *roachpb.AdminScatterResponse) roachpb.NodeID {
	// A request from a 20.1 node will not have a RangeInfos with a lease.
	// For this mixed-version state, we'll report the destination as node 0
	// and suffer a bit of inefficiency.
	if len(res.RangeInfos) > 0 {
		// If the lease is not populated, we return the 0 value anyway. We receive 1
		// RangeInfo per range that was scattered. Since we send a scatter request
		// to each range that we make, we are only interested in the first range,
		// which contains the key at which we're splitting and scattering.
		return res.RangeInfos[0].Lease.Replica.NodeID
	}

	return roachpb.NodeID(0)
}

var splitAndScatterOutputTypes = []*types.T{
	types.Bytes, // Span key for the range router
	types.Bytes, // RestoreDataEntry bytes
}

// splitAndScatterProcessor is given a set of spans (specified as
// RestoreSpanEntry's) to distribute across the cluster. Depending on which node
// the span ends up on, it forwards RestoreSpanEntry as bytes along with the key
// of the span on a row. It expects an output RangeRouter and before it emits
// each row, it updates the entry in the RangeRouter's map with the destination
// of the scatter.
type splitAndScatterProcessor struct {
	flowCtx   *execinfra.FlowCtx
	spec      execinfrapb.SplitAndScatterSpec
	output    execinfra.RowReceiver
	scatterer splitAndScatterer
}

var _ execinfra.Processor = &splitAndScatterProcessor{}

// OutputTypes implements the execinfra.Processor interface.
func (ssp *splitAndScatterProcessor) OutputTypes() []*types.T {
	return splitAndScatterOutputTypes
}

func newSplitAndScatterProcessor(
	flowCtx *execinfra.FlowCtx,
	_ int32,
	spec execinfrapb.SplitAndScatterSpec,
	output execinfra.RowReceiver,
) (execinfra.Processor, error) {
	db := flowCtx.Cfg.DB
	kr, err := storageccl.MakeKeyRewriterFromRekeys(spec.Rekeys)
	if err != nil {
		return nil, err
	}

	var scatterer splitAndScatterer = makeSplitAndScatterer(db, kr)
	ssp := &splitAndScatterProcessor{
		flowCtx:   flowCtx,
		spec:      spec,
		output:    output,
		scatterer: scatterer,
	}
	return ssp, nil
}

type entryNode struct {
	entry execinfrapb.RestoreSpanEntry
	node  roachpb.NodeID
}

// scatteredChunk is the entries of a chunk of entries to process along with the
// node the chunk was scattered to.
type scatteredChunk struct {
	destination roachpb.NodeID
	entries     []execinfrapb.RestoreSpanEntry
}

// Run implements the execinfra.Processor interface.
func (ssp *splitAndScatterProcessor) Run(ctx context.Context) {
	ctx, span := tracing.ChildSpan(ctx, "splitAndScatterProcessor")
	defer tracing.FinishSpan(span)
	defer ssp.output.ProducerDone()

	numEntries := 0
	for _, chunk := range ssp.spec.Chunks {
		numEntries += len(chunk.Entries)
	}
	// Large enough so that it never blocks.
	doneScatterCh := make(chan entryNode, numEntries)

	// A cache for routing datums, so only 1 is allocated per node.
	routingDatumCache := make(map[roachpb.NodeID]rowenc.EncDatum)

	var err error
	splitAndScatterCtx, cancelSplitAndScatter := context.WithCancel(ctx)
	defer cancelSplitAndScatter()
	// Note that the loop over doneScatterCh should prevent this goroutine from
	// leaking when there are no errors. However, if that loop needs to exit
	// early, runSplitAndScatter's context will be canceled.
	go func() {
		defer close(doneScatterCh)
		err = runSplitAndScatter(splitAndScatterCtx, ssp.flowCtx, &ssp.spec, ssp.scatterer, doneScatterCh)
		if err != nil {
			log.Errorf(ctx, "error while running split and scatter: %+v", err)
		}
	}()

	for scatteredEntry := range doneScatterCh {
		entry := scatteredEntry.entry
		entryBytes, err := protoutil.Marshal(&entry)
		if err != nil {
			ssp.output.Push(nil, &execinfrapb.ProducerMetadata{Err: err})
			break
		}

		// The routing datums informs the router which output stream should be used.
		routingDatum, ok := routingDatumCache[scatteredEntry.node]
		if !ok {
			routingDatum, _ = routingDatumsForNode(scatteredEntry.node)
			routingDatumCache[scatteredEntry.node] = routingDatum
		}

		row := rowenc.EncDatumRow{
			routingDatum,
			rowenc.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(entryBytes))),
		}
		ssp.output.Push(row, nil)
	}

	if err != nil {
		ssp.output.Push(nil, &execinfrapb.ProducerMetadata{Err: err})
		return
	}
}

func runSplitAndScatter(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	spec *execinfrapb.SplitAndScatterSpec,
	scatterer splitAndScatterer,
	doneScatterCh chan entryNode,
) error {
	g := ctxgroup.WithContext(ctx)

	importSpanChunksCh := make(chan scatteredChunk)
	g.GoCtx(func(ctx context.Context) error {
		// Chunks' leaseholders should be randomly placed throughout the
		// cluster.
		defer close(importSpanChunksCh)
		for i, importSpanChunk := range spec.Chunks {
			scatterKey := importSpanChunk.Entries[0].Span.Key
			if i+1 < len(spec.Chunks) {
				// Split at the start of the next chunk, to partition off a
				// prefix of the space to scatter.
				splitKey := spec.Chunks[i+1].Entries[0].Span.Key
				if err := scatterer.split(ctx, flowCtx.Codec(), splitKey); err != nil {
					return err
				}
			}
			chunkDestination, err := scatterer.scatter(ctx, flowCtx.Codec(), scatterKey)
			if err != nil {
				return err
			}

			sc := scatteredChunk{
				destination: chunkDestination,
				entries:     importSpanChunk.Entries,
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case importSpanChunksCh <- sc:
			}
		}
		return nil
	})

	// TODO(pbardea): This tries to cover for a bad scatter by having 2 * the
	// number of nodes in the cluster. Is it necessary?
	splitScatterWorkers := 2
	for worker := 0; worker < splitScatterWorkers; worker++ {
		g.GoCtx(func(ctx context.Context) error {
			for importSpanChunk := range importSpanChunksCh {
				chunkDestination := importSpanChunk.destination
				for i, importEntry := range importSpanChunk.entries {
					nextChunkIdx := i + 1

					log.VInfof(ctx, 2, "processing a span [%s,%s)", importEntry.Span.Key, importEntry.Span.EndKey)
					var splitKey roachpb.Key
					if nextChunkIdx < len(importSpanChunk.entries) {
						// Split at the next entry.
						splitKey = importSpanChunk.entries[nextChunkIdx].Span.Key
						if err := scatterer.split(ctx, flowCtx.Codec(), splitKey); err != nil {
							return err
						}
					}

					scatteredEntry := entryNode{
						entry: importEntry,
						node:  chunkDestination,
					}

					select {
					case <-ctx.Done():
						return ctx.Err()
					case doneScatterCh <- scatteredEntry:
					}
				}
			}
			return nil
		})
	}

	return g.Wait()
}

func routingDatumsForNode(nodeID roachpb.NodeID) (rowenc.EncDatum, rowenc.EncDatum) {
	routingBytes := roachpb.Key(fmt.Sprintf("node%d", nodeID))
	startDatum := rowenc.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(routingBytes)))
	endDatum := rowenc.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(routingBytes.Next())))
	return startDatum, endDatum
}

// routingSpanForNode provides the mapping to be used during distsql planning
// when setting up the output router.
func routingSpanForNode(nodeID roachpb.NodeID) ([]byte, []byte, error) {
	var alloc rowenc.DatumAlloc
	startDatum, endDatum := routingDatumsForNode(nodeID)

	startBytes, endBytes := make([]byte, 0), make([]byte, 0)
	startBytes, err := startDatum.Encode(splitAndScatterOutputTypes[0], &alloc, descpb.DatumEncoding_ASCENDING_KEY, startBytes)
	if err != nil {
		return nil, nil, err
	}
	endBytes, err = endDatum.Encode(splitAndScatterOutputTypes[0], &alloc, descpb.DatumEncoding_ASCENDING_KEY, endBytes)
	if err != nil {
		return nil, nil, err
	}
	return startBytes, endBytes, nil
}

func init() {
	rowexec.NewSplitAndScatterProcessor = newSplitAndScatterProcessor
}
