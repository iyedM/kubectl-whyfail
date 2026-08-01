package rules

import (
	"fmt"
	"strings"
	"time"
)

// readinessNeverReadyRule explains the quietest failure of all: the pod is
// Running, nothing restarts, no error is printed — and yet it never receives
// traffic, because its readiness probe never succeeds.
//
// Unlike a failing liveness probe, a failing readiness probe kills nothing; it
// just holds the pod out of its Service endpoints forever. The usual cause is
// a probe pointed at the wrong port or path.
var readinessNeverReadyRule = Rule{
	Name: "readiness_never_ready",

	Match: func(ctx *DiagnosticContext) bool {
		_, ok := neverReadyContainer(ctx)
		return ok
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		c, _ := neverReadyContainer(ctx)
		p := c.ReadinessProbe
		target := probeTarget(p)

		var evidence string
		if e, ok := lastEventMessageContaining(ctx, "Readiness probe failed"); ok {
			evidence = "\nLast probe error: " + strings.TrimSpace(e.Message)
		}
		evidenceFR := strings.Replace(evidence, "\nLast probe error: ", "\nDernière erreur de probe : ", 1)

		// A probe aimed at a port the container never declares is the single
		// most common version of this bug, and it is worth calling out.
		var portNote, portNoteFR string
		if p != nil && !declaresPort(c, p.Port) && len(c.Ports) > 0 {
			portNote = fmt.Sprintf(
				"\nThe probe targets port %s, but the container only declares %s. That is very likely the mistake.",
				p.Port, declaredPorts(c))
			portNoteFR = fmt.Sprintf(
				"\nLa probe vise le port %s, alors que le conteneur ne déclare que %s. C'est très probablement l'erreur.",
				p.Port, declaredPorts(c))
		}

		age := ""
		ageFR := ""
		if ctx.Pod.StartTime != nil {
			d := ctx.CollectedAt.Sub(*ctx.Pod.StartTime).Round(0)
			if d > 0 {
				age = fmt.Sprintf(" It has been running for %s without ever becoming ready.", shortDuration(d))
				ageFR = fmt.Sprintf(" Il tourne depuis %s sans jamais devenir ready.", shortDuration(d))
			}
		}

		cause := fmt.Sprintf(
			"Container %q is running fine but never becomes ready: its readiness probe (%s) has never succeeded, "+
				"so Kubernetes keeps the pod out of every Service that selects it — the pod receives no traffic and a "+
				"rolling update using it will stall.%s%s%s\n"+
				"The process itself has not crashed (%s), so this is a probe configuration problem far more often than an application problem.",
			c.Name, target, age, portNote, evidence, countEN(c.RestartCount, "restart", "restarts"))
		causeFR := fmt.Sprintf(
			"Le conteneur %q tourne correctement mais ne devient jamais ready : sa readiness probe (%s) n'a jamais réussi, "+
				"donc Kubernetes le maintient hors de tous les Services qui le sélectionnent — le pod ne reçoit aucun trafic et "+
				"un rolling update basé dessus restera bloqué.%s%s%s\n"+
				"Le processus n'a pas planté (%s) : c'est bien plus souvent un problème de configuration de probe qu'un problème applicatif.",
			c.Name, target, ageFR, portNoteFR, evidenceFR, countFR(c.RestartCount, "redémarrage", "redémarrages"))

		suggestion := fmt.Sprintf(
			"Ask the container itself whether the endpoint answers:\n"+
				"  kubectl exec %s -n %s -c %s -- wget -qO- %s\n"+
				"  kubectl port-forward %s -n %s %s:%s   # then curl it locally\n"+
				"Check three things in order: the port matches what the app listens on, the path exists and returns 2xx/3xx, "+
				"and the app binds 0.0.0.0 rather than 127.0.0.1 (a loopback bind answers from inside the container but not from the kubelet).",
			ctx.Pod.Name, ctx.Pod.Namespace, c.Name, orNA(probeURLFromInside(p)),
			ctx.Pod.Name, ctx.Pod.Namespace, probePort(p), probePort(p))
		suggestionFR := fmt.Sprintf(
			"Demandez au conteneur lui-même si l'endpoint répond :\n"+
				"  kubectl exec %s -n %s -c %s -- wget -qO- %s\n"+
				"  kubectl port-forward %s -n %s %s:%s   # puis curl en local\n"+
				"Vérifiez trois choses dans l'ordre : le port correspond à celui écouté par l'application, le chemin existe et renvoie 2xx/3xx, "+
				"et l'application écoute sur 0.0.0.0 et non 127.0.0.1 (un bind loopback répond depuis l'intérieur du conteneur mais pas au kubelet).",
			ctx.Pod.Name, ctx.Pod.Namespace, c.Name, orNA(probeURLFromInside(p)),
			ctx.Pod.Name, ctx.Pod.Namespace, probePort(p), probePort(p))

		return Diagnosis{
			Cause:      ctx.L(cause, causeFR),
			Suggestion: ctx.L(suggestion, suggestionFR),
			Confidence: ConfidenceHigh,
		}
	},
}

// neverReadyContainer returns a running-but-never-ready container.
func neverReadyContainer(ctx *DiagnosticContext) (container, bool) {
	if !strings.EqualFold(ctx.Pod.Phase, "Running") {
		return container{}, false
	}
	if podConditionTrue(ctx, "Ready") {
		return container{}, false
	}

	readinessFailed := false
	for _, e := range eventsWithReason(ctx, "Unhealthy", "ProbeWarning") {
		if containsFold(e.Message, "Readiness probe failed") {
			readinessFailed = true
			break
		}
	}

	return findContainer(ctx, func(c container) bool {
		if c.IsInit || c.Ready || c.ReadinessProbe == nil {
			return false
		}
		if c.State.Type != "Running" {
			return false
		}
		// A container that keeps dying is a crash loop, explained elsewhere;
		// this rule is specifically about the pod that looks healthy.
		if c.RestartCount > 2 || wasOOMKilled(c) {
			return false
		}
		// Either the kubelet said the probe failed, or the probe points at a
		// port the container does not even expose.
		return readinessFailed || (len(c.Ports) > 0 && !declaresPort(c, c.ReadinessProbe.Port))
	})
}

func podConditionTrue(ctx *DiagnosticContext, condType string) bool {
	for _, c := range ctx.Pod.Conditions {
		if strings.EqualFold(c.Type, condType) {
			return strings.EqualFold(c.Status, "True")
		}
	}
	return false
}

func declaredPorts(c container) string {
	var ps []string
	for _, p := range c.Ports {
		ps = append(ps, fmt.Sprint(p.ContainerPort))
	}
	if len(ps) == 0 {
		return "none"
	}
	return strings.Join(ps, ", ")
}

func probePort(p *probeInfo) string {
	if p == nil || p.Port == "" {
		return "8080"
	}
	return p.Port
}

// shortDuration renders a duration the way kubectl does (2h13m, 45s).
func shortDuration(d time.Duration) string {
	s := d.String()
	// Trim sub-second noise: "2h13m4.123456s" -> "2h13m4s".
	if i := strings.Index(s, "."); i >= 0 {
		j := i
		for j < len(s) && (s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
			j++
		}
		s = s[:i] + s[j:]
	}
	return s
}
