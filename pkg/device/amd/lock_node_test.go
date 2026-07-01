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
	"context"
	"testing"

	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Project-HAMi/HAMi/pkg/util/client"
	"github.com/Project-HAMi/HAMi/pkg/util/nodelock"
)

func gpuPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "gpu-app",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					"amd.com/gpu": *resource.NewQuantity(1, resource.DecimalSI),
				},
			},
		}}},
	}
}

// Test_LockNode verifies that LockNode acquires the shared node lock only for
// pods that actually request an AMD device, and skips it otherwise.
func Test_LockNode(t *testing.T) {
	config := AMDConfig{ResourceCountName: "amd.com/gpu"}
	tests := []struct {
		name    string
		pod     *corev1.Pod
		hasLock bool
	}{
		{
			name: "no GPU container — skip lock",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-no-gpu", Namespace: "default"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
			hasLock: false,
		},
		{
			name:    "has GPU container — acquires lock",
			pod:     gpuPod("pod-gpu"),
			hasLock: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client.KubeClient = fake.NewClientset()
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node", Annotations: map[string]string{}},
			}
			_, err := client.KubeClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
			assert.NilError(t, err)

			dev := InitAMDGPUDevice(config)
			err = dev.LockNode(node, tt.pod)
			assert.NilError(t, err)

			updated, err := client.KubeClient.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
			assert.NilError(t, err)
			_, ok := updated.Annotations[nodelock.NodeLockKey]
			assert.Equal(t, ok, tt.hasLock)
		})
	}
}

// Test_LockNode_ReleaseRoundTrip verifies a lock taken for a GPU pod is removed
// again by ReleaseNodeLock.
func Test_LockNode_ReleaseRoundTrip(t *testing.T) {
	client.KubeClient = fake.NewClientset()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node", Annotations: map[string]string{}},
	}
	_, err := client.KubeClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
	assert.NilError(t, err)

	dev := InitAMDGPUDevice(AMDConfig{ResourceCountName: "amd.com/gpu"})
	pod := gpuPod("pod-gpu")

	assert.NilError(t, dev.LockNode(node, pod))
	locked, err := client.KubeClient.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
	assert.NilError(t, err)
	if _, ok := locked.Annotations[nodelock.NodeLockKey]; !ok {
		t.Fatalf("expected node lock to be set")
	}

	assert.NilError(t, dev.ReleaseNodeLock(locked, pod))
	released, err := client.KubeClient.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
	assert.NilError(t, err)
	if _, ok := released.Annotations[nodelock.NodeLockKey]; ok {
		t.Errorf("expected node lock to be released")
	}
}
