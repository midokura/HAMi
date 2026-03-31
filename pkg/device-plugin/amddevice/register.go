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
	"os/exec"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device/amd"
	"github.com/Project-HAMi/HAMi/pkg/util"
)

// GPUInfo holds parsed rocminfo data for a single AMD GPU agent.
type GPUInfo struct {
	MarketingName string
	GfxName       string
	ComputeUnits  int
	VRAMMb        int32
}

// parseRocmInfo parses the output of `rocminfo` and returns GPU information.
// It detects GPU agents, their compute units, marketing name, gfx name, and VRAM.
func parseRocmInfo(output string) ([]GPUInfo, error) {
	lines := strings.Split(output, "\n")
	var gpus []GPUInfo
	var current *GPUInfo
	inGPUAgent := false
	inPool := false
	isCoarseGrained := false

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])

		// Detect start of a new agent section (e.g. "Agent 1", "Agent 2")
		if strings.HasPrefix(line, "Agent ") {
			// Reset state and create a new candidate GPUInfo
			inGPUAgent = false
			inPool = false
			isCoarseGrained = false
			current = &GPUInfo{}
			continue
		}

		if current == nil {
			continue
		}

		// Detect device type - Marketing Name and Name come before this in rocminfo output
		if strings.HasPrefix(line, "Device Type:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Device Type:"))
			if val == "GPU" {
				inGPUAgent = true
			} else {
				// Not a GPU agent, discard
				current = nil
			}
			continue
		}

		// Parse Name line containing gfx
		if strings.HasPrefix(line, "Name:") && !inPool {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			if strings.Contains(val, "gfx") {
				current.GfxName = val
			}
			continue
		}

		// Parse Marketing Name
		if strings.HasPrefix(line, "Marketing Name:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Marketing Name:"))
			current.MarketingName = val
			continue
		}

		// Parse Compute Unit count
		if strings.HasPrefix(line, "Compute Unit:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Compute Unit:"))
			cu, err := strconv.Atoi(val)
			if err == nil {
				current.ComputeUnits = cu
			}
			continue
		}

		// Detect pool sections
		if strings.HasPrefix(line, "Pool ") {
			inPool = true
			isCoarseGrained = false
			continue
		}

		// Detect COARSE GRAINED segment (VRAM pool)
		if inPool && strings.HasPrefix(line, "Segment:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Segment:"))
			if strings.Contains(val, "GLOBAL") && strings.Contains(val, "COARSE GRAINED") {
				isCoarseGrained = true
			} else {
				isCoarseGrained = false
			}
			continue
		}

		// Parse Size from COARSE GRAINED pool (GPU agents only)
		if inGPUAgent && inPool && isCoarseGrained && strings.HasPrefix(line, "Size:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Size:"))
			// Format: "200998912(0xbfa0000) KB"
			parts := strings.Fields(val)
			if len(parts) >= 1 {
				// Remove any parenthetical hex value
				sizeStr := parts[0]
				if idx := strings.Index(sizeStr, "("); idx > 0 {
					sizeStr = sizeStr[:idx]
				}
				sizeKB, err := strconv.ParseInt(sizeStr, 10, 64)
				if err == nil {
					current.VRAMMb = int32(sizeKB / 1024)
				}
			}
			// VRAM found - finalize this GPU
			gpus = append(gpus, *current)
			// Keep current pointer valid for remaining lines in this agent
			isCoarseGrained = false
			continue
		}
	}

	return gpus, nil
}

// runRocmInfo executes rocminfo and returns its stdout output.
func runRocmInfo() (string, error) {
	cmd := exec.Command("rocminfo")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("rocminfo execution failed: %w", err)
	}
	return string(out), nil
}

// getAPIDevices detects AMD GPUs via rocminfo and returns DeviceInfo for each.
func (p *AMDDevicePlugin) getAPIDevices() ([]*device.DeviceInfo, error) {
	nodeName := util.NodeName
	if nodeName == "" {
		return nil, fmt.Errorf("NODE_NAME not set")
	}

	output, err := runRocmInfo()
	if err != nil {
		return nil, err
	}

	gpus, err := parseRocmInfo(output)
	if err != nil {
		return nil, fmt.Errorf("failed to parse rocminfo output: %w", err)
	}

	if len(gpus) == 0 {
		return nil, fmt.Errorf("no AMD GPU agents found in rocminfo output")
	}

	res := make([]*device.DeviceInfo, 0, len(gpus))
	for i, gpu := range gpus {
		id := fmt.Sprintf("%s-%s-%d", nodeName, amd.AMDDevice, i)

		res = append(res, &device.DeviceInfo{
			ID:      id,
			Index:   uint(i),
			Count:   int32(p.deviceCount),
			Devmem:  gpu.VRAMMb,
			Devcore: int32(gpu.ComputeUnits),
			Type:    amd.AMDDevice, // Must match scheduler's type for Fit() matching
			Numa:    0,
			Health:  true,
		})
		klog.Infof("rocminfo detected GPU %d: id=%s, marketing=%s, gfx=%s, CUs=%d, VRAM=%dMB",
			i, id, gpu.MarketingName, gpu.GfxName, gpu.ComputeUnits, gpu.VRAMMb)
	}

	return res, nil
}

// RegisterInAnnotation detects AMD GPUs and writes their info to node annotations.
func (p *AMDDevicePlugin) RegisterInAnnotation() error {
	devices, err := p.getAPIDevices()
	if err != nil {
		return fmt.Errorf("failed to get AMD GPU devices: %w", err)
	}

	klog.InfoS("AMD RegisterInAnnotation: detected devices", "count", len(devices))

	node, err := util.GetNode(util.NodeName)
	if err != nil {
		klog.Errorln("get node error", err.Error())
		return err
	}

	encodedDevices := device.MarshalNodeDevices(devices)
	if encodedDevices == p.deviceCache {
		return nil
	}
	p.deviceCache = encodedDevices

	annos := make(map[string]string)
	annos[amd.RegisterAnnos] = encodedDevices
	klog.Infof("Patching node %s with AMD device annotation: %s", util.NodeName, encodedDevices)

	err = util.PatchNodeAnnotations(node, annos)
	if err != nil {
		klog.Errorln("patch node error", err.Error())
	}
	return err
}

// WatchAndRegister periodically calls RegisterInAnnotation every 30 seconds.
func (p *AMDDevicePlugin) WatchAndRegister() {
	klog.Info("Starting AMD WatchAndRegister")
	errorSleepInterval := time.Second * 5
	successSleepInterval := time.Second * 30

	for {
		err := p.RegisterInAnnotation()
		if err != nil {
			klog.Errorf("Failed to register AMD annotation: %v", err)
			klog.Infof("Retrying in %v...", errorSleepInterval)
			time.Sleep(errorSleepInterval)
		} else {
			klog.Infof("Successfully registered AMD annotation. Next check in %v...", successSleepInterval)
			time.Sleep(successSleepInterval)
		}
	}
}
