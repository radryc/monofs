package router

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/nativeproto"
)

// NativeGateway serves the framed native protocol from inside the router
// process, reusing the router's authoritative namespace view.
type NativeGateway struct {
	router      *Router
	logger      *slog.Logger
	nextSession atomic.Uint64
}

type nativeGatewaySession struct {
	id           uint64
	nextHandle   uint64
	pathByObject map[nativeproto.ObjectID]string
	objectByPath map[string]nativeproto.ObjectID
	pathByHandle map[uint64]string
}

func NewNativeGateway(r *Router, logger *slog.Logger) *NativeGateway {
	if logger == nil {
		logger = slog.Default()
	}
	return &NativeGateway{
		router: r,
		logger: logger.With("component", "native-gateway"),
	}
}

func (g *NativeGateway) currentGeneration() uint64 {
	return g.router.nativeEffectiveGeneration()
}

func (g *NativeGateway) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		go g.handleConn(conn)
	}
}

func (g *NativeGateway) handleConn(conn net.Conn) {
	defer conn.Close()

	var session *nativeGatewaySession

	for {
		if err := conn.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
			return
		}

		hdr, body, err := nativeproto.ReadFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			g.logger.Debug("native frame read failed", "remote_addr", conn.RemoteAddr(), "error", err)
			return
		}

		replyHdr := nativeproto.Header{
			Opcode:     hdr.Opcode,
			RequestID:  hdr.RequestID,
			SessionID:  hdr.SessionID,
			Status:     nativeproto.StatusOK,
			Generation: g.currentGeneration(),
		}

		replyBody, statusCode, newSessionID, generation := g.dispatchFrame(context.Background(), session, hdr, body)
		replyHdr.Status = statusCode
		replyHdr.Generation = generation
		if newSessionID != 0 {
			replyHdr.SessionID = newSessionID
		}

		if hdr.Opcode == nativeproto.OpcodeMount && statusCode == nativeproto.StatusOK && newSessionID != 0 {
			session = g.newSession(newSessionID)
			if err := g.populateMountSession(session, replyBody); err != nil {
				replyHdr.Status = nativeproto.StatusBackendIO
				replyBody = nil
			}
		}

		if err := nativeproto.WriteFrame(conn, replyHdr, replyBody); err != nil {
			g.logger.Debug("native frame write failed", "remote_addr", conn.RemoteAddr(), "error", err)
			return
		}

		if hdr.Opcode == nativeproto.OpcodeUnmount {
			return
		}
	}
}

func (g *NativeGateway) dispatchFrame(ctx context.Context, session *nativeGatewaySession, hdr nativeproto.Header, body []byte) ([]byte, uint32, uint64, uint64) {
	switch hdr.Opcode {
	case nativeproto.OpcodeHello:
		body, status, generation := g.handleHello(body)
		return body, status, 0, generation
	case nativeproto.OpcodeMount:
		body, status, sessionID, generation := g.handleMount(ctx, body)
		return body, status, sessionID, generation
	case nativeproto.OpcodeUnmount:
		return nil, nativeproto.StatusOK, 0, g.currentGeneration()
	case nativeproto.OpcodeLookup:
		body, status, generation := g.handleLookup(ctx, session, hdr.SessionID, body)
		return body, status, 0, generation
	case nativeproto.OpcodeGetAttr:
		body, status, generation := g.handleGetAttr(ctx, session, hdr.SessionID, body)
		return body, status, 0, generation
	case nativeproto.OpcodeReadDir:
		body, status, generation := g.handleReadDir(ctx, session, hdr.SessionID, body)
		return body, status, 0, generation
	case nativeproto.OpcodeStatFS:
		body, status, generation := g.handleStatFS(ctx, session, hdr.SessionID)
		return body, status, 0, generation
	case nativeproto.OpcodeOpenRead:
		body, status, generation := g.handleOpenRead(ctx, session, hdr.SessionID, body)
		return body, status, 0, generation
	case nativeproto.OpcodeRead:
		body, status, generation := g.handleRead(ctx, session, hdr.SessionID, body)
		return body, status, 0, generation
	case nativeproto.OpcodeClose:
		body, status, generation := g.handleClose(session, hdr.SessionID, body)
		return body, status, 0, generation
	case nativeproto.OpcodePing:
		return nil, nativeproto.StatusOK, 0, g.currentGeneration()
	default:
		return nil, nativeproto.StatusUnsupported, 0, g.currentGeneration()
	}
}

