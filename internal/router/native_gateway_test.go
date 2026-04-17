package router

import (
	"log/slog"
	"net"
	"syscall"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/nativeproto"
)

func TestNativeGatewayHelloMountLookupReadDirAndStatFS(t *testing.T) {
	client, cleanup := newNativeNamespaceTestClient(t, &nativeNamespaceTestNodeServer{
		lookup: map[string]*pb.LookupResponse{
			"github.com": {
				Found: true,
				Ino:   10,
				Mode:  0o755 | uint32(syscall.S_IFDIR),
			},
		},
		attr: map[string]*pb.GetAttrResponse{
			"github.com": {
				Found: true,
				Ino:   10,
				Mode:  0o755 | uint32(syscall.S_IFDIR),
				Nlink: 2,
			},
		},
		readdir: map[string][]*pb.DirEntry{
			"": {
				{Name: "github.com", Mode: 0o755 | uint32(syscall.S_IFDIR), Ino: 10},
			},
		},
	})
	defer cleanup()

	router := NewRouter(DefaultRouterConfig(), nil)
	defer router.Close()
	router.nodes["node-1"] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:         "node-1",
			Address:        "bufnet",
			Healthy:        true,
			Weight:         1,
			DiskTotalBytes: 8 * nativeNamespaceBlockSize,
			DiskFreeBytes:  4 * nativeNamespaceBlockSize,
			TotalFiles:     1,
		},
		client: client,
		status: NodeActive,
	}

	gateway := NewNativeGateway(router, slog.Default())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer listener.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- gateway.Serve(listener)
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial() error = %v", err)
	}
	defer conn.Close()

	helloReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeHello,
		RequestID: 1,
	}, nativeproto.EncodeHelloRequest(nativeproto.HelloRequest{
		MinVersion: nativeproto.Version1,
		MaxVersion: nativeproto.Version1,
		ClientKind: "conformance",
	}))
	if helloReply.Status != nativeproto.StatusOK {
		t.Fatalf("hello status = %d", helloReply.Status)
	}

	mountReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeMount,
		RequestID: 2,
	}, nativeproto.EncodeMountRequest(nativeproto.MountRequest{
		ClientID:   "native-test",
		Hostname:   "localhost",
		MountFlags: nativeproto.MountFlagReadOnly | nativeproto.MountFlagOverlayWrites,
	}))
	if mountReply.Status != nativeproto.StatusOK {
		t.Fatalf("mount status = %d", mountReply.Status)
	}

	mountResp, err := nativeproto.DecodeMountResponse(mountReply.Body)
	if err != nil {
		t.Fatalf("DecodeMountResponse() error = %v", err)
	}
	if mountReply.SessionID == 0 {
		t.Fatal("expected non-zero session id in mount reply header")
	}
	if got, want := mountResp.NamespaceGeneration, router.nativeEffectiveGeneration(); got != want {
		t.Fatalf("mount namespace generation = %d, want %d", got, want)
	}

	readdirReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeReadDir,
		RequestID: 3,
		SessionID: mountReply.SessionID,
	}, nativeproto.EncodeReadDirRequest(nativeproto.ReadDirRequest{
		DirObjectID: mountResp.RootObjectID,
		MaxEntries:  16,
	}))
	if readdirReply.Status != nativeproto.StatusOK {
		t.Fatalf("readdir status = %d", readdirReply.Status)
	}
	readdirResp, err := nativeproto.DecodeReadDirResponse(readdirReply.Body)
	if err != nil {
		t.Fatalf("DecodeReadDirResponse() error = %v", err)
	}
	if len(readdirResp.Entries) != 1 || readdirResp.Entries[0].Name != "github.com" {
		t.Fatalf("readdir entries = %+v", readdirResp.Entries)
	}

	lookupReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeLookup,
		RequestID: 4,
		SessionID: mountReply.SessionID,
	}, nativeproto.EncodeLookupRequest(nativeproto.LookupRequest{
		ParentObjectID: mountResp.RootObjectID,
		Name:           "github.com",
	}))
	if lookupReply.Status != nativeproto.StatusOK {
		t.Fatalf("lookup status = %d", lookupReply.Status)
	}
	lookupResp, err := nativeproto.DecodeLookupResponse(lookupReply.Body)
	if err != nil {
		t.Fatalf("DecodeLookupResponse() error = %v", err)
	}
	if !lookupResp.Found || lookupResp.Attr.Ino != 10 {
		t.Fatalf("lookup response = %+v", lookupResp)
	}

	getattrReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeGetAttr,
		RequestID: 5,
		SessionID: mountReply.SessionID,
	}, nativeproto.EncodeGetAttrRequest(nativeproto.GetAttrRequest{
		ObjectID: lookupResp.ObjectID,
	}))
	if getattrReply.Status != nativeproto.StatusOK {
		t.Fatalf("getattr status = %d", getattrReply.Status)
	}

	statfsReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeStatFS,
		RequestID: 6,
		SessionID: mountReply.SessionID,
	}, nil)
	if statfsReply.Status != nativeproto.StatusOK {
		t.Fatalf("statfs status = %d", statfsReply.Status)
	}
	statfsResp, err := nativeproto.DecodeStatFSResponse(statfsReply.Body)
	if err != nil {
		t.Fatalf("DecodeStatFSResponse() error = %v", err)
	}
	if statfsResp.Blocks != 8 {
		t.Fatalf("statfs blocks = %d, want 8", statfsResp.Blocks)
	}

	router.bumpNativeNamespaceGeneration("test")
	pingReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodePing,
		RequestID: 7,
		SessionID: mountReply.SessionID,
	}, nil)
	if pingReply.Status != nativeproto.StatusOK {
		t.Fatalf("ping status = %d", pingReply.Status)
	}
	if got, want := pingReply.Generation, router.nativeEffectiveGeneration(); got != want {
		t.Fatalf("ping generation = %d, want %d", got, want)
	}

	_ = listener.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("gateway serve error = %v", err)
	}
}

