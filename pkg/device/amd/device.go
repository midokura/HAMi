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

package amd

import (
	"errors"
	"flag"
	"fmt"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device/common"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type AMDDevices struct {
	resourceCountName  string
	resourceMemoryName string
	resourceCoreName   string
	totalCUs           int
	totalMemoryMB      int32
}

const (
	AMDDevice          = "AMDGPU"
	AMDDeviceSelection = "amd.com/gpu-index"
	AMDUseUUID         = "amd.com/use-gpu-uuid"
	AMDNoUseUUID       = "amd.com/nouse-gpu-uuid"
	AMDAssignedNode    = "amd.com/predicate-node"
	RegisterAnnos      = "hami.io/node-amd-register"
)

type AMDConfig struct {
	ResourceCountName  string `yaml:"resourceCountName"`
	ResourceMemoryName string `yaml:"resourceMemoryName"`
	ResourceCoreName   string `yaml:"resourceCoreName"`
	TotalCUs           int    `yaml:"totalCUs"`
	TotalMemoryMB      int32  `yaml:"totalMemoryMB"`
}

func InitAMDGPUDevice(config AMDConfig) *AMDDevices {
	if _, ok := device.SupportDevices[AMDDevice]; !ok {
		device.SupportDevices[AMDDevice] = "hami.io/amd-devices-allocated"
	}
	if _, ok := device.InRequestDevices[AMDDevice]; !ok {
		device.InRequestDevices[AMDDevice] = "hami.io/amd-devices-allocated"
	}
	totalCUs := config.TotalCUs
	if totalCUs <= 0 {
		klog.Warning("AMD GPU: totalCUs not configured. GPU partitioning (gpucores) will be disabled. " +
			"Set totalCUs in scheduler config, or implement rocminfo auto-detection in Device Plugin.")
	}
	totalMemoryMB := config.TotalMemoryMB
	if totalMemoryMB <= 0 {
		klog.Warning("AMD GPU: totalMemoryMB not configured. GPU memory limiting (gpumem) will be disabled. " +
			"Set totalMemoryMB in scheduler config, or implement rocminfo auto-detection in Device Plugin.")
	}
	dev := &AMDDevices{
		resourceCountName:  config.ResourceCountName,
		resourceMemoryName: config.ResourceMemoryName,
		resourceCoreName:   config.ResourceCoreName,
		totalCUs:           totalCUs,
		totalMemoryMB:      totalMemoryMB,
	}
	klog.InfoS("AMD GPU device initialized",
		"totalCUs", totalCUs,
		"totalMemoryMB", totalMemoryMB,
		"resourceCoreName", config.ResourceCoreName)
	return dev
}

func (dev *AMDDevices) CommonWord() string {
	return AMDDevice
}

// ParseConfig registers AMD-specific flags. Currently no flags are needed;
// GPU specs are auto-detected by the device plugin via rocminfo.
func ParseConfig(fs *flag.FlagSet) {
}

func (dev *AMDDevices) MutateAdmission(ctr *corev1.Container, p *corev1.Pod) (bool, error) {
	_, ok := ctr.Resources.Limits[corev1.ResourceName(dev.resourceCountName)]
	if !ok {
		_, ok = ctr.Resources.Limits[corev1.ResourceName(dev.resourceMemoryName)]
	}
	klog.Infoln("MutateAdmission result", ok)
	return ok, nil
}