func (g *NativeGateway) handleHello(body []byte) ([]byte, uint32, uint64) {
	req, err := nativeproto.DecodeHelloRequest(body)
	if err != nil {
		return nil, nativeproto.StatusInvalidRequest, g.currentGeneration()
	}
	if req.MinVersion > nativeproto.Version1 || req.MaxVersion < nativeproto.Version1 {
		return nil, nativeproto.StatusUnsupported, g.currentGeneration()
	}

	resp := nativeproto.HelloResponse{
		SelectedVersion: nativeproto.Version1,
		ServerCaps:      nativeproto.CapabilityRouteTTLs | nativeproto.CapabilityStatFS,
		MaxFrameBytes:   nativeproto.MaxFrameBytes,
		MaxReadBytes:    nativeproto.MaxReadBytes,
	}
	return nativeproto.EncodeHelloResponse(resp), nativeproto.StatusOK, g.currentGeneration()
}

func (g *NativeGateway) handleMount(ctx context.Context, body []byte) ([]byte, uint32, uint64, uint64) {
	_, err := nativeproto.DecodeMountRequest(body)
	if err != nil {
		return nil, nativeproto.StatusInvalidRequest, 0, g.currentGeneration()
	}

	info, err := g.router.NativeMountInfo(ctx)
	if err != nil {
		return nil, mapErrorStatus(err), 0, g.currentGeneration()
	}

	resp := nativeproto.MountResponse{
		ClusterVersion:      uint64(info.ClusterVersion),
		NamespaceGeneration: uint64(info.NamespaceGeneration),
		GuardianVisible:     info.GuardianVisible,
		RootObjectID:        pathObjectID(""),
		Root:                attrFromGetAttr(info.Root),
		EntryTTLMS:          durationMS(info.TTLs.EntryTTL),
		AttrTTLMS:           durationMS(info.TTLs.AttrTTL),
		DirTTLMS:            durationMS(info.TTLs.DirTTL),
		RouteTTLMS:          durationMS(info.TTLs.RouteTTL),
	}

	sessionID := g.nextSession.Add(1)
	return nativeproto.EncodeMountResponse(resp), nativeproto.StatusOK, sessionID, uint64(info.NamespaceGeneration)
}

func (g *NativeGateway) handleLookup(ctx context.Context, session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64) {
	if session == nil || session.id != sessionID {
		return nil, nativeproto.StatusStaleNamespace, g.currentGeneration()
	}

	req, err := nativeproto.DecodeLookupRequest(body)
	if err != nil {
		return nil, nativeproto.StatusInvalidRequest, g.currentGeneration()
	}
	if req.Name == "" || strings.Contains(req.Name, "/") {
		return nil, nativeproto.StatusInvalidRequest, g.currentGeneration()
	}

	parentPath, ok := session.pathForObject(req.ParentObjectID)
	if !ok {
		return nil, nativeproto.StatusStaleNamespace, g.currentGeneration()
	}

	path := req.Name
	if parentPath != "" {
		path = parentPath + "/" + req.Name
	}

	lookup, generation, err := g.router.NativeLookup(ctx, path)
	if err != nil {
		return nil, mapErrorStatus(err), uint64(generation)
	}

	resp := nativeproto.LookupResponse{
		Found:      lookup.GetFound(),
		EntryTTLMS: durationMS(DefaultNativeTTLConfig().EntryTTL),
	}
	if lookup.GetFound() {
		objectID := session.bindPath(path)
		resp.ObjectID = objectID
		attr, _, attrErr := g.router.NativeGetAttr(ctx, path)
		if attrErr == nil && attr.GetFound() {
			resp.Attr = attrFromGetAttr(attr)
		} else {
			resp.Attr = attrFromLookup(lookup)
		}
	}

	return nativeproto.EncodeLookupResponse(resp), nativeproto.StatusOK, uint64(generation)
}

func (g *NativeGateway) handleGetAttr(ctx context.Context, session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64) {
	if session == nil || session.id != sessionID {
		return nil, nativeproto.StatusStaleNamespace, g.currentGeneration()
	}

	req, err := nativeproto.DecodeGetAttrRequest(body)
	if err != nil {
		return nil, nativeproto.StatusInvalidRequest, g.currentGeneration()
	}

	path, ok := session.pathForObject(req.ObjectID)
	if !ok {
		return nil, nativeproto.StatusStaleNamespace, g.currentGeneration()
	}

	attr, generation, err := g.router.NativeGetAttr(ctx, path)
	if err != nil {
		return nil, mapErrorStatus(err), uint64(generation)
	}

	resp := nativeproto.GetAttrResponse{
		Found:     attr.GetFound(),
		AttrTTLMS: durationMS(DefaultNativeTTLConfig().AttrTTL),
	}
	if attr.GetFound() {
		resp.Attr = attrFromGetAttr(attr)
	}

	return nativeproto.EncodeGetAttrResponse(resp), nativeproto.StatusOK, uint64(generation)
}

