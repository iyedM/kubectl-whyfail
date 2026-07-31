package collector

import "time"

// DiagnosticContext is a flat, JSON-serializable snapshot of everything the
// rules engine and the LLM fallback are allowed to look at.
//
// It deliberately contains no Kubernetes API types: rules must be testable
// from a plain JSON fixture in testdata/, without a cluster and without
// client-go. The collector is the only thing that knows how to fill it in.
type DiagnosticContext struct {
	Pod        PodInfo         `json:"pod"`
	Containers []ContainerInfo `json:"containers"`
	Events     []Event         `json:"events"`
	Node       *NodeInfo       `json:"node,omitempty"`
	PVCs       []PVCInfo       `json:"pvcs,omitempty"`

	// Lang is the language requested for user-facing messages ("en" or "fr").
	// It carries a presentation choice, not diagnostic data; it is set by the
	// caller, never by the collector.
	Lang string `json:"lang,omitempty"`

	// CollectedAt is when the snapshot was taken, used for age-based reasoning.
	CollectedAt time.Time `json:"collectedAt"`
}

// PodInfo is the pod-level view: what `kubectl describe pod` shows at the top.
type PodInfo struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Phase             string            `json:"phase"`
	Reason            string            `json:"reason,omitempty"`
	Message           string            `json:"message,omitempty"`
	NodeName          string            `json:"nodeName,omitempty"`
	QOSClass          string            `json:"qosClass,omitempty"`
	RestartPolicy     string            `json:"restartPolicy,omitempty"`
	Conditions        []PodCondition    `json:"conditions,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	ImagePullSecrets  []string          `json:"imagePullSecrets,omitempty"`
	ServiceAccount    string            `json:"serviceAccount,omitempty"`
	Volumes           []VolumeInfo      `json:"volumes,omitempty"`
	StartTime         *time.Time        `json:"startTime,omitempty"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
}

// PodCondition mirrors a single entry of pod.status.conditions.
type PodCondition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	Reason             string     `json:"reason,omitempty"`
	Message            string     `json:"message,omitempty"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty"`
}

