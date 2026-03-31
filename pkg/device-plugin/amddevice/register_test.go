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
	"testing"
)

// Realistic rocminfo output from an MI300X GPU (trimmed for test readability).
const rocminfoMI300X = `HSA System Attributes
Runtime Version:                  1.14
Runtime Ext Version:              1.6
System Timestamp Freq. (MHz):     100
Sig. Max Wait Duration (ns):      18446744073709551615
Machine Model:                    LARGE
System Endianness:                LITTLE

==========
HSA Agents
==========
*******
Agent 1
*******
  Name:                    host-cpu
  Marketing Name:          AMD EPYC 9534 64-Core Processor
  Vendor Name:             CPU
  Feature:                 None specified
  Device Type:             CPU
  Pool Info:
    Pool 1
      Segment:                 GLOBAL; FLAGS: KERNARG, FINE GRAINED
      Size:                    536870912(0x20000000) KB

*******
Agent 2
*******
  Name:                    gfx942
  Marketing Name:          AMD Instinct MI300X
  Vendor Name:             AMD
  Feature:                 KERNEL_DISPATCH
  Device Type:             GPU
  Compute Unit:            304
  Pool Info:
    Pool 1
      Segment:                 GLOBAL; FLAGS: FINE GRAINED
      Size:                    196608(0x30000) KB
    Pool 2
      Segment:                 GLOBAL; FLAGS: COARSE GRAINED
      Size:                    196608(0x30000) KB
    Pool 3
      Segment:                 GROUP
      Size:                    64(0x40) KB
`

// Two GPUs on same node.
const rocminfoTwoGPUs = `HSA System Attributes
Runtime Version:                  1.14

==========
HSA Agents
==========
*******
Agent 1
*******
  Name:                    host-cpu
  Marketing Name:          AMD EPYC 9534 64-Core Processor
  Device Type:             CPU

*******
Agent 2
*******
  Name:                    gfx942
  Marketing Name:          AMD Instinct MI300X
  Device Type:             GPU
  Compute Unit:            304
  Pool Info:
    Pool 1
      Segment:                 GLOBAL; FLAGS: COARSE GRAINED
      Size:                    196608(0x30000) KB

*******
Agent 3
*******
  Name:                    gfx942
  Marketing Name:          AMD Instinct MI300X
  Device Type:             GPU
  Compute Unit:            304
  Pool Info:
    Pool 1
      Segment:                 GLOBAL; FLAGS: COARSE GRAINED
      Size:                    196608(0x30000) KB
`

// MI210 with different CU count and VRAM.
const rocminfoMI210 = `==========
HSA Agents
==========
*******
Agent 1
*******
  Name:                    host-cpu
  Device Type:             CPU

*******
Agent 2
*******
  Name:                    gfx90a
  Marketing Name:          AMD Instinct MI210
  Device Type:             GPU
  Compute Unit:            104
  Pool Info:
    Pool 1
      Segment:                 GLOBAL; FLAGS: COARSE GRAINED
      Size:                    65536(0x10000) KB
`

// CPU-only node.
const rocminfoCPUOnly = `==========
HSA Agents
==========
*******
Agent 1
*******
  Name:                    host-cpu
  Marketing Name:          AMD EPYC 9534 64-Core Processor
  Device Type:             CPU
`

func TestParseRocmInfo_MI300X(t *testing.T) {
	gpus, err := parseRocmInfo(rocminfoMI300X)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(gpus))
	}
	gpu := gpus[0]
	if gpu.MarketingName != "AMD Instinct MI300X" {
		t.Errorf("MarketingName = %q, want %q", gpu.MarketingName, "AMD Instinct MI300X")
	}
	if gpu.GfxName != "gfx942" {
		t.Errorf("GfxName = %q, want %q", gpu.GfxName, "gfx942")
	}
	if gpu.ComputeUnits != 304 {
		t.Errorf("ComputeUnits = %d, want 304", gpu.ComputeUnits)
	}
	// 196608 KB = 192 MB
	if gpu.VRAMMb != 192 {
		t.Errorf("VRAMMb = %d, want 192", gpu.VRAMMb)
	}
}

func TestParseRocmInfo_TwoGPUs(t *testing.T) {
	gpus, err := parseRocmInfo(rocminfoTwoGPUs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("expected 2 GPUs, got %d", len(gpus))
	}
	for i, gpu := range gpus {
		if gpu.ComputeUnits != 304 {
			t.Errorf("GPU %d: ComputeUnits = %d, want 304", i, gpu.ComputeUnits)
		}
		if gpu.VRAMMb != 192 {
			t.Errorf("GPU %d: VRAMMb = %d, want 192", i, gpu.VRAMMb)
		}
	}
}

func TestParseRocmInfo_MI210(t *testing.T) {
	gpus, err := parseRocmInfo(rocminfoMI210)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(gpus))
	}
	gpu := gpus[0]
	if gpu.MarketingName != "AMD Instinct MI210" {
		t.Errorf("MarketingName = %q, want %q", gpu.MarketingName, "AMD Instinct MI210")
	}
	if gpu.GfxName != "gfx90a" {
		t.Errorf("GfxName = %q, want %q", gpu.GfxName, "gfx90a")
	}
	if gpu.ComputeUnits != 104 {
		t.Errorf("ComputeUnits = %d, want 104", gpu.ComputeUnits)
	}
	// 65536 KB = 64 MB
	if gpu.VRAMMb != 64 {
		t.Errorf("VRAMMb = %d, want 64", gpu.VRAMMb)
	}
}

func TestParseRocmInfo_CPUOnly(t *testing.T) {
	gpus, err := parseRocmInfo(rocminfoCPUOnly)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gpus) != 0 {
		t.Errorf("expected 0 GPUs, got %d", len(gpus))
	}
}

func TestParseRocmInfo_Empty(t *testing.T) {
	gpus, err := parseRocmInfo("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gpus) != 0 {
		t.Errorf("expected 0 GPUs, got %d", len(gpus))
	}
}

func TestParseRocmInfo_SkipsFineGrainedPool(t *testing.T) {
	// The MI300X output has both FINE GRAINED and COARSE GRAINED pools.
	// Only COARSE GRAINED should be used for VRAM size.
	gpus, err := parseRocmInfo(rocminfoMI300X)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(gpus))
	}
	// Should use COARSE GRAINED pool size (196608 KB = 192 MB),
	// not FINE GRAINED pool.
	if gpus[0].VRAMMb != 192 {
		t.Errorf("VRAMMb = %d, want 192 (COARSE GRAINED pool)", gpus[0].VRAMMb)
	}
}
