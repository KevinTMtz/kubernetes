/*
Copyright 2015 The Kubernetes Authors.

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

package exec

import (
	"bytes"
	"errors"
	"sync"

	remote "k8s.io/cri-client/pkg"
	"k8s.io/kubernetes/pkg/kubelet/util/ioutils"
	"k8s.io/kubernetes/pkg/probe"

	"k8s.io/klog/v2"
	"k8s.io/utils/exec"
)

const (
	maxReadLength = 10 * 1 << 10 // 10KB
)

type limitedBuffer struct {
	buf bytes.Buffer
	lw  ioutils.LimitedWriter
}

var limitedBufferPool = sync.Pool{
	New: func() any {
		lb := &limitedBuffer{}
		lb.buf.Grow(512)
		lb.lw.W = &lb.buf
		return lb
	},
}

// New creates a Prober.
func New() Prober {
	return execProber{}
}

// Prober is an interface defining the Probe object for container readiness/liveness checks.
type Prober interface {
	Probe(e exec.Cmd) (probe.Result, string, error)
}

type execProber struct{}

// Probe executes a command to check the liveness/readiness of container
// from executing a command. Returns the Result status, command output, and
// errors if any.
func (pr execProber) Probe(e exec.Cmd) (probe.Result, string, error) {
	lb := limitedBufferPool.Get().(*limitedBuffer)
	lb.buf.Reset()
	lb.lw.W = &lb.buf
	lb.lw.N = maxReadLength
	defer func() {
		if lb.buf.Cap() <= maxReadLength*2 {
			lb.buf.Reset()
			lb.lw.W = &lb.buf
			lb.lw.N = 0
			limitedBufferPool.Put(lb)
		}
	}()

	e.SetStderr(&lb.lw)
	e.SetStdout(&lb.lw)
	err := e.Start()
	if err == nil {
		err = e.Wait()
	}
	e.SetStderr(nil)
	e.SetStdout(nil)

	if klogV4 := klog.V(4); klogV4.Enabled() {
		klogV4.Infof("Exec probe response: %q", lb.buf.String())
	}
	if err != nil {
		exit, ok := err.(exec.ExitError)
		if ok {
			output := lb.buf.String()
			if exit.ExitStatus() == 0 {
				return probe.Success, output, nil
			}
			return probe.Failure, output, nil
		}

		if errors.Is(err, remote.ErrCommandTimedOut) {
			return probe.Failure, err.Error(), nil
		}

		return probe.Unknown, "", err
	}
	return probe.Success, lb.buf.String(), nil
}
