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

package volumemanager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	storagev1client "k8s.io/client-go/kubernetes/typed/storage/v1"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/populator"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/reconciler"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/configmap"
	"k8s.io/kubernetes/pkg/volume/csi"
	"k8s.io/kubernetes/pkg/volume/downwardapi"
	"k8s.io/kubernetes/pkg/volume/emptydir"
	"k8s.io/kubernetes/pkg/volume/projected"
	"k8s.io/kubernetes/pkg/volume/secret"
	volumetest "k8s.io/kubernetes/pkg/volume/testing"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/nestedpendingoperations"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/mount-utils"
	"k8s.io/utils/ptr"
)

const (
	benchNodeName      = k8stypes.NodeName("node-volume-bench-01")
	benchAttachPlug    = "kubernetes.io/bench-block-fs"
	benchCSIDriverName = "csi.bench.k8s.io"
	benchNumNamespaces = 10
	benchPodsPerNS     = 6
	benchTotalPods     = benchNumNamespaces * benchPodsPerNS // 60 steady-state pods
)

// benchMounter implements mount.Interface in memory with O(1) path tracking and
// zero unbounded action-log retention across benchmark waves.
type benchMounter struct {
	mu     sync.RWMutex
	mounts map[string]mount.MountPoint
}

func newBenchMounter() *benchMounter {
	return &benchMounter{
		mounts: make(map[string]mount.MountPoint, 256),
	}
}

func (m *benchMounter) Mount(source, target, fstype string, options []string) error {
	return m.MountSensitive(source, target, fstype, options, nil)
}

func (m *benchMounter) MountSensitive(source, target, fstype string, options, sensitiveOptions []string) error {
	opts := make([]string, 0, len(options)+len(sensitiveOptions))
	opts = append(opts, options...)
	opts = append(opts, sensitiveOptions...)
	m.mu.Lock()
	m.mounts[target] = mount.MountPoint{
		Device: source,
		Path:   target,
		Type:   fstype,
		Opts:   opts,
	}
	m.mu.Unlock()
	return nil
}

func (m *benchMounter) MountSensitiveWithoutSystemd(source, target, fstype string, options, sensitiveOptions []string) error {
	return m.MountSensitive(source, target, fstype, options, sensitiveOptions)
}

func (m *benchMounter) MountSensitiveWithoutSystemdWithMountFlags(source, target, fstype string, options, sensitiveOptions, _ []string) error {
	return m.MountSensitive(source, target, fstype, options, sensitiveOptions)
}

func (m *benchMounter) Unmount(target string) error {
	m.mu.Lock()
	delete(m.mounts, target)
	m.mu.Unlock()
	return nil
}

func (m *benchMounter) List() ([]mount.MountPoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]mount.MountPoint, 0, len(m.mounts))
	for _, mp := range m.mounts {
		res = append(res, mp)
	}
	return res, nil
}

func (m *benchMounter) IsLikelyNotMountPoint(file string) (bool, error) {
	if _, err := os.Stat(file); err != nil {
		return true, err
	}
	m.mu.RLock()
	_, ok := m.mounts[file]
	m.mu.RUnlock()
	return !ok, nil
}

func (m *benchMounter) IsMountPoint(file string) (bool, error) {
	notMnt, err := m.IsLikelyNotMountPoint(file)
	return !notMnt, err
}

func (m *benchMounter) CanSafelySkipMountPointCheck() bool {
	return false
}

func (m *benchMounter) GetMountRefs(pathname string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mp, ok := m.mounts[pathname]
	if !ok {
		return nil, nil
	}
	var refs []string
	for p, other := range m.mounts {
		if p != pathname && (other.Device == pathname || other.Device == mp.Device) {
			refs = append(refs, p)
		}
	}
	return refs, nil
}

var _ mount.Interface = &benchMounter{}

// benchKubeClient provides O(1) in-memory CoreV1 and StorageV1 operations for Node,
// CSINode, PV, and PVC objects without client-go/testing ObjectTracker reflection
// or unbounded Action logs.
type benchKubeClient struct {
	clientset.Interface
	core    *benchCoreV1Client
	storage *benchStorageV1Client
}

type benchCoreV1Client struct {
	corev1client.CoreV1Interface
	mu   sync.RWMutex
	node *v1.Node
	pvs  map[string]*v1.PersistentVolume
	pvcs map[string]*v1.PersistentVolumeClaim // "ns/name" -> *v1.PersistentVolumeClaim
}

type benchStorageV1Client struct {
	storagev1client.StorageV1Interface
	mu       sync.RWMutex
	csiNodes map[string]*storagev1.CSINode
}

func newBenchKubeClient(node *v1.Node) *benchKubeClient {
	return &benchKubeClient{
		core: &benchCoreV1Client{
			node: node,
			pvs:  make(map[string]*v1.PersistentVolume, 128),
			pvcs: make(map[string]*v1.PersistentVolumeClaim, 128),
		},
		storage: &benchStorageV1Client{
			csiNodes: make(map[string]*storagev1.CSINode, 4),
		},
	}
}

func (c *benchKubeClient) CoreV1() corev1client.CoreV1Interface {
	return c.core
}

func (c *benchKubeClient) StorageV1() storagev1client.StorageV1Interface {
	return c.storage
}

func (s *benchStorageV1Client) CSINodes() storagev1client.CSINodeInterface {
	return &benchCSINodeClient{storage: s}
}

type benchCSINodeClient struct {
	storagev1client.CSINodeInterface
	storage *benchStorageV1Client
}

func (c *benchCSINodeClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*storagev1.CSINode, error) {
	c.storage.mu.RLock()
	node, ok := c.storage.csiNodes[name]
	c.storage.mu.RUnlock()
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "storage.k8s.io", Resource: "csinodes"}, name)
	}
	return node.DeepCopy(), nil
}

func (c *benchCSINodeClient) Create(_ context.Context, csiNode *storagev1.CSINode, _ metav1.CreateOptions) (*storagev1.CSINode, error) {
	cp := csiNode.DeepCopy()
	c.storage.mu.Lock()
	c.storage.csiNodes[cp.Name] = cp
	c.storage.mu.Unlock()
	return cp.DeepCopy(), nil
}

func (c *benchCSINodeClient) Update(_ context.Context, csiNode *storagev1.CSINode, _ metav1.UpdateOptions) (*storagev1.CSINode, error) {
	cp := csiNode.DeepCopy()
	c.storage.mu.Lock()
	c.storage.csiNodes[cp.Name] = cp
	c.storage.mu.Unlock()
	return cp.DeepCopy(), nil
}

func (c *benchCSINodeClient) Delete(_ context.Context, name string, _ metav1.DeleteOptions) error {
	c.storage.mu.Lock()
	delete(c.storage.csiNodes, name)
	c.storage.mu.Unlock()
	return nil
}

func (c *benchCoreV1Client) Nodes() corev1client.NodeInterface {
	return &benchNodeClient{core: c}
}

func (c *benchCoreV1Client) PersistentVolumes() corev1client.PersistentVolumeInterface {
	return &benchPVClient{core: c}
}

func (c *benchCoreV1Client) PersistentVolumeClaims(namespace string) corev1client.PersistentVolumeClaimInterface {
	return &benchPVCClient{core: c, ns: namespace}
}

type benchNodeClient struct {
	corev1client.NodeInterface
	core *benchCoreV1Client
}

func (n *benchNodeClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*v1.Node, error) {
	n.core.mu.RLock()
	defer n.core.mu.RUnlock()
	if n.core.node != nil && n.core.node.Name == name {
		return n.core.node, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, name)
}

type benchPVClient struct {
	corev1client.PersistentVolumeInterface
	core *benchCoreV1Client
}

func (p *benchPVClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*v1.PersistentVolume, error) {
	p.core.mu.RLock()
	pv, ok := p.core.pvs[name]
	p.core.mu.RUnlock()
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumes"}, name)
	}
	return pv, nil
}

type benchPVCClient struct {
	corev1client.PersistentVolumeClaimInterface
	core *benchCoreV1Client
	ns   string
}

func (p *benchPVCClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*v1.PersistentVolumeClaim, error) {
	p.core.mu.RLock()
	pvc, ok := p.core.pvcs[p.ns+"/"+name]
	p.core.mu.RUnlock()
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, name)
	}
	return pvc, nil
}

func (p *benchPVCClient) Patch(_ context.Context, name string, _ k8stypes.PatchType, data []byte, _ metav1.PatchOptions, _ ...string) (*v1.PersistentVolumeClaim, error) {
	key := p.ns + "/" + name
	p.core.mu.Lock()
	defer p.core.mu.Unlock()
	cur, ok := p.core.pvcs[key]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, name)
	}
	origBytes, err := json.Marshal(cur)
	if err != nil {
		return nil, err
	}
	patchedBytes, err := strategicpatch.StrategicMergePatch(origBytes, data, v1.PersistentVolumeClaim{})
	if err != nil {
		return nil, err
	}
	var updated v1.PersistentVolumeClaim
	if err := json.Unmarshal(patchedBytes, &updated); err != nil {
		return nil, err
	}
	p.core.pvcs[key] = &updated
	return updated.DeepCopy(), nil
}

type benchCSIDriverLister struct {
	drivers map[string]*storagev1.CSIDriver
}

