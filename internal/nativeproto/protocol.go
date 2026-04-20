package nativeproto

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	FrameMagic  uint32 = 0x53464e4d
	Version1    uint16 = 1
	HeaderSize         = 52

	MaxFrameBytes uint32 = 1 << 20
	MaxReadBytes  uint32 = 256 << 10
)

const (
	OpcodeHello   uint16 = 0x0001
	OpcodeMount   uint16 = 0x0002
	OpcodeUnmount uint16 = 0x0003

	OpcodeLookup  uint16 = 0x0010
	OpcodeGetAttr uint16 = 0x0011
	OpcodeReadDir uint16 = 0x0012
	OpcodeStatFS  uint16 = 0x0014

	OpcodeOpenRead uint16 = 0x0020
	OpcodeRead     uint16 = 0x0021
	OpcodeClose    uint16 = 0x0022

	OpcodeWatch uint16 = 0x0030
	OpcodePing  uint16 = 0x0031
)

const (
	FlagMore uint32 = 1 << 0
)

const (
	StatusOK uint32 = iota
	StatusInvalidRequest
	StatusAuth
	StatusNotFound
	StatusNotDir
	StatusIsDir
	StatusStaleNamespace
	StatusStaleRoute
	StatusUnavailable
	StatusBackendIO
	StatusCancelled
	StatusUnsupported
)

const (
	CapabilityRouteTTLs uint64 = 1 << 0
	CapabilityStatFS    uint64 = 1 << 1
)

const (
	MountFlagReadOnly      uint32 = 1 << 0
	MountFlagOverlayWrites uint32 = 1 << 1
	MountFlagDebug         uint32 = 1 << 2
)

type ObjectID [16]byte

type Header struct {
	Magic     uint32
	Version   uint16
	Opcode    uint16
	Flags     uint32
	HeaderLen uint32
	BodyLen   uint32
	RequestID uint64
	SessionID uint64
	Status    uint32
	Reserved  uint32
	Generation uint64
}

type Attr struct {
	Ino   uint64
	Mode  uint32
	Size  uint64
	Mtime int64
	Atime int64
	Ctime int64
	Nlink uint32
	UID   uint32
	GID   uint32
}

type HelloRequest struct {
	MinVersion    uint16
	MaxVersion    uint16
	RequestedCaps uint64
	ClientKind    string
	ClientVersion string
	KernelRelease string
}

type HelloResponse struct {
	SelectedVersion uint16
	ServerCaps      uint64
	MaxFrameBytes   uint32
	MaxReadBytes    uint32
}

type MountRequest struct {
	ClientID   string
	Hostname   string
	AuthToken  string
	MountFlags uint32
}

type MountResponse struct {
	ClusterVersion      uint64
	NamespaceGeneration uint64
	GuardianVisible     bool
	RootObjectID        ObjectID
	Root                Attr
	EntryTTLMS          uint32
	AttrTTLMS           uint32
	DirTTLMS            uint32
	RouteTTLMS          uint32
}

type LookupRequest struct {
	ParentObjectID ObjectID
	Name           string
}

type LookupResponse struct {
	Found      bool
	EntryTTLMS uint32
	ObjectID   ObjectID
	Attr       Attr
}

type GetAttrRequest struct {
	ObjectID ObjectID
}

type GetAttrResponse struct {
	Found     bool
	AttrTTLMS uint32
	Attr      Attr
}

type DirEntry struct {
	Name     string
	ObjectID ObjectID
	Ino      uint64
	Mode     uint32
}

type ReadDirRequest struct {
	DirObjectID ObjectID
	Cookie      uint64
	MaxEntries  uint32
	MaxBytes    uint32
}

type ReadDirResponse struct {
	DirTTLMS   uint32
	NextCookie uint64
	EOF        bool
	Entries    []DirEntry
}

type StatFSResponse struct {
	Blocks uint64
	Bfree  uint64
	Bavail uint64
	Files  uint64
	Ffree  uint64
	Bsize  uint32
	Frsize uint32
	NameLen uint32
}

type OpenReadRequest struct {
	ObjectID ObjectID
}

type OpenReadResponse struct {
	HandleID   uint64
	Attr       Attr
	RouteTTLMS uint32
}

type ReadRequest struct {
	HandleID uint64
	Offset   uint64
	Length   uint32
}

type ReadResponse struct {
	EOF  bool
	Data []byte
}

