/*
Copyright 2026 The Kubernetes Authors.

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

package prober

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/ktesting"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/status"
	statustest "k8s.io/kubernetes/pkg/kubelet/status/testing"
	kubeletutil "k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/probe"
	execprobe "k8s.io/kubernetes/pkg/probe/exec"
	httpprobe "k8s.io/kubernetes/pkg/probe/http"
)

// benchHTTPClient implements httpprobe.GetHTTPInterface in memory so that
// BenchmarkContainerProbes exercises the full pkg/kubelet/prober and pkg/probe/http
// request construction, redirect checker, and response body reading paths without
// exhausting kernel ephemeral TCP ports.
type staticBody struct {
	offset int
}

var okPayload = []byte("ok\n")
var staticRespHeader = http.Header{"Content-Type": []string{"text/plain"}}

func (b *staticBody) Read(p []byte) (int, error) {
	if b.offset >= len(okPayload) {
		return 0, io.EOF
	}
	n := copy(p, okPayload[b.offset:])
	b.offset += n
	return n, nil
}

func (b *staticBody) Close() error {
	b.offset = 0
	return nil
}

type benchHTTPClient struct {
	resp http.Response
	body staticBody
}

func (c *benchHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.body.offset = 0
	c.resp.StatusCode = http.StatusOK
	c.resp.Status = "200 OK"
	c.resp.Header = staticRespHeader
	c.resp.Body = &c.body
	c.resp.Request = req
	return &c.resp, nil
}

type benchHTTPProber struct {
	followNonLocalRedirects bool
	client                  *benchHTTPClient
}

func (p benchHTTPProber) Probe(req *http.Request, timeout time.Duration) (probe.Result, string, error) {
	_ = httpprobe.RedirectChecker(p.followNonLocalRedirects)
	return httpprobe.DoHTTPProbe(req, p.client)
}

func (p benchHTTPProber) ProbeH2C(req *http.Request, timeout time.Duration) (probe.Result, string, error) {
	_ = httpprobe.RedirectChecker(p.followNonLocalRedirects)
	return httpprobe.DoHTTPProbe(req, p.client)
}

// BenchmarkContainerProbes simulates a high-density node probing wave across
// 60 pods (120 active probe workers: 60 HTTP readiness/liveness workers and
// 60 Exec readiness/liveness workers).
func BenchmarkContainerProbes(b *testing.B) {
	logger, ctx := ktesting.NewTestContext(b)

	podManager := kubepod.NewBasicPodManager()
	podStartupLatencyTracker := kubeletutil.NewPodStartupLatencyTracker()
	statusMgr := status.NewManager(&fake.Clientset{}, podManager, &statustest.FakePodDeletionSafetyProvider{}, podStartupLatencyTracker)

	runner := &containertest.FakeContainerCommandRunner{
		Stdout: "healthcheck: container status healthy\n",
	}
	m := NewManager(
		statusMgr,
		results.NewManager(),
		results.NewManager(),
		results.NewManager(),
		runner,
		&record.FakeRecorder{},
	).(*manager)

	m.prober.exec = execprobe.New()
	m.prober.http = benchHTTPProber{followNonLocalRedirects: false, client: &benchHTTPClient{}}

	// Drain result manager update channels just as Kubelet's syncLoop does,
	// since results.NewManager() uses a 20-element buffered channel.
	stopDrain := make(chan struct{})
	defer close(stopDrain)
	for _, ch := range []<-chan results.Update{
		m.readinessManager.Updates(),
		m.livenessManager.Updates(),
		m.startupManager.Updates(),
	} {
		go func(u <-chan results.Update) {
			for {
				select {
				case <-u:
				case <-stopDrain:
					return
				}
			}
		}(ch)
	}

	const numPods = 60
	workers := make([]*worker, 0, numPods*2)
	startedTrue := true

	for i := 0; i < numPods; i++ {
		uid := types.UID(fmt.Sprintf("bench-pod-uid-%03d", i))
		containerID := kubecontainer.ContainerID{
			Type: "containerd",
			ID:   fmt.Sprintf("bench-container-id-%03d", i),
		}
		containerName := fmt.Sprintf("app-container-%03d", i)

		var probeHandler v1.ProbeHandler
		if i < numPods/2 {
			probeHandler = v1.ProbeHandler{
				HTTPGet: &v1.HTTPGetAction{
					Path:   "/healthz/ready?verbose=1",
					Port:   intstr.FromString("http-health"),
					Scheme: v1.URISchemeHTTP,
					HTTPHeaders: []v1.HTTPHeader{
						{Name: "X-Probe-Source", Value: "kubelet"},
						{Name: "X-Pod-Index", Value: fmt.Sprintf("%d", i)},
					},
				},
			}
		} else {
			probeHandler = v1.ProbeHandler{
				Exec: &v1.ExecAction{
					Command: []string{"/bin/healthcheck", "--path=$(PROBE_PATH)", "--env=$(SERVICE_ENV)"},
				},
			}
		}

		readinessProbe := &v1.Probe{
			ProbeHandler:     probeHandler,
			TimeoutSeconds:   1,
			PeriodSeconds:    10,
			SuccessThreshold: 1,
			FailureThreshold: 3,
		}
		livenessProbe := readinessProbe.DeepCopy()

		container := v1.Container{
			Name: containerName,
			Ports: []v1.ContainerPort{
				{Name: "grpc", ContainerPort: 50051},
				{Name: "metrics", ContainerPort: 9090},
				{Name: "http-health", ContainerPort: 8080},
			},
			Env: []v1.EnvVar{
				{Name: "PROBE_PATH", Value: "/healthz"},
				{Name: "SERVICE_ENV", Value: "production"},
			},
			ReadinessProbe: readinessProbe,
			LivenessProbe:  livenessProbe,
		}

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID:       uid,
				Name:      fmt.Sprintf("bench-pod-%03d", i),
				Namespace: "default",
			},
			Spec: v1.PodSpec{
				Containers:    []v1.Container{container},
				RestartPolicy: v1.RestartPolicyAlways,
			},
		}

		podManager.AddPod(pod)
		podStatus := v1.PodStatus{
			Phase:  v1.PodRunning,
			PodIP:  fmt.Sprintf("10.244.1.%d", (i%200)+10),
			PodIPs: []v1.PodIP{{IP: fmt.Sprintf("10.244.1.%d", (i%200)+10)}},
			ContainerStatuses: []v1.ContainerStatus{
				{
					Name:        containerName,
					ContainerID: containerID.String(),
					Ready:       true,
					Started:     &startedTrue,
					State: v1.ContainerState{
						Running: &v1.ContainerStateRunning{
							StartedAt: metav1.Now(),
						},
					},
				},
			},
		}
		m.statusManager.SetPodStatus(logger, pod, podStatus)

		rw := newWorker(m, readiness, pod, container)
		lw := newWorker(m, liveness, pod, container)
		workers = append(workers, rw, lw)
	}

	// Prime initial results so steady-state probing wave is measured alongside status checks.
	for _, w := range workers {
		if !w.doProbe(ctx) {
			b.Fatalf("expected worker doProbe to return keepGoing=true")
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, w := range workers {
			if !w.doProbe(ctx) {
				b.Fatalf("unexpected probe termination")
			}
		}
	}
}
