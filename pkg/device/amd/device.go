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
	"encoding/json"
	"flag"
	"fmt"
	"slices"
	"strings"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device/common"
	"github.com/Project-HAMi/HAMi/pkg/util/nodelock"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type AMDDevices struct {
	resourceCountName  string
	resourceMemoryName string
	resourceCoreName   string
}

const (
	AMDDevice          = "AMDGPU"
	AMDCommonWord      = "AMDGPU"
	AMDDeviceSelection = "amd.com/gpu-index"
	AMDUseUUID         = "amd.com/use-gpu-uuid"
	AMDNoUseUUID       = "amd.com/nouse-gpu-uuid"
	AMDAssignedNode    = "amd.com/predicate-node"
	Mi300xMemory       = 192000
	// Mi300xCU is the compute-unit (CU) count of an MI300X, used as the total
	// CU count for bitmap partitioning until per-device discovery lands.
	Mi300xCU = 304
	// AMDCUMaskAnno carries the per-device CU bitmap as a JSON array
	// [{"uuid":"<UUID>","cu_mask":"<mask_hex>"}]. The scheduler writes it; the
	// device plugin reads it to inject ROC_GLOBAL_CU_MASK. It uses the AMD
	// vendor namespace (amd.com/) and is kept separate from the shared
	// allocation annotation so no shared encoding change is needed.
	AMDCUMaskAnno = "amd.com/cu-mask"
	// NodeLockAMD is the node-wide scheduling lock shared across device types
	// (the nodelock package hardcodes "hami.io/mutex.lock"). It makes the CU
	// bitmap read-modify-write atomic so concurrent pods never receive
	// overlapping masks.
	NodeLockAMD = "hami.io/mutex.lock"
)

type AMDConfig struct {
	ResourceCountName  string `yaml:"resourceCountName"`
	ResourceMemoryName string `yaml:"resourceMemoryName"`
	ResourceCoreName   string `yaml:"resourceCoreName"`
}

func InitAMDGPUDevice(config AMDConfig) *AMDDevices {
	_, ok := device.SupportDevices[AMDDevice]
	if !ok {
		device.SupportDevices[AMDDevice] = "hami.io/amd-devices-allocated"
	}
	return &AMDDevices{
		resourceCountName:  config.ResourceCountName,
		resourceMemoryName: config.ResourceMemoryName,
		resourceCoreName:   config.ResourceCoreName,
	}
}

func (dev *AMDDevices) CommonWord() string {
	return AMDCommonWord
}

func ParseConfig(fs *flag.FlagSet) {
}

func (dev *AMDDevices) MutateAdmission(ctr *corev1.Container, p *corev1.Pod) (bool, error) {
	_, ok := ctr.Resources.Limits[corev1.ResourceName(dev.resourceCountName)]
	if !ok {
		_, ok = ctr.Resources.Limits[corev1.ResourceName(dev.resourceMemoryName)]
	}
	klog.Infoln("MutateAdmsssion result", ok)
	return ok, nil
}

func (dev *AMDDevices) GetNodeDevices(n corev1.Node) ([]*device.DeviceInfo, error) {
	nodedevices := []*device.DeviceInfo{}
	i := 0
	counts, ok := n.Status.Capacity.Name(corev1.ResourceName(dev.resourceCountName), resource.DecimalSI).AsInt64()
	if !ok || counts == 0 {
		return []*device.DeviceInfo{}, fmt.Errorf("device not found %s", dev.resourceCountName)
	}
	for int64(i) < counts {
		nodedevices = append(nodedevices, &device.DeviceInfo{
			Index:        uint(i),
			ID:           n.Name + "-" + AMDDevice + "-" + fmt.Sprint(i),
			Count:        1,
			Devmem:       Mi300xMemory,
			Devcore:      Mi300xCU,
			Type:         AMDDevice,
			Numa:         0,
			Health:       true,
			CustomInfo:   map[string]any{CUTotalKey: Mi300xCU},
			DeviceVendor: AMDCommonWord,
		})
		i++
	}
	i = 0
	for i < len(nodedevices) {
		klog.V(4).Infoln("Registered AMD nodedevices:", nodedevices[i])
		i++
	}
	return nodedevices, nil
}

func (dev *AMDDevices) PatchAnnotations(pod *corev1.Pod, annoinput *map[string]string, pd device.PodDevices) map[string]string {
	devlist, ok := pd[AMDDevice]
	if ok && len(devlist) > 0 {
		(*annoinput)[device.SupportDevices[AMDDevice]] = device.EncodePodSingleDevice(devlist)
		// AMD-specific: carry the CU bitmap in a dedicated annotation so the
		// shared container encoding stays unchanged. The device plugin reads
		// this to inject ROC_GLOBAL_CU_MASK.
		if cuMask := encodeCUMaskAnno(devlist); cuMask != "" {
			(*annoinput)[AMDCUMaskAnno] = cuMask
		}
	}
	klog.V(4).InfoS("annos", "input", (*annoinput))
	return *annoinput
}

