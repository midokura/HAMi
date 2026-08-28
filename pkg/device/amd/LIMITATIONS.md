# Known Limitations — AMD GPU Virtualization (HAMi-core)

This document describes known limitations of HAMi-core's AMD GPU resource isolation mechanisms.

## 1. CU Mask: Userspace Enforcement Only

**Mechanism:** CU (Compute Unit) partitioning uses `ROC_GLOBAL_CU_MASK`, an environment variable read by the ROCm runtime.

**Limitation:** Because it is an environment variable, a process running inside a container can override or unset it. This means CU partitioning is a **soft limit** — it relies on cooperative tenants and cannot prevent a malicious workload from consuming all CUs.

**Comparison:**
| Resource | Enforcement level | Bypassable from container? |
|----------|------------------|---------------------------|
| CPU (cgroups) | Kernel | No |
| Memory (cgroups) | Kernel | No |
| GPU memory (LD_AUDIT) | Userspace (library interposition) | Difficult* |
| GPU CU mask (ROC_GLOBAL_CU_MASK) | Userspace (env var) | Yes |

\* GPU memory limiting via `LD_AUDIT` interposes `hipMalloc`/`hipFree` at the dynamic linker level. While not kernel-enforced, bypassing it requires deliberately loading HIP without the audit library, which is significantly harder than changing an environment variable.

**Impact:** In multi-tenant environments, CU isolation should be treated as best-effort. It is effective for fair scheduling among cooperative workloads but does not provide security isolation.

### Path to Kernel-Level CU Enforcement

AMD has proposed kernel-level CU masking via MQD (Micro Queue Descriptor) registers:

- **Mechanism:** Write CU masks to `compute_static_thread_mgmt_se0-3` registers in the MQD, enforced by the GPU hardware command processor
- **Kernel patch (GFX11):** [amd-gfx mailing list](https://www.mail-archive.com/amd-gfx@lists.freedesktop.org/msg135729.html) — adds `set_cu_mask` support to KFD (Kernel Fusion Driver)
- **Interface:** KFD ioctl, allowing a device plugin to set per-queue CU masks directly without relying on environment variables

Once CDNA3 (MI300X) equivalents of these patches land in the upstream kernel and KFD exposes the ioctl, HAMi's device plugin could call the ioctl directly — removing the need for `ROC_GLOBAL_CU_MASK` entirely and achieving parity with Kubernetes' kernel-level resource enforcement model.

**Status (as of March 2026):** GFX11 patches exist; CDNA3/MI300X support is pending.

## 2. GPU Memory Limiting: Userspace Library Interposition

**Mechanism:** `LD_AUDIT` (via `libamvgpu.so`) intercepts HIP memory allocation calls (`hipMalloc`, `hipFree`, `hipMemGetInfo`) at the dynamic linker level.

**Limitation:** This is not kernel-enforced. A process that directly invokes GPU syscalls or loads HIP without the audit library could bypass the limit. However:
- `LD_AUDIT` is set by the container runtime before the entrypoint executes
- Bypassing requires low-level knowledge of the ROCm/KFD driver interface
- The shared memory tracking (`/dev/shm`) provides cross-process accounting within a pod

**Comparison with NVIDIA:** NVIDIA's MIG (Multi-Instance GPU) provides hardware-level memory partitioning. AMD does not currently have an equivalent for CDNA architectures.

## 3. `amd-smi` Reports Physical Resources

`amd-smi` reads GPU information via sysfs/DRM, not through HIP. Since HAMi's interposition operates at the HIP library level, `amd-smi` will report physical GPU memory and CU counts regardless of HAMi limits.

This is expected behavior, not a bug. Applications should use `hipMemGetInfo()` (which is virtualized by HAMi) rather than `amd-smi` for memory availability checks.

## Recommendations

- **Cooperative multi-tenancy:** Current HAMi AMD support is suitable for environments where tenants are trusted (e.g., internal ML teams sharing a GPU cluster)
- **Adversarial multi-tenancy:** Wait for kernel-level CU masking (KFD ioctl) before relying on CU isolation for security boundaries
- **Defense in depth:** Combine HAMi limits with Kubernetes NetworkPolicy, PodSecurityPolicy/Standards, and RBAC to reduce the attack surface
