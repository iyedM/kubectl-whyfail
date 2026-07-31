package collector

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func crashingPod() *corev1.Pod {
	started := false
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-7d9f8b6c-x2kpl",
			Namespace: "production",
			UID:       "pod-uid-1",
			Labels:    map[string]string{"app": "api"},
		},
		Spec: corev1.PodSpec{
			NodeName:           "node-a",
			RestartPolicy:      corev1.RestartPolicyAlways,
			ServiceAccountName: "api",
			ImagePullSecrets:   []corev1.LocalObjectReference{{Name: "regcred"}},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "api-data"},
					},
				},
			},
			InitContainers: []corev1.Container{{Name: "init-db", Image: "busybox:1.36"}},
			Containers: []corev1.Container{
				{
					Name:    "api",
					Image:   "acme/api:2.0.0",
					Command: []string{"/app/api"},
					Args:    []string{"--port=8080"},
					Ports:   []corev1.ContainerPort{{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/healthz",
								Port:   intstr.FromInt32(8080),
								Scheme: corev1.URISchemeHTTP,
							},
						},
						InitialDelaySeconds: 3,
						PeriodSeconds:       5,
						FailureThreshold:    2,
					},
					Env: []corev1.EnvVar{
						{
							Name: "DATABASE_URL",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "api-db"},
									Key:                  "dsn",
								},
							},
						},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/api"}},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSBurstable,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "api",
					Image:        "acme/api:2.0.0",
					Ready:        false,
					Started:      &started,
					RestartCount: 7,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "CrashLoopBackOff",
							Message: "back-off 2m40s restarting failed container",
						},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:   "Error",
							ExitCode: 137,
						},
					},
				},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "init-db",
					Ready: true,
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0},
					},
				},
			},
		},
	}
}

func TestCollect(t *testing.T) {
	pod := crashingPod()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"kubernetes.io/arch": "amd64"}},
		Spec:       corev1.NodeSpec{Taints: []corev1.Taint{{Key: "dedicated", Value: "api", Effect: corev1.TaintEffectNoSchedule}}},
		Status: corev1.NodeStatus{
			NodeInfo:    corev1.NodeSystemInfo{Architecture: "amd64", OperatingSystem: "linux"},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "api-data", Namespace: "production"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}

	now := metav1.NewTime(time.Now())
	mine := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-1", Namespace: "production"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, UID: pod.UID},
		Type:           corev1.EventTypeWarning,
		Reason:         "Unhealthy",
		Message:        "Liveness probe failed: connection refused",
		Source:         corev1.EventSource{Component: "kubelet"},
		Count:          21,
		FirstTimestamp: now,
		LastTimestamp:  now,
	}
	// An event for a different pod in the same namespace must be filtered out.
	theirs := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-2", Namespace: "production"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "some-other-pod", UID: "pod-uid-2"},
		Type:           corev1.EventTypeWarning,
		Reason:         "BackOff",
		Message:        "should not appear",
		FirstTimestamp: now,
		LastTimestamp:  now,
	}

	client := fake.NewSimpleClientset(pod, node, pvc, mine, theirs)

	dc, err := Collect(client, "production", pod.Name)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if dc.Pod.Name != pod.Name || dc.Pod.Namespace != "production" {
		t.Errorf("pod identity not carried through: %+v", dc.Pod)
	}
	if dc.Pod.Phase != "Running" || dc.Pod.QOSClass != "Burstable" {
		t.Errorf("pod status not carried through: phase=%q qos=%q", dc.Pod.Phase, dc.Pod.QOSClass)
	}
	if len(dc.Pod.ImagePullSecrets) != 1 || dc.Pod.ImagePullSecrets[0] != "regcred" {
		t.Errorf("imagePullSecrets not collected: %v", dc.Pod.ImagePullSecrets)
	}
	if len(dc.Pod.Volumes) != 1 || dc.Pod.Volumes[0].Type != "persistentVolumeClaim" || dc.Pod.Volumes[0].Source != "api-data" {
		t.Errorf("volume not flattened: %+v", dc.Pod.Volumes)
	}

	// Init containers come first and are flagged.
	if len(dc.Containers) != 2 {
		t.Fatalf("expected 2 containers (1 init + 1 app), got %d", len(dc.Containers))
	}
	if !dc.Containers[0].IsInit || dc.Containers[0].Name != "init-db" {
		t.Errorf("init container should come first and be flagged: %+v", dc.Containers[0])
	}

	api := dc.Containers[1]
	if api.RestartCount != 7 {
		t.Errorf("restart count = %d, want 7", api.RestartCount)
	}
	if api.State.Type != "Waiting" || api.State.Reason != "CrashLoopBackOff" {
		t.Errorf("current state not flattened: %+v", api.State)
	}
	if api.LastState == nil || api.LastState.ExitCode != 137 {
		t.Errorf("last termination state not flattened: %+v", api.LastState)
	}
	if api.LivenessProbe == nil || api.LivenessProbe.Kind != "http" || api.LivenessProbe.Port != "8080" || api.LivenessProbe.Path != "/healthz" {
		t.Errorf("liveness probe not flattened: %+v", api.LivenessProbe)
	}
	if api.Resources.LimitsMemoryBytes != 512*1024*1024 {
		t.Errorf("memory limit = %d bytes, want %d", api.Resources.LimitsMemoryBytes, 512*1024*1024)
	}
	if api.Resources.RequestsCPUMillis != 250 {
		t.Errorf("cpu request = %dm, want 250m", api.Resources.RequestsCPUMillis)
	}
	if len(api.EnvRefs) != 1 || api.EnvRefs[0].From != "secret" || api.EnvRefs[0].Key != "dsn" {
		t.Errorf("env refs not collected: %+v", api.EnvRefs)
	}
	if len(api.Mounts) != 1 || api.Mounts[0].MountPath != "/var/lib/api" {
		t.Errorf("mounts not collected: %+v", api.Mounts)
	}
	// A restarted container must have had its previous logs requested.
	if api.PreviousLogs == "" && api.PreviousLogsErr == "" {
		t.Error("previous logs were never fetched for a restarted container")
	}

	// Only this pod's events, and nothing from the neighbour.
	if len(dc.Events) != 1 {
		t.Fatalf("expected exactly 1 event for this pod, got %d: %+v", len(dc.Events), dc.Events)
	}
	if dc.Events[0].Reason != "Unhealthy" || dc.Events[0].Source != "kubelet" || dc.Events[0].Count != 21 {
		t.Errorf("event not flattened: %+v", dc.Events[0])
	}

	if dc.Node == nil || dc.Node.Architecture != "amd64" {
		t.Fatalf("node info not collected: %+v", dc.Node)
	}
	if len(dc.Node.Taints) != 1 || dc.Node.Taints[0].Effect != "NoSchedule" {
		t.Errorf("node taints not collected: %+v", dc.Node.Taints)
	}

	if len(dc.PVCs) != 1 || dc.PVCs[0].Phase != "Bound" || dc.PVCs[0].RequestedStorage != "20Gi" {
		t.Errorf("pvc not collected: %+v", dc.PVCs)
	}

	if dc.CollectedAt.IsZero() {
		t.Error("CollectedAt was not set")
	}
	// The collector must not decide anything about language.
	if dc.Lang != "" {
		t.Errorf("collector set Lang to %q; that is the caller's job", dc.Lang)
	}
}