func (l *benchCSIDriverLister) List(_ labels.Selector) ([]*storagev1.CSIDriver, error) {
	res := make([]*storagev1.CSIDriver, 0, len(l.drivers))
	for _, d := range l.drivers {
		res = append(res, d)
	}
	return res, nil
}

func (l *benchCSIDriverLister) Get(name string) (*storagev1.CSIDriver, error) {
	d, ok := l.drivers[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "storage.k8s.io", Resource: "csidrivers"}, name)
	}
	return d, nil
}

// benchVolumeHost implements volume.KubeletVolumeHost backed by fast in-memory
// Secret/ConfigMap/Token stores (mirroring Kubelet's mounted-object managers)
// and a real temporary directory for AtomicWriter / EmptyDir / Projected mounts.
type benchVolumeHost struct {
	rootDir         string
	nodeName        k8stypes.NodeName
	kubeClient      *benchKubeClient
	pluginMgr       *volume.VolumePluginMgr
	mounter         *benchMounter
	hostUtil        hostutil.HostUtils
	subpather       subpath.Interface
	csiDriverLister storagelistersv1.CSIDriverLister
	informerFactory informers.SharedInformerFactory
	recorder        record.EventRecorder

	objMu            sync.RWMutex
	secrets          map[string]*v1.Secret
	configMaps       map[string]*v1.ConfigMap
	tokenSeq         atomic.Int64
	csiNodeReadyOnce sync.Once
	csiNodeReadyCh   chan struct{}
}

var _ volume.KubeletVolumeHost = &benchVolumeHost{}
var _ volume.CSIDriverVolumeHost = &benchVolumeHost{}

func (h *benchVolumeHost) GetPluginDir(pluginName string) string {
	return filepath.Join(h.rootDir, "plugins", pluginName)
}

func (h *benchVolumeHost) GetVolumeDevicePluginDir(pluginName string) string {
	return filepath.Join(h.rootDir, "plugins", pluginName, "volumeDevices")
}

func (h *benchVolumeHost) GetPodsDir() string {
	return filepath.Join(h.rootDir, "pods")
}

func (h *benchVolumeHost) GetPodVolumeDir(podUID k8stypes.UID, pluginName, volumeName string) string {
	return filepath.Join(h.rootDir, "pods", string(podUID), "volumes", pluginName, volumeName)
}

func (h *benchVolumeHost) GetPodVolumeDeviceDir(podUID k8stypes.UID, pluginName string) string {
	return filepath.Join(h.rootDir, "pods", string(podUID), "volumeDevices", pluginName)
}

func (h *benchVolumeHost) GetPodPluginDir(podUID k8stypes.UID, pluginName string) string {
	return filepath.Join(h.rootDir, "pods", string(podUID), "plugins", pluginName)
}

func (h *benchVolumeHost) GetKubeClient() clientset.Interface {
	return h.kubeClient
}

func (h *benchVolumeHost) NewWrapperMounter(volName string, spec volume.Spec, pod *v1.Pod) (volume.Mounter, error) {
	if spec.Volume != nil {
		spec.Volume.Name = "wrapped_" + volName
	}
	plug, err := h.pluginMgr.FindPluginBySpec(&spec)
	if err != nil {
		return nil, err
	}
	return plug.NewMounter(&spec, pod)
}

func (h *benchVolumeHost) NewWrapperUnmounter(volName string, spec volume.Spec, podUID k8stypes.UID) (volume.Unmounter, error) {
	if spec.Volume != nil {
		spec.Volume.Name = "wrapped_" + volName
	}
	plug, err := h.pluginMgr.FindPluginBySpec(&spec)
	if err != nil {
		return nil, err
	}
	return plug.NewUnmounter(spec.Name(), podUID)
}

func (h *benchVolumeHost) GetMounter() mount.Interface {
	return h.mounter
}

func (h *benchVolumeHost) GetHostUtil() hostutil.HostUtils {
	return h.hostUtil
}

func (h *benchVolumeHost) GetNodeAllocatable() (v1.ResourceList, error) {
	return v1.ResourceList{}, nil
}

func (h *benchVolumeHost) GetAttachedVolumesFromNodeStatus() (map[v1.UniqueVolumeName]string, error) {
	h.kubeClient.core.mu.RLock()
	defer h.kubeClient.core.mu.RUnlock()
	res := make(map[v1.UniqueVolumeName]string)
	if h.kubeClient.core.node != nil {
		for _, av := range h.kubeClient.core.node.Status.VolumesAttached {
			res[av.Name] = av.DevicePath
		}
	}
	return res, nil
}

func (h *benchVolumeHost) GetSecretFunc() func(namespace, name string) (*v1.Secret, error) {
	return func(namespace, name string) (*v1.Secret, error) {
		h.objMu.RLock()
		sec, ok := h.secrets[namespace+"/"+name]
		h.objMu.RUnlock()
		if !ok {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
		}
		return sec, nil
	}
}

func (h *benchVolumeHost) GetConfigMapFunc() func(namespace, name string) (*v1.ConfigMap, error) {
	return func(namespace, name string) (*v1.ConfigMap, error) {
		h.objMu.RLock()
		cm, ok := h.configMaps[namespace+"/"+name]
		h.objMu.RUnlock()
		if !ok {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
		}
		return cm, nil
	}
}

func (h *benchVolumeHost) GetServiceAccountTokenFunc() func(string, string, *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
	expTime := metav1.NewTime(time.Unix(1800000000, 0))
	return func(namespace, name string, tr *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		seq := h.tokenSeq.Load()
		out := *tr
		out.Status = authenticationv1.TokenRequestStatus{
			Token:               fmt.Sprintf("eyJhbGciOiJSUzI1NiJ9.%s.%s.gen-%d", namespace, name, seq),
			ExpirationTimestamp: expTime,
		}
		return &out, nil
	}
}

func (h *benchVolumeHost) DeleteServiceAccountTokenFunc() func(k8stypes.UID) {
	return func(k8stypes.UID) {}
}

func (h *benchVolumeHost) GetNodeLabels() (map[string]string, error) {
	return map[string]string{"kubernetes.io/hostname": string(h.nodeName), "topology.kubernetes.io/zone": "us-central1-a"}, nil
}

func (h *benchVolumeHost) GetNodeName() k8stypes.NodeName {
	return h.nodeName
}

func (h *benchVolumeHost) GetEventRecorder() record.EventRecorder {
	return h.recorder
}

func (h *benchVolumeHost) GetSubpather() subpath.Interface {
	return h.subpather
}

func (h *benchVolumeHost) SetKubeletError(err error) {
	if err == nil && h.csiNodeReadyCh != nil {
		h.csiNodeReadyOnce.Do(func() { close(h.csiNodeReadyCh) })
	}
}

func (h *benchVolumeHost) GetInformerFactory() informers.SharedInformerFactory {
	return h.informerFactory
}

func (h *benchVolumeHost) CSIDriverLister() storagelistersv1.CSIDriverLister {
	return h.csiDriverLister
}

func (h *benchVolumeHost) CSIDriversSynced() toolscache.InformerSynced {
	return func() bool { return true }
}

func (h *benchVolumeHost) WaitForCacheSync() error {
	return nil
}

func (h *benchVolumeHost) GetTrustAnchorsByName(_ context.Context, _ string, _ bool) ([]byte, error) {
	return []byte("-----BEGIN CERTIFICATE-----\nbench-ca\n-----END CERTIFICATE-----\n"), nil
}

func (h *benchVolumeHost) GetTrustAnchorsBySigner(_ context.Context, _ string, _ *metav1.LabelSelector, _ bool) ([]byte, error) {
	return []byte("-----BEGIN CERTIFICATE-----\nbench-signer\n-----END CERTIFICATE-----\n"), nil
}

func (h *benchVolumeHost) GetPodCertificateCredentialBundle(_ context.Context, _, _, _, _ string, _ int) ([]byte, []byte, error) {
	return []byte("key\n"), []byte("cert\n"), nil
}

// benchAttachableFSPlugin models an attachable, device-mountable, node-expandable,
// SELinux-mount-capable persistent volume plugin (backed by FlexVolume source
// with Driver == benchAttachPlug) and records lifecycle events for parity checks.
type benchAttachableFSPlugin struct {
	host         volume.VolumeHost
	mounter      *benchMounter
	recordOps    bool
	mu           sync.Mutex
	events       []string
	resizedSizes map[string]int64
	seLinuxCtx   map[string]string
}

var _ volume.VolumePlugin = &benchAttachableFSPlugin{}
var _ volume.AttachableVolumePlugin = &benchAttachableFSPlugin{}
var _ volume.DeviceMountableVolumePlugin = &benchAttachableFSPlugin{}
var _ volume.NodeExpandableVolumePlugin = &benchAttachableFSPlugin{}

func (p *benchAttachableFSPlugin) Init(host volume.VolumeHost) error {
	p.host = host
	if bm, ok := host.GetMounter().(*benchMounter); ok {
		p.mounter = bm
	}
	if p.resizedSizes == nil {
		p.resizedSizes = make(map[string]int64, 64)
	}
	if p.seLinuxCtx == nil {
		p.seLinuxCtx = make(map[string]string, 64)
	}
	return nil
}

func (p *benchAttachableFSPlugin) recordEvent(ev string) {
	if !p.recordOps {
		return
	}
	p.mu.Lock()
	p.events = append(p.events, ev)
	p.mu.Unlock()
}