// VolumeInfo describes one pod volume, enough to link a container mount back
// to the PVC / ConfigMap / Secret that backs it.
type VolumeInfo struct {
	Name string `json:"name"`
	// Type is "persistentVolumeClaim", "configMap", "secret", "emptyDir", ...
	Type string `json:"type"`
	// Source is the referenced object name (claim name, ConfigMap name, ...).
	Source   string `json:"source,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// ContainerInfo merges the spec and the status of a single container, plus its
// logs. Init containers are included with IsInit set.
type ContainerInfo struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	ImageID      string `json:"imageID,omitempty"`
	IsInit       bool   `json:"isInit,omitempty"`
	Ready        bool   `json:"ready"`
	Started      bool   `json:"started"`
	RestartCount int32  `json:"restartCount"`

	// State is the current container state, LastState the previous one (set
	// when the container has already been restarted at least once).
	State     ContainerState  `json:"state"`
	LastState *ContainerState `json:"lastState,omitempty"`

	Command []string   `json:"command,omitempty"`
	Args    []string   `json:"args,omitempty"`
	WorkDir string     `json:"workingDir,omitempty"`
	Ports   []PortInfo `json:"ports,omitempty"`

	Resources      Resources `json:"resources"`
	LivenessProbe  *Probe    `json:"livenessProbe,omitempty"`
	ReadinessProbe *Probe    `json:"readinessProbe,omitempty"`
	StartupProbe   *Probe    `json:"startupProbe,omitempty"`

	// EnvRefs lists the ConfigMap/Secret keys this container pulls in through
	// env / envFrom, so a config error can be traced back to its source.
	EnvRefs []EnvRef `json:"envRefs,omitempty"`
	// Mounts maps mount paths to pod volume names.
	Mounts []MountInfo `json:"mounts,omitempty"`

	// Logs holds the tail of the current container's logs, PreviousLogs the
	// tail of the previous (crashed) instance. The *LogsErr fields record why
	// a read failed instead of silently returning an empty string.
	Logs            string `json:"logs,omitempty"`
	LogsErr         string `json:"logsErr,omitempty"`
	PreviousLogs    string `json:"previousLogs,omitempty"`
	PreviousLogsErr string `json:"previousLogsErr,omitempty"`
}

// ContainerState is a flattened v1.ContainerState: exactly one of the three
// kinds is meaningful, given by Type.
type ContainerState struct {
	// Type is "Waiting", "Running", "Terminated" or "" when unknown.
	Type       string     `json:"type"`
	Reason     string     `json:"reason,omitempty"`
	Message    string     `json:"message,omitempty"`
	ExitCode   int32      `json:"exitCode,omitempty"`
	Signal     int32      `json:"signal,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// PortInfo is a declared container port.
type PortInfo struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

// Resources carries requests/limits both as the original quantity strings (for
// display) and as raw numbers (for comparison).
type Resources struct {
	RequestsCPU         string `json:"requestsCpu,omitempty"`
	RequestsMemory      string `json:"requestsMemory,omitempty"`
	LimitsCPU           string `json:"limitsCpu,omitempty"`
	LimitsMemory        string `json:"limitsMemory,omitempty"`
	RequestsCPUMillis   int64  `json:"requestsCpuMillis,omitempty"`
	LimitsCPUMillis     int64  `json:"limitsCpuMillis,omitempty"`
	RequestsMemoryBytes int64  `json:"requestsMemoryBytes,omitempty"`
	LimitsMemoryBytes   int64  `json:"limitsMemoryBytes,omitempty"`
}

// Probe is a flattened v1.Probe. Kind tells which handler is configured.
type Probe struct {
	// Kind is "http", "tcp", "exec" or "grpc".
	Kind    string   `json:"kind"`
	Path    string   `json:"path,omitempty"`
	Port    string   `json:"port,omitempty"`
	Scheme  string   `json:"scheme,omitempty"`
	Host    string   `json:"host,omitempty"`
	Command []string `json:"command,omitempty"`

	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int32 `json:"periodSeconds,omitempty"`
	TimeoutSeconds      int32 `json:"timeoutSeconds,omitempty"`
	SuccessThreshold    int32 `json:"successThreshold,omitempty"`
	FailureThreshold    int32 `json:"failureThreshold,omitempty"`
}

// EnvRef records one env var (or envFrom block) sourced from a ConfigMap or a
// Secret.
type EnvRef struct {
	// From is "configMap" or "secret".
	From string `json:"from"`
	// Name is the referenced object, Key the referenced key (empty for envFrom).
	Name     string `json:"name"`
	Key      string `json:"key,omitempty"`
	EnvVar   string `json:"envVar,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// MountInfo links a container mount path to a pod volume.
type MountInfo struct {
	VolumeName string `json:"volumeName"`
	MountPath  string `json:"mountPath"`
	ReadOnly   bool   `json:"readOnly,omitempty"`
}

// Event is a flattened core/v1 Event related to the pod (or to one of its PVCs).
type Event struct {
	Type           string     `json:"type"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	Source         string     `json:"source,omitempty"`
	Count          int32      `json:"count,omitempty"`
	InvolvedKind   string     `json:"involvedKind,omitempty"`
	InvolvedName   string     `json:"involvedName,omitempty"`
	FirstTimestamp *time.Time `json:"firstTimestamp,omitempty"`
	LastTimestamp  *time.Time `json:"lastTimestamp,omitempty"`
}

// NodeInfo describes the node the pod landed on, when it has one.
type NodeInfo struct {
	Name          string            `json:"name"`
	Architecture  string            `json:"architecture,omitempty"`
	OS            string            `json:"os,omitempty"`
	Unschedulable bool              `json:"unschedulable,omitempty"`
	Conditions    []NodeCondition   `json:"conditions,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Allocatable   map[string]string `json:"allocatable,omitempty"`
	Taints        []Taint           `json:"taints,omitempty"`
}

// NodeCondition mirrors a single entry of node.status.conditions.
type NodeCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Taint is a node taint.
type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// PVCInfo describes a PersistentVolumeClaim referenced by the pod, with the
// events targeting that claim.
type PVCInfo struct {
	Name             string   `json:"name"`
	Phase            string   `json:"phase"`
	StorageClass     *string  `json:"storageClass,omitempty"`
	VolumeName       string   `json:"volumeName,omitempty"`
	RequestedStorage string   `json:"requestedStorage,omitempty"`
	AccessModes      []string `json:"accessModes,omitempty"`
	Events           []Event  `json:"events,omitempty"`
}

// L picks the English or the French variant of a user-facing string according
// to the language carried by the context. English is the default.
func (c *DiagnosticContext) L(en, fr string) string {
	if c != nil && c.Lang == "fr" {
		return fr
	}
	return en
}
