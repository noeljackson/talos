// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provision provides abstract definitions for Talos cluster provisioners.
package provision

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/bundle"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
)

// Provisioner is an interface each provisioner should implement.
//
//nolint:interfacebloat
type Provisioner interface {
	Create(context.Context, ClusterRequest, ...Option) (Cluster, error)
	Destroy(context.Context, Cluster, ...Option) error

	Reflect(ctx context.Context, clusterName, stateDirectory string) (Cluster, error)

	GenOptions(NetworkRequest, *config.VersionContract) ([]generate.Option, []bundle.Option)

	GetInClusterKubernetesControlPlaneEndpoint(req NetworkRequest, controlPlanePort int) string
	GetExternalKubernetesControlPlaneEndpoint(req NetworkRequest, controlPlanePort int) string
	GetTalosAPIEndpoints(NetworkRequest) []string

	GetFirstInterface() v1alpha1.IfaceSelector
	GetFirstInterfaceName() string

	Close() error

	UserDiskName(index int) string
}

// RebootProvisioner is an optional interface implemented by provisioners that support
// forcefully rebooting individual cluster nodes.
type RebootProvisioner interface {
	// RebootNode forcefully reboots a single cluster node.
	RebootNode(ctx context.Context, cluster Cluster, node NodeInfo) error
}

// DiskLayout selects an ordered subset of a node's original disks by serial.
// It never accepts backing-file paths. DiskBootOnly forbids installation media,
// direct kernel boot, config-media injection, and firmware-variable resets.
type DiskLayout struct {
	Serials      []string `json:"serials"`
	DiskBootOnly bool     `json:"diskBootOnly"`
}

// DiskBootState records a successful QEMU process start, checked against the
// live process command line before it is returned by the provisioner.
type DiskBootState struct {
	Generation string   `json:"generation"`
	ProcessID  int      `json:"processID"`
	Arguments  []string `json:"arguments"`
}

// DiskLayoutProvisioner is an opt-in native VM testing capability. Changes take
// effect at a cold reboot, not by online hot-unplugging the running guest.
type DiskLayoutProvisioner interface {
	RebootNodeWithDisks(context.Context, Cluster, NodeInfo, DiskLayout) (string, error)
	NodeDiskBootState(context.Context, Cluster, NodeInfo) (DiskBootState, error)
}

const (
	// HTTPProbeDefaultTimeout is the default provisioner-side HTTP probe timeout.
	HTTPProbeDefaultTimeout = 5 * time.Second
	// HTTPProbeMinTimeout is the minimum provisioner-side HTTP probe timeout.
	HTTPProbeMinTimeout = 100 * time.Millisecond
	// HTTPProbeMaxTimeout is the maximum provisioner-side HTTP probe timeout.
	HTTPProbeMaxTimeout = 30 * time.Second
	// HTTPProbeMaxResponseBody is the maximum response body returned by a provisioner-side HTTP probe.
	HTTPProbeMaxResponseBody = 64 * 1024
)

// HTTPProbeRequest describes a bounded HTTP request originating in the provisioner network namespace.
type HTTPProbeRequest struct {
	IP      netip.Addr
	Port    uint16
	Path    string
	Timeout time.Duration
}

// Normalize validates an HTTP probe request and applies bounded defaults.
func (request HTTPProbeRequest) Normalize() (HTTPProbeRequest, error) {
	if !request.IP.IsValid() {
		return HTTPProbeRequest{}, fmt.Errorf("HTTP probe IP is required")
	}

	if request.Port == 0 {
		return HTTPProbeRequest{}, fmt.Errorf("HTTP probe port is required")
	}

	if !strings.HasPrefix(request.Path, "/") {
		return HTTPProbeRequest{}, fmt.Errorf("HTTP probe path must start with '/'")
	}

	switch {
	case request.Timeout == 0:
		request.Timeout = HTTPProbeDefaultTimeout
	case request.Timeout < HTTPProbeMinTimeout:
		request.Timeout = HTTPProbeMinTimeout
	case request.Timeout > HTTPProbeMaxTimeout:
		request.Timeout = HTTPProbeMaxTimeout
	}

	return request, nil
}

// HTTPProbeResponse is the bounded result of a provisioner-side HTTP probe.
// Failure is reserved for HTTP transport failures; HTTP status failures are returned as StatusCode.
type HTTPProbeResponse struct {
	StatusCode int
	Body       []byte
	Failure    string
}

// HTTPProbeProvisioner is implemented by provisioners which can probe from their external network namespace.
type HTTPProbeProvisioner interface {
	ProbeHTTP(ctx context.Context, cluster Cluster, request HTTPProbeRequest) (HTTPProbeResponse, error)
}
