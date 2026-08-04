package rules

import (
	"fmt"
	"strings"

	"github.com/iyedM/kubectl-why-fail/internal/collector"
)

// pvcSchedulingSignals are the scheduler messages that point at storage rather
// than at CPU/memory.
var pvcSchedulingSignals = []string{
	"unbound immediate persistentvolumeclaims",
	"pod has unbound",
	"volume node affinity conflict",
	"had volume node affinity conflict",
	"waiting for a volume to be created",
	"waiting for first consumer",
	"no persistent volumes available",
	"persistentvolumeclaim",
}

// pendingPVCRule explains a pod stuck Pending because its storage never became
// available: the PVC is unbound, its StorageClass does not exist, or the only
// matching volume lives in a zone the pod cannot reach.
var pendingPVCRule = Rule{
	Name: "pending_pvc",

	Match: func(ctx *DiagnosticContext) bool {
		if !isPending(ctx) {
			return false
		}
		if e, ok := lastEventWithReason(ctx, "FailedScheduling"); ok && containsAnyFold(e.Message, pvcSchedulingSignals...) {
			return true
		}
		_, stuck := stuckPVC(ctx)
		return stuck
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		pvc, hasPVC := stuckPVC(ctx)

		schedulerMsg := ""
		if e, ok := lastEventWithReason(ctx, "FailedScheduling"); ok {
			schedulerMsg = strings.TrimSpace(e.Message)
		}

		var cause, causeFR, suggestion, suggestionFR string

		switch {
		case hasPVC && pvc.Phase == "NotFound":
			cause = fmt.Sprintf(
				"Pod %q is Pending because it mounts a PersistentVolumeClaim named %q that does not exist in namespace %q. "+
					"The scheduler will never place the pod until the claim is created.",
				ctx.Pod.Name, pvc.Name, ctx.Pod.Namespace)
			causeFR = fmt.Sprintf(
				"Le pod %q est Pending parce qu'il monte un PersistentVolumeClaim nommé %q qui n'existe pas dans le namespace %q. "+
					"Le scheduler ne placera jamais le pod tant que le claim n'est pas créé.",
				ctx.Pod.Name, pvc.Name, ctx.Pod.Namespace)
			suggestion = fmt.Sprintf(
				"Create the claim, or fix the claimName in the pod spec:\n"+
					"  kubectl get pvc -n %s\n"+
					"A StatefulSet creates its own claims from volumeClaimTemplates — if this pod belongs to one, check that the template name matches.",
				ctx.Pod.Namespace)
			suggestionFR = fmt.Sprintf(
				"Créez le claim, ou corrigez le claimName dans la spec du pod :\n"+
					"  kubectl get pvc -n %s\n"+
					"Un StatefulSet crée ses propres claims via volumeClaimTemplates — si ce pod en fait partie, vérifiez que le nom du template correspond.",
				ctx.Pod.Namespace)

		case containsFold(schedulerMsg, "volume node affinity conflict"):
			cause = fmt.Sprintf(
				"Pod %q is Pending because of a volume node affinity conflict: its PersistentVolume exists, but it is bound to a "+
					"zone or node the pod cannot be scheduled onto. A zonal disk can only be mounted by a pod running in the same zone.\n"+
					"Scheduler said: %s",
				ctx.Pod.Name, schedulerMsg)
			causeFR = fmt.Sprintf(
				"Le pod %q est Pending à cause d'un conflit d'affinité de nœud sur le volume : son PersistentVolume existe, mais il est lié à "+
					"une zone ou un node sur lequel le pod ne peut pas être planifié. Un disque zonal ne peut être monté que par un pod de la même zone.\n"+
					"Réponse du scheduler : %s",
				ctx.Pod.Name, schedulerMsg)
			suggestion = "Schedule the pod into the volume's zone (nodeSelector / nodeAffinity on topology.kubernetes.io/zone), or use a StorageClass with volumeBindingMode: WaitForFirstConsumer so the volume is provisioned in whatever zone the pod lands in."
			suggestionFR = "Planifiez le pod dans la zone du volume (nodeSelector / nodeAffinity sur topology.kubernetes.io/zone), ou utilisez une StorageClass avec volumeBindingMode: WaitForFirstConsumer pour que le volume soit provisionné dans la zone où atterrit le pod."

		case hasPVC:
			class := ctx.L("(none — the cluster default StorageClass was used)", "(aucune — la StorageClass par défaut du cluster a été utilisée)")
			if pvc.StorageClass != nil {
				if *pvc.StorageClass == "" {
					class = ctx.L(`"" (explicitly no class: only a manually created PV can satisfy it)`, `"" (explicitement aucune classe : seul un PV créé à la main peut la satisfaire)`)
				} else {
					class = *pvc.StorageClass
				}
			}
			provisionErr := pvcProvisioningError(pvc)

			cause = fmt.Sprintf(
				"Pod %q is Pending because its PersistentVolumeClaim %q is still %s — no volume has been bound to it.\n"+
					"StorageClass: %s\nRequested: %s",
				ctx.Pod.Name, pvc.Name, pvc.Phase, class, orNA(pvc.RequestedStorage))
			causeFR = fmt.Sprintf(
				"Le pod %q est Pending parce que son PersistentVolumeClaim %q est toujours %s — aucun volume ne lui a été lié.\n"+
					"StorageClass : %s\nDemandé : %s",
				ctx.Pod.Name, pvc.Name, pvc.Phase, class, orNA(pvc.RequestedStorage))
			if provisionErr != "" {
				cause += "\nProvisioner said: " + provisionErr
				causeFR += "\nRéponse du provisioner : " + provisionErr
			}

			suggestion = fmt.Sprintf(
				"Find out why the claim is not bound:\n"+
					"  kubectl describe pvc %s -n %s\n"+
					"  kubectl get storageclass\n"+
					"Common causes: the named StorageClass does not exist, the cluster has no default class, "+
					"the CSI driver is not installed, or the class uses WaitForFirstConsumer and the pod is also blocked for another reason.",
				pvc.Name, ctx.Pod.Namespace)
			suggestionFR = fmt.Sprintf(
				"Cherchez pourquoi le claim n'est pas lié :\n"+
					"  kubectl describe pvc %s -n %s\n"+
					"  kubectl get storageclass\n"+
					"Causes fréquentes : la StorageClass nommée n'existe pas, le cluster n'a pas de classe par défaut, "+
					"le driver CSI n'est pas installé, ou la classe est en WaitForFirstConsumer et le pod est aussi bloqué pour une autre raison.",
				pvc.Name, ctx.Pod.Namespace)

		default:
			cause = fmt.Sprintf(
				"Pod %q is Pending because of its storage: the scheduler is waiting on a PersistentVolumeClaim that is not bound.\n"+
					"Scheduler said: %s",
				ctx.Pod.Name, schedulerMsg)
			causeFR = fmt.Sprintf(
				"Le pod %q est Pending à cause de son stockage : le scheduler attend un PersistentVolumeClaim qui n'est pas lié.\n"+
					"Réponse du scheduler : %s",
				ctx.Pod.Name, schedulerMsg)
			suggestion = fmt.Sprintf(
				"Inspect the claims this pod mounts:\n  kubectl get pvc -n %s\n  kubectl get storageclass",
				ctx.Pod.Namespace)
			suggestionFR = fmt.Sprintf(
				"Inspectez les claims montés par ce pod :\n  kubectl get pvc -n %s\n  kubectl get storageclass",
				ctx.Pod.Namespace)
		}

		return Diagnosis{
			Cause:      ctx.L(cause, causeFR),
			Suggestion: ctx.L(suggestion, suggestionFR),
			Confidence: ConfidenceHigh,
		}
	},
}

// stuckPVC returns the first claim of the pod that is not Bound.
func stuckPVC(ctx *DiagnosticContext) (collector.PVCInfo, bool) {
	for _, p := range ctx.PVCs {
		if !strings.EqualFold(p.Phase, "Bound") {
			return p, true
		}
	}
	return collector.PVCInfo{}, false
}

// pvcProvisioningError surfaces the provisioner's own complaint, which is
// usually far more specific than the scheduler's.
func pvcProvisioningError(p collector.PVCInfo) string {
	for i := len(p.Events) - 1; i >= 0; i-- {
		e := p.Events[i]
		if strings.EqualFold(e.Type, "Warning") || containsAnyFold(e.Reason, "ProvisioningFailed", "FailedBinding") {
			return strings.TrimSpace(e.Message)
		}
	}
	return ""
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}
