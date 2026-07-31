// Package collector reads the state of a pod from a Kubernetes cluster and
// flattens it into a DiagnosticContext.
//
// This layer is deliberately dumb: it reads, it converts, it never decides why
// something is broken. All diagnosis lives in internal/rules and, as a last
// resort, internal/llmfallback.
package collector

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// logTailLines is how many lines of container logs we pull. Enough to catch a
// panic or a stack trace, small enough to stay readable and cheap to ship to
// an LLM.
const logTailLines = 100

// Collect gathers everything known about a pod: its spec and status, the
// namespace events targeting it, the logs of the current and previous
// container instances, and the state of the PVCs and node it depends on.
//
// Failures to read the optional parts (logs, node, PVCs) are recorded in the
// returned context rather than propagated: a pod stuck in ImagePullBackOff has
// no logs to read, and that absence is itself a useful signal. Only the pod
// read itself is fatal.
func Collect(clientset kubernetes.Interface, namespace, podName string) (*DiagnosticContext, error) {
	return CollectWithContext(context.Background(), clientset, namespace, podName)
}

// CollectWithContext is Collect with an explicit context for cancellation and
// timeouts.
func CollectWithContext(ctx context.Context, clientset kubernetes.Interface, namespace, podName string) (*DiagnosticContext, error) {
	if clientset == nil {
		return nil, fmt.Errorf("collector: nil clientset")
	}
	if podName == "" {
		return nil, fmt.Errorf("collector: empty pod name")
	}
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}

	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("pod %q not found in namespace %q", podName, namespace)
		}
		return nil, fmt.Errorf("reading pod %s/%s: %w", namespace, podName, err)
	}

	dc := &DiagnosticContext{
		Pod:         podInfo(pod),
		Containers:  containerInfos(ctx, clientset, pod),
		Events:      podEvents(ctx, clientset, pod),
		CollectedAt: time.Now().UTC(),
	}
	dc.Node = nodeInfo(ctx, clientset, pod.Spec.NodeName)
	dc.PVCs = pvcInfos(ctx, clientset, pod)

	return dc, nil
}

func podInfo(pod *corev1.Pod) PodInfo {
	pi := PodInfo{
		Name:           pod.Name,
		Namespace:      pod.Namespace,
		Phase:          string(pod.Status.Phase),
		Reason:         pod.Status.Reason,
		Message:        pod.Status.Message,
		NodeName:       pod.Spec.NodeName,
		QOSClass:       string(pod.Status.QOSClass),
		RestartPolicy:  string(pod.Spec.RestartPolicy),
		Labels:         pod.Labels,
		NodeSelector:   pod.Spec.NodeSelector,
		ServiceAccount: pod.Spec.ServiceAccountName,
	}
	if pod.Status.StartTime != nil {
		pi.StartTime = timePtr(pod.Status.StartTime.Time)
	}
	if pod.DeletionTimestamp != nil {
		pi.DeletionTimestamp = timePtr(pod.DeletionTimestamp.Time)
	}
	for _, s := range pod.Spec.ImagePullSecrets {
		pi.ImagePullSecrets = append(pi.ImagePullSecrets, s.Name)
	}
	for _, c := range pod.Status.Conditions {
		pc := PodCondition{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		}
		if !c.LastTransitionTime.IsZero() {
			pc.LastTransitionTime = timePtr(c.LastTransitionTime.Time)
		}
		pi.Conditions = append(pi.Conditions, pc)
	}
	for _, v := range pod.Spec.Volumes {
		pi.Volumes = append(pi.Volumes, volumeInfo(v))
	}
	return pi
}

func volumeInfo(v corev1.Volume) VolumeInfo {
	vi := VolumeInfo{Name: v.Name}
	switch {
	case v.PersistentVolumeClaim != nil:
		vi.Type = "persistentVolumeClaim"
		vi.Source = v.PersistentVolumeClaim.ClaimName
	case v.ConfigMap != nil:
		vi.Type = "configMap"
		vi.Source = v.ConfigMap.Name
		vi.Optional = v.ConfigMap.Optional != nil && *v.ConfigMap.Optional
	case v.Secret != nil:
		vi.Type = "secret"
		vi.Source = v.Secret.SecretName
		vi.Optional = v.Secret.Optional != nil && *v.Secret.Optional
	case v.EmptyDir != nil:
		vi.Type = "emptyDir"
	case v.HostPath != nil:
		vi.Type = "hostPath"
		vi.Source = v.HostPath.Path
	case v.Projected != nil:
		vi.Type = "projected"
	default:
		vi.Type = "other"
	}
	return vi
}

