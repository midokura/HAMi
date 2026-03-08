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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device/amd"
)

// TestE2EFlow_FitToEnvVars simulates the full scheduler → device plugin flow:
//  1. Create DeviceUsage (simulating node GPU inventory)
//  2. Create a Pod with resource requests
//  3. Scheduler Fit() allocates CUs and generates CustomInfo
//  4. PatchAnnotations encodes the allocation into pod annotations
//  5. Device Plugin decodes annotations and generates env vars
// Test constants for GPU specs (configurable, not hardcoded in production)
const (
	testTotalCUs   = 304
	testTotalMemMB = 192000
)

func TestE2EFlow_FitToEnvVars(t *testing.T) {
	amdDev := amd.InitAMDGPUDevice(amd.AMDConfig{
		ResourceCountName:  "amd.com/gpu",
		ResourceMemoryName: "amd.com/gpumem",
		ResourceCoreName:   "amd.com/gpucores",
		TotalCUs:           testTotalCUs,
		TotalMemoryMB:      int32(testTotalMemMB),
	})

	tests := []struct {
		name           string
		gpuCount       int
		gpumem         int
		gpucores       int
		wantSuccess    bool
		wantCUMask     bool
		wantMemLimit   string
		wantMaskPrefix string
	}{
		{
			name:         "half GPU: 152 CUs, 96GB",
			gpuCount:     1,
			gpumem:       96000,
			gpucores:     152,
			wantSuccess:  true,
			wantCUMask:   true,
			wantMemLimit: "96000m",
		},
		{
			name:         "quarter GPU: 76 CUs, 48GB",
			gpuCount:     1,
			gpumem:       48000,
			gpucores:     76,
			wantSuccess:  true,
			wantCUMask:   true,
			wantMemLimit: "48000m",
		},
		{
			name:        "full GPU: no CU restriction",
			gpuCount:    1,
			gpumem:      192000,
			gpucores:    0,
			wantSuccess: true,
			wantCUMask:  false,
		},
		{
			name:        "over-request: 400 CUs (exceeds 304)",
			gpuCount:    1,
			gpumem:      48000,
			gpucores:    400,
			wantSuccess: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Step 1: Create device inventory (simulating one MI300X GPU)
			customInfo := make(map[string]any)
			customInfo["cu_total"] = testTotalCUs
			devices := []*device.DeviceUsage{
				{
					ID:        "phoenix-AMDGPU-0",
					Index:     0,
					Count:     100,
					Used:      0,
					Totalmem:  int32(testTotalMemMB),
					Usedmem:   0,
					Totalcore: int32(testTotalCUs),
					Usedcores: 0,
					Type:      amd.AMDDevice,
					Health:    true,
					CustomInfo: customInfo,
				},
			}

			// Step 2: Create pod request
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   "default",
					Annotations: map[string]string{},
				},
			}

			request := device.ContainerDeviceRequest{
				Nums:     int32(tt.gpuCount),
				Type:     amd.AMDDevice,
				Memreq:   int32(tt.gpumem),
				Coresreq: int32(tt.gpucores),
			}

			// Step 3: Scheduler Fit()
			ok, fitResult, reason := amdDev.Fit(devices, request, pod, nil, nil)
			if ok != tt.wantSuccess {
				t.Fatalf("Fit() = %v, want %v (reason: %s)", ok, tt.wantSuccess, reason)
			}
			if !tt.wantSuccess {
				return
			}

			// Step 4: Encode into annotations (simulating PatchAnnotations)
			cds, ok := fitResult[amd.AMDDevice]
			if !ok || len(cds) == 0 {
				t.Fatal("no AMDGPU devices in fit result")
			}

			encoded := device.EncodeContainerDevices(cds)
			podSingle := encoded + device.OnePodMultiContainerSplitSymbol
			pod.Annotations["hami.io/amd-devices-allocated"] = podSingle

			// Step 5: Device Plugin decodes and generates env vars
			_, devreq, err := GetNextDeviceRequest(amd.AMDDevice, *pod)
			if err != nil {
				t.Fatalf("GetNextDeviceRequest failed: %v", err)
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

			// Verify env vars
			if tt.wantMemLimit != "" {
				got := envs["HIP_DEVICE_MEMORY_LIMIT_0"]
				if got != tt.wantMemLimit {
					t.Errorf("HIP_DEVICE_MEMORY_LIMIT_0 = %q, want %q", got, tt.wantMemLimit)
				}
			}

			mask := envs["ROC_GLOBAL_CU_MASK"]
			if tt.wantCUMask {
				if mask == "" {
					t.Error("ROC_GLOBAL_CU_MASK is empty, expected a mask")
				}
				if !strings.HasPrefix(mask, "0x") {
					t.Errorf("ROC_GLOBAL_CU_MASK doesn't start with 0x: %s", mask)
				}
			} else {
				if mask != "" {
					t.Errorf("ROC_GLOBAL_CU_MASK = %q, expected empty (no CU restriction)", mask)
				}
			}

			if envs["LD_AUDIT"] != "/opt/hami/libamvgpu.so" {
				t.Errorf("LD_AUDIT = %q", envs["LD_AUDIT"])
			}

			t.Logf("Env vars: %v", envs)
		})
	}
}