func (dev *AMDDevices) GetNodeDevices(n corev1.Node) ([]*device.DeviceInfo, error) {
	// First try reading from node annotation written by the device plugin (rocminfo auto-detection).
	devEncoded, ok := n.Annotations[RegisterAnnos]
	if ok && devEncoded != "" {
		nodedevices, err := device.UnMarshalNodeDevices(devEncoded)
		if err != nil {
			klog.ErrorS(err, "failed to decode AMD node devices from annotation", "node", n.Name, "annotation", devEncoded)
			return []*device.DeviceInfo{}, err
		}
		if len(nodedevices) == 0 {
			return []*device.DeviceInfo{}, errors.New("no AMD GPU found in node annotation")
		}
		// Enrich with CustomInfo (CUTotalKey for bitmap partitioning) and DeviceVendor
		for _, nd := range nodedevices {
			nd.DeviceVendor = dev.CommonWord()
			if nd.CustomInfo == nil {
				nd.CustomInfo = make(map[string]any)
			}
			nd.CustomInfo[CUTotalKey] = int(nd.Devcore)
		}
		for _, nd := range nodedevices {
			klog.InfoS("AMD nodedevice from annotation",
				"id", nd.ID, "totalCUs", nd.Devcore, "mem", nd.Devmem,
				"virtualDevices", nd.Count, "type", nd.Type)
		}
		return nodedevices, nil
	}

	// Fallback: annotation not present, use hardcoded config values.
	klog.InfoS("AMD RegisterAnnos not found, falling back to config-based device info", "node", n.Name)
	nodedevices := []*device.DeviceInfo{}

	// The kubelet capacity (amd.com/gpu) reports the number of VIRTUAL devices
	// registered by the device plugin (e.g. 10). This is NOT the number of
	// physical GPUs. For CU bitmap partitioning, we need one DeviceInfo per
	// PHYSICAL GPU, with Count set to the number of virtual devices (pods)
	// that can share it.
	virtualCount, ok := n.Status.Capacity.Name(corev1.ResourceName(dev.resourceCountName), resource.DecimalSI).AsInt64()
	if !ok || virtualCount == 0 {
		return []*device.DeviceInfo{}, fmt.Errorf("device not found %s", dev.resourceCountName)
	}

	// Fallback: assume 1 physical GPU per node with config values.
	physicalGPUs := 1

	// Each physical GPU gets one DeviceInfo with:
	//   Count = virtualCount (max pods that can share this GPU)
	//   A single CU bitmap shared across all pods on this GPU
	for i := range physicalGPUs {
		customInfo := make(map[string]any)
		customInfo[CUTotalKey] = dev.totalCUs
		nodedevices = append(nodedevices, &device.DeviceInfo{
			Index:        uint(i),
			ID:           n.Name + "-" + AMDDevice + "-" + fmt.Sprint(i),
			Count:        int32(virtualCount), // Virtual devices per physical GPU
			Devmem:       dev.totalMemoryMB,
			Devcore:      int32(dev.totalCUs),
			Type:         AMDDevice,
			Numa:         0,
			Health:       true,
			CustomInfo:   customInfo,
			DeviceVendor: AMDDevice,
		})
	}
	for _, nd := range nodedevices {
		klog.InfoS("Registered AMD nodedevice (fallback)",
			"id", nd.ID, "totalCUs", dev.totalCUs, "mem", nd.Devmem,
			"virtualDevices", nd.Count)
	}
	return nodedevices, nil
}

func (dev *AMDDevices) PatchAnnotations(pod *corev1.Pod, annoinput *map[string]string, pd device.PodDevices) map[string]string {
	devlist, ok := pd[AMDDevice]
	if ok && len(devlist) > 0 {
		(*annoinput)[device.SupportDevices[AMDDevice]] = device.EncodePodSingleDevice(devlist)
	}
	klog.V(4).InfoS("annos", "input", (*annoinput))
	return *annoinput
}

// LockNode is a no-op for AMD. CU bitmap allocation is atomic within Fit().
func (dev *AMDDevices) LockNode(n *corev1.Node, p *corev1.Pod) error {
	return nil
}

// ReleaseNodeLock is a no-op for AMD. See LockNode.
func (dev *AMDDevices) ReleaseNodeLock(n *corev1.Node, p *corev1.Pod) error {
	return nil
}

// NodeCleanUp is a no-op for AMD. Device state is managed by the device plugin.
func (dev *AMDDevices) NodeCleanUp(nn string) error {
	return nil
}

func (dev *AMDDevices) checkType(n device.ContainerDeviceRequest) bool {
	return n.Type == AMDDevice
}

