package rules

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// insufficientRE pulls the resource names out of a scheduler message such as
// "0/5 nodes are available: 3 Insufficient cpu, 2 Insufficient memory."
//
// The name must start and end alphanumeric so an extended resource keeps its
// dots ("nvidia.com/gpu") while the sentence's trailing period does not become
// part of it ("cpu." would read badly in the diagnosis).
var insufficientRE = regexp.MustCompile(`(?i)Insufficient\s+([a-z0-9](?:[a-z0-9./\-_]*[a-z0-9])?)`)

// pendingResourcesRule explains a pod that stays Pending because no node has
// room for what it requests.
//
// The scheduler is explicit about this in its FailedScheduling event, so the
// rule keys on "Insufficient <resource>" rather than on Pending alone — a pod
// can be Pending for a dozen unrelated reasons.
var pendingResourcesRule = Rule{
	Name: "pending_resources",

	Match: func(ctx *DiagnosticContext) bool {
		if !isPending(ctx) {
			return false
		}
		e, ok := lastEventWithReason(ctx, "FailedScheduling")
		return ok && insufficientRE.MatchString(e.Message)
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		e, _ := lastEventWithReason(ctx, "FailedScheduling")
		msg := strings.TrimSpace(e.Message)
		short := scarceResources(msg)

		requested := requestSummary(ctx)

		cause := fmt.Sprintf(
			"Pod %q cannot be scheduled: no node has enough %s left to satisfy its requests. "+
				"The pod is not broken — the cluster simply has nowhere to put it.\n"+
				"It requests: %s\nScheduler said: %s",
			ctx.Pod.Name, short, requested, msg)
		causeFR := fmt.Sprintf(
			"Le pod %q ne peut pas être planifié : aucun node n'a assez de %s disponible pour satisfaire ses requests. "+
				"Le pod n'est pas cassé — le cluster n'a simplement pas de place pour lui.\n"+
				"Il demande : %s\nRéponse du scheduler : %s",
			ctx.Pod.Name, short, requested, msg)

		suggestion := fmt.Sprintf(
			"Either shrink the request or grow the cluster:\n"+
				"  • Lower resources.requests.%s to what the app actually uses (requests are a reservation, not a limit).\n"+
				"  • Or add a node / let the cluster autoscaler add one.\n"+
				"See what is actually free per node:\n"+
				"  kubectl describe nodes | grep -A5 'Allocated resources'",
			short)
		suggestionFR := fmt.Sprintf(
			"Réduisez la demande ou agrandissez le cluster :\n"+
				"  • Baissez resources.requests.%s à ce que l'application consomme réellement (une request est une réservation, pas une limite).\n"+
				"  • Ou ajoutez un node / laissez l'autoscaler en ajouter un.\n"+
				"Voyez ce qui est réellement libre par node :\n"+
				"  kubectl describe nodes | grep -A5 'Allocated resources'",
			short)

		return Diagnosis{
			Cause:      ctx.L(cause, causeFR),
			Suggestion: ctx.L(suggestion, suggestionFR),
			Confidence: ConfidenceHigh,
		}
	},
}

func isPending(ctx *DiagnosticContext) bool {
	return strings.EqualFold(ctx.Pod.Phase, "Pending")
}

// scarceResources lists the distinct resources the scheduler reported as
// insufficient, e.g. "cpu and memory".
func scarceResources(msg string) string {
	seen := map[string]bool{}
	for _, m := range insufficientRE.FindAllStringSubmatch(msg, -1) {
		seen[strings.ToLower(m[1])] = true
	}
	if len(seen) == 0 {
		return "capacity"
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// requestSummary renders the pod's total resource requests per container.
func requestSummary(ctx *DiagnosticContext) string {
	var parts []string
	for _, c := range ctx.Containers {
		var fields []string
		if c.Resources.RequestsCPU != "" {
			fields = append(fields, "cpu="+c.Resources.RequestsCPU)
		}
		if c.Resources.RequestsMemory != "" {
			fields = append(fields, "memory="+c.Resources.RequestsMemory)
		}
		if len(fields) == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", c.Name, strings.Join(fields, ", ")))
	}
	if len(parts) == 0 {
		return "no explicit resource requests"
	}
	return strings.Join(parts, "; ")
}
