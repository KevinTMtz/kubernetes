/*
Copyright 2018 The Kubernetes Authors.

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

package pod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	clientset "k8s.io/client-go/kubernetes"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
)

const (
	maxPooledPatchBufferCap = 64 * 1024

	unchangedPatchPrefix = `{"metadata":{"uid":`
	unchangedPatchSuffix = `}}`
	emptyStatusJSON      = `{}`
	statusKeyMid         = `},"status":`
	statusKeyOnlyPrefix  = `{"status":`
)

var podStatusPatchMeta, podStatusPatchMetaErr = strategicpatch.NewPatchMetaFromStruct(v1.PodStatus{})

type podStatusPatchBuffers struct {
	oldBuf bytes.Buffer
	newBuf bytes.Buffer
}

var podStatusPatchBufferPool = sync.Pool{
	New: func() any {
		b := &podStatusPatchBuffers{}
		b.oldBuf.Grow(1024)
		b.newBuf.Grow(1024)
		return b
	},
}

// PatchPodStatus patches pod status. It returns true and avoids an update if the patch contains no changes.
func PatchPodStatus(ctx context.Context, c clientset.Interface, namespace, name string, uid types.UID, oldPodStatus, newPodStatus v1.PodStatus) (*v1.Pod, []byte, bool, error) {
	patchBytes, unchanged, err := preparePatchBytesForPodStatus(namespace, name, uid, oldPodStatus, newPodStatus)
	if err != nil {
		return nil, nil, false, err
	}
	if unchanged {
		return nil, patchBytes, true, nil
	}

	updatedPod, err := c.CoreV1().Pods(namespace).Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	if err != nil {
		return nil, nil, false, fmt.Errorf("failed to patch status %q for pod %q/%q: %v", patchBytes, namespace, name, err)
	}
	return updatedPod, patchBytes, false, nil
}

func trimJSONEncoderNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return b[:len(b)-1]
	}
	return b
}

func buildUnchangedPodStatusPatch(uid types.UID) ([]byte, bool) {
	if len(uid) == 0 {
		return []byte(emptyStatusJSON), false
	}
	patchBytes := make([]byte, 0, len(unchangedPatchPrefix)+len(uid)+2+len(unchangedPatchSuffix))
	patchBytes = append(patchBytes, unchangedPatchPrefix...)
	patchBytes = strconv.AppendQuote(patchBytes, string(uid))
	patchBytes = append(patchBytes, unchangedPatchSuffix...)
	return patchBytes, true
}

func preparePatchBytesForPodStatus(namespace, name string, uid types.UID, oldPodStatus, newPodStatus v1.PodStatus) ([]byte, bool, error) {
	bufs := podStatusPatchBufferPool.Get().(*podStatusPatchBuffers)
	bufs.oldBuf.Reset()
	bufs.newBuf.Reset()
	defer func() {
		if bufs.oldBuf.Cap() <= maxPooledPatchBufferCap && bufs.newBuf.Cap() <= maxPooledPatchBufferCap {
			bufs.oldBuf.Reset()
			bufs.newBuf.Reset()
			podStatusPatchBufferPool.Put(bufs)
		}
	}()

	if err := json.NewEncoder(&bufs.oldBuf).Encode(&oldPodStatus); err != nil {
		return nil, false, fmt.Errorf("failed to Marshal oldData for pod %q/%q: %v", namespace, name, err)
	}
	oldStatusBytes := trimJSONEncoderNewline(bufs.oldBuf.Bytes())

	if err := json.NewEncoder(&bufs.newBuf).Encode(&newPodStatus); err != nil {
		return nil, false, fmt.Errorf("failed to Marshal newData for pod %q/%q: %v", namespace, name, err)
	}
	newStatusBytes := trimJSONEncoderNewline(bufs.newBuf.Bytes())

	if bytes.Equal(oldStatusBytes, newStatusBytes) {
		patchBytes, unchanged := buildUnchangedPodStatusPatch(uid)
		return patchBytes, unchanged, nil
	}

	var (
		statusPatch []byte
		err         error
	)
	if podStatusPatchMetaErr == nil {
		statusPatch, err = strategicpatch.CreateTwoWayMergePatchUsingLookupPatchMeta(oldStatusBytes, newStatusBytes, podStatusPatchMeta)
	} else {
		statusPatch, err = strategicpatch.CreateTwoWayMergePatch(oldStatusBytes, newStatusBytes, v1.PodStatus{})
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to CreateTwoWayMergePatch for pod %q/%q: %v", namespace, name, err)
	}
	if bytes.Equal(statusPatch, []byte(emptyStatusJSON)) {
		patchBytes, unchanged := buildUnchangedPodStatusPatch(uid)
		return patchBytes, unchanged, nil
	}

	if len(uid) == 0 {
		patchBytes := make([]byte, 0, len(statusKeyOnlyPrefix)+len(statusPatch)+1)
		patchBytes = append(patchBytes, statusKeyOnlyPrefix...)
		patchBytes = append(patchBytes, statusPatch...)
		patchBytes = append(patchBytes, '}')
		return patchBytes, false, nil
	}

	patchBytes := make([]byte, 0, len(unchangedPatchPrefix)+len(uid)+2+len(statusKeyMid)+len(statusPatch)+1)
	patchBytes = append(patchBytes, unchangedPatchPrefix...)
	patchBytes = strconv.AppendQuote(patchBytes, string(uid))
	patchBytes = append(patchBytes, statusKeyMid...)
	patchBytes = append(patchBytes, statusPatch...)
	patchBytes = append(patchBytes, '}')
	return patchBytes, false, nil
}

// ReplaceOrAppendPodCondition replaces the first pod condition with equal type or appends if there is none
func ReplaceOrAppendPodCondition(conditions []v1.PodCondition, condition *v1.PodCondition) []v1.PodCondition {
	if i, _ := podutil.GetPodConditionFromList(conditions, condition.Type); i >= 0 {
		conditions[i] = *condition
	} else {
		conditions = append(conditions, *condition)
	}
	return conditions
}