// CheckHealth always reports healthy. AMD GPU health is monitored by the device plugin
// via rocminfo, not by the scheduler.
func (dev *AMDDevices) CheckHealth(devType string, n *corev1.Node) (bool, bool) {
	return true, true
}

func (dev *AMDDevices) GetResourceNames() device.ResourceNames {
	return device.ResourceNames{
		ResourceCountName:  dev.resourceCountName,
		ResourceMemoryName: dev.resourceMemoryName,
		ResourceCoreName:   dev.resourceCoreName,
	}
}

func (dev *AMDDevices) GenerateResourceRequests(ctr *corev1.Container) device.ContainerDeviceRequest {
	klog.Info("Start to count AMD devices for container ", ctr.Name)
	amdResourceCount := corev1.ResourceName(dev.resourceCountName)
	v, ok := ctr.Resources.Limits[amdResourceCount]
	if !ok {
		v, ok = ctr.Resources.Requests[amdResourceCount]
	}
	if !ok {
		return device.ContainerDeviceRequest{}
	}
	n, ok := v.AsInt64()
	if !ok {
		return device.ContainerDeviceRequest{}
	}

	// Parse memory request (MB)
	memreq := dev.totalMemoryMB
	if dev.resourceMemoryName != "" {
		if mv, ok := ctr.Resources.Limits[corev1.ResourceName(dev.resourceMemoryName)]; ok {
			if m, ok := mv.AsInt64(); ok {
				memreq = int32(m)
			}
		}
	}

	// Parse CU (core) request - number of CUs, not percentage
	coresreq := int32(0) // 0 means use all available CUs
	if dev.resourceCoreName != "" {
		if cv, ok := ctr.Resources.Limits[corev1.ResourceName(dev.resourceCoreName)]; ok {
			if c, ok := cv.AsInt64(); ok {
				coresreq = int32(c)
			}
		}
	}

	klog.InfoS("Detected AMD device request",
		"container", ctr.Name,
		"deviceCount", n,
		"memreq", memreq,
		"coresreq(CUs)", coresreq)

	return device.ContainerDeviceRequest{
		Nums:             int32(n),
		Type:             AMDDevice,
		Memreq:           memreq,
		MemPercentagereq: 0,
		Coresreq:         coresreq,
	}
}

func (dev *AMDDevices) ScoreNode(node *corev1.Node, podDevices device.PodSingleDevice, previous []*device.DeviceUsage, policy string) float32 {
	return 0
}

func (dev *AMDDevices) AddResourceUsage(pod *corev1.Pod, n *device.DeviceUsage, ctr *device.ContainerDevice) error {
	n.Used++
	n.Usedcores += ctr.Usedcores
	n.Usedmem += ctr.Usedmem

	// Apply CU allocation to device bitmap
	if ctr.CustomInfo != nil {
		if cuStartRaw, ok := ctr.CustomInfo[CUStartKey]; ok {
			if cuCountRaw, ok := ctr.CustomInfo[CUCountKey]; ok {
				if n.CustomInfo == nil {
					n.CustomInfo = make(map[string]any)
				}
				cuStart := toInt(cuStartRaw)
				cuCount := toInt(cuCountRaw)
				totalCUs := getTotalCUs(n.CustomInfo)
				bitmap := getCUBitmap(n.CustomInfo, totalCUs)
				if err := allocateCUs(bitmap, cuStart, cuCount); err != nil {
					klog.ErrorS(err, "Failed to apply CU allocation", "device", n.ID)
				}
				klog.InfoS("Applied CU allocation to device usage",
					"device", n.ID,
					"cuStart", cuStart, "cuCount", cuCount,
					"freeRemaining", countFreeCUs(bitmap, totalCUs))
			}
		}
	}
	return nil
}