// cuMaskEntry is one element of the amd.com/cu-mask annotation JSON array.
type cuMaskEntry struct {
	UUID   string `json:"uuid"`
	CUMask string `json:"cu_mask"`
}

// encodeCUMaskAnno builds the amd.com/cu-mask value as a JSON array
// [{"uuid":"<UUID>","cu_mask":"<mask_hex>"}] from the allocated devices.
func encodeCUMaskAnno(pd device.PodSingleDevice) string {
	var entries []cuMaskEntry
	for _, ctrDevs := range pd {
		for _, cd := range ctrDevs {
			if cd.CustomInfo == nil {
				continue
			}
			if mask, ok := cd.CustomInfo[CUMaskKey].(string); ok && mask != "" {
				entries = append(entries, cuMaskEntry{UUID: cd.UUID, CUMask: mask})
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return ""
	}
	return string(b)
}

// lookupCUMask returns the CU mask for the given device UUID from the
// amd.com/cu-mask annotation, or "" if absent.
func lookupCUMask(annotations map[string]string, uuid string) string {
	raw, ok := annotations[AMDCUMaskAnno]
	if !ok || raw == "" {
		return ""
	}
	var entries []cuMaskEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return ""
	}
	for _, e := range entries {
		if e.UUID == uuid {
			return e.CUMask
		}
	}
	return ""
}

func (dev *AMDDevices) LockNode(n *corev1.Node, p *corev1.Pod) error {
	found := false
	for _, val := range p.Spec.Containers {
		if dev.GenerateResourceRequests(&val).Nums > 0 {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	return nodelock.LockNode(n.Name, NodeLockAMD, p)
}

func (dev *AMDDevices) ReleaseNodeLock(n *corev1.Node, p *corev1.Pod) error {
	found := false
	for _, val := range p.Spec.Containers {
		if dev.GenerateResourceRequests(&val).Nums > 0 {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	return nodelock.ReleaseNodeLock(n.Name, NodeLockAMD, p, false)
}

func (dev *AMDDevices) NodeCleanUp(nn string) error {
	return nil
}

func (dev *AMDDevices) checkType(n device.ContainerDeviceRequest) (bool, bool, bool) {
	if strings.Compare(n.Type, AMDDevice) == 0 {
		return true, true, false
	}
	return false, false, false
}

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
	//amdResourceMemory := corev1.ResourceName(dev.resourceMemoryName)
	v, ok := ctr.Resources.Limits[amdResourceCount]
	if !ok {
		v, ok = ctr.Resources.Requests[amdResourceCount]
	}
	if ok {
		if n, ok := v.AsInt64(); ok {
			// Memory limit (MB); default to the whole device when unset.
			memreq := int32(Mi300xMemory)
			if dev.resourceMemoryName != "" {
				if mv, ok := ctr.Resources.Limits[corev1.ResourceName(dev.resourceMemoryName)]; ok {
					if m, ok := mv.AsInt64(); ok {
						memreq = int32(m)
					}
				}
			}
			// CU count; 0 means use all CUs (whole-card).
			coresreq := int32(0)
			if dev.resourceCoreName != "" {
				if cv, ok := ctr.Resources.Limits[corev1.ResourceName(dev.resourceCoreName)]; ok {
					if c, ok := cv.AsInt64(); ok {
						coresreq = int32(c)
					}
				}
			}
			klog.InfoS("Detected AMD device request",
				"container", ctr.Name,
				"deviceCount", n, "memreq", memreq, "coresreq(CUs)", coresreq)
			return device.ContainerDeviceRequest{
				Nums:             int32(n),
				Type:             AMDDevice,
				Memreq:           memreq,
				MemPercentagereq: 0,
				Coresreq:         coresreq,
			}
		}
	}
	return device.ContainerDeviceRequest{}
}

func (dev *AMDDevices) ScoreNode(node *corev1.Node, podDevices device.PodSingleDevice, previous []*device.DeviceUsage, policy string) float32 {
	return 0
}

func (dev *AMDDevices) AddResourceUsage(pod *corev1.Pod, n *device.DeviceUsage, ctr *device.ContainerDevice) error {
	n.Used++
	n.Usedcores += ctr.Usedcores
	n.Usedmem += ctr.Usedmem

	// Reconstruct CU occupancy into the device bitmap so later allocations stay
	// non-overlapping. The current scheduling cycle carries start/count in the
	// in-memory ContainerDevice; already-scheduled pods carry the mask in the
	// hami.io/amd-cu-mask annotation.
	if n.CustomInfo == nil {
		n.CustomInfo = make(map[string]any)
	}
	totalCUs := getTotalCUs(n.CustomInfo)
	if totalCUs == 0 {
		return nil
	}
	bitmap := getCUBitmap(n.CustomInfo, totalCUs)
	if ctr.CustomInfo != nil {
		if startRaw, ok := ctr.CustomInfo[CUStartKey]; ok {
			if countRaw, ok := ctr.CustomInfo[CUCountKey]; ok {
				if err := allocateCUs(bitmap, toInt(startRaw), toInt(countRaw)); err != nil {
					klog.ErrorS(err, "Failed to apply CU allocation", "device", n.ID)
				}
				return nil
			}
		}
	}
	if mask := lookupCUMask(pod.GetAnnotations(), ctr.UUID); mask != "" {
		if maskInt, err := parseCUMask(mask); err == nil {
			bitmap.Or(bitmap, maskInt)
		}
	}
	return nil
}

func (amddevice *AMDDevices) Fit(devices []*device.DeviceUsage, request device.ContainerDeviceRequest, pod *corev1.Pod, nodeinfo *device.NodeInfo, allocated *device.PodDevices) (bool, map[string]device.ContainerDevices, string) {
	k := request
	originReq := k.Nums
	klog.InfoS("Allocating device for container request", "pod", klog.KObj(pod), "card request", k)
	tmpDevs := make(map[string]device.ContainerDevices)
	reason := make(map[string]int)
	for i, v := range slices.Backward(devices) {
		dev := v
		klog.V(4).InfoS("scoring pod", "pod", klog.KObj(pod), "device", dev.ID, "Memreq", k.Memreq, "MemPercentagereq", k.MemPercentagereq, "Coresreq", k.Coresreq, "Nums", k.Nums, "device index", i)

		klog.V(3).InfoS("Type check", "device", dev.Type, "req", k.Type, "dev=", dev)
		if !strings.Contains(dev.Type, k.Type) {
			reason[common.CardTypeMismatch]++
			continue
		}

		_, found, _ := amddevice.checkType(k)
		if !found {
			reason[common.CardTypeMismatch]++
			klog.V(5).InfoS(common.CardTypeMismatch, "pod", klog.KObj(pod), "device", dev.ID, dev.Type, k.Type)
			continue
		}
		if !device.CheckUUID(pod.GetAnnotations(), dev.ID, AMDUseUUID, AMDNoUseUUID, amddevice.CommonWord()) {
			reason[common.CardUUIDMismatch]++
			klog.V(5).InfoS(common.CardUUIDMismatch, "pod", klog.KObj(pod), "device", dev.ID, "current device info is:", *dev)
			continue
		}

		if dev.Count <= dev.Used {
			reason[common.CardTimeSlicingExhausted]++
			klog.V(5).InfoS(common.CardTimeSlicingExhausted, "pod", klog.KObj(pod), "device", dev.ID, "count", dev.Count, "used", dev.Used)
			continue
		}

		// Memory availability.
		if k.Memreq > 0 && dev.Totalmem-dev.Usedmem < k.Memreq {
			reason[common.CardInsufficientMemory]++
			klog.V(5).InfoS(common.CardInsufficientMemory, "pod", klog.KObj(pod), "device", dev.ID, "available", dev.Totalmem-dev.Usedmem, "request", k.Memreq)
			continue
		}

		// CU availability (only when a CU count is requested).
		if dev.CustomInfo == nil {
			dev.CustomInfo = make(map[string]any)
		}
		requestedCUs := int(k.Coresreq)
		if requestedCUs > 0 {
			totalCUs := getTotalCUs(dev.CustomInfo)
			bitmap := getCUBitmap(dev.CustomInfo, totalCUs)
			if start, free := findFreeCURange(bitmap, totalCUs, requestedCUs); start < 0 {
				reason[common.CardInsufficientCore]++
				klog.V(5).InfoS(common.CardInsufficientCore, "pod", klog.KObj(pod), "device", dev.ID, "request", requestedCUs, "free", free)
				continue
			}
		}

		klog.V(5).InfoS("find fit device", "pod", klog.KObj(pod), "device", dev.ID)

		if k.Nums > 0 {
			k.Nums--
			ctrCustomInfo := map[string]any{}
			if requestedCUs > 0 {
				mask, cuStart, allocOK := tryAllocateCUs(dev.CustomInfo, int(dev.Index), requestedCUs)
				if !allocOK {
					reason[common.CardInsufficientCore]++
					k.Nums++ // rollback the count decrement
					continue
				}
				ctrCustomInfo[CUMaskKey] = mask
				ctrCustomInfo[CUStartKey] = cuStart
				ctrCustomInfo[CUCountKey] = requestedCUs
			}
			tmpDevs[k.Type] = append(tmpDevs[k.Type], device.ContainerDevice{
				Idx:        int(dev.Index),
				UUID:       dev.ID,
				Type:       k.Type,
				Usedmem:    k.Memreq,
				Usedcores:  k.Coresreq,
				CustomInfo: ctrCustomInfo,
			})
		}
		if k.Nums == 0 {
			klog.V(4).InfoS("device allocate success", "pod", klog.KObj(pod), "allocate device", tmpDevs)
			return true, tmpDevs, ""
		}
	}
	if len(tmpDevs) > 0 {
		reason[common.AllocatedCardsInsufficientRequest] = len(tmpDevs)
		klog.V(5).InfoS(common.AllocatedCardsInsufficientRequest, "pod", klog.KObj(pod), "request", originReq, "allocated", len(tmpDevs))
	}
	return false, tmpDevs, common.GenReason(reason, len(devices))
}
