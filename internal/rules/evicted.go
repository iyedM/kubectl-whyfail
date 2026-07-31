package rules

import (
	"fmt"
	"regexp"
	"strings"
)

// lowOnResourceRE parses "The node was low on resource: ephemeral-storage."
var lowOnResourceRE = regexp.MustCompile(`(?i)low on resource:\s*\[?([a-z0-9\-.]+)\]?`)

// evictedRule explains a pod the kubelet deleted to protect its node.
//
// Eviction is not a crash: the pod did nothing wrong at the moment it was
// killed, it was simply the one chosen when the node ran out of memory or
// disk. Which resource ran out, and why this pod was picked, are the two
// things the user actually needs.
var evictedRule = Rule{
	Name: "evicted",

	Match: func(ctx *DiagnosticContext) bool {
		if strings.EqualFold(ctx.Pod.Reason, "Evicted") {
			return true
		}
		if e, ok := lastEventWithReason(ctx, "Evicted"); ok && e.Message != "" {
			return true
		}
		return strings.EqualFold(ctx.Pod.Phase, "Failed") && containsFold(ctx.Pod.Message, "evicted")
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		msg := ctx.Pod.Message
		if msg == "" {
			if e, ok := lastEventWithReason(ctx, "Evicted"); ok {
				msg = e.Message
			}
		}
		msg = strings.TrimSpace(msg)

		resource := "memory"
		if m := lowOnResourceRE.FindStringSubmatch(msg); len(m) > 1 {
			resource = strings.ToLower(m[1])
		} else if containsAnyFold(msg, "disk", "ephemeral-storage", "DiskPressure") {
			resource = "ephemeral-storage"
		}

		node := ctx.Pod.NodeName
		if node == "" {
			node = ctx.L("its node", "son node")
		}

		qos := ctx.Pod.QOSClass
		var qosNote, qosNoteFR string
		switch {
		case strings.EqualFold(qos, "BestEffort"):
			qosNote = "\nThis pod has QoS class BestEffort (no requests set at all), which makes it the very first candidate for eviction."
			qosNoteFR = "\nCe pod a la classe QoS BestEffort (aucune request définie), ce qui en fait le tout premier candidat à l'éviction."
		case strings.EqualFold(qos, "Burstable"):
			qosNote = "\nThis pod has QoS class Burstable (it uses more than it requests), so it is evicted before any Guaranteed pod."
			qosNoteFR = "\nCe pod a la classe QoS Burstable (il consomme plus qu'il ne demande), il est donc évincé avant tout pod Guaranteed."
		}

		cause := fmt.Sprintf(
			"Pod %q was evicted: node %s ran out of %s, and the kubelet deleted this pod to reclaim it. "+
				"The pod itself did not crash — it was sacrificed to keep the node alive.%s\nKubelet said: %s",
			ctx.Pod.Name, node, resource, qosNote, orNA(msg))
		causeFR := fmt.Sprintf(
			"Le pod %q a été évincé : le node %s a manqué de %s, et le kubelet a supprimé ce pod pour en récupérer. "+
				"Le pod n'a pas planté — il a été sacrifié pour préserver le node.%s\nRéponse du kubelet : %s",
			ctx.Pod.Name, node, resource, qosNoteFR, orNA(msg))

		var fix, fixFR string
		if resource == "memory" {
			fix = "  • Set resources.requests.memory (and matching limits) so the pod is protected and the scheduler stops overcommitting the node.\n" +
				"  • Find what is really consuming the node:  kubectl top pods -A --sort-by=memory"
			fixFR = "  • Définissez resources.requests.memory (et des limits correspondantes) pour protéger le pod et empêcher le scheduler de surcharger le node.\n" +
				"  • Trouvez ce qui consomme réellement le node :  kubectl top pods -A --sort-by=memory"
		} else {
			fix = "  • Set resources.requests.ephemeral-storage, and stop writing logs/temp files into the container filesystem — use an emptyDir or a volume.\n" +
				"  • Check image and log accumulation on the node:  kubectl describe node " + ctx.Pod.NodeName
			fixFR = "  • Définissez resources.requests.ephemeral-storage, et cessez d'écrire logs/fichiers temporaires dans le filesystem du conteneur — utilisez un emptyDir ou un volume.\n" +
				"  • Vérifiez l'accumulation d'images et de logs sur le node :  kubectl describe node " + ctx.Pod.NodeName
		}

		suggestion := fmt.Sprintf(
			"The evicted pod object is only a tombstone; delete it and fix the pressure:\n%s\n"+
				"  • Clean up:  kubectl delete pod %s -n %s\n"+
				"  • A controller (Deployment/StatefulSet) has already recreated a replacement — if it is evicted too, the node is genuinely undersized.",
			fix, ctx.Pod.Name, ctx.Pod.Namespace)
		suggestionFR := fmt.Sprintf(
			"L'objet pod évincé n'est qu'une trace ; supprimez-le et traitez la pression :\n%s\n"+
				"  • Nettoyage :  kubectl delete pod %s -n %s\n"+
				"  • Un contrôleur (Deployment/StatefulSet) a déjà recréé un remplaçant — s'il est évincé aussi, le node est réellement sous-dimensionné.",
			fixFR, ctx.Pod.Name, ctx.Pod.Namespace)

		return Diagnosis{
			Cause:      ctx.L(cause, causeFR),
			Suggestion: ctx.L(suggestion, suggestionFR),
			Confidence: ConfidenceHigh,
		}
	},
}