func (p *benchAttachableFSPlugin) GetPluginName() string {
	return benchAttachPlug
}

func (p *benchAttachableFSPlugin) GetVolumeName(spec *volume.Spec) (string, error) {
	if spec == nil {
		return "", fmt.Errorf("nil spec")
	}
	if spec.PersistentVolume != nil && spec.PersistentVolume.Spec.FlexVolume != nil {
		return spec.PersistentVolume.Spec.FlexVolume.Options["volumeID"], nil
	}
	if spec.Volume != nil && spec.Volume.FlexVolume != nil {
		return spec.Volume.FlexVolume.Options["volumeID"], nil
	}
	return "", fmt.Errorf("unsupported spec")
}

func (p *benchAttachableFSPlugin) CanSupport(spec *volume.Spec) bool {
	if spec == nil {
		return false
	}
	if spec.PersistentVolume != nil && spec.PersistentVolume.Spec.FlexVolume != nil {
		return spec.PersistentVolume.Spec.FlexVolume.Driver == benchAttachPlug
	}
	if spec.Volume != nil && spec.Volume.FlexVolume != nil {
		return spec.Volume.FlexVolume.Driver == benchAttachPlug
	}
	return false
}

func (p *benchAttachableFSPlugin) RequiresRemount(*volume.Spec) bool {
	return false
}

func (p *benchAttachableFSPlugin) SupportsMountOption() bool {
	return true
}

func (p *benchAttachableFSPlugin) SupportsSELinuxContextMount(*volume.Spec) (bool, error) {
	return true, nil
}

func (p *benchAttachableFSPlugin) ConstructVolumeSpec(volumeName, _ string) (volume.ReconstructedVolume, error) {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: volumeName},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				FlexVolume: &v1.FlexPersistentVolumeSource{
					Driver:  benchAttachPlug,
					Options: map[string]string{"volumeID": volumeName},
				},
			},
		},
	}
	return volume.ReconstructedVolume{Spec: volume.NewSpecFromPersistentVolume(pv, false)}, nil
}

func (p *benchAttachableFSPlugin) NewMounter(spec *volume.Spec, pod *v1.Pod) (volume.Mounter, error) {
	volID, err := p.GetVolumeName(spec)
	if err != nil {
		return nil, err
	}
	return &benchPVMounter{
		plugin:   p,
		podUID:   pod.UID,
		volName:  spec.Name(),
		volID:    volID,
		readOnly: spec.ReadOnly,
	}, nil
}

func (p *benchAttachableFSPlugin) NewUnmounter(volName string, podUID k8stypes.UID) (volume.Unmounter, error) {
	return &benchPVMounter{
		plugin:  p,
		podUID:  podUID,
		volName: volName,
	}, nil
}

func (p *benchAttachableFSPlugin) NewAttacher() (volume.Attacher, error) {
	return &benchPVAttacher{plugin: p}, nil
}

func (p *benchAttachableFSPlugin) NewDetacher() (volume.Detacher, error) {
	return &benchPVAttacher{plugin: p}, nil
}

func (p *benchAttachableFSPlugin) CanAttach(*volume.Spec) (bool, error) {
	return true, nil
}

func (p *benchAttachableFSPlugin) VerifyExhaustedResource(*volume.Spec) bool {
	return false
}

func (p *benchAttachableFSPlugin) CanDeviceMount(*volume.Spec) (bool, error) {
	return true, nil
}

func (p *benchAttachableFSPlugin) NewDeviceMounter() (volume.DeviceMounter, error) {
	return &benchPVAttacher{plugin: p}, nil
}

func (p *benchAttachableFSPlugin) NewDeviceUnmounter() (volume.DeviceUnmounter, error) {
	return &benchPVAttacher{plugin: p}, nil
}

func (p *benchAttachableFSPlugin) GetDeviceMountRefs(deviceMountPath string) ([]string, error) {
	if p.mounter == nil {
		return nil, nil
	}
	return p.mounter.GetMountRefs(deviceMountPath)
}

func (p *benchAttachableFSPlugin) RequiresFSResize() bool {
	return true
}

func (p *benchAttachableFSPlugin) NodeExpand(resizeOpts volume.NodeResizeOptions) (bool, error) {
	volID, _ := p.GetVolumeName(resizeOpts.VolumeSpec)
	p.mu.Lock()
	p.resizedSizes[volID] = resizeOpts.NewSize.Value()
	p.mu.Unlock()
	p.recordEvent("NodeExpand:" + volID)
	return true, nil
}

type benchPVMounter struct {
	volume.MetricsNil
	plugin   *benchAttachableFSPlugin
	podUID   k8stypes.UID
	volName  string
	volID    string
	readOnly bool
}

func (m *benchPVMounter) GetPath() string {
	return m.plugin.host.GetPodVolumeDir(m.podUID, "kubernetes.io~bench-block-fs", m.volName)
}

func (m *benchPVMounter) GetAttributes() volume.Attributes {
	return volume.Attributes{
		ReadOnly:       m.readOnly,
		Managed:        !m.readOnly,
		SELinuxRelabel: false,
	}
}

func (m *benchPVMounter) SetUp(mounterArgs volume.MounterArgs) error {
	return m.SetUpAt(m.GetPath(), mounterArgs)
}

func (m *benchPVMounter) SetUpAt(dir string, mounterArgs volume.MounterArgs) error {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	var opts []string
	if mounterArgs.SELinuxLabel != "" {
		opts = []string{"context=" + mounterArgs.SELinuxLabel}
		m.plugin.mu.Lock()
		m.plugin.seLinuxCtx[m.volID] = mounterArgs.SELinuxLabel
		m.plugin.mu.Unlock()
	}
	if m.plugin.mounter != nil {
		_ = m.plugin.mounter.Mount("/dev/"+m.volID, dir, "ext4", opts)
	}
	m.plugin.recordEvent("SetUp:" + m.volName)
	return nil
}

func (m *benchPVMounter) TearDown() error {
	return m.TearDownAt(m.GetPath())
}

func (m *benchPVMounter) TearDownAt(dir string) error {
	if m.plugin.mounter != nil {
		_ = m.plugin.mounter.Unmount(dir)
	}
	_ = os.RemoveAll(dir)
	m.plugin.recordEvent("TearDown:" + m.volName)
	return nil
}

type benchPVAttacher struct {
	plugin *benchAttachableFSPlugin
}

func (a *benchPVAttacher) Attach(spec *volume.Spec, _ k8stypes.NodeName) (string, error) {
	volID, err := a.plugin.GetVolumeName(spec)
	if err != nil {
		return "", err
	}
	a.plugin.recordEvent("Attach:" + volID)
	return "/dev/" + volID, nil
}

func (a *benchPVAttacher) VolumesAreAttached(specs []*volume.Spec, _ k8stypes.NodeName) (map[*volume.Spec]bool, error) {
	res := make(map[*volume.Spec]bool, len(specs))
	for _, s := range specs {
		res[s] = true
	}
	return res, nil
}

func (a *benchPVAttacher) WaitForAttach(spec *volume.Spec, devicePath string, _ *v1.Pod, _ time.Duration) (string, error) {
	volID, err := a.plugin.GetVolumeName(spec)
	if err != nil {
		return "", err
	}
	a.plugin.recordEvent("WaitForAttach:" + volID)
	if devicePath != "" {
		return devicePath, nil
	}
	return "/dev/" + volID, nil
}

func (a *benchPVAttacher) GetDeviceMountPath(spec *volume.Spec) (string, error) {
	volID, err := a.plugin.GetVolumeName(spec)
	if err != nil {
		return "", err
	}
	return filepath.Join(a.plugin.host.GetPluginDir("kubernetes.io~bench-block-fs"), "mounts", volID), nil
}

func (a *benchPVAttacher) MountDevice(spec *volume.Spec, devicePath, deviceMountPath string, args volume.DeviceMounterArgs) error {
	volID, _ := a.plugin.GetVolumeName(spec)
	if err := os.MkdirAll(deviceMountPath, 0750); err != nil {
		return err
	}
	var opts []string
	if args.SELinuxLabel != "" {
		opts = []string{"context=" + args.SELinuxLabel}
		a.plugin.mu.Lock()
		a.plugin.seLinuxCtx[volID] = args.SELinuxLabel
		a.plugin.mu.Unlock()
	}
	if a.plugin.mounter != nil {
		_ = a.plugin.mounter.Mount(devicePath, deviceMountPath, "ext4", opts)
	}
	a.plugin.recordEvent("MountDevice:" + volID)
	return nil
}

func (a *benchPVAttacher) Detach(volumeName string, _ k8stypes.NodeName) error {
	a.plugin.recordEvent("Detach:" + volumeName)
	return nil
}

func (a *benchPVAttacher) UnmountDevice(deviceMountPath string) error {
	volID := filepath.Base(deviceMountPath)
	if a.plugin.mounter != nil {
		_ = a.plugin.mounter.Unmount(deviceMountPath)
	}
	_ = os.RemoveAll(deviceMountPath)
	a.plugin.recordEvent("UnmountDevice:" + volID)
	return nil
}

// benchPodStateProvider tracks pod termination/removal state for populator checks.
type benchPodStateProvider struct {
	mu          sync.RWMutex
	terminating map[k8stypes.UID]struct{}
	removed     map[k8stypes.UID]struct{}
}

