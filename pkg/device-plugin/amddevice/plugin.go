/*
Copyright 2024 The HAMi Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package amddevice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	kubeletdevicepluginv1beta1 "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device/amd"
	"github.com/Project-HAMi/HAMi/pkg/util"
	"github.com/Project-HAMi/HAMi/pkg/util/nodelock"
)

const (
	NodeLockAMD       = "hami.io/mutex.lock"
	amdSocketPrefix   = "hami-amd"
	libamvgpuHostPath = "/opt/hami/libamvgpu.so"
)

// AMDDevicePlugin implements the Kubernetes device plugin API for AMD GPUs.
type AMDDevicePlugin struct {
	resourceName string
	deviceCount  int
	socket       string
	logLevel     string

	server      *grpc.Server
	stop        chan struct{}
	deviceCache string // cached encoded device info to avoid redundant annotation patches
}

// NewAMDDevicePlugin creates a new AMD device plugin instance.
func NewAMDDevicePlugin(resourceName string, deviceCount int, logLevel string) *AMDDevicePlugin {
	return &AMDDevicePlugin{
		resourceName: resourceName,
		deviceCount:  deviceCount,
		socket:       kubeletdevicepluginv1beta1.DevicePluginPath + amdSocketPrefix + ".sock",
		logLevel:     logLevel,
		stop:         make(chan struct{}),
	}
}

// Start starts the gRPC server and registers with kubelet.
func (p *AMDDevicePlugin) Start(kubeletSocket string) error {
	if err := p.Serve(); err != nil {
		return fmt.Errorf("failed to start gRPC server: %w", err)
	}
	if err := p.Register(kubeletSocket); err != nil {
		p.Stop()
		return fmt.Errorf("failed to register with kubelet: %w", err)
	}
	klog.InfoS("AMD device plugin started", "resource", p.resourceName, "devices", p.deviceCount)
	return nil
}

// Stop stops the gRPC server.
func (p *AMDDevicePlugin) Stop() error {
	if p == nil || p.server == nil {
		return nil
	}
	klog.Infof("Stopping AMD device plugin for '%s'", p.resourceName)
	close(p.stop)
	p.server.Stop()
	if err := os.Remove(p.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Serve starts the gRPC server.
func (p *AMDDevicePlugin) Serve() error {
	os.Remove(p.socket)
	sock, err := net.Listen("unix", p.socket)
	if err != nil {
		return err
	}

	p.server = grpc.NewServer()
	kubeletdevicepluginv1beta1.RegisterDevicePluginServer(p.server, p)

	go func() {
		klog.Infof("Starting gRPC server for '%s'", p.resourceName)
		if err := p.server.Serve(sock); err != nil {
			klog.Errorf("gRPC server for '%s' crashed: %v", p.resourceName, err)
		}
	}()

	conn, err := p.dial(p.socket, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// Register registers the device plugin with kubelet.
func (p *AMDDevicePlugin) Register(kubeletSocket string) error {
	conn, err := p.dial(kubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := kubeletdevicepluginv1beta1.NewRegistrationClient(conn)
	_, err = client.Register(context.Background(), &kubeletdevicepluginv1beta1.RegisterRequest{
		Version:      kubeletdevicepluginv1beta1.Version,
		Endpoint:     path.Base(p.socket),
		ResourceName: p.resourceName,
		Options:      &kubeletdevicepluginv1beta1.DevicePluginOptions{},
	})
	return err
}

func (p *AMDDevicePlugin) dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return grpc.DialContext(ctx, "unix://"+unixSocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
}

// GetDevicePluginOptions returns options for this plugin.
func (p *AMDDevicePlugin) GetDevicePluginOptions(context.Context, *kubeletdevicepluginv1beta1.Empty) (*kubeletdevicepluginv1beta1.DevicePluginOptions, error) {
	return &kubeletdevicepluginv1beta1.DevicePluginOptions{}, nil
}

// ListAndWatch lists devices and watches for health changes.
func (p *AMDDevicePlugin) ListAndWatch(_ *kubeletdevicepluginv1beta1.Empty, s kubeletdevicepluginv1beta1.DevicePlugin_ListAndWatchServer) error {
	devs := make([]*kubeletdevicepluginv1beta1.Device, p.deviceCount)
	for i := range devs {
		devs[i] = &kubeletdevicepluginv1beta1.Device{
			ID:     fmt.Sprintf("amd-gpu-%d", i),
			Health: kubeletdevicepluginv1beta1.Healthy,
		}
	}
	s.Send(&kubeletdevicepluginv1beta1.ListAndWatchResponse{Devices: devs})
	<-p.stop
	return nil
}

// GetPreferredAllocation is not used for AMD GPUs.
func (p *AMDDevicePlugin) GetPreferredAllocation(context.Context, *kubeletdevicepluginv1beta1.PreferredAllocationRequest) (*kubeletdevicepluginv1beta1.PreferredAllocationResponse, error) {
	return &kubeletdevicepluginv1beta1.PreferredAllocationResponse{}, nil
}

// PreStartContainer is not needed for AMD GPUs.
func (p *AMDDevicePlugin) PreStartContainer(context.Context, *kubeletdevicepluginv1beta1.PreStartContainerRequest) (*kubeletdevicepluginv1beta1.PreStartContainerResponse, error) {
	return &kubeletdevicepluginv1beta1.PreStartContainerResponse{}, nil
}

// Allocate handles device allocation requests from kubelet.
// It reads the scheduler's allocation decisions from pod annotations and injects
// the appropriate environment variables and volume mounts for AMD GPU virtualization.
func (p *AMDDevicePlugin) Allocate(ctx context.Context, reqs *kubeletdevicepluginv1beta1.AllocateRequest) (*kubeletdevicepluginv1beta1.AllocateResponse, error) {
	klog.InfoS("AMD Allocate", "request", reqs)
	responses := kubeletdevicepluginv1beta1.AllocateResponse{}
	nodename := os.Getenv(util.NodeNameEnvName)

	current, err := util.GetPendingPod(ctx, nodename)
	if err != nil {
		return &kubeletdevicepluginv1beta1.AllocateResponse{}, err
	}
	klog.Infof("AMD Allocate pod %s/%s, annotations=%+v", current.Namespace, current.Name, current.Annotations)

	for range reqs.ContainerRequests {
		_, devreq, err := GetNextDeviceRequest(amd.AMDDevice, *current)
		if err != nil {
			podAllocationFailed(nodename, current)
			return &kubeletdevicepluginv1beta1.AllocateResponse{}, err
		}

		response := &kubeletdevicepluginv1beta1.ContainerAllocateResponse{
			Envs:    make(map[string]string),
			Mounts:  []*kubeletdevicepluginv1beta1.Mount{},
			Devices: []*kubeletdevicepluginv1beta1.DeviceSpec{},
		}

		// Inject environment variables for each allocated device
		for i, dev := range devreq {
			// Memory limit per device
			if dev.Usedmem > 0 {
				response.Envs[fmt.Sprintf("HIP_DEVICE_MEMORY_LIMIT_%d", i)] = fmt.Sprintf("%dm", dev.Usedmem)
			}

			// CU mask from CustomInfo (set by scheduler's Fit())
			if dev.CustomInfo != nil {
				if mask, ok := dev.CustomInfo[amd.CUMaskKey]; ok {
					if maskStr, ok := mask.(string); ok {
						response.Envs["ROC_GLOBAL_CU_MASK"] = maskStr
					}
				}
			}
		}

		// LD_AUDIT for memory virtualization via libamvgpu.so
		response.Envs["LD_AUDIT"] = libamvgpuHostPath

		// Log level
		if p.logLevel != "" {
			response.Envs["LIBHIP_LOG_LEVEL"] = p.logLevel
		}

		// Mount libamvgpu.so from host
		if _, err := os.Stat(libamvgpuHostPath); err == nil {
			response.Mounts = append(response.Mounts, &kubeletdevicepluginv1beta1.Mount{
				ContainerPath: libamvgpuHostPath,
				HostPath:      libamvgpuHostPath,
				ReadOnly:      true,
			})
		}

		// Mount GPU device files
		response.Devices = append(response.Devices,
			&kubeletdevicepluginv1beta1.DeviceSpec{
				ContainerPath: "/dev/kfd",
				HostPath:      "/dev/kfd",
				Permissions:   "rw",
			},
			&kubeletdevicepluginv1beta1.DeviceSpec{
				ContainerPath: "/dev/dri",
				HostPath:      "/dev/dri",
				Permissions:   "rw",
			},
		)

		// Erase processed device request from annotation
		if err := EraseNextDeviceTypeFromAnnotation(amd.AMDDevice, *current); err != nil {
			podAllocationFailed(nodename, current)
			return &kubeletdevicepluginv1beta1.AllocateResponse{}, err
		}

		responses.ContainerResponses = append(responses.ContainerResponses, response)
	}

	klog.InfoS("AMD Allocate response", "responses", responses.ContainerResponses)
	podAllocationTrySuccess(nodename, current)
	return &responses, nil
}

// GetNextDeviceRequest decodes the next AMD device request from pod annotations.
func GetNextDeviceRequest(dtype string, p corev1.Pod) (string, device.ContainerDevices, error) {
	pdevices, err := device.DecodePodDevices(device.InRequestDevices, p.Annotations)
	if err != nil {
		return "", device.ContainerDevices{}, err
	}
	pd, ok := pdevices[dtype]
	if !ok {
		return "", device.ContainerDevices{}, errors.New("AMD device request not found in annotations")
	}
	for ctridx, ctrDevice := range pd {
		if len(ctrDevice) > 0 {
			ctrName := ""
			if ctridx < len(p.Spec.Containers) {
				ctrName = p.Spec.Containers[ctridx].Name
			}
			return ctrName, ctrDevice, nil
		}
	}
	return "", device.ContainerDevices{}, errors.New("AMD device request not found")
}

// EraseNextDeviceTypeFromAnnotation removes the processed device request.
func EraseNextDeviceTypeFromAnnotation(dtype string, p corev1.Pod) error {
	pdevices, err := device.DecodePodDevices(device.InRequestDevices, p.Annotations)
	if err != nil {
		return err
	}
	pd, ok := pdevices[dtype]
	if !ok {
		return errors.New("erase AMD device annotation not found")
	}
	res := device.PodSingleDevice{}
	found := false
	for _, val := range pd {
		if found {
			res = append(res, val)
		} else {
			if len(val) > 0 {
				found = true
				res = append(res, device.ContainerDevices{})
			} else {
				res = append(res, val)
			}
		}
	}
	newannos := make(map[string]string)
	newannos[device.InRequestDevices[dtype]] = device.EncodePodSingleDevice(res)
	return util.PatchPodAnnotations(&p, newannos)
}

func podAllocationFailed(nodename string, pod *corev1.Pod) {
	klog.Infof("AMD pod allocation failed for %s/%s", pod.Namespace, pod.Name)
	newAnnos := map[string]string{util.DeviceBindPhase: util.DeviceBindFailed}
	if err := util.PatchPodAnnotations(pod, newAnnos); err != nil {
		klog.Errorf("Failed to patch pod annotations: %v", err)
		return
	}
	if err := nodelock.ReleaseNodeLock(nodename, NodeLockAMD, pod, false); err != nil {
		klog.Errorf("Failed to release node lock: %v", err)
	}
}

func podAllocationTrySuccess(nodename string, pod *corev1.Pod) {
	annos := pod.Annotations[device.InRequestDevices[amd.AMDDevice]]
	for _, val := range device.DevicesToHandle {
		if len(annos) > 0 && annos != val {
			return
		}
	}
	klog.Infof("All AMD devices allocated, releasing lock")
	newAnnos := map[string]string{util.DeviceBindPhase: util.DeviceBindSuccess}
	if err := util.PatchPodAnnotations(pod, newAnnos); err != nil {
		klog.Errorf("Failed to patch pod annotations: %v", err)
		return
	}
	if err := nodelock.ReleaseNodeLock(nodename, NodeLockAMD, pod, false); err != nil {
		klog.Errorf("Failed to release node lock: %v", err)
	}
}
