package rules

import (
	"fmt"
	"strings"
)

// imagePullRule covers ImagePullBackOff / ErrImagePull / InvalidImageName: the
// kubelet cannot fetch the image at all.
//
// The registry error message says which of the three usual causes it is — a
// bad tag, missing credentials, or an unreachable registry — so the rule reads
// it rather than guessing. Architecture mismatches also surface as pull
// failures; those are explicitly left to imagepull_arch (rule 10).
var imagePullRule = Rule{
	Name: "imagepull",

	Match: func(ctx *DiagnosticContext) bool {
		if hasArchMismatchSignal(ctx) {
			return false
		}
		_, ok := imagePullFailure(ctx)
		return ok
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		c, _ := imagePullFailure(ctx)
		msg := imagePullMessage(ctx, c)
		image := c.Image
		if image == "" {
			image = "<unknown>"
		}

		var cause, causeFR, suggestion, suggestionFR string

		switch {
		case strings.EqualFold(c.State.Reason, "InvalidImageName"):
			cause = fmt.Sprintf("The image reference %q on container %q is not a valid image name, so the kubelet never even contacted a registry.", image, c.Name)
			causeFR = fmt.Sprintf("La référence d'image %q du conteneur %q n'est pas un nom d'image valide : le kubelet n'a même pas contacté de registre.", image, c.Name)
			suggestion = "Fix the reference. A valid one looks like [registry/][namespace/]name[:tag|@sha256:...] — check for a stray space, an uppercase letter, or a missing tag after the colon."
			suggestionFR = "Corrigez la référence. Une référence valide ressemble à [registre/][namespace/]nom[:tag|@sha256:...] — cherchez un espace parasite, une majuscule, ou un tag manquant après les deux-points."

		case containsAnyFold(msg, "unauthorized", "authentication required", "pull access denied", "denied: requested access", "403 Forbidden", "no basic auth credentials"):
			secrets := "none"
			secretsFR := "aucun"
			if len(ctx.Pod.ImagePullSecrets) > 0 {
				secrets = strings.Join(ctx.Pod.ImagePullSecrets, ", ")
				secretsFR = secrets
			}
			cause = fmt.Sprintf(
				"The registry refused to serve %q for container %q: the pull is unauthenticated or the credentials are wrong. "+
					"imagePullSecrets on this pod: %s.\nRegistry said: %s",
				image, c.Name, secrets, msg)
			causeFR = fmt.Sprintf(
				"Le registre a refusé de servir %q pour le conteneur %q : le pull n'est pas authentifié ou les identifiants sont mauvais. "+
					"imagePullSecrets sur ce pod : %s.\nRéponse du registre : %s",
				image, c.Name, secretsFR, msg)
			suggestion = fmt.Sprintf(
				"Create a pull secret and attach it to the pod's service account:\n"+
					"  kubectl create secret docker-registry regcred -n %s \\\n"+
					"    --docker-server=<registry> --docker-username=<user> --docker-password=<token>\n"+
					"  kubectl patch serviceaccount %s -n %s \\\n"+
					"    -p '{\"imagePullSecrets\":[{\"name\":\"regcred\"}]}'\n"+
					"If a secret is already listed, confirm it exists in THIS namespace — pull secrets are not shared across namespaces.",
				ctx.Pod.Namespace, serviceAccountOrDefault(ctx), ctx.Pod.Namespace)
			suggestionFR = fmt.Sprintf(
				"Créez un secret de pull et attachez-le au service account du pod :\n"+
					"  kubectl create secret docker-registry regcred -n %s \\\n"+
					"    --docker-server=<registre> --docker-username=<user> --docker-password=<token>\n"+
					"  kubectl patch serviceaccount %s -n %s \\\n"+
					"    -p '{\"imagePullSecrets\":[{\"name\":\"regcred\"}]}'\n"+
					"Si un secret est déjà listé, vérifiez qu'il existe bien dans CE namespace — les pull secrets ne sont pas partagés entre namespaces.",
				ctx.Pod.Namespace, serviceAccountOrDefault(ctx), ctx.Pod.Namespace)

		case containsAnyFold(msg, "not found", "manifest unknown", "manifest for", "does not exist", "repository does not exist", "404"):
			cause = fmt.Sprintf(
				"The registry has no image matching %q for container %q — the repository or the tag does not exist. "+
					"This is almost always a typo in the name or a tag that was never pushed (or was garbage-collected).\nRegistry said: %s",
				image, c.Name, msg)
			causeFR = fmt.Sprintf(
				"Le registre n'a aucune image correspondant à %q pour le conteneur %q — le dépôt ou le tag n'existe pas. "+
					"C'est presque toujours une faute de frappe dans le nom, ou un tag jamais poussé (ou supprimé par le GC).\nRéponse du registre : %s",
				image, c.Name, msg)
			suggestion = fmt.Sprintf(
				"Check the reference character by character, then confirm the tag exists:\n"+
					"  docker manifest inspect %s\n"+
					"If the tag is built by CI, make sure the build actually pushed before the rollout started.",
				image)
			suggestionFR = fmt.Sprintf(
				"Vérifiez la référence caractère par caractère, puis confirmez que le tag existe :\n"+
					"  docker manifest inspect %s\n"+
					"Si le tag est construit par la CI, assurez-vous que le build a bien poussé avant le déploiement.",
				image)

		case containsAnyFold(msg, "no such host", "dial tcp", "connection refused", "i/o timeout", "timeout", "certificate", "x509", "tls"):
			cause = fmt.Sprintf(
				"The kubelet could not reach the registry hosting %q for container %q — this is a network or TLS problem on the node, not a problem with the image.\nRegistry said: %s",
				image, c.Name, msg)
			causeFR = fmt.Sprintf(
				"Le kubelet n'a pas pu joindre le registre hébergeant %q pour le conteneur %q — c'est un problème réseau ou TLS sur le node, pas un problème d'image.\nRéponse du registre : %s",
				image, c.Name, msg)
			suggestion = "Check the registry hostname resolves and is reachable from the node, that any proxy/firewall allows it, and — for a private registry with a self-signed CA — that the node's container runtime trusts that CA."
			suggestionFR = "Vérifiez que le nom d'hôte du registre se résout et est joignable depuis le node, qu'aucun proxy/pare-feu ne le bloque, et — pour un registre privé avec une CA auto-signée — que le runtime du node fait confiance à cette CA."

		default:
			cause = fmt.Sprintf(
				"The kubelet failed to pull image %q for container %q (%s).\nRegistry said: %s",
				image, c.Name, c.State.Reason, msg)
			causeFR = fmt.Sprintf(
				"Le kubelet n'a pas réussi à récupérer l'image %q pour le conteneur %q (%s).\nRéponse du registre : %s",
				image, c.Name, c.State.Reason, msg)
			suggestion = fmt.Sprintf(
				"Verify the image exists and is readable with the pod's credentials:\n"+
					"  docker manifest inspect %s\n"+
					"Then check the pod's imagePullSecrets and the node's access to the registry.",
				image)
			suggestionFR = fmt.Sprintf(
				"Vérifiez que l'image existe et est lisible avec les identifiants du pod :\n"+
					"  docker manifest inspect %s\n"+
					"Puis contrôlez les imagePullSecrets du pod et l'accès du node au registre.",
				image)
		}

		return Diagnosis{
			Cause:      ctx.L(cause, causeFR),
			Suggestion: ctx.L(suggestion, suggestionFR),
			Confidence: ConfidenceHigh,
		}
	},
}

