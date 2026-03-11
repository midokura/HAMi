## Introduction

**We now support AMD GPU by implementing most device-sharing features as nvidia-GPU**, including:

***GPU sharing***: Each task can allocate a portion of GPU instead of a whole GPU card, thus GPU can be shared among multiple tasks.

***Device Memory Control***: GPUs can be allocated with certain device memory size (e.g. `amd.com/gpumem: 48000` allocates 48GB VRAM) and have made it that it does not exceed the boundary.

***Device Compute Unit limitation***: GPUs can be allocated with a specific number of Compute Units (CUs) via exclusive spatial partitioning (e.g. `amd.com/gpucores: 152` allocates 152 CUs out of 304 on MI300X).

***GPU UUID Specification***: You can specify which GPU to use or to avoid for a certain task, by setting `amd.com/use-gpu-uuid` or `amd.com/nouse-gpu-uuid` annotations.

## Prerequisites

* AMD GPU with ROCm driver >= 6.2
* ROCm runtime installed on the host (`rocminfo` must be available)
* Kubernetes version >= 1.18
* helm > 3.0

## Enabling AMD GPU-sharing Support

### 1. Deploy HAMi

Label your AMD GPU nodes for scheduling:

```
kubectl label nodes {nodeid} gpu=on
```

Deploy HAMi:

```
helm install hami hami-charts/hami -n kube-system
```

Customize your installation by adjusting the [configs](config.md). The default AMD GPU resource names are:

| Config | Default |
|--------|---------|
| `resourceCountName` | `amd.com/gpu` |
| `resourceMemoryName` | `amd.com/gpumem` |
| `resourceCoreName` | `amd.com/gpucores` |

### 2. Deploy AMD device plugin

The AMD device plugin must be deployed separately on each GPU node. It detects GPU specifications (CU count, VRAM size) via `rocminfo` and registers them as node annotations.

Build and deploy the device plugin:

```bash
# Build the device plugin image
docker build -f docker/Dockerfile.amd-device-plugin -t hami-amd-device-plugin:latest .

# Build libamvgpu.so (memory virtualization library)
cd libvgpu
docker build -f Dockerfile.hip -t libamvgpu-builder .
docker run --rm -v $(pwd)/dist:/dist libamvgpu-builder
sudo mkdir -p /opt/hami
sudo cp dist/libamvgpu.so /opt/hami/
```

Deploy the device plugin DaemonSet:

```bash
kubectl apply -f deployments/device-plugin.yaml
```

### 3. Verify

```
kubectl get pods -n kube-system
kubectl get node -o yaml | grep -A5 "hami.io/node-amd-register"
```

If `hami-amd-device-plugin` pods are in the *Running* state and node annotations show GPU information, your installation is successful.

## Running AMD GPU jobs

AMD GPUs can now be requested by a container
using the `amd.com/gpu`, `amd.com/gpumem` and `amd.com/gpucores` resource types:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: amd-gpu-demo
spec:
  containers:
    - name: demo
      image: rocm/pytorch:rocm6.2_ubuntu22.04_py3.10_pytorch_release_2.3.0
      command: ["python3", "-c"]
      args:
        - |
          import torch
          print(f"GPU: {torch.cuda.get_device_name(0)}")
          free, total = torch.cuda.mem_get_info()
          print(f"Memory: {free/1e9:.1f}GB free / {total/1e9:.1f}GB total")
      resources:
        limits:
          amd.com/gpu: 1        # requesting 1 GPU
          amd.com/gpumem: 48000 # 48GB device memory limit
          amd.com/gpucores: 152 # 152 Compute Units (half of MI300X)
```

You can also request memory-only or CU-only:

```yaml
      # Memory limit only (all CUs available)
      resources:
        limits:
          amd.com/gpu: 1
          amd.com/gpumem: 48000
```

```yaml
      # CU limit only (full memory available)
      resources:
        limits:
          amd.com/gpu: 1
          amd.com/gpucores: 152
```

## Verify GPU virtualization inside the container

Check the environment variables injected by the device plugin:

```
echo "Memory limit: $HIP_DEVICE_MEMORY_LIMIT_0"
echo "CU mask: $ROC_GLOBAL_CU_MASK"
echo "LD_AUDIT: $LD_AUDIT"
```

Verify with PyTorch:

```
python3 -c "
import torch
free, total = torch.cuda.mem_get_info()
print(f'Memory: {free/1e9:.1f}GB / {total/1e9:.1f}GB')
"
```

If memory shows the limited value (e.g. 48GB instead of 192GB), GPU virtualization is working.

## Notes

1. GPU-sharing in init containers is not supported. Pods with `amd.com/gpumem` in init containers will never be scheduled.

2. CU partitioning uses spatial isolation (exclusive CU assignment), not time-slicing. Each pod gets a non-overlapping range of CUs. This provides predictable performance but limits the number of concurrent tenants to `total_CUs / requested_CUs`.

3. Memory virtualization uses `LD_AUDIT` (not `LD_PRELOAD`). This is required for ROCm 7.x compatibility. The device plugin injects `LD_AUDIT=/opt/hami/libamvgpu.so` automatically.

4. `ROC_GLOBAL_CU_MASK` must use hex-only format (e.g. `0xFFFF`). The `GPU_INDEX:0xHEX` prefix format causes parsing errors on multi-XCD GPUs like MI300X.