func newBenchPodStateProvider() *benchPodStateProvider {
	return &benchPodStateProvider{
		terminating: make(map[k8stypes.UID]struct{}),
		removed:     make(map[k8stypes.UID]struct{}),
	}
}

func (p *benchPodStateProvider) ShouldPodContainersBeTerminating(uid k8stypes.UID) bool {
	p.mu.RLock()
	_, ok := p.terminating[uid]
	p.mu.RUnlock()
	return ok
}

func (p *benchPodStateProvider) ShouldPodRuntimeBeRemoved(uid k8stypes.UID) bool {
	p.mu.RLock()
	_, ok := p.removed[uid]
	p.mu.RUnlock()
	return ok
}

func (p *benchPodStateProvider) setRemoved(uid k8stypes.UID, removed bool) {
	p.mu.Lock()
	if removed {
		p.terminating[uid] = struct{}{}
		p.removed[uid] = struct{}{}
	} else {
		delete(p.terminating, uid)
		delete(p.removed, uid)
	}
	p.mu.Unlock()
}

// gatedPodManager wraps kubepod.Manager to allow deterministic, zero-sleep
// single-stepping of DesiredStateOfWorldPopulator.populatorLoop(ctx).
type gatedPodManager struct {
	inner     kubepod.Manager
	ctx       context.Context
	gated     atomic.Bool
	readyOnce sync.Once
	readyCh   chan struct{}
	stepCh    chan struct{}
	ackCh     chan struct{}
	inStep    bool
}

func newGatedPodManager(ctx context.Context) *gatedPodManager {
	g := &gatedPodManager{
		inner:   kubepod.NewBasicPodManager(),
		ctx:     ctx,
		readyCh: make(chan struct{}),
		stepCh:  make(chan struct{}),
		ackCh:   make(chan struct{}),
	}
	g.gated.Store(true)
	return g
}

func (g *gatedPodManager) GetPodByUID(uid k8stypes.UID) (*v1.Pod, bool) {
	return g.inner.GetPodByUID(uid)
}

func (g *gatedPodManager) GetPods() []*v1.Pod {
	if !g.gated.Load() {
		return g.inner.GetPods()
	}
	g.readyOnce.Do(func() { close(g.readyCh) })
	if g.inStep {
		g.inStep = false
		select {
		case <-g.ctx.Done():
			return nil
		case g.ackCh <- struct{}{}:
		}
	}
	select {
	case <-g.ctx.Done():
		return nil
	case <-g.stepCh:
		g.inStep = true
	}
	return g.inner.GetPods()
}

// volumeBenchEnv encapsulates a complete Kubelet VolumeManager + real volume
// plugin harness for deterministic work-normalized benchmarking and parity tests.
type volumeBenchEnv struct {
	ctx           context.Context
	cancel        context.CancelFunc
	closeOnce     sync.Once
	wg            sync.WaitGroup
	rootDir       string
	node          *v1.Node
	kubeClient    *benchKubeClient
	host          *benchVolumeHost
	mounter       *benchMounter
	attachPlug    *benchAttachableFSPlugin
	csiPlug       volume.VolumePlugin
	pluginMgr     *volume.VolumePluginMgr
	podMgr        *gatedPodManager
	stateProvider *benchPodStateProvider
	vm            *volumeManager

	recReadyOnce sync.Once
	recReadyCh   chan struct{}
	recStepCh    chan struct{}
	recAckCh     chan struct{}
	recInStep    bool
	unmountReady atomic.Bool

	pods          []*v1.Pod
	podNames      []volumetypes.UniquePodName
	pvVolNames    []v1.UniqueVolumeName
	projVolNames  []v1.UniqueVolumeName
	extraVolNames []v1.UniqueVolumeName
	csiSpecs      []*volume.Spec
	resizablePV   *v1.PersistentVolume
	resizablePod  *v1.Pod
	resizableVol  v1.UniqueVolumeName
	tmpfsPod      *v1.Pod
	jobPods       [2]*v1.Pod
	jobPVs        [2]*v1.PersistentVolume
	jobPVCs       [2]*v1.PersistentVolumeClaim
	jobVolNames   [2]v1.UniqueVolumeName
}

func newVolumeBenchEnv(tb testing.TB, recordOps bool) *volumeBenchEnv {
	tb.Helper()
	ctx, cancel := context.WithCancel(klog.NewContext(context.Background(), klog.Background()))
	rootDir := tb.TempDir()

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(benchNodeName),
			UID:  "node-uid-volume-bench-01",
		},
	}
	kubeClient := newBenchKubeClient(node)
	bm := newBenchMounter()
	csiMode := storagev1.VolumeLifecyclePersistent
	csiLister := &benchCSIDriverLister{
		drivers: map[string]*storagev1.CSIDriver{
			benchCSIDriverName: {
				ObjectMeta: metav1.ObjectMeta{Name: benchCSIDriverName},
				Spec: storagev1.CSIDriverSpec{
					AttachRequired:       ptr.To(true),
					PodInfoOnMount:       ptr.To(true),
					SELinuxMount:         ptr.To(true),
					VolumeLifecycleModes: []storagev1.VolumeLifecycleMode{csiMode},
				},
			},
		},
	}

	host := &benchVolumeHost{
		rootDir:         rootDir,
		nodeName:        benchNodeName,
		kubeClient:      kubeClient,
		pluginMgr:       &volume.VolumePluginMgr{},
		mounter:         bm,
		hostUtil:        hostutil.NewFakeHostUtil(nil),
		subpather:       &subpath.FakeSubpath{},
		csiDriverLister: csiLister,
		recorder:        &record.FakeRecorder{Events: nil},
		secrets:         make(map[string]*v1.Secret, 64),
		configMaps:      make(map[string]*v1.ConfigMap, 64),
		csiNodeReadyCh:  make(chan struct{}),
	}
	host.informerFactory = informers.NewSharedInformerFactory(kubeClient, time.Minute)

	attachPlug := &benchAttachableFSPlugin{recordOps: recordOps}
	var allPlugins []volume.VolumePlugin
	allPlugins = append(allPlugins, projected.ProbeVolumePlugins()...)
	allPlugins = append(allPlugins, secret.ProbeVolumePlugins()...)
	allPlugins = append(allPlugins, configmap.ProbeVolumePlugins()...)
	allPlugins = append(allPlugins, downwardapi.ProbeVolumePlugins()...)
	allPlugins = append(allPlugins, emptydir.ProbeVolumePlugins()...)
	csiPlugins := csi.ProbeVolumePlugins()
	allPlugins = append(allPlugins, csiPlugins...)
	allPlugins = append(allPlugins, attachPlug)
	require.NoError(tb, host.pluginMgr.InitPlugins(allPlugins, nil, host))
	<-host.csiNodeReadyCh

	podMgr := newGatedPodManager(ctx)
	stateProvider := newBenchPodStateProvider()
	rawVM := NewVolumeManager(
		true, /* controllerAttachDetachEnabled */
		benchNodeName,
		podMgr,
		stateProvider,
		kubeClient,
		host.pluginMgr,
		bm,
		host.hostUtil,
		host.GetPodsDir(),
		host.recorder,
		volumetest.NewBlockVolumePathHandler(),
		nil,
		0,
	)
	vm := rawVM.(*volumeManager)

	env := &volumeBenchEnv{
		ctx:           ctx,
		cancel:        cancel,
		rootDir:       rootDir,
		node:          node,
		kubeClient:    kubeClient,
		host:          host,
		mounter:       bm,
		attachPlug:    attachPlug,
		csiPlug:       csiPlugins[0],
		pluginMgr:     host.pluginMgr,
		podMgr:        podMgr,
		stateProvider: stateProvider,
		vm:            vm,
		recReadyCh:    make(chan struct{}),
		recStepCh:     make(chan struct{}),
		recAckCh:      make(chan struct{}),
		pods:          make([]*v1.Pod, 0, benchTotalPods),
		podNames:      make([]volumetypes.UniquePodName, 0, benchTotalPods),
		pvVolNames:    make([]v1.UniqueVolumeName, 0, benchTotalPods),
		projVolNames:  make([]v1.UniqueVolumeName, 0, benchTotalPods),
		extraVolNames: make([]v1.UniqueVolumeName, 0, benchTotalPods),
		csiSpecs:      make([]*volume.Spec, 0, 4),
	}

	// Use FakeSELinuxLabelTranslator so SELinux mount context translation and mismatch
	// checks execute deterministically regardless of whether host OS SELinux is enabled.
	vm.desiredStateOfWorld = cache.NewDesiredStateOfWorld(host.pluginMgr, volumeutil.NewFakeSELinuxLabelTranslator())

	// Replace populator & reconciler with 1ns-sleep instances gated by stepCh/ackCh.
	vm.desiredStateOfWorldPopulator = populator.NewDesiredStateOfWorldPopulator(
		kubeClient,
		time.Nanosecond, /* loopSleepDuration */
		podMgr,
		stateProvider,
		vm.desiredStateOfWorld,
		vm.actualStateOfWorld,
		vm.csiMigratedPluginManager,
		vm.intreeToCSITranslator,
		vm.volumePluginMgr,
	)
	vm.reconciler = reconciler.NewReconciler(
		kubeClient,
		true,            /* controllerAttachDetachEnabled */
		time.Nanosecond, /* loopSleepDuration */
		waitForAttachTimeout,
		benchNodeName,
		vm.desiredStateOfWorld,
		vm.actualStateOfWorld,
		env.reconcilerStepGate,
		vm.operationExecutor,
		bm,
		host.hostUtil,
		vm.volumePluginMgr,
		host.GetPodsDir(),
	)

	env.populateFixtures()

	sourcesReady := config.NewSourcesReady(func(_ sets.Set[string]) bool { return true })
	env.wg.Add(2)
	go func() {
		defer env.wg.Done()
		vm.desiredStateOfWorldPopulator.Run(ctx, sourcesReady)
	}()
	go func() {
		defer env.wg.Done()
		vm.reconciler.Run(ctx)
	}()
	<-podMgr.readyCh
	<-env.recReadyCh
	tb.Cleanup(env.close)

	// Initial node volume attach & mount convergence across all 60 pods (180 volumes).
	// Pass 0 runs with unmountReady=false (mirroring Kubelet startup before readyToUnmount)
	// so all 60 PVs complete VerifyControllerAttachedVolume before unmountDetachDevices is armed.
	env.stepPopulator()
	inUse := vm.GetVolumesInUse()
	vm.MarkVolumesAsReportedInUse(inUse)
	env.stepReconciler()
	env.waitAllSteadyVolumesIdle()
	env.unmountReady.Store(true)

	for pass := 0; pass < 6; pass++ {
		env.stepReconciler()
		env.waitAllSteadyVolumesIdle()
		allMounted := true
		for _, podName := range env.podNames {
			if len(vm.GetMountedVolumesForPod(podName)) != 3 {
				allMounted = false
				break
			}
		}
		if allMounted {
			break
		}
	}

	for i, podName := range env.podNames {
		mounted := vm.GetMountedVolumesForPod(podName)
		if len(mounted) != 3 {
			tb.Fatalf("pod %d (%s) expected 3 mounted volumes after startup, got %d", i, podName, len(mounted))
		}
	}

	return env
}