func TestCollectMissingPod(t *testing.T) {
	client := fake.NewSimpleClientset()

	_, err := Collect(client, "production", "ghost")
	if err == nil {
		t.Fatal("expected an error for a pod that does not exist")
	}
	if got := err.Error(); got != `pod "ghost" not found in namespace "production"` {
		t.Errorf("error should be actionable, got: %s", got)
	}
}

func TestCollectOptionalReadsDegradeGracefully(t *testing.T) {
	// A Pending pod has no node and its PVC may not exist yet. Neither is
	// fatal: those absences are themselves diagnostic signal.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-0", Namespace: "data"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "missing-claim"}},
			}},
			Containers: []corev1.Container{{Name: "app", Image: "app:1"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	dc, err := Collect(fake.NewSimpleClientset(pod), "data", "pending-0")
	if err != nil {
		t.Fatalf("Collect should not fail when optional reads fail: %v", err)
	}
	if dc.Node != nil {
		t.Errorf("unscheduled pod should have no node info, got %+v", dc.Node)
	}
	if len(dc.PVCs) != 1 || dc.PVCs[0].Phase != "NotFound" {
		t.Errorf("a missing claim should be reported as NotFound, got %+v", dc.PVCs)
	}
}

func TestCollectRejectsBadInput(t *testing.T) {
	if _, err := Collect(nil, "ns", "pod"); err == nil {
		t.Error("expected an error for a nil clientset")
	}
	if _, err := Collect(fake.NewSimpleClientset(), "ns", ""); err == nil {
		t.Error("expected an error for an empty pod name")
	}
}

func TestLangHelper(t *testing.T) {
	var dc DiagnosticContext
	if got := dc.L("english", "français"); got != "english" {
		t.Errorf("default language should be English, got %q", got)
	}
	dc.Lang = "fr"
	if got := dc.L("english", "français"); got != "français" {
		t.Errorf("Lang=fr should select French, got %q", got)
	}
	var nilCtx *DiagnosticContext
	if got := nilCtx.L("english", "français"); got != "english" {
		t.Errorf("nil context should fall back to English, got %q", got)
	}
}