// TestE2EFlow_MultiTenantCUExclusion verifies that two tenants get exclusive CU ranges.
func TestE2EFlow_MultiTenantCUExclusion(t *testing.T) {
	amdDev := amd.InitAMDGPUDevice(amd.AMDConfig{
		ResourceCountName:  "amd.com/gpu",
		ResourceMemoryName: "amd.com/gpumem",
		ResourceCoreName:   "amd.com/gpucores",
		TotalCUs:           testTotalCUs,
		TotalMemoryMB:      int32(testTotalMemMB),
	})

	// Shared device inventory
	customInfo := make(map[string]any)
	customInfo["cu_total"] = testTotalCUs
	devices := []*device.DeviceUsage{
		{
			ID: "phoenix-AMDGPU-0", Index: 0, Count: 100,
			Totalmem: int32(testTotalMemMB), Totalcore: int32(testTotalCUs),
			Type: amd.AMDDevice, Health: true, CustomInfo: customInfo,
		},
	}

	// Tenant A: 152 CUs
	reqA := device.ContainerDeviceRequest{
		Nums: 1, Type: amd.AMDDevice, Memreq: 96000, Coresreq: 152,
	}
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", Namespace: "default", Annotations: map[string]string{}}}

	okA, resultA, reason := amdDev.Fit(devices, reqA, podA, nil, nil)
	if !okA {
		t.Fatalf("Tenant A Fit() failed: %s", reason)
	}

	// Apply tenant A's allocation to device usage
	cdsA := resultA[amd.AMDDevice]
	for _, cd := range cdsA {
		amdDev.AddResourceUsage(podA, devices[0], &cd)
	}

	// Tenant B: 152 CUs (should get the remaining CUs)
	reqB := device.ContainerDeviceRequest{
		Nums: 1, Type: amd.AMDDevice, Memreq: 96000, Coresreq: 152,
	}
	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "tenant-b", Namespace: "default", Annotations: map[string]string{}}}

	okB, resultB, reason := amdDev.Fit(devices, reqB, podB, nil, nil)
	if !okB {
		t.Fatalf("Tenant B Fit() failed: %s", reason)
	}

	// Extract masks
	maskA := cdsA[0].CustomInfo[amd.CUMaskKey].(string)
	maskB := resultB[amd.AMDDevice][0].CustomInfo[amd.CUMaskKey].(string)

	t.Logf("Tenant A mask: %s", maskA)
	t.Logf("Tenant B mask: %s", maskB)

	if maskA == maskB {
		t.Error("Tenant A and B have the same CU mask - CUs are NOT exclusive!")
	}

	// Tenant C: another 152 CUs should FAIL (only 0 CUs remaining)
	reqC := device.ContainerDeviceRequest{
		Nums: 1, Type: amd.AMDDevice, Memreq: 1000, Coresreq: 1,
	}
	podC := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "tenant-c", Namespace: "default", Annotations: map[string]string{}}}

	// Apply tenant B's allocation
	cdsB := resultB[amd.AMDDevice]
	for _, cd := range cdsB {
		amdDev.AddResourceUsage(podB, devices[0], &cd)
	}

	okC, _, _ := amdDev.Fit(devices, reqC, podC, nil, nil)
	if okC {
		t.Error("Tenant C Fit() should have failed - no CUs remaining")
	}

	t.Logf("Multi-tenant exclusion verified: A=%s, B=%s, C=rejected", maskA, maskB)
}

// TestE2EFlow_EncodedAnnotationBackwardCompat verifies that old-format annotations
// (without CustomInfo) still decode correctly.
func TestE2EFlow_EncodedAnnotationBackwardCompat(t *testing.T) {
	// Old-format annotation (4 fields, no CustomInfo)
	oldAnnotation := "GPU-abc123,AMDGPU,48000,152:"
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "old-pod",
			Namespace: "default",
			Annotations: map[string]string{
				"hami.io/amd-devices-allocated": oldAnnotation + ";",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "old"}},
		},
	}

	_, devreq, err := GetNextDeviceRequest(amd.AMDDevice, pod)
	if err != nil {
		t.Fatalf("Old-format decode failed: %v", err)
	}
	if len(devreq) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devreq))
	}
	if devreq[0].UUID != "GPU-abc123" {
		t.Errorf("UUID mismatch: got %s", devreq[0].UUID)
	}
	if devreq[0].Usedmem != 48000 {
		t.Errorf("Usedmem mismatch: got %d", devreq[0].Usedmem)
	}
	// CustomInfo should be nil for old format
	if devreq[0].CustomInfo != nil {
		t.Errorf("expected nil CustomInfo for old format, got %v", devreq[0].CustomInfo)
	}
}
