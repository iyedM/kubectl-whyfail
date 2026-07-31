package rules

import (
	"fmt"
	"strings"
)

// The substrings that tell image pull failures apart.
//
// They are named here rather than inlined in Explain because imagePullMessage
// uses the same lists to decide which event is worth quoting: if the two ever
// drifted apart, the rule could pick a message it then fails to classify.
//
// Every entry is a phrasing observed in the wild. The wording of a pull error
// comes from the container runtime (containerd, CRI-O, Docker) and from the
// registry (Hub, ECR, GCR/Artifact Registry, ACR, Quay, Harbor), not from
// Kubernetes, so a signal set built from a single cluster will not survive
// contact with another one.
var (
	// Rate limiting. Docker Hub meters anonymous pulls per source IP, so this
	// is one of the most common production failures — and the advice for it
	// shares nothing with the other branches.
	imagePullRateLimitSignals = []string{
		"toomanyrequests", "too many requests", "pull rate limit",
		"rate limit exceeded", "throttl", "429 too many requests",
		"status code 429", "status: 429",
	}

	imagePullAuthSignals = []string{
		"unauthorized", "unauthenticated", "authentication required",
		"authorization failed", "authorization token has expired",
		"token has expired", "reauthenticate",
		"pull access denied", "access denied", "denied: requested access",
		"denied: permission", "permission denied on resource",
		"insufficient_scope", "insufficient scope",
		"no basic auth credentials", "docker login",
		"403 forbidden", "status code 403", "status: 403",
		"401 unauthorized", "status code 401", "status: 401",
	}

	// Network and TLS. Checked before the "missing image" signals because
	// those are far more generic and would otherwise swallow these.
	imagePullNetworkSignals = []string{
		"no such host", "dial tcp", "dial udp", "lookup ",
		"connection refused", "connection reset", "connection timed out",
		"no route to host", "network is unreachable",
		"i/o timeout", "tls handshake timeout", "context deadline exceeded",
		"client.timeout exceeded", "server misbehaving",
		"temporary failure in name resolution", "proxyconnect",
		"certificate", "x509", "tls: ",
		"server gave http response to https client",
	}

	// Missing repository or tag. Deliberately last: "not found" is the single
	// most generic string a registry can return, and a bare "404" would even
	// match a sha256 digest, so HTTP status codes are matched with context.
	imagePullMissingSignals = []string{
		"manifest unknown", "manifest_unknown", "name unknown", "name_unknown",
		"repository does not exist", "does not exist", "no such image",
		"was deleted or has expired", "reference does not exist",
		"not found", "404 not found", "status code 404", "status: 404",
	}
)

// imagePullKind is the cause Explain has identified.
type imagePullKind int

const (
	pullUnknown imagePullKind = iota
	pullInvalidName
	pullRateLimited
	pullAuth
	pullNetwork
	pullMissing
)

// classifyImagePull maps a registry error onto a cause.
//
// Order matters and encodes specificity: an unambiguous quota or credential
// error wins over a network hint, which in turn wins over the very generic
// "not found". Keeping this a standalone function is what lets the test suite
// pin the behaviour against real messages from several runtimes and registries
// without constructing a full pod each time.
func classifyImagePull(stateReason, msg string) imagePullKind {
	switch {
	case strings.EqualFold(stateReason, "InvalidImageName"):
		return pullInvalidName
	case containsAnyFold(msg, imagePullRateLimitSignals...):
		return pullRateLimited
	case containsAnyFold(msg, imagePullAuthSignals...):
		return pullAuth
	case containsAnyFold(msg, imagePullNetworkSignals...):
		return pullNetwork
	case containsAnyFold(msg, imagePullMissingSignals...):
		return pullMissing
	default:
		return pullUnknown
	}
}

// imagePullCauseSignals is every pattern classifyImagePull can act on. A
// message matching none of them lands in the default branch, which is exactly
// the outcome imagePullMessage tries to avoid when a better message exists.
var imagePullCauseSignals = func() []string {
	var all []string
	all = append(all, imagePullRateLimitSignals...)
	all = append(all, imagePullAuthSignals...)
	all = append(all, imagePullNetworkSignals...)
	all = append(all, imagePullMissingSignals...)
	return all
}()

