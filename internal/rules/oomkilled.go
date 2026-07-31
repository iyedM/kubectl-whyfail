package rules

import "fmt"

// oomKilledRule catches a container killed by the kernel OOM killer because it
// tried to allocate past its memory limit.
//
// The signal is unambiguous: the kubelet records the termination reason as
// OOMKilled (exit code 137). No inference needed, hence high confidence.
var oomKilledRule = Rule{
	Name: "oomkilled",

	Match: func(ctx *DiagnosticContext) bool {
		_, ok := oomContainer(ctx)
		return ok
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		c, _ := oomContainer(ctx)

		limit := c.Resources.LimitsMemory
		if limit == "" {
			limit = ctx.L("no memory limit set", "aucune limite mémoire définie")
		}

		var loop string
		var loopFR string
		if isCrashLooping(c) {
			loop = fmt.Sprintf(" It has restarted %d times, so it is being OOM-killed repeatedly, not once.", c.RestartCount)
			loopFR = fmt.Sprintf(" Il a redémarré %d fois : il est donc tué par l'OOM de façon répétée, pas une seule fois.", c.RestartCount)
		}

		cause := fmt.Sprintf(
			"Container %q was OOMKilled: it tried to allocate more memory than its limit of %s, "+
				"and the kernel killed it (exit code 137).%s\n"+
				"This is a hard limit — the container is not slowed down when it approaches it, it is killed outright.",
			c.Name, limit, loop)
		causeFR := fmt.Sprintf(
			"Le conteneur %q a été OOMKilled : il a tenté d'allouer plus de mémoire que sa limite de %s, "+
				"et le noyau l'a tué (code de sortie 137).%s\n"+
				"C'est une limite stricte — le conteneur n'est pas ralenti à l'approche, il est tué net.",
			c.Name, limit, loopFR)

		var advice, adviceFR string
		switch {
		case c.Resources.LimitsMemoryBytes == 0:
			advice = "The container has no memory limit, so it was killed under node memory pressure. Set an explicit resources.limits.memory so the scheduler can place it safely."
			adviceFR = "Le conteneur n'a pas de limite mémoire : il a été tué sous la pression mémoire du node. Définissez un resources.limits.memory explicite pour que le scheduler puisse le placer correctement."
		default:
			suggested := humanBytes(c.Resources.LimitsMemoryBytes * 2)
			advice = fmt.Sprintf(
				"Raise the limit, then verify it is enough:\n"+
					"  kubectl set resources deployment/<name> -n %s --limits=memory=%s\n"+
					"Also raise requests.memory to match if the app genuinely needs that much, "+
					"otherwise the pod gets a Burstable QoS class and is evicted first under pressure.",
				ctx.Pod.Namespace, suggested)
			adviceFR = fmt.Sprintf(
				"Augmentez la limite, puis vérifiez qu'elle suffit :\n"+
					"  kubectl set resources deployment/<nom> -n %s --limits=memory=%s\n"+
					"Augmentez aussi requests.memory si l'application en a réellement besoin, "+
					"sinon le pod se retrouve en QoS Burstable et sera évincé en premier sous pression.",
				ctx.Pod.Namespace, suggested)
		}

		suggestion := advice + fmt.Sprintf(
			"\nIf memory grows without bound, it is a leak, not a sizing problem — check the trend with:\n"+
				"  kubectl top pod %s -n %s --containers",
			ctx.Pod.Name, ctx.Pod.Namespace)
		suggestionFR := adviceFR + fmt.Sprintf(
			"\nSi la mémoire croît sans limite, c'est une fuite et non un problème de dimensionnement — vérifiez la tendance :\n"+
				"  kubectl top pod %s -n %s --containers",
			ctx.Pod.Name, ctx.Pod.Namespace)

		return Diagnosis{
			Cause:      ctx.L(cause, causeFR),
			Suggestion: ctx.L(suggestion, suggestionFR),
			Confidence: ConfidenceHigh,
		}
	},
}

func oomContainer(ctx *DiagnosticContext) (container, bool) {
	return findContainer(ctx, wasOOMKilled)
}
