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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device/amd"
)

func init() {
	// Register AMD device type for annotation encoding/decoding
	if _, ok := device.SupportDevices[amd.AMDDevice]; !ok {
		device.SupportDevices[amd.AMDDevice] = "hami.io/amd-devices-allocated"
	}
	if device.InRequestDevices == nil {
		device.InRequestDevices = make(map[string]string)
	}
	device.InRequestDevices[amd.AMDDevice] = "hami.io/amd-devices-allocated"
}

func TestGetNextDeviceRequest(t *testing.T) {
	// Simulate what the scheduler writes: a pod with AMD device annotation
	devs := device.ContainerDevices{
		{
			UUID:      "node1-AMDGPU-0",
			Type:      "AMDGPU",
			Usedmem:   48000,
			Usedcores: 152,
			CustomInfo: map[string]any{
				"cu_mask":  "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
				"cu_start": 0,
				"cu_count": 152,
			},
		},
	}

	// Encode as the scheduler would
	encoded := device.EncodeContainerDevices(devs)
	podSingle := encoded + device.OnePodMultiContainerSplitSymbol

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Annotations: map[string]string{
				"hami.io/amd-devices-allocated": podSingle,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "test-container"},
			},
		},
	}

	ctrName, devreq, err := GetNextDeviceRequest(amd.AMDDevice, pod)
	if err != nil {
		t.Fatalf("GetNextDeviceRequest failed: %v", err)
	}

	if ctrName != "test-container" {
		t.Errorf("expected container name 'test-container', got '%s'", ctrName)
	}

	if len(devreq) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devreq))
	}

	dev := devreq[0]
	if dev.UUID != "node1-AMDGPU-0" {
		t.Errorf("UUID mismatch: got %s", dev.UUID)
	}
	if dev.Usedmem != 48000 {
		t.Errorf("Usedmem mismatch: got %d", dev.Usedmem)
	}
	if dev.Usedcores != 152 {
		t.Errorf("Usedcores mismatch: got %d", dev.Usedcores)
	}

	// Verify CustomInfo round-trip
	if dev.CustomInfo == nil {
		t.Fatal("CustomInfo is nil after decode")
	}
	mask, ok := dev.CustomInfo[amd.CUMaskKey]
	if !ok {
		t.Fatal("cu_mask not found in CustomInfo")
	}
	maskStr, ok := mask.(string)
	if !ok {
		t.Fatalf("cu_mask is not string, got %T", mask)
	}
	if maskStr != "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF" {
		t.Errorf("cu_mask mismatch: got %s", maskStr)
	}
}

func TestGetNextDeviceRequest_MultiDevice(t *testing.T) {
	// Two devices allocated to one container
	devs := device.ContainerDevices{
		{
			UUID: "node1-AMDGPU-0", Type: "AMDGPU", Usedmem: 48000, Usedcores: 76,
			CustomInfo: map[string]any{
				"cu_mask": "0xFFFFFFFFFFFFFFFFFFF", "cu_start": 0, "cu_count": 76,
			},
		},
		{
			UUID: "node1-AMDGPU-1", Type: "AMDGPU", Usedmem: 96000, Usedcores: 152,
			CustomInfo: map[string]any{
				"cu_mask": "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", "cu_start": 0, "cu_count": 152,
			},
		},
	}

	encoded := device.EncodeContainerDevices(devs)
	podSingle := encoded + device.OnePodMultiContainerSplitSymbol

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-gpu-pod",
			Namespace: "default",
			Annotations: map[string]string{
				"hami.io/amd-devices-allocated": podSingle,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "worker"}},
		},
	}

	_, devreq, err := GetNextDeviceRequest(amd.AMDDevice, pod)
	if err != nil {
		t.Fatalf("GetNextDeviceRequest failed: %v", err)
	}
	if len(devreq) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devreq))
	}

	// Check first device
	if devreq[0].CustomInfo["cu_mask"] != "0xFFFFFFFFFFFFFFFFFFF" {
		t.Errorf("device 0 cu_mask mismatch: got %v", devreq[0].CustomInfo["cu_mask"])
	}
	if devreq[0].Usedmem != 48000 {
		t.Errorf("device 0 Usedmem mismatch: got %d", devreq[0].Usedmem)
	}

	// Check second device
	if devreq[1].CustomInfo["cu_mask"] != "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF" {
		t.Errorf("device 1 cu_mask mismatch: got %v", devreq[1].CustomInfo["cu_mask"])
	}
	if devreq[1].Usedmem != 96000 {
		t.Errorf("device 1 Usedmem mismatch: got %d", devreq[1].Usedmem)
	}
}

func TestGetNextDeviceRequest_NoCUMask(t *testing.T) {
	// Device without CU restriction (full GPU)
	devs := device.ContainerDevices{
		{UUID: "node1-AMDGPU-0", Type: "AMDGPU", Usedmem: 192000, Usedcores: 0},
	}

	encoded := device.EncodeContainerDevices(devs)
	podSingle := encoded + device.OnePodMultiContainerSplitSymbol

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "full-gpu-pod",
			Namespace: "default",
			Annotations: map[string]string{
				"hami.io/amd-devices-allocated": podSingle,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "full"}},
		},
	}

	_, devreq, err := GetNextDeviceRequest(amd.AMDDevice, pod)
	if err != nil {
		t.Fatalf("GetNextDeviceRequest failed: %v", err)
	}
	if len(devreq) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devreq))
	}
	// No CustomInfo expected for full GPU
	if len(devreq[0].CustomInfo) != 0 {
		t.Errorf("expected empty CustomInfo, got %v", devreq[0].CustomInfo)
	}
}

func TestAllocateEnvVarGeneration(t *testing.T) {
	// Simulate what Allocate() does internally: extract env vars from device request
	devreq := device.ContainerDevices{
		{
			UUID: "node1-AMDGPU-0", Type: "AMDGPU", Usedmem: 48000, Usedcores: 152,
			CustomInfo: map[string]any{
				"cu_mask":  "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
				"cu_start": float64(0),
				"cu_count": float64(152),
			},
		},
	}

	envs := make(map[string]string)

	for i, dev := range devreq {
		if dev.Usedmem > 0 {
			envs[fmt.Sprintf("HIP_DEVICE_MEMORY_LIMIT_%d", i)] = fmt.Sprintf("%dm", dev.Usedmem)
		}
		if dev.CustomInfo != nil {
			if mask, ok := dev.CustomInfo[amd.CUMaskKey]; ok {
				if maskStr, ok := mask.(string); ok {
					envs["ROC_GLOBAL_CU_MASK"] = maskStr
				}
			}
		}
	}
	envs["LD_AUDIT"] = "/opt/hami/libamvgpu.so"

	// Verify all expected env vars
	if envs["HIP_DEVICE_MEMORY_LIMIT_0"] != "48000m" {
		t.Errorf("HIP_DEVICE_MEMORY_LIMIT_0 = %q, want '48000m'", envs["HIP_DEVICE_MEMORY_LIMIT_0"])
	}
	if envs["ROC_GLOBAL_CU_MASK"] != "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF" {
		t.Errorf("ROC_GLOBAL_CU_MASK = %q, want '0xFF...'", envs["ROC_GLOBAL_CU_MASK"])
	}
	if envs["LD_AUDIT"] != "/opt/hami/libamvgpu.so" {
		t.Errorf("LD_AUDIT = %q, want '/opt/hami/libamvgpu.so'", envs["LD_AUDIT"])
	}
}