func TestNativeGatewayOpenReadReadAndClose(t *testing.T) {
	client, cleanup := newNativeNamespaceTestClient(t, &nativeNamespaceTestNodeServer{
		lookup: map[string]*pb.LookupResponse{
			"README.md": {
				Found: true,
				Ino:   42,
				Mode:  0o644 | uint32(syscall.S_IFREG),
				Size:  11,
			},
		},
		attr: map[string]*pb.GetAttrResponse{
			"README.md": {
				Found: true,
				Ino:   42,
				Mode:  0o644 | uint32(syscall.S_IFREG),
				Size:  11,
				Nlink: 1,
			},
		},
		readdir: map[string][]*pb.DirEntry{
			"": {
				{Name: "README.md", Mode: 0o644 | uint32(syscall.S_IFREG), Ino: 42},
			},
		},
		read: map[string][]byte{
			"README.md": []byte("hello world"),
		},
	})
	defer cleanup()

	router := NewRouter(DefaultRouterConfig(), nil)
	defer router.Close()
	router.nodes["node-1"] = &nodeState{
		info: &pb.NodeInfo{
			NodeId:         "node-1",
			Address:        "bufnet",
			Healthy:        true,
			Weight:         1,
			DiskTotalBytes: 8 * nativeNamespaceBlockSize,
			DiskFreeBytes:  4 * nativeNamespaceBlockSize,
			TotalFiles:     1,
		},
		client: client,
		status: NodeActive,
	}

	gateway := NewNativeGateway(router, slog.Default())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer listener.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- gateway.Serve(listener)
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial() error = %v", err)
	}
	defer conn.Close()

	_ = roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeHello,
		RequestID: 1,
	}, nativeproto.EncodeHelloRequest(nativeproto.HelloRequest{
		MinVersion: nativeproto.Version1,
		MaxVersion: nativeproto.Version1,
		ClientKind: "conformance",
	}))

	mountReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeMount,
		RequestID: 2,
	}, nativeproto.EncodeMountRequest(nativeproto.MountRequest{
		ClientID:   "native-test",
		Hostname:   "localhost",
		MountFlags: nativeproto.MountFlagReadOnly,
	}))
	mountResp, err := nativeproto.DecodeMountResponse(mountReply.Body)
	if err != nil {
		t.Fatalf("DecodeMountResponse() error = %v", err)
	}

	lookupReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeLookup,
		RequestID: 3,
		SessionID: mountReply.SessionID,
	}, nativeproto.EncodeLookupRequest(nativeproto.LookupRequest{
		ParentObjectID: mountResp.RootObjectID,
		Name:           "README.md",
	}))
	if lookupReply.Status != nativeproto.StatusOK {
		t.Fatalf("lookup status = %d", lookupReply.Status)
	}
	lookupResp, err := nativeproto.DecodeLookupResponse(lookupReply.Body)
	if err != nil {
		t.Fatalf("DecodeLookupResponse() error = %v", err)
	}

	openReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeOpenRead,
		RequestID: 4,
		SessionID: mountReply.SessionID,
	}, nativeproto.EncodeOpenReadRequest(nativeproto.OpenReadRequest{
		ObjectID: lookupResp.ObjectID,
	}))
	if openReply.Status != nativeproto.StatusOK {
		t.Fatalf("open status = %d", openReply.Status)
	}
	openResp, err := nativeproto.DecodeOpenReadResponse(openReply.Body)
	if err != nil {
		t.Fatalf("DecodeOpenReadResponse() error = %v", err)
	}

	readReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeRead,
		RequestID: 5,
		SessionID: mountReply.SessionID,
	}, nativeproto.EncodeReadRequest(nativeproto.ReadRequest{
		HandleID: openResp.HandleID,
		Offset:   0,
		Length:   5,
	}))
	if readReply.Status != nativeproto.StatusOK {
		t.Fatalf("read status = %d", readReply.Status)
	}
	readResp, err := nativeproto.DecodeReadResponse(readReply.Body)
	if err != nil {
		t.Fatalf("DecodeReadResponse() error = %v", err)
	}
	if string(readResp.Data) != "hello" || readResp.EOF {
		t.Fatalf("read response = %+v", readResp)
	}

	closeReply := roundTripNativeFrame(t, conn, nativeproto.Header{
		Opcode:    nativeproto.OpcodeClose,
		RequestID: 6,
		SessionID: mountReply.SessionID,
	}, nativeproto.EncodeCloseRequest(nativeproto.CloseRequest{
		HandleID: openResp.HandleID,
	}))
	if closeReply.Status != nativeproto.StatusOK {
		t.Fatalf("close status = %d", closeReply.Status)
	}

	_ = listener.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("gateway serve error = %v", err)
	}
}

type nativeReply struct {
	nativeproto.Header
	Body []byte
}

func roundTripNativeFrame(t *testing.T, conn net.Conn, hdr nativeproto.Header, body []byte) nativeReply {
	t.Helper()

	if err := nativeproto.WriteFrame(conn, hdr, body); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}
	replyHdr, replyBody, err := nativeproto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	return nativeReply{
		Header: replyHdr,
		Body:   replyBody,
	}
}