func (g *NativeGateway) handleReadDir(ctx context.Context, session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64) {
	if session == nil || session.id != sessionID {
		return nil, nativeproto.StatusStaleNamespace, g.currentGeneration()
	}

	req, err := nativeproto.DecodeReadDirRequest(body)
	if err != nil {
		return nil, nativeproto.StatusInvalidRequest, g.currentGeneration()
	}

	path, ok := session.pathForObject(req.DirObjectID)
	if !ok {
		return nil, nativeproto.StatusStaleNamespace, g.currentGeneration()
	}

	entries, generation, err := g.router.NativeReadDir(ctx, path)
	if err != nil {
		return nil, mapErrorStatus(err), uint64(generation)
	}

	start := int(req.Cookie)
	if start < 0 || start > len(entries) {
		start = len(entries)
	}
	limit := len(entries) - start
	if req.MaxEntries > 0 && int(req.MaxEntries) < limit {
		limit = int(req.MaxEntries)
	}

	outEntries := make([]nativeproto.DirEntry, 0, limit)
	for _, entry := range entries[start : start+limit] {
		childPath := entry.GetName()
		if path != "" {
			childPath = path + "/" + entry.GetName()
		}
		outEntries = append(outEntries, nativeproto.DirEntry{
			Name:     entry.GetName(),
			ObjectID: session.bindPath(childPath),
			Ino:      entry.GetIno(),
			Mode:     entry.GetMode(),
		})
	}

	nextCookie := uint64(start + len(outEntries))
	resp := nativeproto.ReadDirResponse{
		DirTTLMS:   durationMS(DefaultNativeTTLConfig().DirTTL),
		NextCookie: nextCookie,
		EOF:        nextCookie >= uint64(len(entries)),
		Entries:    outEntries,
	}

	return nativeproto.EncodeReadDirResponse(resp), nativeproto.StatusOK, uint64(generation)
}

func (g *NativeGateway) handleStatFS(ctx context.Context, session *nativeGatewaySession, sessionID uint64) ([]byte, uint32, uint64) {
	if session == nil || session.id != sessionID {
		return nil, nativeproto.StatusStaleNamespace, g.currentGeneration()
	}

	statfs, err := g.router.NativeStatFS(ctx)
	if err != nil {
		return nil, mapErrorStatus(err), g.currentGeneration()
	}

	resp := nativeproto.StatFSResponse{
		Blocks:  statfs.Blocks,
		Bfree:   statfs.Bfree,
		Bavail:  statfs.Bavail,
		Files:   statfs.Files,
		Ffree:   statfs.Ffree,
		Bsize:   statfs.Bsize,
		Frsize:  statfs.Frsize,
		NameLen: statfs.NameLen,
	}
	return nativeproto.EncodeStatFSResponse(resp), nativeproto.StatusOK, uint64(statfs.NamespaceGeneration)
}

func (g *NativeGateway) handleOpenRead(ctx context.Context, session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64) {
	generation := g.currentGeneration()

	if session == nil || session.id != sessionID {
		return nil, nativeproto.StatusStaleNamespace, generation
	}

	req, err := nativeproto.DecodeOpenReadRequest(body)
	if err != nil {
		return nil, nativeproto.StatusInvalidRequest, generation
	}

	path, ok := session.pathForObject(req.ObjectID)
	if !ok {
		return nil, nativeproto.StatusStaleNamespace, generation
	}

	attr, nativeGeneration, err := g.router.NativeGetAttr(ctx, path)
	generation = uint64(nativeGeneration)
	if err != nil {
		return nil, mapErrorStatus(err), generation
	}
	if !attr.GetFound() {
		return nil, nativeproto.StatusNotFound, generation
	}
	if attr.GetMode()&uint32(syscall.S_IFMT) == uint32(syscall.S_IFDIR) {
		return nil, nativeproto.StatusIsDir, generation
	}

	handleID := session.bindHandle(path)
	resp := nativeproto.OpenReadResponse{
		HandleID:   handleID,
		Attr:       attrFromGetAttr(attr),
		RouteTTLMS: durationMS(DefaultNativeTTLConfig().RouteTTL),
	}
	return nativeproto.EncodeOpenReadResponse(resp), nativeproto.StatusOK, generation
}