func (e *volumeBenchEnv) reconcilerStepGate() bool {
	e.recReadyOnce.Do(func() { close(e.recReadyCh) })
	if e.recInStep {
		e.recInStep = false
		select {
		case <-e.ctx.Done():
			return false
		case e.recAckCh <- struct{}{}:
		}
	}
	select {
	case <-e.ctx.Done():
		return false
	case <-e.recStepCh:
		e.recInStep = true
	}
	return e.unmountReady.Load() && e.vm.desiredStateOfWorldPopulator.HasAddedPods()
}

func (e *volumeBenchEnv) stepPopulator() {
	e.podMgr.stepCh <- struct{}{}
	<-e.podMgr.ackCh
}

func (e *volumeBenchEnv) stepReconciler() {
	e.recStepCh <- struct{}{}
	<-e.recAckCh
}

func (e *volumeBenchEnv) waitVolumesIdle(volNames ...v1.UniqueVolumeName) {
	for _, v := range volNames {
		for e.vm.operationExecutor.IsOperationPending(v, nestedpendingoperations.EmptyUniquePodName, nestedpendingoperations.EmptyNodeName) {
			time.Sleep(50 * time.Microsecond)
		}
	}
}

func (e *volumeBenchEnv) waitAllSteadyVolumesIdle() {
	e.waitVolumesIdle(e.pvVolNames...)
	e.waitVolumesIdle(e.projVolNames...)
	e.waitVolumesIdle(e.extraVolNames...)
}

func (e *volumeBenchEnv) close() {
	e.closeOnce.Do(func() {
		e.cancel()
		e.wg.Wait()
	})
}