// imagePullBoilerplate are the messages the kubelet repeats on every retry
// once it has already given up. They restate the pod's status and carry no
// cause at all, so they must never win over the original error.
var imagePullBoilerplate = []string{
	"Error: ErrImagePull",
	"Error: ImagePullBackOff",
	"Error: ErrImageNeverPull",
	"Error: ImageInspectError",
	"Error: RegistryUnavailable",
	"Error: InvalidImageName",
}

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

		switch classifyImagePull(c.State.Reason, msg) {
		case pullInvalidName:
			cause = fmt.Sprintf("The image reference %q on container %q is not a valid image name, so the kubelet never even contacted a registry.", image, c.Name)
			causeFR = fmt.Sprintf("La référence d'image %q du conteneur %q n'est pas un nom d'image valide : le kubelet n'a même pas contacté de registre.", image, c.Name)
			suggestion = "Fix the reference. A valid one looks like [registry/][namespace/]name[:tag|@sha256:...] — check for a stray space, an uppercase letter, or a missing tag after the colon."
			suggestionFR = "Corrigez la référence. Une référence valide ressemble à [registre/][namespace/]nom[:tag|@sha256:...] — cherchez un espace parasite, une majuscule, ou un tag manquant après les deux-points."

		case pullRateLimited:
			cause = fmt.Sprintf(
				"The registry is rate-limiting pulls of %q for container %q. Nothing is wrong with the image, the tag "+
					"or your credentials — the registry simply refused to serve another pull right now.\n"+
					"Docker Hub meters anonymous pulls per source IP, so every node behind the same NAT gateway shares "+
					"one quota: a busy cluster can exhaust it even if this workload is small.\nRegistry said: %s",
				image, c.Name, msg)
			causeFR = fmt.Sprintf(
				"Le registre limite le débit des pulls de %q pour le conteneur %q. Ni l'image, ni le tag, ni vos "+
					"identifiants ne sont en cause — le registre a simplement refusé un pull de plus pour le moment.\n"+
					"Docker Hub compte les pulls anonymes par IP source : tous les nodes derrière la même passerelle NAT "+
					"partagent un seul quota, qu'un cluster chargé peut épuiser même si cette charge de travail est petite.\n"+
					"Réponse du registre : %s",
				image, c.Name, msg)
			suggestion = fmt.Sprintf(
				"Stop pulling anonymously, or stop pulling from the public registry:\n"+
					"  • Authenticate — even a free account raises the limit well above the anonymous one:\n"+
					"      kubectl create secret docker-registry regcred -n %s \\\n"+
					"        --docker-server=https://index.docker.io/v1/ --docker-username=<user> --docker-password=<token>\n"+
					"  • Or mirror the image into your own registry (ECR/GCR/ACR/Harbor) and pull from there.\n"+
					"  • Or run a pull-through cache so the cluster pulls each image once.\n"+
					"  • Set imagePullPolicy: IfNotPresent so a restart reuses the cached image instead of re-pulling.",
				ctx.Pod.Namespace)
			suggestionFR = fmt.Sprintf(
				"Cessez de puller anonymement, ou cessez de puller depuis le registre public :\n"+
					"  • Authentifiez-vous — même un compte gratuit relève nettement la limite anonyme :\n"+
					"      kubectl create secret docker-registry regcred -n %s \\\n"+
					"        --docker-server=https://index.docker.io/v1/ --docker-username=<user> --docker-password=<token>\n"+
					"  • Ou recopiez l'image dans votre propre registre (ECR/GCR/ACR/Harbor) et pullez depuis là.\n"+
					"  • Ou installez un cache pull-through pour que le cluster ne pulle chaque image qu'une fois.\n"+
					"  • Mettez imagePullPolicy: IfNotPresent pour qu'un redémarrage réutilise l'image en cache.",
				ctx.Pod.Namespace)

		case pullAuth:
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

		case pullMissing:
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

		case pullNetwork:
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

// imagePullMessage picks the most informative text available, which is not the
// most recent one.
//
// A failing pull produces a burst of Failed events, and only the first carries
// the registry's actual answer:
//
//	Failed  Failed to pull image "polinux/stress": ... dial tcp: lookup
//	        registry-1.docker.io: Try again
//	Failed  Error: ErrImagePull
//	Failed  Error: ImagePullBackOff
//
// The kubelet then repeats the last two on every retry, so taking the latest
// event yields "Error: ImagePullBackOff" — which names the symptom the user
// already saw in `kubectl get pod`, and sends Explain to its default branch
// instead of the network one. Preferring a message that actually matches a
// known cause is what keeps the diagnosis specific.
func imagePullMessage(ctx *DiagnosticContext, c container) string {
	candidates := make([]string, 0, len(ctx.Events)+1)
	for _, e := range eventsWithReason(ctx, "Failed") {
		candidates = append(candidates, e.Message)
	}
	// The waiting message sometimes holds the registry error verbatim, which
	// is how the architecture rule finds it too.
	candidates = append(candidates, c.State.Message)

	// 1. Prefer a message naming a cause Explain can act on. Among those, the
	//    longest wins: containerd nests the real error deepest.
	if best := longestMatching(candidates, func(s string) bool {
		return containsAnyFold(s, imagePullCauseSignals...)
	}); best != "" {
		return best
	}

	// 2. Nothing conclusive: take the most detailed message that is not the
	//    kubelet restating the pod's status.
	if best := longestMatching(candidates, func(s string) bool {
		return !isImagePullBoilerplate(s)
	}); best != "" {
		return best
	}

	// 3. Everything is boilerplate. Report it rather than nothing.
	if e, ok := lastEventWithReason(ctx, "Failed"); ok && strings.TrimSpace(e.Message) != "" {
		return strings.TrimSpace(e.Message)
	}
	if c.State.Message != "" {
		return strings.TrimSpace(c.State.Message)
	}
	return strings.TrimSpace(c.State.Reason)
}

// longestMatching returns the longest trimmed candidate satisfying pred.
func longestMatching(candidates []string, pred func(string) bool) string {
	var best string
	for _, s := range candidates {
		s = strings.TrimSpace(s)
		if s == "" || len(s) <= len(best) || !pred(s) {
			continue
		}
		best = s
	}
	return best
}

// isImagePullBoilerplate reports whether a message is one the kubelet repeats
// on every retry. The comparison is on the whole message, so a detailed error
// that merely starts with "Error: " is not discarded.
func isImagePullBoilerplate(msg string) bool {
	msg = strings.TrimSpace(msg)
	for _, b := range imagePullBoilerplate {
		if strings.EqualFold(msg, b) {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(msg), "back-off pulling image")
}

func serviceAccountOrDefault(ctx *DiagnosticContext) string {
	if ctx.Pod.ServiceAccount != "" {
		return ctx.Pod.ServiceAccount
	}
	return "default"
}
