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
	"fmt"
	"math/big"
	"strings"

	"k8s.io/klog/v2"
)

// CU bitmap management for AMD GPU CU-level partitioning.
//
// Each GPU device maintains a bitmap of allocated CUs in DeviceUsage.CustomInfo.
// When a pod requests N CUs, the scheduler finds a contiguous range of free CUs,
// marks them as allocated, and stores the resulting ROC_GLOBAL_CU_MASK in
// ContainerDevice.CustomInfo for the device plugin to inject.

const (
	// maxCUBits is the maximum number of CU bits supported in the bitmap.
	// This is a safety limit to prevent unbounded bit manipulation.
	// Maximum CU bits to track. MI300X has 304 CUs; 1024 provides margin for future GPUs.
	maxCUBits = 1024

	// CustomInfo keys.
	CUBitmapKey = "cu_bitmap" // *big.Int stored in DeviceUsage.CustomInfo
	CUTotalKey  = "cu_total"  // int: total CUs on this device
	CUMaskKey   = "cu_mask"   // string: ROC_GLOBAL_CU_MASK value in ContainerDevice.CustomInfo
	CUStartKey  = "cu_start"  // int: start CU index (for debugging/logging)
	CUCountKey  = "cu_count"  // int: number of CUs allocated
)

// toInt converts a value from map[string]any to int.
// Handles int, int32, int64, float64 (JSON unmarshalling produces float64).
func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int32:
		return int(t)
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return 0
}

// getCUBitmap retrieves or initializes the CU bitmap from CustomInfo.
func getCUBitmap(customInfo map[string]any, totalCUs int) *big.Int {
	if v, ok := customInfo[CUBitmapKey]; ok {
		if bm, ok := v.(*big.Int); ok {
			return bm
		}
	}
	// Initialize empty bitmap
	bm := new(big.Int)
	customInfo[CUBitmapKey] = bm
	customInfo[CUTotalKey] = totalCUs
	return bm
}

// getTotalCUs returns the total CU count for a device.
// Returns 0 if not configured (GPU partitioning disabled).
func getTotalCUs(customInfo map[string]any) int {
	if v, ok := customInfo[CUTotalKey]; ok {
		return toInt(v)
	}
	return 0
}

// findFreeCURange finds a contiguous range of `count` free CUs in the bitmap.
// Returns the start index (or -1 if unavailable) and the total number of free CUs.
func findFreeCURange(bitmap *big.Int, totalCUs, count int) (start int, freeCount int) {
	if count <= 0 || count > totalCUs {
		return -1, 0
	}

	consecutive := 0
	rangeStart := 0
	foundStart := -1

	for i := range totalCUs {
		if bitmap.Bit(i) == 0 {
			freeCount++
			if consecutive == 0 {
				rangeStart = i
			}
			consecutive++
			if consecutive >= count && foundStart < 0 {
				foundStart = rangeStart
			}
		} else {
			consecutive = 0
		}
	}

	return foundStart, freeCount
}

// allocateCUs marks a range of CUs as allocated in the bitmap.
// Returns an error if the range exceeds the bitmap capacity.
func allocateCUs(bitmap *big.Int, start, count int) error {
	if start < 0 || count < 0 || start+count > maxCUBits {
		return fmt.Errorf("allocateCUs: invalid range [%d, %d) exceeds limit %d", start, start+count, maxCUBits)
	}
	for i := start; i < start+count; i++ {
		bitmap.SetBit(bitmap, i, 1)
	}
	return nil
}

// freeCUs marks a range of CUs as free in the bitmap.
// Returns an error if the range exceeds the bitmap capacity.
func freeCUs(bitmap *big.Int, start, count int) error {
	if start < 0 || count < 0 || start+count > maxCUBits {
		return fmt.Errorf("freeCUs: invalid range [%d, %d) exceeds limit %d", start, start+count, maxCUBits)
	}
	for i := start; i < start+count; i++ {
		bitmap.SetBit(bitmap, i, 0)
	}
	return nil
}

// countFreeCUs returns the number of free CUs in the bitmap.
func countFreeCUs(bitmap *big.Int, totalCUs int) int {
	free := 0
	for i := range totalCUs {
		if bitmap.Bit(i) == 0 {
			free++
		}
	}
	return free
}

// buildCUMask generates the ROC_GLOBAL_CU_MASK value for a given CU range.
// Format: hex only, NO GPU prefix (e.g. "0x3FFFFFFFFF").
// The "GPU_INDEX:0xHEX" format causes parsing bugs on multi-XCD GPUs like MI300X.
func buildCUMask(cuStart, cuCount int) string {
	mask := new(big.Int)
	for i := cuStart; i < cuStart+cuCount; i++ {
		mask.SetBit(mask, i, 1)
	}
	return fmt.Sprintf("0x%s", strings.ToUpper(mask.Text(16)))
}

// tryAllocateCUs attempts to allocate CUs from a device.
// Returns the CU mask string and start index on success, or empty string on failure.
func tryAllocateCUs(customInfo map[string]any, gpuIndex, requestedCUs int) (mask string, cuStart int, ok bool) {
	totalCUs := getTotalCUs(customInfo)
	bitmap := getCUBitmap(customInfo, totalCUs)

	// If no CU request (0), allocate all available CUs
	if requestedCUs <= 0 {
		requestedCUs = totalCUs
	}

	start, free := findFreeCURange(bitmap, totalCUs, requestedCUs)
	if start < 0 {
		klog.V(4).InfoS("No contiguous CU range available",
			"requested", requestedCUs,
			"free", free,
			"total", totalCUs,
			"gpuIndex", gpuIndex)
		return "", -1, false
	}

	if err := allocateCUs(bitmap, start, requestedCUs); err != nil {
		klog.ErrorS(err, "Failed to allocate CUs", "gpuIndex", gpuIndex)
		return "", -1, false
	}
	mask = buildCUMask(start, requestedCUs)

	klog.InfoS("Allocated CU range",
		"gpuIndex", gpuIndex,
		"cuStart", start,
		"cuCount", requestedCUs,
		"freeRemaining", free-requestedCUs,
		"mask", mask)

	return mask, start, true
}