func (e *volumeBenchEnv) populateFixtures() {
	fsMode := v1.PersistentVolumeFilesystem
	qty10Gi := resource.MustParse("10Gi")
	qty64Mi := resource.MustParse("64Mi")
	defaultMode := int32(0644)
	secretMode := int32(0400)
	expSeconds := int64(3600)

	// Populate per-namespace Secrets & ConfigMaps.
	for nsIdx := 0; nsIdx < benchNumNamespaces; nsIdx++ {
		ns := fmt.Sprintf("ns-%02d", nsIdx)
		e.host.secrets[ns+"/app-tls"] = &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app-tls"},
			Data: map[string][]byte{
				"tls.crt": []byte("-----BEGIN CERTIFICATE-----\nMIIB-steady-cert-" + ns + "\n-----END CERTIFICATE-----\n"),
				"tls.key": []byte("-----BEGIN PRIVATE KEY-----\nMIIE-steady-key-" + ns + "\n-----END PRIVATE KEY-----\n"),
			},
		}
		e.host.secrets[ns+"/api-creds"] = &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "api-creds"},
			Data: map[string][]byte{
				"username": []byte("svc-user-" + ns),
				"api-key":  []byte("secret-api-key-0123456789abcdef-" + ns),
				"region":   []byte("us-central1"),
			},
		}
		e.host.configMaps[ns+"/kube-root-ca.crt"] = &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kube-root-ca.crt"},
			Data: map[string]string{
				"ca.crt":   "-----BEGIN CERTIFICATE-----\nMIIC-root-ca-" + ns + "\n-----END CERTIFICATE-----\n",
				"app.conf": "log_level=info\nmax_conns=256\nnamespace=" + ns + "\n",
			},
		}
		e.host.configMaps[ns+"/feature-flags"] = &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "feature-flags"},
			Data: map[string]string{
				"feature.yaml": "enableBatching: true\nwindowMs: 50\n",
				"endpoints":    "10.0.0.1:8080,10.0.0.2:8080",
			},
		}
	}

	var attached []v1.AttachedVolume
	seenAttached := sets.New[v1.UniqueVolumeName]()

	for i := 0; i < benchTotalPods; i++ {
		nsIdx := i / benchPodsPerNS
		ns := fmt.Sprintf("ns-%02d", nsIdx)
		podUID := k8stypes.UID(fmt.Sprintf("pod-uid-%03d", i))
		podNameStr := fmt.Sprintf("workload-pod-%03d", i)

		// First 20 pods share 10 RWX PVs in pairs; remaining 40 pods use dedicated RWO/RWOP PVs.
		pvIdx := i
		accessMode := v1.ReadWriteOncePod
		if i < 20 {
			pvIdx = i / 2
			accessMode = v1.ReadWriteMany
		}
		pvName := fmt.Sprintf("pv-%03d", pvIdx)
		volID := fmt.Sprintf("vol-disk-%03d", pvIdx)
		claimName := fmt.Sprintf("pvc-%03d", pvIdx)
		uniquePVName := v1.UniqueVolumeName(benchAttachPlug + "/" + volID)

		if _, exists := e.kubeClient.core.pvs[pvName]; !exists {
			var pvAnnotations map[string]string
			if pvIdx%2 == 0 {
				pvAnnotations = map[string]string{
					volumeutil.VolumeGidAnnotationKey: fmt.Sprintf("%d", 3000+pvIdx),
				}
			}
			pv := &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name:        pvName,
					Annotations: pvAnnotations,
				},
				Spec: v1.PersistentVolumeSpec{
					AccessModes: []v1.PersistentVolumeAccessMode{accessMode},
					Capacity:    v1.ResourceList{v1.ResourceStorage: qty10Gi.DeepCopy()},
					PersistentVolumeSource: v1.PersistentVolumeSource{
						FlexVolume: &v1.FlexPersistentVolumeSource{
							Driver:  benchAttachPlug,
							FSType:  "ext4",
							Options: map[string]string{"volumeID": volID},
						},
					},
					ClaimRef:   &v1.ObjectReference{Namespace: ns, Name: claimName},
					VolumeMode: &fsMode,
				},
			}
			pvc := &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: claimName},
				Spec: v1.PersistentVolumeClaimSpec{
					AccessModes: []v1.PersistentVolumeAccessMode{accessMode},
					VolumeName:  pvName,
					VolumeMode:  &fsMode,
					Resources: v1.VolumeResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceStorage: qty10Gi.DeepCopy()},
					},
				},
				Status: v1.PersistentVolumeClaimStatus{
					Phase:    v1.ClaimBound,
					Capacity: v1.ResourceList{v1.ResourceStorage: qty10Gi.DeepCopy()},
				},
			}
			e.kubeClient.core.pvs[pvName] = pv
			e.kubeClient.core.pvcs[ns+"/"+claimName] = pvc
		}
		if !seenAttached.Has(uniquePVName) {
			seenAttached.Insert(uniquePVName)
			attached = append(attached, v1.AttachedVolume{
				Name:       uniquePVName,
				DevicePath: "/dev/" + volID,
			})
		}

		var extraVol v1.Volume
		var extraUnique v1.UniqueVolumeName
		switch i % 3 {
		case 0:
			extraVol = v1.Volume{
				Name: "extra-vol",
				VolumeSource: v1.VolumeSource{
					Secret: &v1.SecretVolumeSource{
						SecretName:  "api-creds",
						DefaultMode: &defaultMode,
						Items: []v1.KeyToPath{
							{Key: "username", Path: "username"},
							{Key: "api-key", Path: "creds/api-key", Mode: &secretMode},
							{Key: "region", Path: "region"},
						},
					},
				},
			}
			extraUnique = v1.UniqueVolumeName("kubernetes.io/secret/" + string(podUID) + "-extra-vol")
		case 1:
			extraVol = v1.Volume{
				Name: "extra-vol",
				VolumeSource: v1.VolumeSource{
					ConfigMap: &v1.ConfigMapVolumeSource{
						LocalObjectReference: v1.LocalObjectReference{Name: "feature-flags"},
						DefaultMode:          &defaultMode,
						Items: []v1.KeyToPath{
							{Key: "feature.yaml", Path: "settings/feature.yaml"},
							{Key: "endpoints", Path: "endpoints"},
						},
					},
				},
			}
			extraUnique = v1.UniqueVolumeName("kubernetes.io/configmap/" + string(podUID) + "-extra-vol")
		default:
			medium := v1.StorageMediumDefault
			if i == 2 {
				medium = v1.StorageMediumMemory
			}
			extraVol = v1.Volume{
				Name: "extra-vol",
				VolumeSource: v1.VolumeSource{
					EmptyDir: &v1.EmptyDirVolumeSource{
						Medium:    medium,
						SizeLimit: &qty64Mi,
					},
				},
			}
			extraUnique = v1.UniqueVolumeName("kubernetes.io/empty-dir/" + string(podUID) + "-extra-vol")
		}

		var secCtx *v1.PodSecurityContext
		if accessMode == v1.ReadWriteOncePod {
			secCtx = &v1.PodSecurityContext{
				SupplementalGroups: []int64{1000},
				SELinuxOptions: &v1.SELinuxOptions{
					User:  "system_u",
					Type:  "container_t",
					Level: fmt.Sprintf("s0:c%d,c%d", i+1, i+101),
				},
			}
		} else {
			secCtx = &v1.PodSecurityContext{
				SupplementalGroups: []int64{1000},
			}
		}

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podNameStr,
				Namespace: ns,
				UID:       podUID,
				Labels: map[string]string{
					"app.kubernetes.io/name": "serving-worker",
					"shard":                  fmt.Sprintf("s-%02d", i%8),
				},
				Annotations: map[string]string{
					"config.kubernetes.io/checksum": "rev-0",
				},
			},
			Spec: v1.PodSpec{
				ServiceAccountName: "default",
				SecurityContext:    secCtx,
				Containers: []v1.Container{
					{
						Name:  "app",
						Image: "registry.k8s.io/pause:3.10",
						VolumeMounts: []v1.VolumeMount{
							{Name: "data-pv", MountPath: "/var/lib/data"},
							{Name: "kube-api-access", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
							{Name: "extra-vol", MountPath: "/etc/extra"},
						},
					},
				},
				Volumes: []v1.Volume{
					{
						Name: "data-pv",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								ClaimName: claimName,
							},
						},
					},
					{
						Name: "kube-api-access",
						VolumeSource: v1.VolumeSource{
							Projected: &v1.ProjectedVolumeSource{
								DefaultMode: &defaultMode,
								Sources: []v1.VolumeProjection{
									{
										ServiceAccountToken: &v1.ServiceAccountTokenProjection{
											Path:              "token",
											ExpirationSeconds: &expSeconds,
											Audience:          "https://kubernetes.default.svc",
										},
									},
									{
										ConfigMap: &v1.ConfigMapProjection{
											LocalObjectReference: v1.LocalObjectReference{Name: "kube-root-ca.crt"},
											Items: []v1.KeyToPath{
												{Key: "ca.crt", Path: "ca.crt"},
												{Key: "app.conf", Path: "config/app.conf"},
											},
										},
									},
									{
										DownwardAPI: &v1.DownwardAPIProjection{
											Items: []v1.DownwardAPIVolumeFile{
												{Path: "podinfo/name", FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}},
												{Path: "podinfo/namespace", FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}},
												{Path: "podinfo/labels", FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.labels"}},
												{Path: "podinfo/annotations", FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.annotations"}},
											},
										},
									},
									{
										Secret: &v1.SecretProjection{
											LocalObjectReference: v1.LocalObjectReference{Name: "app-tls"},
											Items: []v1.KeyToPath{
												{Key: "tls.crt", Path: "tls/tls.crt"},
												{Key: "tls.key", Path: "tls/tls.key", Mode: &secretMode},
											},
										},
									},
								},
							},
						},
					},
					extraVol,
				},
			},
		}

		e.pods = append(e.pods, pod)
		e.podNames = append(e.podNames, volumeutil.GetUniquePodName(pod))
		e.pvVolNames = append(e.pvVolNames, uniquePVName)
		e.projVolNames = append(e.projVolNames, v1.UniqueVolumeName("kubernetes.io/projected/"+string(podUID)+"-kube-api-access"))
		e.extraVolNames = append(e.extraVolNames, extraUnique)

		if i == 2 {
			e.tmpfsPod = pod
		}
		if i == 25 {
			e.resizablePod = pod
			e.resizablePV = e.kubeClient.core.pvs[pvName]
			e.resizableVol = uniquePVName
		}
	}

	// Prepare 2 reusable short-lived Job pod fixtures for per-wave attach/mount -> unmount/detach churn.
	for j := 0; j < 2; j++ {
		ns := "ns-00"
		podUID := k8stypes.UID(fmt.Sprintf("job-pod-uid-%02d", j))
		pvName := fmt.Sprintf("job-pv-%02d", j)
		volID := fmt.Sprintf("job-vol-disk-%02d", j)
		claimName := fmt.Sprintf("job-pvc-%02d", j)
		uniquePVName := v1.UniqueVolumeName(benchAttachPlug + "/" + volID)

		pv := &v1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: pvName},
			Spec: v1.PersistentVolumeSpec{
				AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
				Capacity:    v1.ResourceList{v1.ResourceStorage: qty10Gi.DeepCopy()},
				PersistentVolumeSource: v1.PersistentVolumeSource{
					FlexVolume: &v1.FlexPersistentVolumeSource{
						Driver:  benchAttachPlug,
						FSType:  "ext4",
						Options: map[string]string{"volumeID": volID},
					},
				},
				ClaimRef:   &v1.ObjectReference{Namespace: ns, Name: claimName},
				VolumeMode: &fsMode,
			},
		}
		pvc := &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: claimName},
			Spec: v1.PersistentVolumeClaimSpec{
				AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
				VolumeName:  pvName,
				VolumeMode:  &fsMode,
				Resources: v1.VolumeResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceStorage: qty10Gi.DeepCopy()},
				},
			},
			Status: v1.PersistentVolumeClaimStatus{
				Phase:    v1.ClaimBound,
				Capacity: v1.ResourceList{v1.ResourceStorage: qty10Gi.DeepCopy()},
			},
		}
		e.kubeClient.core.pvs[pvName] = pv
		e.kubeClient.core.pvcs[ns+"/"+claimName] = pvc
		attached = append(attached, v1.AttachedVolume{
			Name:       uniquePVName,
			DevicePath: "/dev/" + volID,
		})

		e.jobPVs[j] = pv
		e.jobPVCs[j] = pvc
		e.jobVolNames[j] = uniquePVName
		e.jobPods[j] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("batch-job-%02d", j),
				Namespace: ns,
				UID:       podUID,
				Labels:    map[string]string{"job": "ephemeral"},
			},
			Spec: v1.PodSpec{
				ServiceAccountName: "default",
				Containers: []v1.Container{
					{
						Name:  "worker",
						Image: "registry.k8s.io/pause:3.10",
						VolumeMounts: []v1.VolumeMount{
							{Name: "job-data", MountPath: "/work"},
							{Name: "job-proj", MountPath: "/var/run/secrets/tokens", ReadOnly: true},
							{Name: "job-scratch", MountPath: "/scratch"},
						},
					},
				},
				Volumes: []v1.Volume{
					{
						Name: "job-data",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
						},
					},
					{
						Name: "job-proj",
						VolumeSource: v1.VolumeSource{
							Projected: &v1.ProjectedVolumeSource{
								DefaultMode: &defaultMode,
								Sources: []v1.VolumeProjection{
									{
										ServiceAccountToken: &v1.ServiceAccountTokenProjection{
											Path:              "token",
											ExpirationSeconds: &expSeconds,
										},
									},
									{
										Secret: &v1.SecretProjection{
											LocalObjectReference: v1.LocalObjectReference{Name: "app-tls"},
										},
									},
								},
							},
						},
					},
					{
						Name: "job-scratch",
						VolumeSource: v1.VolumeSource{
							EmptyDir: &v1.EmptyDirVolumeSource{Medium: v1.StorageMediumDefault},
						},
					},
				},
			},
		}
	}

	// Prepare 4 CSI PersistentVolume specs for CSI plugin path/mounter/SELinux queries.
	for k := 0; k < 4; k++ {
		csiPV := &v1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("csi-pv-%02d", k)},
			Spec: v1.PersistentVolumeSpec{
				AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod},
				Capacity:    v1.ResourceList{v1.ResourceStorage: qty10Gi.DeepCopy()},
				PersistentVolumeSource: v1.PersistentVolumeSource{
					CSI: &v1.CSIPersistentVolumeSource{
						Driver:       benchCSIDriverName,
						VolumeHandle: fmt.Sprintf("csi-handle-%02d", k),
						FSType:       "ext4",
					},
				},
				VolumeMode: &fsMode,
			},
		}
		e.csiSpecs = append(e.csiSpecs, volume.NewSpecFromPersistentVolume(csiPV, false))
	}

	e.node.Status.VolumesAttached = attached
	e.podMgr.inner.SetPods(e.pods)
}

