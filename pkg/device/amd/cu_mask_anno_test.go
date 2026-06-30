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
	"math/big"
	"testing"

	"github.com/Project-HAMi/HAMi/pkg/device"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Test_CUMask_AnnotationRoundTrip verifies the Option-A transport end to end:
// the scheduler allocates a CU bitmap, persists it in the dedicated
// hami.io/amd-cu-mask annotation (PatchAnnotations), and rebuilds device
// occupancy from that annotation (AddResourceUsage) so the next allocation is
// non-overlapping — all without touching the shared container encoding.
func Test_CUMask_AnnotationRoundTrip(t *testing.T) {
	dev := InitAMDGPUDevice(AMDConfig{
		ResourceCountName: "amd.com/gpu",
		ResourceCoreName:  "amd.com/gpucores",
	})

	const totalCU = 16
	newDev := func() []*device.DeviceUsage {
		return []*device.DeviceUsage{{
			ID:         "gpu-0",
			Index:      0,
			Count:      8,
			Totalcore:  totalCU,
			Type:       AMDDevice,
			Health:     true,
			CustomInfo: map[string]any{CUTotalKey: totalCU},
		}}
	}

	// Pod A allocates 4 CUs.
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
	reqA := device.ContainerDeviceRequest{Nums: 1, Type: AMDDevice, Coresreq: 4}
	fitA, resA, _ := dev.Fit(newDev(), reqA, podA, &device.NodeInfo{}, &device.PodDevices{})
	if !fitA {
		t.Fatalf("pod A did not fit")
	}

	// PatchAnnotations must write the dedicated CU-mask annotation.
	annosA := map[string]string{}
	dev.PatchAnnotations(podA, &annosA, device.PodDevices{AMDDevice: device.PodSingleDevice{resA[AMDDevice]}})
	maskA := lookupCUMask(annosA, "gpu-0")
	if maskA == "" {
		t.Fatalf("hami.io/amd-cu-mask was not written; annos=%v", annosA)
	}
	if got := resA[AMDDevice][0].CustomInfo[CUMaskKey].(string); got != maskA {
		t.Fatalf("annotation mask %q != in-memory mask %q", maskA, got)
	}

	// Reconstruct occupancy on a fresh device purely from the annotation,
	// simulating how the scheduler rebuilds state from an already-scheduled pod
	// whose decoded ContainerDevice carries no CustomInfo.
	existingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: annosA}}
	devsB := newDev()
	ctrA := &device.ContainerDevice{UUID: "gpu-0", Type: AMDDevice}
	if err := dev.AddResourceUsage(existingPod, devsB[0], ctrA); err != nil {
		t.Fatalf("AddResourceUsage: %v", err)
	}

	// Pod B allocates 4 more CUs on the reconstructed device.
	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
	reqB := device.ContainerDeviceRequest{Nums: 1, Type: AMDDevice, Coresreq: 4}
	fitB, resB, reasonB := dev.Fit(devsB, reqB, podB, &device.NodeInfo{}, &device.PodDevices{})
	if !fitB {
		t.Fatalf("pod B did not fit on reconstructed device: %s", reasonB)
	}
	maskB := resB[AMDDevice][0].CustomInfo[CUMaskKey].(string)

	// The two masks must be non-overlapping.
	mA, err := parseCUMask(maskA)
	if err != nil {
		t.Fatalf("parseCUMask(A): %v", err)
	}
	mB, err := parseCUMask(maskB)
	if err != nil {
		t.Fatalf("parseCUMask(B): %v", err)
	}
	if new(big.Int).And(mA, mB).Sign() != 0 {
		t.Errorf("CU masks overlap: A=%s B=%s", maskA, maskB)
	}
}

// Test_CUMask_NoAnnotationWhenWholeCard verifies that a whole-card allocation
// (no gpucores request) writes no CU-mask annotation.
func Test_CUMask_NoAnnotationWhenWholeCard(t *testing.T) {
	dev := InitAMDGPUDevice(AMDConfig{ResourceCountName: "amd.com/gpu"})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
	req := device.ContainerDeviceRequest{Nums: 1, Type: AMDDevice, Coresreq: 0}
	devices := []*device.DeviceUsage{{
		ID: "gpu-0", Index: 0, Count: 8, Type: AMDDevice, Health: true,
		CustomInfo: map[string]any{CUTotalKey: 16},
	}}
	fit, res, _ := dev.Fit(devices, req, pod, &device.NodeInfo{}, &device.PodDevices{})
	if !fit {
		t.Fatalf("whole-card pod did not fit")
	}
	annos := map[string]string{}
	dev.PatchAnnotations(pod, &annos, device.PodDevices{AMDDevice: device.PodSingleDevice{res[AMDDevice]}})
	if v, ok := annos[AMDCUMaskAnno]; ok {
		t.Errorf("unexpected CU-mask annotation for whole-card alloc: %q", v)
	}
}