func (g *NativeGateway) handleRead(ctx context.Context, session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64) {
	generation := g.currentGeneration()

	if session == nil || session.id != sessionID {
		return nil, nativeproto.StatusStaleNamespace, generation
	}

	req, err := nativeproto.DecodeReadRequest(body)
	if err != nil {
		return nil, nativeproto.StatusInvalidRequest, generation
	}

	path, ok := session.pathForHandle(req.HandleID)
	if !ok {
		return nil, nativeproto.StatusStaleNamespace, generation
	}

	data, err := g.router.NativeRead(ctx, path, int64(req.Offset), int64(req.Length))
	if err != nil {
		return nil, mapErrorStatus(err), generation
	}

	resp := nativeproto.ReadResponse{
		EOF:  req.Length == 0 || len(data) < int(req.Length),
		Data: data,
	}
	return nativeproto.EncodeReadResponse(resp), nativeproto.StatusOK, generation
}

func (g *NativeGateway) handleClose(session *nativeGatewaySession, sessionID uint64, body []byte) ([]byte, uint32, uint64) {
	if session == nil || session.id != sessionID {
		return nil, nativeproto.StatusStaleNamespace, g.currentGeneration()
	}

	req, err := nativeproto.DecodeCloseRequest(body)
	if err != nil {
		return nil, nativeproto.StatusInvalidRequest, g.currentGeneration()
	}
	session.releaseHandle(req.HandleID)
	return nil, nativeproto.StatusOK, g.currentGeneration()
}

func (g *NativeGateway) newSession(id uint64) *nativeGatewaySession {
	return &nativeGatewaySession{
		id:           id,
		pathByObject: make(map[nativeproto.ObjectID]string),
		objectByPath: make(map[string]nativeproto.ObjectID),
		pathByHandle: make(map[uint64]string),
	}
}

func (g *NativeGateway) populateMountSession(session *nativeGatewaySession, body []byte) error {
	resp, err := nativeproto.DecodeMountResponse(body)
	if err != nil {
		return err
	}
	session.pathByObject[resp.RootObjectID] = ""
	session.objectByPath[""] = resp.RootObjectID
	return nil
}

func (s *nativeGatewaySession) bindPath(path string) nativeproto.ObjectID {
	if objectID, exists := s.objectByPath[path]; exists {
		return objectID
	}

	objectID := pathObjectID(path)
	s.objectByPath[path] = objectID
	s.pathByObject[objectID] = path
	return objectID
}

func (s *nativeGatewaySession) pathForObject(objectID nativeproto.ObjectID) (string, bool) {
	path, ok := s.pathByObject[objectID]
	return path, ok
}

func (s *nativeGatewaySession) bindHandle(path string) uint64 {
	s.nextHandle++
	s.pathByHandle[s.nextHandle] = path
	return s.nextHandle
}

func (s *nativeGatewaySession) pathForHandle(handleID uint64) (string, bool) {
	path, ok := s.pathByHandle[handleID]
	return path, ok
}

func (s *nativeGatewaySession) releaseHandle(handleID uint64) {
	delete(s.pathByHandle, handleID)
}

func pathObjectID(path string) nativeproto.ObjectID {
	sum := sha256.Sum256([]byte("monofs-native-path:" + path))
	var id nativeproto.ObjectID
	copy(id[:], sum[:len(id)])
	return id
}

func attrFromLookup(resp *pb.LookupResponse) nativeproto.Attr {
	return nativeproto.Attr{
		Ino:   resp.GetIno(),
		Mode:  resp.GetMode(),
		Size:  resp.GetSize(),
		Mtime: resp.GetMtime(),
	}
}

func attrFromGetAttr(resp *pb.GetAttrResponse) nativeproto.Attr {
	return nativeproto.Attr{
		Ino:   resp.GetIno(),
		Mode:  resp.GetMode(),
		Size:  resp.GetSize(),
		Mtime: resp.GetMtime(),
		Atime: resp.GetAtime(),
		Ctime: resp.GetCtime(),
		Nlink: resp.GetNlink(),
		UID:   resp.GetUid(),
		GID:   resp.GetGid(),
	}
}

func durationMS(d time.Duration) uint32 {
	return uint32(d / time.Millisecond)
}

func mapErrorStatus(err error) uint32 {
	if err == nil {
		return nativeproto.StatusOK
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nativeproto.StatusCancelled
	default:
		return nativeproto.StatusUnavailable
	}
}
