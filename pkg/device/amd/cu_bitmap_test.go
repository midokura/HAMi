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
	"strings"
	"testing"
)

// --- findFreeCURange tests ---

func TestFindFreeCURange_EmptyBitmap(t *testing.T) {
	bitmap := new(big.Int)
	tests := []struct {
		name     string
		totalCUs int
		count    int
		want     int
	}{
		{"request 1 CU", 304, 1, 0},
		{"request 76 CUs", 304, 76, 0},
		{"request 152 CUs", 304, 152, 0},
		{"request all 304 CUs", 304, 304, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := findFreeCURange(bitmap, tt.totalCUs, tt.count)
			if got != tt.want {
				t.Errorf("findFreeCURange() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFindFreeCURange_PartiallyFilled(t *testing.T) {
	bitmap := new(big.Int)
	// Allocate first 152 CUs
	allocateCUs(bitmap, 0, 152)

	// Request 152 more should start at 152
	got, _ := findFreeCURange(bitmap, 304, 152)
	if got != 152 {
		t.Errorf("findFreeCURange() = %d, want 152", got)
	}

	// Request 153 should fail (only 152 free)
	got, _ = findFreeCURange(bitmap, 304, 153)
	if got != -1 {
		t.Errorf("findFreeCURange() = %d, want -1", got)
	}
}

func TestFindFreeCURange_FullBitmap(t *testing.T) {
	bitmap := new(big.Int)
	allocateCUs(bitmap, 0, 304)

	got, _ := findFreeCURange(bitmap, 304, 1)
	if got != -1 {
		t.Errorf("findFreeCURange() = %d, want -1", got)
	}
}

func TestFindFreeCURange_ExceedsTotalCUs(t *testing.T) {
	bitmap := new(big.Int)

	got, _ := findFreeCURange(bitmap, 304, 305)
	if got != -1 {
		t.Errorf("findFreeCURange() = %d, want -1", got)
	}
}

func TestFindFreeCURange_ZeroCount(t *testing.T) {
	bitmap := new(big.Int)

	got, _ := findFreeCURange(bitmap, 304, 0)
	if got != -1 {
		t.Errorf("findFreeCURange() = %d, want -1", got)
	}
}

func TestFindFreeCURange_NegativeCount(t *testing.T) {
	bitmap := new(big.Int)

	got, _ := findFreeCURange(bitmap, 304, -1)
	if got != -1 {
		t.Errorf("findFreeCURange() = %d, want -1", got)
	}
}

func TestFindFreeCURange_Fragmented(t *testing.T) {
	bitmap := new(big.Int)
	// Create fragmentation: allocate every other 4-CU block
	// [0-3]=used, [4-7]=free, [8-11]=used, [12-15]=free, ...
	for i := 0; i < 304; i += 8 {
		allocateCUs(bitmap, i, 4)
	}

	// Can find 4 contiguous free CUs
	got, _ := findFreeCURange(bitmap, 304, 4)
	if got != 4 {
		t.Errorf("findFreeCURange() = %d, want 4", got)
	}

	// Cannot find 5 contiguous free CUs
	got, _ = findFreeCURange(bitmap, 304, 5)
	if got != -1 {
		t.Errorf("findFreeCURange() = %d, want -1", got)
	}
}

// --- allocateCUs / freeCUs tests ---

func TestAllocateAndFreeCUs(t *testing.T) {
	bitmap := new(big.Int)

	// Allocate CUs 10-19
	allocateCUs(bitmap, 10, 10)

	// Verify bits 10-19 are set
	for i := 10; i < 20; i++ {
		if bitmap.Bit(i) != 1 {
			t.Errorf("bit %d should be 1 after allocate", i)
		}
	}
	// Verify surrounding bits are clear
	if bitmap.Bit(9) != 0 {
		t.Error("bit 9 should be 0")
	}
	if bitmap.Bit(20) != 0 {
		t.Error("bit 20 should be 0")
	}

	// Free CUs 10-19
	freeCUs(bitmap, 10, 10)

	// Verify bitmap is back to zero
	if bitmap.BitLen() != 0 {
		t.Errorf("bitmap should be empty after free, bitlen = %d", bitmap.BitLen())
	}
}

func TestAllocateFreeCycles(t *testing.T) {
	bitmap := new(big.Int)

	for cycle := range 3 {
		allocateCUs(bitmap, 0, 152)
		if countFreeCUs(bitmap, 304) != 152 {
			t.Errorf("cycle %d: expected 152 free after allocate", cycle)
		}
		freeCUs(bitmap, 0, 152)
		if countFreeCUs(bitmap, 304) != 304 {
			t.Errorf("cycle %d: expected 304 free after free", cycle)
		}
	}
}

func TestAllocateCUs_BoundsCheck(t *testing.T) {
	bitmap := new(big.Int)

	// Allocate beyond maxCUBits should return error
	err := allocateCUs(bitmap, 1020, 10)
	if err == nil {
		t.Error("allocateCUs should return error when start+count > maxCUBits")
	}

	// Negative start
	err = allocateCUs(bitmap, -1, 10)
	if err == nil {
		t.Error("allocateCUs should return error for negative start")
	}
}

func TestFreeCUs_BoundsCheck(t *testing.T) {
	bitmap := new(big.Int)

	// Free beyond maxCUBits should return error
	err := freeCUs(bitmap, 1020, 10)
	if err == nil {
		t.Error("freeCUs should return error when start+count > maxCUBits")
	}

	// Negative count
	err = freeCUs(bitmap, 0, -1)
	if err == nil {
		t.Error("freeCUs should return error for negative count")
	}
}

// --- countFreeCUs tests ---

func TestCountFreeCUs_EmptyBitmap(t *testing.T) {
	bitmap := new(big.Int)

	got := countFreeCUs(bitmap, 304)
	if got != 304 {
		t.Errorf("countFreeCUs() = %d, want 304", got)
	}
}

func TestCountFreeCUs_FullBitmap(t *testing.T) {
	bitmap := new(big.Int)
	allocateCUs(bitmap, 0, 304)

	got := countFreeCUs(bitmap, 304)
	if got != 0 {
		t.Errorf("countFreeCUs() = %d, want 0", got)
	}
}

func TestCountFreeCUs_PartiallyAllocated(t *testing.T) {
	bitmap := new(big.Int)
	allocateCUs(bitmap, 0, 100)

	got := countFreeCUs(bitmap, 304)
	if got != 204 {
		t.Errorf("countFreeCUs() = %d, want 204", got)
	}
}

// --- buildCUMask tests ---

func TestBuildCUMask_SingleBitPositions(t *testing.T) {
	tests := []struct {
		name    string
		cuStart int
		cuCount int
	}{
		{"bit 0", 0, 1},
		{"bit 63", 63, 1},
		{"bit 64", 64, 1},
		{"bit 127", 127, 1},
		{"bit 303", 303, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mask := buildCUMask(tt.cuStart, tt.cuCount)
			if !strings.HasPrefix(mask, "0x") {
				t.Errorf("mask %q should start with 0x", mask)
			}

			// Verify the mask has the correct bit set
			expected := new(big.Int)
			expected.SetBit(expected, tt.cuStart, 1)
			expectedStr := "0x" + strings.ToUpper(expected.Text(16))
			if mask != expectedStr {
				t.Errorf("buildCUMask(%d, %d) = %q, want %q", tt.cuStart, tt.cuCount, mask, expectedStr)
			}
		})
	}
}

func TestBuildCUMask_FullRange(t *testing.T) {
	mask := buildCUMask(0, 304)
	if !strings.HasPrefix(mask, "0x") {
		t.Errorf("mask %q should start with 0x", mask)
	}

	// Parse back and verify all 304 bits are set
	parsed := new(big.Int)
	parsed.SetString(mask[2:], 16)
	for i := range 304 {
		if parsed.Bit(i) != 1 {
			t.Errorf("bit %d should be set in full range mask", i)
		}
	}
}

func TestBuildCUMask_HexFormat(t *testing.T) {
	mask := buildCUMask(0, 4)
	// Bits 0-3 set = 0xF
	if mask != "0xF" {
		t.Errorf("buildCUMask(0, 4) = %q, want \"0xF\"", mask)
	}
}

// --- tryAllocateCUs tests ---

func TestTryAllocateCUs_FreshDevice(t *testing.T) {
	customInfo := map[string]any{
		CUTotalKey: 304,
	}

	mask, cuStart, ok := tryAllocateCUs(customInfo, 0, 76)
	if !ok {
		t.Fatal("tryAllocateCUs should succeed on fresh device")
	}
	if cuStart != 0 {
		t.Errorf("cuStart = %d, want 0", cuStart)
	}
	if !strings.HasPrefix(mask, "0x") {
		t.Errorf("mask %q should start with 0x", mask)
	}
}

func TestTryAllocateCUs_ZeroRequestAllocatesAll(t *testing.T) {
	customInfo := map[string]any{
		CUTotalKey: 64,
	}

	mask, cuStart, ok := tryAllocateCUs(customInfo, 0, 0)
	if !ok {
		t.Fatal("tryAllocateCUs with 0 should allocate all CUs")
	}
	if cuStart != 0 {
		t.Errorf("cuStart = %d, want 0", cuStart)
	}

	// Verify all 64 CUs are now allocated
	bitmap := getCUBitmap(customInfo, 64)
	free := countFreeCUs(bitmap, 64)
	if free != 0 {
		t.Errorf("after allocating all, free = %d, want 0", free)
	}
	_ = mask
}

func TestTryAllocateCUs_Fragmented(t *testing.T) {
	customInfo := map[string]any{
		CUTotalKey: 128,
	}

	// Allocate first 64, then free middle 32 (CUs 16-47)
	bitmap := getCUBitmap(customInfo, 128)
	allocateCUs(bitmap, 0, 64)
	freeCUs(bitmap, 16, 32)

	// Request 32 should find the gap at 16
	mask, cuStart, ok := tryAllocateCUs(customInfo, 0, 32)
	if !ok {
		t.Fatal("tryAllocateCUs should find fragmented gap")
	}
	if cuStart != 16 {
		t.Errorf("cuStart = %d, want 16", cuStart)
	}
	if !strings.HasPrefix(mask, "0x") {
		t.Errorf("mask %q should start with 0x", mask)
	}
}

func TestTryAllocateCUs_InsufficientCUs(t *testing.T) {
	customInfo := map[string]any{
		CUTotalKey: 64,
	}

	// Allocate all
	bitmap := getCUBitmap(customInfo, 64)
	allocateCUs(bitmap, 0, 64)

	_, _, ok := tryAllocateCUs(customInfo, 0, 1)
	if ok {
		t.Error("tryAllocateCUs should fail when no CUs available")
	}
}

// --- Word boundary tests ---

func TestWordBoundary_CrossBit63_64(t *testing.T) {
	bitmap := new(big.Int)

	// Allocate range crossing the 64-bit word boundary
	allocateCUs(bitmap, 60, 8) // bits 60-67

	for i := 60; i < 68; i++ {
		if bitmap.Bit(i) != 1 {
			t.Errorf("bit %d should be set", i)
		}
	}
	if bitmap.Bit(59) != 0 {
		t.Error("bit 59 should be clear")
	}
	if bitmap.Bit(68) != 0 {
		t.Error("bit 68 should be clear")
	}

	// Request 60 contiguous: first 60 (0-59) are free, so starts at 0
	got, _ := findFreeCURange(bitmap, 128, 60)
	if got != 0 {
		t.Errorf("findFreeCURange() = %d, want 0", got)
	}

	// Request 61 contiguous: 0-59 is only 60, next free block starts at 68 (68-127 = 60 CUs)
	got, _ = findFreeCURange(bitmap, 128, 61)
	if got != -1 {
		// 0-59 = 60 free, 68-127 = 60 free, neither is 61
		t.Errorf("findFreeCURange() = %d, want -1", got)
	}
}

func TestWordBoundary_VariousTotalCUs(t *testing.T) {
	tests := []struct {
		name     string
		totalCUs int
	}{
		{"63 CUs", 63},
		{"64 CUs", 64},
		{"65 CUs", 65},
		{"128 CUs", 128},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bitmap := new(big.Int)

			// Allocate all, verify count
			allocateCUs(bitmap, 0, tt.totalCUs)
			free := countFreeCUs(bitmap, tt.totalCUs)
			if free != 0 {
				t.Errorf("countFreeCUs = %d, want 0", free)
			}

			// Free all, verify count
			freeCUs(bitmap, 0, tt.totalCUs)
			free = countFreeCUs(bitmap, tt.totalCUs)
			if free != tt.totalCUs {
				t.Errorf("countFreeCUs = %d, want %d", free, tt.totalCUs)
			}

			// Allocate exactly all CUs
			got, _ := findFreeCURange(bitmap, tt.totalCUs, tt.totalCUs)
			if got != 0 {
				t.Errorf("findFreeCURange(all) = %d, want 0", got)
			}

			// Cannot allocate totalCUs+1
			got, _ = findFreeCURange(bitmap, tt.totalCUs, tt.totalCUs+1)
			if got != -1 {
				t.Errorf("findFreeCURange(all+1) = %d, want -1", got)
			}
		})
	}
}

// --- getCUBitmap tests ---

func TestGetCUBitmap_Initializes(t *testing.T) {
	customInfo := map[string]any{}
	bitmap := getCUBitmap(customInfo, 304)

	if bitmap == nil {
		t.Fatal("getCUBitmap should return non-nil bitmap")
	}
	if bitmap.BitLen() != 0 {
		t.Error("new bitmap should be empty")
	}
	if customInfo[CUTotalKey] != 304 {
		t.Errorf("CUTotalKey = %v, want 304", customInfo[CUTotalKey])
	}
}

func TestGetCUBitmap_ReturnsExisting(t *testing.T) {
	existing := new(big.Int)
	existing.SetBit(existing, 5, 1)
	customInfo := map[string]any{
		CUBitmapKey: existing,
	}

	bitmap := getCUBitmap(customInfo, 304)
	if bitmap != existing {
		t.Error("getCUBitmap should return existing bitmap")
	}
}

// --- getTotalCUs tests ---

func TestGetTotalCUs_Unconfigured(t *testing.T) {
	customInfo := map[string]any{}
	got := getTotalCUs(customInfo)
	if got != 0 {
		t.Errorf("getTotalCUs() = %d, want 0 (unconfigured)", got)
	}
}

func TestGetTotalCUs_Int(t *testing.T) {
	customInfo := map[string]any{CUTotalKey: 128}
	got := getTotalCUs(customInfo)
	if got != 128 {
		t.Errorf("getTotalCUs() = %d, want 128", got)
	}
}

func TestGetTotalCUs_Int64(t *testing.T) {
	customInfo := map[string]any{CUTotalKey: int64(256)}
	got := getTotalCUs(customInfo)
	if got != 256 {
		t.Errorf("getTotalCUs() = %d, want 256", got)
	}
}
