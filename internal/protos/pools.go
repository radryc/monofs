// Package protos provides performance optimizations for protobuf message handling.
package protos

import (
	"sync"

	pb "github.com/radryc/monofs/api/proto"
)

// MessagePools holds sync.Pool instances for commonly used protobuf messages.
// These are the high-frequency message types from the gRPC API.
type MessagePools struct {
	readRequestPool    *sync.Pool
	writeRequestPool   *sync.Pool
	writeResponsePool  *sync.Pool
	lookupRequestPool  *sync.Pool
	lookupResponsePool *sync.Pool
	readDirRequestPool *sync.Pool
	dirEntryPool       *sync.Pool
	dataChunkPool      *sync.Pool
}

// NewMessagePools creates and initializes all message pools.
func NewMessagePools() *MessagePools {
	return &MessagePools{
		readRequestPool: &sync.Pool{
			New: func() interface{} {
				return &pb.ReadRequest{}
			},
		},
		writeRequestPool: &sync.Pool{
			New: func() interface{} {
				return &pb.WriteRequest{}
			},
		},
		writeResponsePool: &sync.Pool{
			New: func() interface{} {
				return &pb.WriteResponse{}
			},
		},
		lookupRequestPool: &sync.Pool{
			New: func() interface{} {
				return &pb.LookupRequest{}
			},
		},
		lookupResponsePool: &sync.Pool{
			New: func() interface{} {
				return &pb.LookupResponse{}
			},
		},
		readDirRequestPool: &sync.Pool{
			New: func() interface{} {
				return &pb.ReadDirRequest{}
			},
		},
		dirEntryPool: &sync.Pool{
			New: func() interface{} {
				return &pb.DirEntry{}
			},
		},
		dataChunkPool: &sync.Pool{
			New: func() interface{} {
				return &pb.DataChunk{}
			},
		},
	}
}

// GetReadRequest returns a ReadRequest from the pool.
func (p *MessagePools) GetReadRequest() *pb.ReadRequest {
	return p.readRequestPool.Get().(*pb.ReadRequest)
}

// PutReadRequest returns a ReadRequest to the pool.
func (p *MessagePools) PutReadRequest(msg *pb.ReadRequest) {
	if msg != nil {
		*msg = pb.ReadRequest{} // Reset to zero value
		p.readRequestPool.Put(msg)
	}
}

// GetWriteRequest returns a WriteRequest from the pool.
func (p *MessagePools) GetWriteRequest() *pb.WriteRequest {
	return p.writeRequestPool.Get().(*pb.WriteRequest)
}

// PutWriteRequest returns a WriteRequest to the pool.
func (p *MessagePools) PutWriteRequest(msg *pb.WriteRequest) {
	if msg != nil {
		*msg = pb.WriteRequest{} // Reset to zero value
		p.writeRequestPool.Put(msg)
	}
}

// GetWriteResponse returns a WriteResponse from the pool.
func (p *MessagePools) GetWriteResponse() *pb.WriteResponse {
	return p.writeResponsePool.Get().(*pb.WriteResponse)
}

// PutWriteResponse returns a WriteResponse to the pool.
func (p *MessagePools) PutWriteResponse(msg *pb.WriteResponse) {
	if msg != nil {
		*msg = pb.WriteResponse{} // Reset to zero value
		p.writeResponsePool.Put(msg)
	}
}

// GetLookupRequest returns a LookupRequest from the pool.
func (p *MessagePools) GetLookupRequest() *pb.LookupRequest {
	return p.lookupRequestPool.Get().(*pb.LookupRequest)
}

// PutLookupRequest returns a LookupRequest to the pool.
func (p *MessagePools) PutLookupRequest(msg *pb.LookupRequest) {
	if msg != nil {
		*msg = pb.LookupRequest{} // Reset to zero value
		p.lookupRequestPool.Put(msg)
	}
}

// GetLookupResponse returns a LookupResponse from the pool.
func (p *MessagePools) GetLookupResponse() *pb.LookupResponse {
	return p.lookupResponsePool.Get().(*pb.LookupResponse)
}

// PutLookupResponse returns a LookupResponse to the pool.
func (p *MessagePools) PutLookupResponse(msg *pb.LookupResponse) {
	if msg != nil {
		*msg = pb.LookupResponse{} // Reset to zero value
		p.lookupResponsePool.Put(msg)
	}
}

// GetReadDirRequest returns a ReadDirRequest from the pool.
func (p *MessagePools) GetReadDirRequest() *pb.ReadDirRequest {
	return p.readDirRequestPool.Get().(*pb.ReadDirRequest)
}

// PutReadDirRequest returns a ReadDirRequest to the pool.
func (p *MessagePools) PutReadDirRequest(msg *pb.ReadDirRequest) {
	if msg != nil {
		*msg = pb.ReadDirRequest{} // Reset to zero value
		p.readDirRequestPool.Put(msg)
	}
}

// GetDirEntry returns a DirEntry from the pool.
func (p *MessagePools) GetDirEntry() *pb.DirEntry {
	return p.dirEntryPool.Get().(*pb.DirEntry)
}

// PutDirEntry returns a DirEntry to the pool.
func (p *MessagePools) PutDirEntry(msg *pb.DirEntry) {
	if msg != nil {
		*msg = pb.DirEntry{} // Reset to zero value
		p.dirEntryPool.Put(msg)
	}
}

// GetDataChunk returns a DataChunk from the pool.
func (p *MessagePools) GetDataChunk() *pb.DataChunk {
	return p.dataChunkPool.Get().(*pb.DataChunk)
}

// PutDataChunk returns a DataChunk to the pool.
func (p *MessagePools) PutDataChunk(msg *pb.DataChunk) {
	if msg != nil {
		*msg = pb.DataChunk{} // Reset to zero value
		p.dataChunkPool.Put(msg)
	}
}
