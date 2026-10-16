package node

import (
	"context"
	"net"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Version is reported to kubelet.
var Version = "v2-dev"

// csiServer adapts the daemon to the CSI Identity and Node services. Only
// inline ephemeral volumes are supported, so there is no controller
// service and no staging.
type csiServer struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer
	d *Daemon
}

// ServeCSI serves CSI on a unix socket until ctx ends.
func (d *Daemon) ServeCSI(ctx context.Context, socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o750); err != nil {
		return err
	}
	os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	srv := grpc.NewServer()
	cs := &csiServer{d: d}
	csi.RegisterIdentityServer(srv, cs)
	csi.RegisterNodeServer(srv, cs)
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	return srv.Serve(ln)
}

func (s *csiServer) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: c4v1.DriverName, VendorVersion: Version}, nil
}

func (s *csiServer) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

func (s *csiServer) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}

func (s *csiServer) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: s.d.Cfg.NodeName}, nil
}

func (s *csiServer) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (s *csiServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and target path are required")
	}
	err := s.d.Publish(ctx, PublishRequest{
		VolumeID: req.GetVolumeId(),
		Target:   req.GetTargetPath(),
		ReadOnly: req.GetReadonly(),
		Context:  req.GetVolumeContext(),
	})
	if err != nil {
		s.d.Log.Error(err, "publish", "volume", req.GetVolumeId(), "pod", req.GetVolumeContext()["csi.storage.k8s.io/pod.namespace"]+"/"+req.GetVolumeContext()["csi.storage.k8s.io/pod.name"])
		if ctx.Err() != nil {
			return nil, status.Error(codes.DeadlineExceeded, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *csiServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and target path are required")
	}
	if err := s.d.Unpublish(ctx, req.GetVolumeId(), req.GetTargetPath()); err != nil {
		s.d.Log.Error(err, "unpublish", "volume", req.GetVolumeId())
		if ctx.Err() != nil {
			return nil, status.Error(codes.DeadlineExceeded, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}