// runWave executes one deterministic, work-normalized Kubelet volume & mount
// lifecycle wave across 60 steady-state pods (180 mounted volumes) + 2 churn Job pods.
func (e *volumeBenchEnv) runWave(step int) {
	// 1. 8 steady-state 100ms Kubelet volume populator + reconciler ticks (10Hz hot loop).
	for tick := 0; tick < 8; tick++ {
		e.stepPopulator()
		e.stepReconciler()
	}

	// 2. Kubelet SyncLoop / PLEG / NodeStatus volume manager read queries across all 60 pods.
	for i, pod := range e.pods {
		podName := e.podNames[i]
		_ = e.vm.GetMountedVolumesForPod(podName)
		_ = e.vm.HasPossiblyMountedVolumesForPod(podName)
		_ = e.vm.GetExtraSupplementalGroupsForPod(pod)
	}
	for s := 0; s < 2; s++ {
		inUse := e.vm.GetVolumesInUse()
		e.vm.MarkVolumesAsReportedInUse(inUse)
	}
	_ = e.vm.ReconcilerStatesHasBeenSynced()
	_ = e.vm.VolumeIsAttached(e.pvVolNames[0])

	newTmpfsSize := resource.NewQuantity(int64(64+((step%4)+1)*8)<<20, resource.BinarySI)
	_ = e.vm.ResizeEphemeralVolume(e.tmpfsPod, "extra-vol", newTmpfsSize)

	for k, spec := range e.csiSpecs {
		_, _ = e.pluginMgr.FindPluginBySpec(spec)
		_, _ = e.csiPlug.SupportsSELinuxContextMount(spec)
		if m, err := e.csiPlug.NewMounter(spec, e.pods[k]); err == nil {
			_ = m.GetPath()
		}
		if u, err := e.csiPlug.NewUnmounter(spec.Name(), e.pods[k].UID); err == nil {
			_ = u.GetPath()
		}
	}

	// 3. Periodic SyncPod remounts:
	//    - 6 pods remount with unchanged Secret/ConfigMap/Token/DownwardAPI payloads
	//      (AtomicWriter.Write idempotency check path).
	//    - 2 pods in ns-09 remount after Secret + ConfigMap + Annotation rotation
	//      (AtomicWriter.Write timestamped dir + ..data atomic rename + stale key cleanup).
	baseIdx := (step * 6) % 50
	remountVols := make([]v1.UniqueVolumeName, 0, 16)
	for r := 0; r < 6; r++ {
		idx := baseIdx + r
		e.vm.desiredStateOfWorldPopulator.ReprocessPod(e.podNames[idx])
		remountVols = append(remountVols, e.projVolNames[idx], e.extraVolNames[idx])
	}

	rotNS := "ns-09"
	e.host.objMu.Lock()
	tlsData := map[string][]byte{
		"tls.crt": []byte(fmt.Sprintf("-----BEGIN CERTIFICATE-----\nMIIB-rotated-cert-%d\n-----END CERTIFICATE-----\n", step)),
		"tls.key": []byte(fmt.Sprintf("-----BEGIN PRIVATE KEY-----\nMIIE-rotated-key-%d\n-----END PRIVATE KEY-----\n", step)),
	}
	credsData := map[string][]byte{
		"username": []byte("svc-user-ns-09"),
		"api-key":  []byte(fmt.Sprintf("rotated-key-%06d", step)),
		"region":   []byte("us-central1"),
	}
	if step%2 == 1 {
		credsData["extra-note"] = []byte("canary-active")
	}
	e.host.secrets[rotNS+"/app-tls"] = &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: rotNS, Name: "app-tls"},
		Data:       tlsData,
	}
	e.host.secrets[rotNS+"/api-creds"] = &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: rotNS, Name: "api-creds"},
		Data:       credsData,
	}
	e.host.configMaps[rotNS+"/kube-root-ca.crt"] = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: rotNS, Name: "kube-root-ca.crt"},
		Data: map[string]string{
			"ca.crt":   "-----BEGIN CERTIFICATE-----\nMIIC-root-ca-ns-09\n-----END CERTIFICATE-----\n",
			"app.conf": fmt.Sprintf("log_level=info\nmax_conns=256\ngen=%d\n", step),
		},
	}
	e.host.objMu.Unlock()

	for _, idx := range []int{55, 57} {
		podCopy := e.pods[idx].DeepCopy()
		podCopy.Annotations["config.kubernetes.io/checksum"] = fmt.Sprintf("rev-%d", step)
		e.pods[idx] = podCopy
		e.podMgr.inner.UpdatePod(podCopy)
		e.vm.desiredStateOfWorldPopulator.ReprocessPod(e.podNames[idx])
		remountVols = append(remountVols, e.projVolNames[idx], e.extraVolNames[idx])
	}

	e.stepPopulator()
	e.stepReconciler()
	e.waitVolumesIdle(remountVols...)

	// 4. Online PV filesystem expansion (fsResize) + 2 ephemeral Job pods attach/mount -> unmount/detach.
	expandedBytes := int64(11+step) << 30 // strictly monotonically increasing GiB across steps
	e.kubeClient.core.mu.Lock()
	pvCopy := e.resizablePV.DeepCopy()
	pvCopy.Spec.Capacity[v1.ResourceStorage] = *resource.NewQuantity(expandedBytes, resource.BinarySI)
	e.resizablePV = pvCopy
	e.kubeClient.core.pvs[pvCopy.Name] = pvCopy
	e.kubeClient.core.mu.Unlock()
	e.vm.desiredStateOfWorldPopulator.ReprocessPod(volumeutil.GetUniquePodName(e.resizablePod))

	for j := 0; j < 2; j++ {
		e.stateProvider.setRemoved(e.jobPods[j].UID, false)
		e.podMgr.inner.AddPod(e.jobPods[j])
	}
	e.stepPopulator()
	e.vm.MarkVolumesAsReportedInUse(e.vm.GetVolumesInUse())
	e.stepReconciler()
	jobProj0 := v1.UniqueVolumeName("kubernetes.io/projected/" + string(e.jobPods[0].UID) + "-job-proj")
	jobProj1 := v1.UniqueVolumeName("kubernetes.io/projected/" + string(e.jobPods[1].UID) + "-job-proj")
	jobEd0 := v1.UniqueVolumeName("kubernetes.io/empty-dir/" + string(e.jobPods[0].UID) + "-job-scratch")
	jobEd1 := v1.UniqueVolumeName("kubernetes.io/empty-dir/" + string(e.jobPods[1].UID) + "-job-scratch")
	e.waitVolumesIdle(e.resizableVol, e.jobVolNames[0], e.jobVolNames[1], jobProj0, jobProj1, jobEd0, jobEd1)
	e.stepReconciler()
	e.waitVolumesIdle(e.jobVolNames[0], e.jobVolNames[1], jobProj0, jobProj1, jobEd0, jobEd1)

	for j := 0; j < 2; j++ {
		_ = e.vm.GetMountedVolumesForPod(volumeutil.GetUniquePodName(e.jobPods[j]))
		e.stateProvider.setRemoved(e.jobPods[j].UID, true)
		e.podMgr.inner.RemovePod(e.jobPods[j])
	}
	e.stepPopulator()
	// Unmount volumes -> UnmountDevice -> MarkVolumeAsDetached
	for u := 0; u < 3; u++ {
		e.stepReconciler()
		e.waitVolumesIdle(e.jobVolNames[0], e.jobVolNames[1], jobProj0, jobProj1, jobEd0, jobEd1)
	}
	for j := 0; j < 2; j++ {
		_ = os.RemoveAll(filepath.Join(e.rootDir, "pods", string(e.jobPods[j].UID)))
	}
}

func BenchmarkVolumeMountLifecycle(b *testing.B) {
	env := newVolumeBenchEnv(b, false)
	defer env.close()

	// Warm up 2 full waves so steady-state pools/directories are initialized before measurement.
	env.runWave(1)
	env.runWave(2)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env.runWave(i + 3)
	}
	b.StopTimer()
}