func (amddevice *AMDDevices) Fit(devices []*device.DeviceUsage, request device.ContainerDeviceRequest, pod *corev1.Pod, nodeinfo *device.NodeInfo, allocated *device.PodDevices) (bool, map[string]device.ContainerDevices, string) {
	k := request
	klog.InfoS("Allocating AMD device for container request",
		"pod", klog.KObj(pod),
		"gpuCount", k.Nums,
		"memreq", k.Memreq,
		"coresreq(CUs)", k.Coresreq)

	tmpDevs := make(map[string]device.ContainerDevices)
	reason := make(map[string]int)

	for i := len(devices) - 1; i >= 0; i-- {
		dev := devices[i]

		// Type check
		if !amddevice.checkType(k) {
			reason[common.CardTypeMismatch]++
			continue
		}

		// UUID check
		if !device.CheckUUID(pod.GetAnnotations(), dev.ID, AMDUseUUID, AMDNoUseUUID, amddevice.CommonWord()) {
			reason[common.CardUUIDMismatch]++
			continue
		}

		// Memory check
		availMem := dev.Totalmem - dev.Usedmem
		if k.Memreq > 0 && availMem < k.Memreq {
			reason[common.CardInsufficientMemory]++
			klog.V(5).InfoS("Insufficient memory",
				"device", dev.ID,
				"available", availMem, "requested", k.Memreq)
			continue
		}

		// CU availability check via bitmap
		if dev.CustomInfo == nil {
			dev.CustomInfo = make(map[string]any)
		}
		requestedCUs := int(k.Coresreq)
		if requestedCUs > 0 {
			totalCUs := getTotalCUs(dev.CustomInfo)
			bitmap := getCUBitmap(dev.CustomInfo, totalCUs)
			start, free := findFreeCURange(bitmap, totalCUs, requestedCUs)
			if start < 0 {
				reason[common.CardInsufficientCore]++
				klog.V(5).InfoS("Insufficient CUs",
					"device", dev.ID,
					"requested", requestedCUs,
					"free", free)
				continue
			}
		}

		klog.V(5).InfoS("Found fit device", "pod", klog.KObj(pod), "device", dev.ID)

		if k.Nums > 0 {
			k.Nums--

			// Allocate CU range from bitmap
			containerCustomInfo := make(map[string]any)
			if requestedCUs > 0 {
				mask, cuStart, ok := tryAllocateCUs(dev.CustomInfo, int(dev.Index), requestedCUs)
				if !ok {
					reason[common.CardInsufficientCore]++
					k.Nums++ // rollback
					continue
				}
				containerCustomInfo[CUMaskKey] = mask
				containerCustomInfo[CUStartKey] = cuStart
				containerCustomInfo[CUCountKey] = requestedCUs
			}

			tmpDevs[k.Type] = append(tmpDevs[k.Type], device.ContainerDevice{
				Idx:        int(dev.Index),
				UUID:       dev.ID,
				Type:       k.Type,
				Usedmem:    k.Memreq,
				Usedcores:  k.Coresreq,
				CustomInfo: containerCustomInfo,
			})
		}
		if k.Nums == 0 {
			klog.InfoS("AMD device allocate success",
				"pod", klog.KObj(pod),
				"devices", tmpDevs)
			return true, tmpDevs, ""
		}
	}

	// Rollback CU allocations on failure
	if len(tmpDevs) > 0 {
		for _, cds := range tmpDevs {
			for _, cd := range cds {
				if cd.CustomInfo != nil {
					if cuStart, ok := cd.CustomInfo[CUStartKey]; ok {
						if cuCount, ok := cd.CustomInfo[CUCountKey]; ok {
							for _, dev := range devices {
								if dev.ID == cd.UUID && dev.CustomInfo != nil {
									totalCUs := getTotalCUs(dev.CustomInfo)
									bitmap := getCUBitmap(dev.CustomInfo, totalCUs)
									cuStartInt, _ := cuStart.(int)
									cuCountInt, _ := cuCount.(int)
									if err := freeCUs(bitmap, cuStartInt, cuCountInt); err != nil {
										klog.ErrorS(err, "Failed to free CUs", "device", dev.ID)
									}
								}
							}
						}
					}
				}
			}
		}
		reason[common.AllocatedCardsInsufficientRequest] = len(tmpDevs)
	}
	return false, tmpDevs, common.GenReason(reason, len(devices))
}
