package rules

import (
	"fmt"
	"strings"

	"github.com/iyedM/kubectl-whyfail/internal/collector"
)

// Type aliases so rule files read without the collector prefix everywhere.
type (
	container = collector.ContainerInfo
	event     = collector.Event
	probeInfo = collector.Probe
)

// containsFold reports whether s contains sub, case-insensitively.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// containsAnyFold reports whether s contains at least one of subs.
func containsAnyFold(s string, subs ...string) bool {
	for _, sub := range subs {
		if containsFold(s, sub) {
			return true
		}
	}
	return false
}

// appContainers returns the pod's regular containers (init containers
// excluded).
func appContainers(ctx *DiagnosticContext) []container {
	var out []container
	for _, c := range ctx.Containers {
		if !c.IsInit {
			out = append(out, c)
		}
	}
	return out
}

// findContainer returns the first container satisfying pred.
func findContainer(ctx *DiagnosticContext, pred func(container) bool) (container, bool) {
	for _, c := range ctx.Containers {
		if pred(c) {
			return c, true
		}
	}
	return container{}, false
}

// waitingWithReason returns the first container waiting with one of the given
// reasons.
func waitingWithReason(ctx *DiagnosticContext, reasons ...string) (container, bool) {
	return findContainer(ctx, func(c container) bool {
		if c.State.Type != "Waiting" {
			return false
		}
		for _, r := range reasons {
			if strings.EqualFold(c.State.Reason, r) {
				return true
			}
		}
		return false
	})
}

// eventsWithReason returns the pod events whose Reason matches any of reasons.
func eventsWithReason(ctx *DiagnosticContext, reasons ...string) []event {
	var out []event
	for _, e := range ctx.Events {
		for _, r := range reasons {
			if strings.EqualFold(e.Reason, r) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// lastEventWithReason returns the most recent event with one of the reasons.
// Events are stored oldest first by the collector.
func lastEventWithReason(ctx *DiagnosticContext, reasons ...string) (event, bool) {
	matches := eventsWithReason(ctx, reasons...)
	if len(matches) == 0 {
		return event{}, false
	}
	return matches[len(matches)-1], true
}

// anyEventMessageContains reports whether some pod event message contains one
// of the substrings.
func anyEventMessageContains(ctx *DiagnosticContext, subs ...string) bool {
	for _, e := range ctx.Events {
		if containsAnyFold(e.Message, subs...) {
			return true
		}
	}
	return false
}

// lastEventMessageContaining returns the most recent event whose message holds
// one of the substrings.
func lastEventMessageContaining(ctx *DiagnosticContext, subs ...string) (event, bool) {
	found := false
	var last event
	for _, e := range ctx.Events {
		if containsAnyFold(e.Message, subs...) {
			last, found = e, true
		}
	}
	return last, found
}

// allLogs concatenates the current and previous logs of a container, which is
// where crash evidence lives.
func allLogs(c container) string {
	return c.Logs + "\n" + c.PreviousLogs
}

// isCrashLooping reports whether the container is in the CrashLoopBackOff wait
// state, or is otherwise restarting repeatedly.
func isCrashLooping(c container) bool {
	if c.State.Type == "Waiting" && strings.EqualFold(c.State.Reason, "CrashLoopBackOff") {
		return true
	}
	return c.RestartCount > 2 && c.LastState != nil && c.LastState.Type == "Terminated" && c.LastState.ExitCode != 0
}

// lastExit returns the previous termination state of a container, if any.
func lastExit(c container) (collector.ContainerState, bool) {
	if c.LastState != nil && c.LastState.Type == "Terminated" {
		return *c.LastState, true
	}
	if c.State.Type == "Terminated" {
		return c.State, true
	}
	return collector.ContainerState{}, false
}

// wasOOMKilled reports whether the container's current or previous termination
// was an OOM kill.
func wasOOMKilled(c container) bool {
	if st, ok := lastExit(c); ok && strings.EqualFold(st.Reason, "OOMKilled") {
		return true
	}
	if c.LastState != nil && strings.EqualFold(c.LastState.Reason, "OOMKilled") {
		return true
	}
	return false
}

// archMismatchSignals are the strings the kubelet and the container runtime
// emit when an image has no variant for the node's platform. The generic image
// pull rule uses them as an exclusion so the dedicated architecture rule can
// fire instead.
var archMismatchSignals = []string{
	"no matching manifest for",
	"no match for platform in manifest",
	"image platform",
	"does not match the detected host platform",
	"cannot be used on this platform",
	"exec format error",
}

func hasArchMismatchSignal(ctx *DiagnosticContext) bool {
	if anyEventMessageContains(ctx, archMismatchSignals...) {
		return true
	}
	for _, c := range ctx.Containers {
		if containsAnyFold(c.State.Message, archMismatchSignals...) {
			return true
		}
		if c.LastState != nil && containsAnyFold(c.LastState.Message, archMismatchSignals...) {
			return true
		}
		if containsAnyFold(allLogs(c), "exec format error") {
			return true
		}
	}
	return false
}

// probeTarget renders a probe endpoint the way a user would type it.
func probeTarget(p *collector.Probe) string {
	if p == nil {
		return ""
	}
	switch p.Kind {
	case "http":
		scheme := strings.ToLower(p.Scheme)
		if scheme == "" {
			scheme = "http"
		}
		path := p.Path
		if path == "" {
			path = "/"
		}
		return fmt.Sprintf("%s://:%s%s", scheme, p.Port, path)
	case "tcp":
		return "tcp://:" + p.Port
	case "grpc":
		return "grpc://:" + p.Port
	case "exec":
		return strings.Join(p.Command, " ")
	}
	return ""
}

// probeURLFromInside renders a probe endpoint as a URL that works when run
// from inside the container, where the kubelet's pod IP is not what you type.
func probeURLFromInside(p *collector.Probe) string {
	if p == nil {
		return ""
	}
	switch p.Kind {
	case "http":
		scheme := strings.ToLower(p.Scheme)
		if scheme == "" {
			scheme = "http"
		}
		path := p.Path
		if path == "" {
			path = "/"
		}
		return fmt.Sprintf("%s://localhost:%s%s", scheme, p.Port, path)
	case "exec":
		return strings.Join(p.Command, " ")
	default:
		return "localhost:" + p.Port
	}
}

// declaresPort reports whether the container declares the given port, either
// by number or by name. An empty or unresolvable port is treated as declared,
// since we cannot prove anything about it.
func declaresPort(c container, port string) bool {
	if port == "" {
		return true
	}
	for _, p := range c.Ports {
		if p.Name == port || fmt.Sprint(p.ContainerPort) == port {
			return true
		}
	}
	return false
}

// humanBytes renders a byte count with the binary unit a Kubernetes user
// expects (128Mi rather than 134 MB).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	suffix := []string{"Ki", "Mi", "Gi", "Ti", "Pi"}[exp]
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d%s", int64(value), suffix)
	}
	return fmt.Sprintf("%.1f%s", value, suffix)
}

// quote renders a shell-ish representation of a command line for messages.
func quote(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}