type CloseRequest struct {
	HandleID uint64
}

func ReadFrame(r io.Reader) (Header, []byte, error) {
	var raw [HeaderSize]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return Header{}, nil, err
	}

	hdr := Header{
		Magic:      binary.LittleEndian.Uint32(raw[0:4]),
		Version:    binary.LittleEndian.Uint16(raw[4:6]),
		Opcode:     binary.LittleEndian.Uint16(raw[6:8]),
		Flags:      binary.LittleEndian.Uint32(raw[8:12]),
		HeaderLen:  binary.LittleEndian.Uint32(raw[12:16]),
		BodyLen:    binary.LittleEndian.Uint32(raw[16:20]),
		RequestID:  binary.LittleEndian.Uint64(raw[20:28]),
		SessionID:  binary.LittleEndian.Uint64(raw[28:36]),
		Status:     binary.LittleEndian.Uint32(raw[36:40]),
		Reserved:   binary.LittleEndian.Uint32(raw[40:44]),
		Generation: binary.LittleEndian.Uint64(raw[44:52]),
	}

	if hdr.Magic != FrameMagic {
		return Header{}, nil, fmt.Errorf("invalid frame magic: 0x%x", hdr.Magic)
	}
	if hdr.Version != Version1 {
		return Header{}, nil, fmt.Errorf("unsupported protocol version: %d", hdr.Version)
	}
	if hdr.HeaderLen != HeaderSize {
		return Header{}, nil, fmt.Errorf("unexpected header length: %d", hdr.HeaderLen)
	}
	if hdr.BodyLen > MaxFrameBytes {
		return Header{}, nil, fmt.Errorf("body too large: %d", hdr.BodyLen)
	}

	body := make([]byte, hdr.BodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return Header{}, nil, err
	}

	return hdr, body, nil
}

func WriteFrame(w io.Writer, hdr Header, body []byte) error {
	if len(body) > int(MaxFrameBytes) {
		return fmt.Errorf("frame body too large: %d", len(body))
	}

	hdr.Magic = FrameMagic
	hdr.Version = Version1
	hdr.HeaderLen = HeaderSize
	hdr.BodyLen = uint32(len(body))

	var raw [HeaderSize]byte
	binary.LittleEndian.PutUint32(raw[0:4], hdr.Magic)
	binary.LittleEndian.PutUint16(raw[4:6], hdr.Version)
	binary.LittleEndian.PutUint16(raw[6:8], hdr.Opcode)
	binary.LittleEndian.PutUint32(raw[8:12], hdr.Flags)
	binary.LittleEndian.PutUint32(raw[12:16], hdr.HeaderLen)
	binary.LittleEndian.PutUint32(raw[16:20], hdr.BodyLen)
	binary.LittleEndian.PutUint64(raw[20:28], hdr.RequestID)
	binary.LittleEndian.PutUint64(raw[28:36], hdr.SessionID)
	binary.LittleEndian.PutUint32(raw[36:40], hdr.Status)
	binary.LittleEndian.PutUint32(raw[40:44], hdr.Reserved)
	binary.LittleEndian.PutUint64(raw[44:52], hdr.Generation)

	if _, err := w.Write(raw[:]); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := w.Write(body)
	return err
}

type encoder struct {
	bytes.Buffer
}

func (e *encoder) u8(v uint8) {
	e.WriteByte(v)
}

func (e *encoder) u16(v uint16) {
	var raw [2]byte
	binary.LittleEndian.PutUint16(raw[:], v)
	e.Write(raw[:])
}

func (e *encoder) u32(v uint32) {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], v)
	e.Write(raw[:])
}

func (e *encoder) u64(v uint64) {
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], v)
	e.Write(raw[:])
}

func (e *encoder) i64(v int64) {
	e.u64(uint64(v))
}

func (e *encoder) bool(v bool) {
	if v {
		e.u8(1)
		return
	}
	e.u8(0)
}

func (e *encoder) objectID(id ObjectID) {
	e.Write(id[:])
}

func (e *encoder) str(v string) {
	e.u32(uint32(len(v)))
	e.WriteString(v)
}

type decoder struct {
	data []byte
	off  int
}

func (d *decoder) remaining() int {
	return len(d.data) - d.off
}

func (d *decoder) u8() (uint8, error) {
	if d.remaining() < 1 {
		return 0, io.ErrUnexpectedEOF
	}
	v := d.data[d.off]
	d.off++
	return v, nil
}