// containerInfos merges spec and status for every container of the pod, init
// containers first, and attaches their logs.
func containerInfos(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod) []ContainerInfo {
	statuses := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	for _, s := range pod.Status.ContainerStatuses {
		statuses[s.Name] = s
	}
	for _, s := range pod.Status.InitContainerStatuses {
		statuses[s.Name] = s
	}

	var out []ContainerInfo
	for _, spec := range pod.Spec.InitContainers {
		out = append(out, containerInfo(ctx, clientset, pod, spec, statuses[spec.Name], true))
	}
	for _, spec := range pod.Spec.Containers {
		out = append(out, containerInfo(ctx, clientset, pod, spec, statuses[spec.Name], false))
	}
	return out
}

func containerInfo(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod, spec corev1.Container, status corev1.ContainerStatus, isInit bool) ContainerInfo {
	ci := ContainerInfo{
		Name:           spec.Name,
		Image:          spec.Image,
		ImageID:        status.ImageID,
		IsInit:         isInit,
		Ready:          status.Ready,
		RestartCount:   status.RestartCount,
		Command:        spec.Command,
		Args:           spec.Args,
		WorkDir:        spec.WorkingDir,
		Resources:      resources(spec.Resources),
		LivenessProbe:  probe(spec.LivenessProbe),
		ReadinessProbe: probe(spec.ReadinessProbe),
		StartupProbe:   probe(spec.StartupProbe),
		State:          containerState(status.State),
		EnvRefs:        envRefs(spec),
	}
	if status.Started != nil {
		ci.Started = *status.Started
	}
	if last := containerState(status.LastTerminationState); last.Type != "" {
		ci.LastState = &last
	}
	for _, p := range spec.Ports {
		ci.Ports = append(ci.Ports, PortInfo{
			Name:          p.Name,
			ContainerPort: p.ContainerPort,
			Protocol:      string(p.Protocol),
		})
	}
	for _, m := range spec.VolumeMounts {
		ci.Mounts = append(ci.Mounts, MountInfo{
			VolumeName: m.Name,
			MountPath:  m.MountPath,
			ReadOnly:   m.ReadOnly,
		})
	}

	ci.Logs, ci.LogsErr = containerLogs(ctx, clientset, pod, spec.Name, false)
	// The previous instance's logs are the interesting ones for a crash loop:
	// they hold the panic that the current (waiting) instance has not reprinted.
	if status.RestartCount > 0 || status.LastTerminationState.Terminated != nil {
		ci.PreviousLogs, ci.PreviousLogsErr = containerLogs(ctx, clientset, pod, spec.Name, true)
	}
	return ci
}

func containerLogs(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod, container string, previous bool) (string, string) {
	tail := int64(logTailLines)
	req := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
		TailLines: &tail,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err.Error()
	}
	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		return strings.TrimSpace(string(data)), err.Error()
	}
	return strings.TrimSpace(string(data)), ""
}

func containerState(s corev1.ContainerState) ContainerState {
	switch {
	case s.Waiting != nil:
		return ContainerState{
			Type:    "Waiting",
			Reason:  s.Waiting.Reason,
			Message: s.Waiting.Message,
		}
	case s.Running != nil:
		cs := ContainerState{Type: "Running"}
		if !s.Running.StartedAt.IsZero() {
			cs.StartedAt = timePtr(s.Running.StartedAt.Time)
		}
		return cs
	case s.Terminated != nil:
		cs := ContainerState{
			Type:     "Terminated",
			Reason:   s.Terminated.Reason,
			Message:  s.Terminated.Message,
			ExitCode: s.Terminated.ExitCode,
			Signal:   s.Terminated.Signal,
		}
		if !s.Terminated.StartedAt.IsZero() {
			cs.StartedAt = timePtr(s.Terminated.StartedAt.Time)
		}
		if !s.Terminated.FinishedAt.IsZero() {
			cs.FinishedAt = timePtr(s.Terminated.FinishedAt.Time)
		}
		return cs
	default:
		return ContainerState{}
	}
}

