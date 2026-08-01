package rules

import (
	"fmt"
	"strings"
)

// crashLoopProbeRule catches the classic "my app is fine, the kubelet keeps
// shooting it" case: a liveness probe fires before the application is able to
// answer, the kubelet kills the container, and the restart loop starts again.
//
// The tell is the combination of Unhealthy/Liveness events with restarts. A
// probe merely being defined proves nothing, and an OOM kill is rule 2's job.
var crashLoopProbeRule = Rule{
	Name: "crashloop_probe",

	Match: func(ctx *DiagnosticContext) bool {
		c, ok := probeKilledContainer(ctx)
		return ok && c.Name != ""
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		c, _ := probeKilledContainer(ctx)
		p := c.LivenessProbe

		target := probeTarget(p)
		var timing string
		if p != nil {
			timing = fmt.Sprintf("initialDelaySeconds=%d, periodSeconds=%d, timeoutSeconds=%d, failureThreshold=%d",
				p.InitialDelaySeconds, p.PeriodSeconds, p.TimeoutSeconds, p.FailureThreshold)
		}

		var detail string
		if e, ok := lastEventMessageContaining(ctx, "Liveness probe failed"); ok {
			detail = strings.TrimSpace(e.Message)
		}

		cause := fmt.Sprintf(
			"Container %q is in CrashLoopBackOff because its liveness probe (%s) keeps failing: "+
				"the kubelet kills the container, restarts it, and the probe fails again (%s so far). "+
				"The probe is configured with %s. The application is most likely still starting up when the "+
				"first probe fires, rather than being genuinely dead.",
			c.Name, target, countEN(c.RestartCount, "restart", "restarts"), timing)
		causeFR := fmt.Sprintf(
			"Le conteneur %q est en CrashLoopBackOff parce que sa liveness probe (%s) échoue en boucle : "+
				"le kubelet tue le conteneur, le redémarre, et la probe échoue à nouveau (%s). "+
				"La probe est configurée avec %s. L'application est très probablement encore en train de "+
				"démarrer quand la première probe se déclenche, plutôt que réellement morte.",
			c.Name, target, countFR(c.RestartCount, "redémarrage", "redémarrages"), timing)
		if detail != "" {
			cause += "\nLast probe error: " + detail
			causeFR += "\nDernière erreur de probe : " + detail
		}

		suggestion := fmt.Sprintf(
			"Give the app time to boot before the liveness probe counts:\n"+
				"  • Add a startupProbe on %s and let it own the slow start (recommended).\n"+
				"  • Or raise initialDelaySeconds (currently %d) above the app's real boot time.\n"+
				"  • Or raise failureThreshold / timeoutSeconds so a slow response is not fatal.\n"+
				"Check the boot time first:  kubectl logs %s -n %s -c %s --previous",
			target, probeInitialDelay(c), ctx.Pod.Name, ctx.Pod.Namespace, c.Name)
		suggestionFR := fmt.Sprintf(
			"Laissez à l'application le temps de démarrer avant que la liveness probe ne compte :\n"+
				"  • Ajoutez une startupProbe sur %s pour gérer le démarrage lent (recommandé).\n"+
				"  • Ou augmentez initialDelaySeconds (actuellement %d) au-delà du temps de démarrage réel.\n"+
				"  • Ou augmentez failureThreshold / timeoutSeconds pour qu'une réponse lente ne soit pas fatale.\n"+
				"Vérifiez d'abord le temps de démarrage :  kubectl logs %s -n %s -c %s --previous",
			target, probeInitialDelay(c), ctx.Pod.Name, ctx.Pod.Namespace, c.Name)

		return Diagnosis{
			Cause:      ctx.L(cause, causeFR),
			Suggestion: ctx.L(suggestion, suggestionFR),
			Confidence: ConfidenceHigh,
		}
	},
}

// probeKilledContainer returns the container being restarted by its own
// liveness probe, if there is one.
func probeKilledContainer(ctx *DiagnosticContext) (container, bool) {
	// The evidence is pod-scoped: events name the container only in their
	// message, so require at least one liveness failure somewhere on the pod.
	livenessFailed := false
	for _, e := range eventsWithReason(ctx, "Unhealthy", "ProbeWarning") {
		if containsFold(e.Message, "Liveness probe failed") {
			livenessFailed = true
			break
		}
	}
	if !livenessFailed {
		return container{}, false
	}

	return findContainer(ctx, func(c container) bool {
		if c.LivenessProbe == nil {
			return false
		}
		// An OOM kill also produces restarts and can trip a probe on the way
		// down; the memory limit is the real cause and rule 2 explains it.
		if wasOOMKilled(c) {
			return false
		}
		if c.RestartCount == 0 {
			return false
		}
		return isCrashLooping(c) || c.RestartCount > 0
	})
}

func probeInitialDelay(c container) int32 {
	if c.LivenessProbe == nil {
		return 0
	}
	return c.LivenessProbe.InitialDelaySeconds
}
