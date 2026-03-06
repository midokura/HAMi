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

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"k8s.io/klog/v2"
	kubeletdevicepluginv1beta1 "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/Project-HAMi/HAMi/pkg/device-plugin/amddevice"
	"github.com/Project-HAMi/HAMi/pkg/device/amd"
	"github.com/Project-HAMi/HAMi/pkg/util/client"
)

func main() {
	resourceName := flag.String("resource-name", "amd.com/gpu", "Resource name to register with kubelet")
	deviceCount := flag.Int("device-count", 0, "Number of GPU devices (0 = auto-detect from node capacity)")
	logLevel := flag.String("log-level", "", "LIBHIP_LOG_LEVEL for libamvgpu.so (1=error,2=warn,3=info,4=debug)")
	kubeletSocket := flag.String("kubelet-socket", kubeletdevicepluginv1beta1.KubeletSocket, "Kubelet gRPC socket path")

	klog.InitFlags(nil)
	flag.Parse()

	klog.InfoS("AMD GPU Device Plugin starting",
		"resourceName", *resourceName,
		"deviceCount", *deviceCount)

	// Initialize AMD device type (registers InRequestDevices mapping)
	amd.InitAMDGPUDevice(amd.AMDConfig{
		ResourceCountName:  *resourceName,
		ResourceMemoryName: "amd.com/gpumem",
		ResourceCoreName:   "amd.com/gpucores",
	})

	// Initialize Kubernetes client
	client.InitGlobalClient()

	// Auto-detect device count from node if not specified
	count := *deviceCount
	if count <= 0 {
		count = detectDeviceCount()
		if count <= 0 {
			klog.Fatal("No AMD GPU devices detected. Use --device-count to specify manually.")
		}
	}

	plugin := amddevice.NewAMDDevicePlugin(*resourceName, count, *logLevel)
	if err := plugin.Start(*kubeletSocket); err != nil {
		klog.Fatalf("Failed to start AMD device plugin: %v", err)
	}
	defer plugin.Stop()

	// Handle OS signals for graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	klog.Infof("Received signal %v, shutting down", sig)
}

// detectDeviceCount tries to detect the number of AMD GPUs by checking /dev/dri/renderD* nodes.
func detectDeviceCount() int {
	// Try environment variable first
	if envCount := os.Getenv("AMD_GPU_COUNT"); envCount != "" {
		if n, err := strconv.Atoi(envCount); err == nil && n > 0 {
			klog.InfoS("Using AMD_GPU_COUNT from environment", "count", n)
			return n
		}
	}

	// Count renderD* nodes under /dev/dri/
	entries, err := os.ReadDir("/dev/dri")
	if err != nil {
		klog.Warningf("Cannot read /dev/dri: %v", err)
		return 0
	}
	count := 0
	for _, e := range entries {
		if len(e.Name()) > 7 && e.Name()[:7] == "renderD" {
			count++
		}
	}
	if count > 0 {
		klog.InfoS("Auto-detected AMD GPU devices", "count", count)
	}
	return count
}

func init() {
	// Ensure NODE_NAME is set
	if os.Getenv("NODE_NAME") == "" {
		hostname, err := os.Hostname()
		if err == nil {
			os.Setenv("NODE_NAME", hostname)
			fmt.Fprintf(os.Stderr, "Warning: NODE_NAME not set, using hostname: %s\n", hostname)
		}
	}
}