// imagePullFailure returns the container that cannot pull its image.
func imagePullFailure(ctx *DiagnosticContext) (container, bool) {
	if c, ok := waitingWithReason(ctx, "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "RegistryUnavailable", "ImageInspectError"); ok {
		return c, true
	}
	// Very early in the pull the container status may still be empty while the
	// Failed event is already there.
	if e, ok := lastEventWithReason(ctx, "Failed"); ok && containsAnyFold(e.Message, "failed to pull image", "error pulling image", "ErrImagePull") {
		if cs := appContainers(ctx); len(cs) > 0 {
			return cs[0], true
		}
	}
	return container{}, false
}

// imagePullMessage picks the most informative text available: the registry
// error in the events, else the container's waiting message.
func imagePullMessage(ctx *DiagnosticContext, c container) string {
	if e, ok := lastEventWithReason(ctx, "Failed"); ok && e.Message != "" {
		return strings.TrimSpace(e.Message)
	}
	if c.State.Message != "" {
		return strings.TrimSpace(c.State.Message)
	}
	return strings.TrimSpace(c.State.Reason)
}

func serviceAccountOrDefault(ctx *DiagnosticContext) string {
	if ctx.Pod.ServiceAccount != "" {
		return ctx.Pod.ServiceAccount
	}
	return "default"
}