func (d *decoder) u16() (uint16, error) {
	if d.remaining() < 2 {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint16(d.data[d.off : d.off+2])
	d.off += 2
	return v, nil
}

func (d *decoder) u32() (uint32, error) {
	if d.remaining() < 4 {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint32(d.data[d.off : d.off+4])
	d.off += 4
	return v, nil
}

func (d *decoder) u64() (uint64, error) {
	if d.remaining() < 8 {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint64(d.data[d.off : d.off+8])
	d.off += 8
	return v, nil
}

func (d *decoder) i64() (int64, error) {
	v, err := d.u64()
	return int64(v), err
}

func (d *decoder) bool() (bool, error) {
	v, err := d.u8()
	if err != nil {
		return false, err
	}
	return v != 0, nil
}

func (d *decoder) objectID() (ObjectID, error) {
	var id ObjectID
	if d.remaining() < len(id) {
		return id, io.ErrUnexpectedEOF
	}
	copy(id[:], d.data[d.off:d.off+len(id)])
	d.off += len(id)
	return id, nil
}

func (d *decoder) str() (string, error) {
	size, err := d.u32()
	if err != nil {
		return "", err
	}
	if d.remaining() < int(size) {
		return "", io.ErrUnexpectedEOF
	}
	v := string(d.data[d.off : d.off+int(size)])
	d.off += int(size)
	return v, nil
}

func encodeAttr(enc *encoder, attr Attr) {
	enc.u64(attr.Ino)
	enc.u32(attr.Mode)
	enc.u64(attr.Size)
	enc.i64(attr.Mtime)
	enc.i64(attr.Atime)
	enc.i64(attr.Ctime)
	enc.u32(attr.Nlink)
	enc.u32(attr.UID)
	enc.u32(attr.GID)
}

func decodeAttr(dec *decoder) (Attr, error) {
	var attr Attr
	var err error
	if attr.Ino, err = dec.u64(); err != nil {
		return Attr{}, err
	}
	if attr.Mode, err = dec.u32(); err != nil {
		return Attr{}, err
	}
	if attr.Size, err = dec.u64(); err != nil {
		return Attr{}, err
	}
	if attr.Mtime, err = dec.i64(); err != nil {
		return Attr{}, err
	}
	if attr.Atime, err = dec.i64(); err != nil {
		return Attr{}, err
	}
	if attr.Ctime, err = dec.i64(); err != nil {
		return Attr{}, err
	}
	if attr.Nlink, err = dec.u32(); err != nil {
		return Attr{}, err
	}
	if attr.UID, err = dec.u32(); err != nil {
		return Attr{}, err
	}
	if attr.GID, err = dec.u32(); err != nil {
		return Attr{}, err
	}
	return attr, nil
}

func EncodeHelloRequest(req HelloRequest) []byte {
	var enc encoder
	enc.u16(req.MinVersion)
	enc.u16(req.MaxVersion)
	enc.u64(req.RequestedCaps)
	enc.str(req.ClientKind)
	enc.str(req.ClientVersion)
	enc.str(req.KernelRelease)
	return enc.Bytes()
}

func DecodeHelloRequest(data []byte) (HelloRequest, error) {
	dec := decoder{data: data}
	var req HelloRequest
	var err error
	if req.MinVersion, err = dec.u16(); err != nil {
		return HelloRequest{}, err
	}
	if req.MaxVersion, err = dec.u16(); err != nil {
		return HelloRequest{}, err
	}
	if req.RequestedCaps, err = dec.u64(); err != nil {
		return HelloRequest{}, err
	}
	if req.ClientKind, err = dec.str(); err != nil {
		return HelloRequest{}, err
	}
	if req.ClientVersion, err = dec.str(); err != nil {
		return HelloRequest{}, err
	}
	if req.KernelRelease, err = dec.str(); err != nil {
		return HelloRequest{}, err
	}
	return req, nil
}

func EncodeHelloResponse(resp HelloResponse) []byte {
	var enc encoder
	enc.u16(resp.SelectedVersion)
	enc.u64(resp.ServerCaps)
	enc.u32(resp.MaxFrameBytes)
	enc.u32(resp.MaxReadBytes)
	return enc.Bytes()
}

func DecodeHelloResponse(data []byte) (HelloResponse, error) {
	dec := decoder{data: data}
	var resp HelloResponse
	var err error
	if resp.SelectedVersion, err = dec.u16(); err != nil {
		return HelloResponse{}, err
	}
	if resp.ServerCaps, err = dec.u64(); err != nil {
		return HelloResponse{}, err
	}
	if resp.MaxFrameBytes, err = dec.u32(); err != nil {
		return HelloResponse{}, err
	}
	if resp.MaxReadBytes, err = dec.u32(); err != nil {
		return HelloResponse{}, err
	}
	return resp, nil
}

func EncodeMountRequest(req MountRequest) []byte {
	var enc encoder
	enc.u32(req.MountFlags)
	enc.str(req.ClientID)
	enc.str(req.Hostname)
	enc.str(req.AuthToken)
	return enc.Bytes()
}

func DecodeMountRequest(data []byte) (MountRequest, error) {
	dec := decoder{data: data}
	var req MountRequest
	var err error
	if req.MountFlags, err = dec.u32(); err != nil {
		return MountRequest{}, err
	}
	if req.ClientID, err = dec.str(); err != nil {
		return MountRequest{}, err
	}
	if req.Hostname, err = dec.str(); err != nil {
		return MountRequest{}, err
	}
	if req.AuthToken, err = dec.str(); err != nil {
		return MountRequest{}, err
	}
	return req, nil
}

func EncodeMountResponse(resp MountResponse) []byte {
	var enc encoder
	enc.u64(resp.ClusterVersion)
	enc.u64(resp.NamespaceGeneration)
	enc.bool(resp.GuardianVisible)
	enc.objectID(resp.RootObjectID)
	encodeAttr(&enc, resp.Root)
	enc.u32(resp.EntryTTLMS)
	enc.u32(resp.AttrTTLMS)
	enc.u32(resp.DirTTLMS)
	enc.u32(resp.RouteTTLMS)
	return enc.Bytes()
}

func DecodeMountResponse(data []byte) (MountResponse, error) {
	dec := decoder{data: data}
	var resp MountResponse
	var err error
	if resp.ClusterVersion, err = dec.u64(); err != nil {
		return MountResponse{}, err
	}
	if resp.NamespaceGeneration, err = dec.u64(); err != nil {
		return MountResponse{}, err
	}
	if resp.GuardianVisible, err = dec.bool(); err != nil {
		return MountResponse{}, err
	}
	if resp.RootObjectID, err = dec.objectID(); err != nil {
		return MountResponse{}, err
	}
	if resp.Root, err = decodeAttr(&dec); err != nil {
		return MountResponse{}, err
	}
	if resp.EntryTTLMS, err = dec.u32(); err != nil {
		return MountResponse{}, err
	}
	if resp.AttrTTLMS, err = dec.u32(); err != nil {
		return MountResponse{}, err
	}
	if resp.DirTTLMS, err = dec.u32(); err != nil {
		return MountResponse{}, err
	}
	if resp.RouteTTLMS, err = dec.u32(); err != nil {
		return MountResponse{}, err
	}
	return resp, nil
}

func EncodeLookupRequest(req LookupRequest) []byte {
	var enc encoder
	enc.objectID(req.ParentObjectID)
	enc.str(req.Name)
	return enc.Bytes()
}

func DecodeLookupRequest(data []byte) (LookupRequest, error) {
	dec := decoder{data: data}
	var req LookupRequest
	var err error
	if req.ParentObjectID, err = dec.objectID(); err != nil {
		return LookupRequest{}, err
	}
	if req.Name, err = dec.str(); err != nil {
		return LookupRequest{}, err
	}
	return req, nil
}

func EncodeLookupResponse(resp LookupResponse) []byte {
	var enc encoder
	enc.bool(resp.Found)
	enc.u32(resp.EntryTTLMS)
	if resp.Found {
		enc.objectID(resp.ObjectID)
		encodeAttr(&enc, resp.Attr)
	}
	return enc.Bytes()
}

func DecodeLookupResponse(data []byte) (LookupResponse, error) {
	dec := decoder{data: data}
	var resp LookupResponse
	var err error
	if resp.Found, err = dec.bool(); err != nil {
		return LookupResponse{}, err
	}
	if resp.EntryTTLMS, err = dec.u32(); err != nil {
		return LookupResponse{}, err
	}
	if resp.Found {
		if resp.ObjectID, err = dec.objectID(); err != nil {
			return LookupResponse{}, err
		}
		if resp.Attr, err = decodeAttr(&dec); err != nil {
			return LookupResponse{}, err
		}
	}
	return resp, nil
}

func EncodeGetAttrRequest(req GetAttrRequest) []byte {
	var enc encoder
	enc.objectID(req.ObjectID)
	return enc.Bytes()
}

func DecodeGetAttrRequest(data []byte) (GetAttrRequest, error) {
	dec := decoder{data: data}
	objectID, err := dec.objectID()
	if err != nil {
		return GetAttrRequest{}, err
	}
	return GetAttrRequest{ObjectID: objectID}, nil
}

func EncodeGetAttrResponse(resp GetAttrResponse) []byte {
	var enc encoder
	enc.bool(resp.Found)
	enc.u32(resp.AttrTTLMS)
	if resp.Found {
		encodeAttr(&enc, resp.Attr)
	}
	return enc.Bytes()
}

func DecodeGetAttrResponse(data []byte) (GetAttrResponse, error) {
	dec := decoder{data: data}
	var resp GetAttrResponse
	var err error
	if resp.Found, err = dec.bool(); err != nil {
		return GetAttrResponse{}, err
	}
	if resp.AttrTTLMS, err = dec.u32(); err != nil {
		return GetAttrResponse{}, err
	}
	if resp.Found {
		if resp.Attr, err = decodeAttr(&dec); err != nil {
			return GetAttrResponse{}, err
		}
	}
	return resp, nil
}

func EncodeReadDirRequest(req ReadDirRequest) []byte {
	var enc encoder
	enc.objectID(req.DirObjectID)
	enc.u64(req.Cookie)
	enc.u32(req.MaxEntries)
	enc.u32(req.MaxBytes)
	return enc.Bytes()
}

func DecodeReadDirRequest(data []byte) (ReadDirRequest, error) {
	dec := decoder{data: data}
	var req ReadDirRequest
	var err error
	if req.DirObjectID, err = dec.objectID(); err != nil {
		return ReadDirRequest{}, err
	}
	if req.Cookie, err = dec.u64(); err != nil {
		return ReadDirRequest{}, err
	}
	if req.MaxEntries, err = dec.u32(); err != nil {
		return ReadDirRequest{}, err
	}
	if req.MaxBytes, err = dec.u32(); err != nil {
		return ReadDirRequest{}, err
	}
	return req, nil
}

func EncodeReadDirResponse(resp ReadDirResponse) []byte {
	var enc encoder
	enc.u32(resp.DirTTLMS)
	enc.u64(resp.NextCookie)
	enc.bool(resp.EOF)
	enc.u32(uint32(len(resp.Entries)))
	for _, entry := range resp.Entries {
		enc.str(entry.Name)
		enc.objectID(entry.ObjectID)
		enc.u64(entry.Ino)
		enc.u32(entry.Mode)
	}
	return enc.Bytes()
}

func DecodeReadDirResponse(data []byte) (ReadDirResponse, error) {
	dec := decoder{data: data}
	var resp ReadDirResponse
	var err error
	if resp.DirTTLMS, err = dec.u32(); err != nil {
		return ReadDirResponse{}, err
	}
	if resp.NextCookie, err = dec.u64(); err != nil {
		return ReadDirResponse{}, err
	}
	if resp.EOF, err = dec.bool(); err != nil {
		return ReadDirResponse{}, err
	}
	count, err := dec.u32()
	if err != nil {
		return ReadDirResponse{}, err
	}
	resp.Entries = make([]DirEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		var entry DirEntry
		if entry.Name, err = dec.str(); err != nil {
			return ReadDirResponse{}, err
		}
		if entry.ObjectID, err = dec.objectID(); err != nil {
			return ReadDirResponse{}, err
		}
		if entry.Ino, err = dec.u64(); err != nil {
			return ReadDirResponse{}, err
		}
		if entry.Mode, err = dec.u32(); err != nil {
			return ReadDirResponse{}, err
		}
		resp.Entries = append(resp.Entries, entry)
	}
	return resp, nil
}

func EncodeStatFSResponse(resp StatFSResponse) []byte {
	var enc encoder
	enc.u64(resp.Blocks)
	enc.u64(resp.Bfree)
	enc.u64(resp.Bavail)
	enc.u64(resp.Files)
	enc.u64(resp.Ffree)
	enc.u32(resp.Bsize)
	enc.u32(resp.Frsize)
	enc.u32(resp.NameLen)
	return enc.Bytes()
}

func DecodeStatFSResponse(data []byte) (StatFSResponse, error) {
	dec := decoder{data: data}
	var resp StatFSResponse
	var err error
	if resp.Blocks, err = dec.u64(); err != nil {
		return StatFSResponse{}, err
	}
	if resp.Bfree, err = dec.u64(); err != nil {
		return StatFSResponse{}, err
	}
	if resp.Bavail, err = dec.u64(); err != nil {
		return StatFSResponse{}, err
	}
	if resp.Files, err = dec.u64(); err != nil {
		return StatFSResponse{}, err
	}
	if resp.Ffree, err = dec.u64(); err != nil {
		return StatFSResponse{}, err
	}
	if resp.Bsize, err = dec.u32(); err != nil {
		return StatFSResponse{}, err
	}
	if resp.Frsize, err = dec.u32(); err != nil {
		return StatFSResponse{}, err
	}
	if resp.NameLen, err = dec.u32(); err != nil {
		return StatFSResponse{}, err
	}
	return resp, nil
}

func EncodeOpenReadRequest(req OpenReadRequest) []byte {
	var enc encoder
	enc.objectID(req.ObjectID)
	return enc.Bytes()
}

func DecodeOpenReadRequest(data []byte) (OpenReadRequest, error) {
	dec := decoder{data: data}
	objectID, err := dec.objectID()
	if err != nil {
		return OpenReadRequest{}, err
	}
	return OpenReadRequest{ObjectID: objectID}, nil
}

func EncodeOpenReadResponse(resp OpenReadResponse) []byte {
	var enc encoder
	enc.u64(resp.HandleID)
	encodeAttr(&enc, resp.Attr)
	enc.u32(resp.RouteTTLMS)
	return enc.Bytes()
}

func DecodeOpenReadResponse(data []byte) (OpenReadResponse, error) {
	dec := decoder{data: data}
	var resp OpenReadResponse
	var err error
	if resp.HandleID, err = dec.u64(); err != nil {
		return OpenReadResponse{}, err
	}
	if resp.Attr, err = decodeAttr(&dec); err != nil {
		return OpenReadResponse{}, err
	}
	if resp.RouteTTLMS, err = dec.u32(); err != nil {
		return OpenReadResponse{}, err
	}
	return resp, nil
}

func EncodeReadRequest(req ReadRequest) []byte {
	var enc encoder
	enc.u64(req.HandleID)
	enc.u64(req.Offset)
	enc.u32(req.Length)
	return enc.Bytes()
}

func DecodeReadRequest(data []byte) (ReadRequest, error) {
	dec := decoder{data: data}
	var req ReadRequest
	var err error
	if req.HandleID, err = dec.u64(); err != nil {
		return ReadRequest{}, err
	}
	if req.Offset, err = dec.u64(); err != nil {
		return ReadRequest{}, err
	}
	if req.Length, err = dec.u32(); err != nil {
		return ReadRequest{}, err
	}
	return req, nil
}

func EncodeReadResponse(resp ReadResponse) []byte {
	var enc encoder
	enc.bool(resp.EOF)
	enc.u32(uint32(len(resp.Data)))
	enc.Write(resp.Data)
	return enc.Bytes()
}

func DecodeReadResponse(data []byte) (ReadResponse, error) {
	dec := decoder{data: data}
	var resp ReadResponse
	var err error
	var size uint32
	if resp.EOF, err = dec.bool(); err != nil {
		return ReadResponse{}, err
	}
	if size, err = dec.u32(); err != nil {
		return ReadResponse{}, err
	}
	if dec.remaining() < int(size) {
		return ReadResponse{}, io.ErrUnexpectedEOF
	}
	resp.Data = append([]byte(nil), dec.data[dec.off:dec.off+int(size)]...)
	return resp, nil
}

func EncodeCloseRequest(req CloseRequest) []byte {
	var enc encoder
	enc.u64(req.HandleID)
	return enc.Bytes()
}

func DecodeCloseRequest(data []byte) (CloseRequest, error) {
	dec := decoder{data: data}
	handleID, err := dec.u64()
	if err != nil {
		return CloseRequest{}, err
	}
	return CloseRequest{HandleID: handleID}, nil
}