func TestVolumeMountLifecycleEquivalence(t *testing.T) {
	t.Run("AttachMountOrderingProjectedAtomicWriterAndFSResize", func(t *testing.T) {
		env := newVolumeBenchEnv(t, true)
		defer env.close()

		// 1. Verify initial projected volume on-disk files, permissions, and ..data symlink.
		pod0 := env.pods[0]
		projDir := env.host.GetPodVolumeDir(pod0.UID, "kubernetes.io~projected", "kube-api-access")
		initialDataTarget, err := os.Readlink(filepath.Join(projDir, "..data"))
		require.NoError(t, err)
		require.NotEmpty(t, initialDataTarget)

		tokenBytes, err := os.ReadFile(filepath.Join(projDir, "token"))
		require.NoError(t, err)
		require.Contains(t, string(tokenBytes), "eyJhbGciOiJSUzI1NiJ9.ns-00.default")

		caBytes, err := os.ReadFile(filepath.Join(projDir, "ca.crt"))
		require.NoError(t, err)
		require.Contains(t, string(caBytes), "MIIC-root-ca-ns-00")

		confBytes, err := os.ReadFile(filepath.Join(projDir, "config/app.conf"))
		require.NoError(t, err)
		require.Contains(t, string(confBytes), "namespace=ns-00")

		nameBytes, err := os.ReadFile(filepath.Join(projDir, "podinfo/name"))
		require.NoError(t, err)
		require.Equal(t, pod0.Name, string(nameBytes))

		tlsKeyInfo, err := os.Stat(filepath.Join(projDir, "tls/tls.key"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0400), tlsKeyInfo.Mode().Perm())

		// 2. Verify unchanged projected remount is idempotent and preserves ..data target without rewriting.
		env.vm.desiredStateOfWorldPopulator.ReprocessPod(env.podNames[0])
		env.stepPopulator()
		env.stepReconciler()
		env.waitVolumesIdle(env.projVolNames[0], env.extraVolNames[0])

		unchangedDataTarget, err := os.Readlink(filepath.Join(projDir, "..data"))
		require.NoError(t, err)
		require.Equal(t, initialDataTarget, unchangedDataTarget, "unchanged payload remount must not create a new timestamp directory")

		// 3. Verify SELinux context propagation for ReadWriteOncePod persistent volumes.
		env.attachPlug.mu.Lock()
		seCtx25 := env.attachPlug.seLinuxCtx["vol-disk-025"]
		env.attachPlug.mu.Unlock()
		require.Equal(t, "system_u:object_r:container_file_t:s0:c26,c126", seCtx25)

		// 4. Verify GetExtraSupplementalGroupsForPod and sorted deduplicated GetVolumesInUse.
		extraGIDs := env.vm.GetExtraSupplementalGroupsForPod(env.pods[0])
		require.Equal(t, []int64{3000}, extraGIDs)
		require.Empty(t, env.vm.GetExtraSupplementalGroupsForPod(env.pods[2])) // pv-001 has no GID annotation

		inUse := env.vm.GetVolumesInUse()
		require.Equal(t, 50, len(inUse))
		require.True(t, slices.IsSorted(inUse), "GetVolumesInUse must remain sorted")

		// 5. Execute two full waves and verify rotated Secret/ConfigMap/DownwardAPI projection,
		// stale key removal, online fsResize, and Job pod attach -> mount -> unmount -> detach ordering.
		secDir57 := env.host.GetPodVolumeDir(env.pods[57].UID, "kubernetes.io~secret", "extra-vol")
		projDir57 := env.host.GetPodVolumeDir(env.pods[57].UID, "kubernetes.io~projected", "kube-api-access")
		oldTs57, err := os.Readlink(filepath.Join(projDir57, "..data"))
		require.NoError(t, err)

		// Wave 1 (odd step: adds extra-note to secret, rotates cert & annotations, expands pv-025 to 12Gi)
		// Note: standalone Secret volume on pod 57 uses explicit Items mapping; pod 55 uses items too.
		// Check pod 57 projected volume atomic swap + old timestamp cleanup:
		env.attachPlug.mu.Lock()
		env.attachPlug.events = nil
		env.attachPlug.mu.Unlock()

		env.runWave(1)

		newTs57, err := os.Readlink(filepath.Join(projDir57, "..data"))
		require.NoError(t, err)
		require.NotEqual(t, oldTs57, newTs57, "rotated projected payload must atomically swap ..data symlink")
		_, err = os.Stat(filepath.Join(projDir57, oldTs57))
		require.True(t, os.IsNotExist(err), "previous timestamped directory must be removed after atomic swap")

		rotCert, err := os.ReadFile(filepath.Join(projDir57, "tls/tls.crt"))
		require.NoError(t, err)
		require.Contains(t, string(rotCert), "MIIB-rotated-cert-1")

		rotAnn, err := os.ReadFile(filepath.Join(projDir57, "podinfo/annotations"))
		require.NoError(t, err)
		require.Contains(t, string(rotAnn), "rev-1")

		rotSecKey, err := os.ReadFile(filepath.Join(secDir57, "creds/api-key"))
		require.NoError(t, err)
		require.Equal(t, "rotated-key-000001", string(rotSecKey))

		// Verify online fsResize updated ActualStateOfWorld claim size and called NodeExpand.
		claimSize := env.vm.actualStateOfWorld.GetClaimSize(env.resizableVol)
		require.Equal(t, int64(12)<<30, claimSize.Value())
		env.attachPlug.mu.Lock()
		expandedVal := env.attachPlug.resizedSizes["vol-disk-025"]
		events := append([]string(nil), env.attachPlug.events...)
		env.attachPlug.mu.Unlock()
		require.Equal(t, int64(12)<<30, expandedVal)

		// Verify Job pod volume lifecycle ordering: WaitForAttach -> MountDevice -> SetUp -> TearDown -> UnmountDevice.
		idxWait := slices.Index(events, "WaitForAttach:job-vol-disk-00")
		idxMntDev := slices.Index(events, "MountDevice:job-vol-disk-00")
		idxSetUp := slices.Index(events, "SetUp:job-pv-00")
		idxTearDown := slices.Index(events, "TearDown:job-pv-00")
		idxUnmntDev := slices.Index(events, "UnmountDevice:job-vol-disk-00")
		require.True(t, idxWait >= 0 && idxWait < idxMntDev, "WaitForAttach must precede MountDevice: %v", events)
		require.True(t, idxMntDev < idxSetUp, "MountDevice must precede SetUp: %v", events)
		require.True(t, idxSetUp < idxTearDown, "SetUp must precede TearDown: %v", events)
		require.True(t, idxTearDown < idxUnmntDev, "TearDown must precede UnmountDevice: %v", events)

		// Verify Job pods have zero residual mounted volumes after teardown.
		for j := 0; j < 2; j++ {
			jobPodName := volumeutil.GetUniquePodName(env.jobPods[j])
			require.False(t, env.vm.HasPossiblyMountedVolumesForPod(jobPodName))
			require.Empty(t, env.vm.GetMountedVolumesForPod(jobPodName))
		}
	})

	t.Run("AtomicWriterKeyRemovalAndSELinuxMismatch", func(t *testing.T) {
		dir := t.TempDir()
		writer, err := volumeutil.NewAtomicWriter(dir, "test-writer")
		require.NoError(t, err)

		require.NoError(t, writer.Write(map[string]volumeutil.FileProjection{
			"a/one.txt": {Data: []byte("111"), Mode: 0644},
			"b/two.txt": {Data: []byte("222"), Mode: 0600},
		}, nil))
		// Rewrite removing b/two.txt and updating a/one.txt.
		require.NoError(t, writer.Write(map[string]volumeutil.FileProjection{
			"a/one.txt": {Data: []byte("111-updated"), Mode: 0644},
		}, nil))

		got, err := os.ReadFile(filepath.Join(dir, "a/one.txt"))
		require.NoError(t, err)
		require.Equal(t, "111-updated", string(got))
		_, err = os.Lstat(filepath.Join(dir, "b/two.txt"))
		require.True(t, os.IsNotExist(err), "removed projected file symlink must be deleted")

		// Verify SELinuxMountMismatchError is recorded on DSW when a second pod requests
		// the same attached volume with a conflicting SELinux level.
		env := newVolumeBenchEnv(t, false)
		defer env.close()

		conflictPod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "selinux-conflict-pod",
				Namespace: env.resizablePod.Namespace,
				UID:       "selinux-conflict-uid",
			},
			Spec: v1.PodSpec{
				SecurityContext: &v1.PodSecurityContext{
					SELinuxOptions: &v1.SELinuxOptions{
						User:  "system_u",
						Type:  "container_t",
						Level: "s0:c999,c1000",
					},
				},
				Containers: []v1.Container{
					{
						Name:         "app",
						VolumeMounts: []v1.VolumeMount{{Name: "data-pv", MountPath: "/data"}},
					},
				},
				Volumes: []v1.Volume{
					{
						Name: "data-pv",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: "pvc-025"},
						},
					},
				},
			},
		}
		env.podMgr.inner.AddPod(conflictPod)
		env.stepPopulator()
		env.stepReconciler()

		errs := env.vm.desiredStateOfWorld.PopPodErrors(volumeutil.GetUniquePodName(conflictPod))
		require.NotEmpty(t, errs, "conflicting SELinux context must record SELinuxMountMismatchError on pod")
		require.Empty(t, env.vm.GetMountedVolumesForPod(volumeutil.GetUniquePodName(conflictPod)))
	})

	t.Run("ConcurrentReadersAndReconcileRaceSafety", func(t *testing.T) {
		env := newVolumeBenchEnv(t, false)
		defer env.close()

		podsSnapshot := slices.Clone(env.pods)
		stopReaders := make(chan struct{})
		var readerWG sync.WaitGroup
		for w := 0; w < 4; w++ {
			readerWG.Add(1)
			go func(worker int) {
				defer readerWG.Done()
				for i := 0; ; i++ {
					select {
					case <-stopReaders:
						return
					default:
					}
					idx := (worker*15 + i) % len(podsSnapshot)
					_ = env.vm.GetMountedVolumesForPod(env.podNames[idx])
					_ = env.vm.HasPossiblyMountedVolumesForPod(env.podNames[idx])
					_ = env.vm.GetExtraSupplementalGroupsForPod(podsSnapshot[idx])
					_ = env.vm.GetVolumesInUse()
				}
			}(w)
		}

		for step := 1; step <= 4; step++ {
			env.runWave(step)
		}
		close(stopReaders)
		readerWG.Wait()
	})
}
