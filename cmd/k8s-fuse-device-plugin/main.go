package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	resourceName = "sretoolhub.com/fuse"
	serverSock   = pluginapi.DevicePluginPath + "fuse.sock"
)

type FuseDevicePlugin struct {
	devs   []*pluginapi.Device
	socket string
	stop   chan struct{}
	health chan *pluginapi.Device
	server *grpc.Server
}

func NewFuseDevicePlugin(number int) *FuseDevicePlugin {
	return &FuseDevicePlugin{
		devs:   createDevices(number),
		socket: serverSock,
		stop:   make(chan struct{}),
		health: make(chan *pluginapi.Device),
	}
}

func (m *FuseDevicePlugin) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

func (m *FuseDevicePlugin) PreStartContainer(_ context.Context, _ *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (m *FuseDevicePlugin) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

func (m *FuseDevicePlugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.devs}); err != nil {
		return err
	}
	for {
		select {
		case <-m.stop:
			return nil
		case d := <-m.health:
			d.Health = pluginapi.Unhealthy
			if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: m.devs}); err != nil {
				return err
			}
		}
	}
}

func (m *FuseDevicePlugin) Allocate(_ context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	devs := m.devs
	responses := &pluginapi.AllocateResponse{}

	for _, req := range reqs.ContainerRequests {
		for _, id := range req.DevicesIDs {
			if !deviceExists(devs, id) {
				return nil, fmt.Errorf("invalid allocation request: unknown device: %s", id)
			}
			responses.ContainerResponses = append(responses.ContainerResponses, &pluginapi.ContainerAllocateResponse{
				Devices: []*pluginapi.DeviceSpec{
					{
						HostPath:      "/dev/fuse",
						ContainerPath: "/dev/fuse",
						Permissions:   "rwm",
					},
				},
			})
		}
	}
	return responses, nil
}

func (m *FuseDevicePlugin) Start() error {
	if err := m.cleanup(); err != nil {
		return err
	}
	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", m.socket, err)
	}
	m.server = grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(m.server, m)
	go func() {
		if err := m.server.Serve(sock); err != nil {
			log.Printf("gRPC server stopped: %v", err)
		}
	}()
	conn, err := dial(m.socket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial health check: %w", err)
	}
	conn.Close()
	return nil
}

func (m *FuseDevicePlugin) Stop() error {
	if m.server == nil {
		return nil
	}
	m.server.GracefulStop()
	m.server = nil
	close(m.stop)
	return m.cleanup()
}

func (m *FuseDevicePlugin) Register(kubeletEndpoint string) error {
	conn, err := dial(kubeletEndpoint, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial kubelet: %w", err)
	}
	defer conn.Close()
	client := pluginapi.NewRegistrationClient(conn)
	req := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: resourceName,
	}
	if _, err := client.Register(context.Background(), req); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	return nil
}

func (m *FuseDevicePlugin) Serve() error {
	if err := m.Start(); err != nil {
		log.Printf("Could not start device plugin: %v", err)
		return err
	}
	log.Printf("Serving on %s", m.socket)
	if err := m.Register(pluginapi.KubeletSocket); err != nil {
		log.Printf("Could not register device plugin: %v", err)
		m.Stop()
		return err
	}
	log.Printf("Registered device plugin %q with Kubelet", resourceName)
	return nil
}

func (m *FuseDevicePlugin) cleanup() error {
	if err := os.Remove(m.socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove socket %s: %w", m.socket, err)
	}
	return nil
}

func dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	return grpc.Dial(unixSocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithTimeout(timeout),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", addr)
		}),
	)
}

func createDevices(number int) []*pluginapi.Device {
	hostname, _ := os.Hostname()
	devs := make([]*pluginapi.Device, 0, number)
	for i := 0; i < number; i++ {
		devs = append(devs, &pluginapi.Device{
			ID:     fmt.Sprintf("fuse-%s-%d", hostname, i),
			Health: pluginapi.Healthy,
		})
	}
	return devs
}

func deviceExists(devs []*pluginapi.Device, id string) bool {
	for _, d := range devs {
		if d.ID == id {
			return true
		}
	}
	return false
}

func main() {
	mountsAllowed := flag.Int("mounts-allowed", 100, "maximum number of concurrent FUSE mounts")
	flag.Parse()

	log.Println("Starting sretoolhub.com/fuse device plugin")
	log.Printf("Advertising %d FUSE devices", *mountsAllowed)

	plugin := NewFuseDevicePlugin(*mountsAllowed)
	if err := plugin.Serve(); err != nil {
		log.Printf("Failed to start: %v", err)
	}
	select {}
}