func resources(rr corev1.ResourceRequirements) Resources {
	var r Resources
	if q, ok := rr.Requests[corev1.ResourceCPU]; ok {
		r.RequestsCPU = q.String()
		r.RequestsCPUMillis = q.MilliValue()
	}
	if q, ok := rr.Requests[corev1.ResourceMemory]; ok {
		r.RequestsMemory = q.String()
		r.RequestsMemoryBytes = q.Value()
	}
	if q, ok := rr.Limits[corev1.ResourceCPU]; ok {
		r.LimitsCPU = q.String()
		r.LimitsCPUMillis = q.MilliValue()
	}
	if q, ok := rr.Limits[corev1.ResourceMemory]; ok {
		r.LimitsMemory = q.String()
		r.LimitsMemoryBytes = q.Value()
	}
	return r
}

func probe(p *corev1.Probe) *Probe {
	if p == nil {
		return nil
	}
	out := &Probe{
		InitialDelaySeconds: p.InitialDelaySeconds,
		PeriodSeconds:       p.PeriodSeconds,
		TimeoutSeconds:      p.TimeoutSeconds,
		SuccessThreshold:    p.SuccessThreshold,
		FailureThreshold:    p.FailureThreshold,
	}
	switch {
	case p.HTTPGet != nil:
		out.Kind = "http"
		out.Path = p.HTTPGet.Path
		out.Port = p.HTTPGet.Port.String()
		out.Scheme = string(p.HTTPGet.Scheme)
		out.Host = p.HTTPGet.Host
	case p.TCPSocket != nil:
		out.Kind = "tcp"
		out.Port = p.TCPSocket.Port.String()
		out.Host = p.TCPSocket.Host
	case p.Exec != nil:
		out.Kind = "exec"
		out.Command = p.Exec.Command
	case p.GRPC != nil:
		out.Kind = "grpc"
		out.Port = fmt.Sprint(p.GRPC.Port)
	}
	return out
}

func envRefs(spec corev1.Container) []EnvRef {
	var refs []EnvRef
	for _, e := range spec.Env {
		if e.ValueFrom == nil {
			continue
		}
		switch {
		case e.ValueFrom.ConfigMapKeyRef != nil:
			r := e.ValueFrom.ConfigMapKeyRef
			refs = append(refs, EnvRef{
				From:     "configMap",
				Name:     r.Name,
				Key:      r.Key,
				EnvVar:   e.Name,
				Optional: r.Optional != nil && *r.Optional,
			})
		case e.ValueFrom.SecretKeyRef != nil:
			r := e.ValueFrom.SecretKeyRef
			refs = append(refs, EnvRef{
				From:     "secret",
				Name:     r.Name,
				Key:      r.Key,
				EnvVar:   e.Name,
				Optional: r.Optional != nil && *r.Optional,
			})
		}
	}
	for _, f := range spec.EnvFrom {
		switch {
		case f.ConfigMapRef != nil:
			refs = append(refs, EnvRef{
				From:     "configMap",
				Name:     f.ConfigMapRef.Name,
				Optional: f.ConfigMapRef.Optional != nil && *f.ConfigMapRef.Optional,
			})
		case f.SecretRef != nil:
			refs = append(refs, EnvRef{
				From:     "secret",
				Name:     f.SecretRef.Name,
				Optional: f.SecretRef.Optional != nil && *f.SecretRef.Optional,
			})
		}
	}
	return refs
}

