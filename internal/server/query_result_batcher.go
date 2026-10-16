package server

import (
	pb "github.com/radryc/monofs/api/proto"
)

// queryResultBatcher buffers marshaled records and flushes them as batched
// QueryResultItem messages (items_json). High-volume streams - metrics above
// all - otherwise pay one gRPC Send plus JSON envelope per record, which
// dominates query latency when a window holds hundreds of thousands of points.
//
// Batches are only produced when the client requests them via the request's
// batch_size field, so legacy clients keep receiving one record per message.
type queryResultBatcher struct {
	send      func(*pb.QueryResultItem) error
	batchSize int
	pending   [][]byte
	bytes     int
}

const (
	maxQueryResultBatchBytes = 1 << 20 // 1 MiB per message
	maxQueryResultBatchItems = 2048
)

func newQueryResultBatcher(send func(*pb.QueryResultItem) error, batchSize int) *queryResultBatcher {
	if batchSize <= 0 {
		batchSize = 1
	}
	if batchSize > maxQueryResultBatchItems {
		batchSize = maxQueryResultBatchItems
	}
	return &queryResultBatcher{send: send, batchSize: batchSize}
}

// add marshals and buffers one record, flushing when the batch is full.
func (b *queryResultBatcher) add(record any) error {
	itemJSON, err := marshalJSON(record)
	if err != nil {
		return err
	}
	if len(itemJSON) >= maxQueryResultBatchBytes {
		// Oversized record: flush the current batch, then send this one alone.
		if err := b.flush(); err != nil {
			return err
		}
		return b.send(&pb.QueryResultItem{ItemsJson: [][]byte{itemJSON}})
	}
	b.pending = append(b.pending, itemJSON)
	b.bytes += len(itemJSON)
	if len(b.pending) >= b.batchSize || b.bytes >= maxQueryResultBatchBytes {
		return b.flush()
	}
	return nil
}

// flush sends the buffered batch, if any. The batch slice is detached before
// sending so a later add cannot mutate a message still in flight.
func (b *queryResultBatcher) flush() error {
	if len(b.pending) == 0 {
		return nil
	}
	batch := b.pending
	b.pending = nil
	b.bytes = 0
	return b.send(&pb.QueryResultItem{ItemsJson: batch})
}
