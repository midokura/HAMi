## Introduction

**We now support AMD GPU by implementing most device-sharing features as nvidia-GPU**, including:

***GPU sharing***: Each task can allocate a portion of GPU instead of a whole GPU card, thus GPU can be shared among multiple tasks.

***Device Memory Control***: GPUs can be allocated with certain device memory size (e.g. `amd.com/gpumem: 48000` allocates 48GB VRAM) and have made it that it does not exceed the boundary.

***Device Compute Unit limitation***: GPUs can be allocated with a specific number of Compute Units (CUs) via exclusive spatial partitioning (e.g. `amd.com/gpucores: 152` allocates 152 CUs out of 304 on MI300X).

***GPU UUID Specification***: You can specify which GPU to use or to avoid for a certain task, by setting `amd.com/use-gpu-uuid` or `amd.com/nouse-gpu-uuid` annotations.

## Prerequisites

* AMD GPU with ROCm driver >= 6.2
* ROCm runtime installed on the host (`rocminfo` must be available)
* Kubernetes (k3s >= 1.26 verified, standard k8s >= 1.18)
* Docker (for building images)
* helm >= 3.0 (for Helm-based deployment)

## Quick Start (k3s)

This section walks through a verified deployment on k3s. For standard Kubernetes with Helm, see [Helm-based Deployment](#helm-based-deployment) below.

### 1. Install k3s

```bash
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--disable=traefik" sh -
```

### 2. Clone HAMi and build components

```bash
git clone https://github.com/Project-HAMi/HAMi.git
cd HAMi
git submodule update --init libvgpu
```

Build `libamvgpu.so` (memory virtualization library):

```bash
cd libvgpu
docker build -f Dockerfile.hip -t libamvgpu-builder .
docker run --rm -v $(pwd)/dist:/dist libamvgpu-builder
sudo mkdir -p /opt/hami
sudo cp dist/libamvgpu.so /opt/hami/
cd ..
```

Build Docker images for the scheduler and device plugin:

```bash
docker build -f docker/Dockerfile.scheduler-build -t hami-scheduler:latest .
docker build -f docker/Dockerfile.amd-device-plugin -t hami-amd-device-plugin:latest .
```

Import images into k3s containerd:

```bash
docker save hami-scheduler:latest | k3s ctr images import -
docker save hami-amd-device-plugin:latest | k3s ctr images import -
```

### 3. Configure k3s scheduler extender

Copy the extender configuration and restart k3s:

```bash
sudo cp deployments/k3s-scheduler-extender-config.yaml /etc/rancher/k3s/scheduler-extender-config.yaml

sudo tee /etc/rancher/k3s/config.yaml > /dev/null <<'EOF'
disable:
  - traefik
kube-scheduler-arg:
  - "config=/etc/rancher/k3s/scheduler-extender-config.yaml"
EOF

sudo systemctl restart k3s
```

Wait for the node to become Ready:

```bash
kubectl get nodes --watch
```

### 4. Deploy scheduler and device plugin

Label your GPU node:

```bash
kubectl label nodes $(hostname) gpu=on
```

Deploy the HAMi scheduler and AMD device plugin:

```bash
kubectl apply -f deployments/amd-scheduler.yaml
kubectl apply -f deployments/amd-device-plugin.yaml
```

### 5. Verify

Check that pods are running:

```
kubectl get pods -n kube-system -l 'app in (hami-scheduler, hami-amd-device-plugin)'
```

Check that GPUs are detected:

```
kubectl get node -o yaml | grep -A5 "hami.io/node-amd-register"
```

Check the node GPU capacity:

```
kubectl get node -o custom-columns='NAME:.metadata.name,GPU:.status.capacity.amd\.com/gpu'
```

If the GPU column shows a number greater than 0, your installation is successful. You can try the [example pod](../examples/amd/default_use.yaml).

> **Troubleshooting:** If `amd.com/gpu` shows `0` after deploying the device plugin, restart it:
> ```
> kubectl rollout restart daemonset/hami-amd-device-plugin -n kube-system
> ```

---

## Helm-based Deployment

For standard Kubernetes clusters, deploy HAMi using Helm:

```
helm repo add hami-charts https://project-hami.github.io/HAMi/
helm install hami hami-charts/hami -n kube-system
```

The Helm chart deploys the scheduler (with kube-scheduler sidecar and admission webhook) and the NVIDIA device plugin. For AMD GPUs, you still need to build and deploy the AMD device plugin separately — follow steps 2 and 4 from the Quick Start above.

Customize your installation by adjusting the [configs](config.md). The default AMD GPU resource names are:

| Config | Default |
|--------|---------|
| `resourceCountName` | `amd.com/gpu` |
| `resourceMemoryName` | `amd.com/gpumem` |
| `resourceCoreName` | `amd.com/gpucores` |

---

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