// podEvents lists the namespace events and keeps those pointing at this pod.
// Filtering client-side (rather than with a field selector) keeps the call
// working against API servers and fakes that ignore field selectors.
func podEvents(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod) []Event {
	list, err := clientset.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []Event
	for i := range list.Items {
		e := &list.Items[i]
		if e.InvolvedObject.Kind != "Pod" || e.InvolvedObject.Name != pod.Name {
			continue
		}
		if pod.UID != "" && e.InvolvedObject.UID != "" && e.InvolvedObject.UID != pod.UID {
			continue
		}
		out = append(out, convertEvent(e))
	}
	sortEvents(out)
	return out
}

func convertEvent(e *corev1.Event) Event {
	ev := Event{
		Type:         e.Type,
		Reason:       e.Reason,
		Message:      e.Message,
		Source:       e.Source.Component,
		Count:        e.Count,
		InvolvedKind: e.InvolvedObject.Kind,
		InvolvedName: e.InvolvedObject.Name,
	}
	if !e.FirstTimestamp.IsZero() {
		ev.FirstTimestamp = timePtr(e.FirstTimestamp.Time)
	}
	if !e.LastTimestamp.IsZero() {
		ev.LastTimestamp = timePtr(e.LastTimestamp.Time)
	}
	return ev
}

// sortEvents orders events oldest first, so the last entry is the most recent.
func sortEvents(evs []Event) {
	sort.SliceStable(evs, func(i, j int) bool {
		return eventTime(evs[i]).Before(eventTime(evs[j]))
	})
}

func eventTime(e Event) time.Time {
	if e.LastTimestamp != nil {
		return *e.LastTimestamp
	}
	if e.FirstTimestamp != nil {
		return *e.FirstTimestamp
	}
	return time.Time{}
}

// nodeInfo reads the node the pod was scheduled on. A pod stuck in Pending has
// none, and a user may lack RBAC on nodes; both cases return nil.
func nodeInfo(ctx context.Context, clientset kubernetes.Interface, nodeName string) *NodeInfo {
	if nodeName == "" {
		return nil
	}
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	ni := &NodeInfo{
		Name:          node.Name,
		Architecture:  node.Status.NodeInfo.Architecture,
		OS:            node.Status.NodeInfo.OperatingSystem,
		Unschedulable: node.Spec.Unschedulable,
		Labels:        node.Labels,
		Allocatable:   map[string]string{},
	}
	for res, q := range node.Status.Allocatable {
		ni.Allocatable[string(res)] = q.String()
	}
	for _, c := range node.Status.Conditions {
		ni.Conditions = append(ni.Conditions, NodeCondition{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}
	for _, t := range node.Spec.Taints {
		ni.Taints = append(ni.Taints, Taint{Key: t.Key, Value: t.Value, Effect: string(t.Effect)})
	}
	return ni
}

// pvcInfos reads every PVC the pod mounts, together with the events targeting
// that claim (where the "no persistent volumes available" message lives).
func pvcInfos(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod) []PVCInfo {
	var out []PVCInfo
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		name := v.PersistentVolumeClaim.ClaimName
		pvc, err := clientset.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			// The claim may simply not exist, which is exactly what a rule
			// wants to know about.
			out = append(out, PVCInfo{Name: name, Phase: "NotFound"})
			continue
		}
		info := PVCInfo{
			Name:         pvc.Name,
			Phase:        string(pvc.Status.Phase),
			StorageClass: pvc.Spec.StorageClassName,
			VolumeName:   pvc.Spec.VolumeName,
			Events:       objectEvents(ctx, clientset, pod.Namespace, "PersistentVolumeClaim", pvc.Name),
		}
		if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			info.RequestedStorage = q.String()
		}
		for _, m := range pvc.Spec.AccessModes {
			info.AccessModes = append(info.AccessModes, string(m))
		}
		out = append(out, info)
	}
	return out
}

func objectEvents(ctx context.Context, clientset kubernetes.Interface, namespace, kind, name string) []Event {
	list, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []Event
	for i := range list.Items {
		e := &list.Items[i]
		if e.InvolvedObject.Kind != kind || e.InvolvedObject.Name != name {
			continue
		}
		out = append(out, convertEvent(e))
	}
	sortEvents(out)
	return out
}

func timePtr(t time.Time) *time.Time {
	u := t.UTC()
	return &u
}
