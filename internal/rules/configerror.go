package rules

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// "configmap \"app-config\" not found" / "secret \"db-creds\" not found"
	missingObjectRE = regexp.MustCompile(`(?i)(configmap|secret)\s+"?([a-z0-9.\-_/]+)"?\s+not found`)
	// "couldn't find key DATABASE_URL in ConfigMap prod/app-config"
	missingKeyRE = regexp.MustCompile(`(?i)couldn'?t find key\s+([A-Za-z0-9._\-]+)\s+in\s+(ConfigMap|Secret)\s+([a-z0-9.\-_/]+)`)
)

// configErrorRule explains CreateContainerConfigError: the image is pulled and
// the container is ready to start, but the kubelet cannot build its
// environment because a referenced ConfigMap/Secret — or a key inside one — is
// missing.
//
// The kubelet's message names the object, so the rule parses it and then ties
// it back to the env/volume reference in the pod spec that caused it.
var configErrorRule = Rule{
	Name: "configerror",

	Match: func(ctx *DiagnosticContext) bool {
		if _, ok := waitingWithReason(ctx, "CreateContainerConfigError", "InvalidImageName/ConfigError"); ok {
			return true
		}
		// Some kubelet versions only report it as an event.
		if e, ok := lastEventWithReason(ctx, "Failed"); ok {
			return missingObjectRE.MatchString(e.Message) || missingKeyRE.MatchString(e.Message)
		}
		return false
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		c, _ := waitingWithReason(ctx, "CreateContainerConfigError", "InvalidImageName/ConfigError")
		msg := configErrorMessage(ctx, c)

		name := c.Name
		if name == "" && len(appContainers(ctx)) > 0 {
			name = appContainers(ctx)[0].Name
		}

		var cause, causeFR, suggestion, suggestionFR string

		switch {
		case missingKeyRE.MatchString(msg):
			m := missingKeyRE.FindStringSubmatch(msg)
			key, kind, object := m[1], m[2], m[3]
			cause = fmt.Sprintf(
				"Container %q cannot start (CreateContainerConfigError): the %s %s exists, but it has no key %q. "+
					"The pod spec asks for that key, so the kubelet refuses to create the container.\n"+
					"Kubelet said: %s",
				name, strings.ToLower(kind), object, key, msg)
			causeFR = fmt.Sprintf(
				"Le conteneur %q ne peut pas démarrer (CreateContainerConfigError) : le %s %s existe, mais il ne contient pas la clé %q. "+
					"La spec du pod réclame cette clé, donc le kubelet refuse de créer le conteneur.\n"+
					"Réponse du kubelet : %s",
				name, strings.ToLower(kind), object, key, msg)
			suggestion = fmt.Sprintf(
				"List the keys that actually exist and fix the reference (or add the key):\n"+
					"  kubectl get %s %s -n %s -o jsonpath='{.data}' | tr ',' '\\n'\n"+
					"Mark the reference optional: true if the app can start without it.",
				strings.ToLower(kind), lastPathSegment(object), ctx.Pod.Namespace)
			suggestionFR = fmt.Sprintf(
				"Listez les clés réellement présentes et corrigez la référence (ou ajoutez la clé) :\n"+
					"  kubectl get %s %s -n %s -o jsonpath='{.data}' | tr ',' '\\n'\n"+
					"Passez la référence en optional: true si l'application peut démarrer sans.",
				strings.ToLower(kind), lastPathSegment(object), ctx.Pod.Namespace)

		case missingObjectRE.MatchString(msg):
			m := missingObjectRE.FindStringSubmatch(msg)
			kind, object := strings.ToLower(m[1]), m[2]
			usedFor := configRefUsage(ctx, kind, object)
			cause = fmt.Sprintf(
				"Container %q cannot start (CreateContainerConfigError): the %s %q does not exist in namespace %q, "+
					"but the pod spec references it%s.\nKubelet said: %s",
				name, kind, object, ctx.Pod.Namespace, usedFor, msg)
			causeFR = fmt.Sprintf(
				"Le conteneur %q ne peut pas démarrer (CreateContainerConfigError) : le %s %q n'existe pas dans le namespace %q, "+
					"alors que la spec du pod le référence%s.\nRéponse du kubelet : %s",
				name, kind, object, ctx.Pod.Namespace, usedFor, msg)
			suggestion = fmt.Sprintf(
				"Create it, or point the pod at the right name:\n"+
					"  kubectl get %s -n %s\n"+
					"ConfigMaps and Secrets are namespaced — one that exists in another namespace is invisible here. "+
					"If it is created by Helm/Kustomize, check that it is part of the same release as this pod.",
				kind, ctx.Pod.Namespace)
			suggestionFR = fmt.Sprintf(
				"Créez-le, ou pointez le pod vers le bon nom :\n"+
					"  kubectl get %s -n %s\n"+
					"Les ConfigMaps et Secrets sont namespacés — un objet existant dans un autre namespace est invisible ici. "+
					"S'il est créé par Helm/Kustomize, vérifiez qu'il fait partie de la même release que ce pod.",
				kind, ctx.Pod.Namespace)

		default:
			refs := describeConfigRefs(ctx, name)
			cause = fmt.Sprintf(
				"Container %q cannot start (CreateContainerConfigError): the kubelet could not assemble its configuration "+
					"from the ConfigMaps/Secrets it references.%s\nKubelet said: %s",
				name, refs, msg)
			causeFR = fmt.Sprintf(
				"Le conteneur %q ne peut pas démarrer (CreateContainerConfigError) : le kubelet n'a pas pu assembler sa configuration "+
					"à partir des ConfigMaps/Secrets référencés.%s\nRéponse du kubelet : %s",
				name, refs, msg)
			suggestion = fmt.Sprintf(
				"Check every ConfigMap and Secret the pod references exists in this namespace, with the expected keys:\n"+
					"  kubectl get configmap,secret -n %s",
				ctx.Pod.Namespace)
			suggestionFR = fmt.Sprintf(
				"Vérifiez que chaque ConfigMap et Secret référencé existe dans ce namespace, avec les clés attendues :\n"+
					"  kubectl get configmap,secret -n %s",
				ctx.Pod.Namespace)
		}

		return Diagnosis{
			Cause:      ctx.L(cause, causeFR),
			Suggestion: ctx.L(suggestion, suggestionFR),
			Confidence: ConfidenceHigh,
		}
	},
}

func configErrorMessage(ctx *DiagnosticContext, c container) string {
	if c.State.Message != "" {
		return strings.TrimSpace(c.State.Message)
	}
	for i := len(ctx.Events) - 1; i >= 0; i-- {
		e := ctx.Events[i]
		if missingObjectRE.MatchString(e.Message) || missingKeyRE.MatchString(e.Message) {
			return strings.TrimSpace(e.Message)
		}
	}
	if e, ok := lastEventWithReason(ctx, "Failed"); ok {
		return strings.TrimSpace(e.Message)
	}
	return ""
}

// configRefUsage says how the missing object is wired into the pod, so the
// user knows which field to fix.
func configRefUsage(ctx *DiagnosticContext, kind, object string) string {
	name := lastPathSegment(object)
	for _, c := range ctx.Containers {
		for _, r := range c.EnvRefs {
			if !strings.EqualFold(r.From, kind) || r.Name != name {
				continue
			}
			if r.Key != "" {
				return fmt.Sprintf(" (as env %s, from key %q)", r.EnvVar, r.Key)
			}
			return " (via envFrom)"
		}
	}
	for _, v := range ctx.Pod.Volumes {
		if strings.EqualFold(v.Type, kind) && v.Source == name {
			return fmt.Sprintf(" (mounted as volume %q)", v.Name)
		}
	}
	return ""
}

// describeConfigRefs lists what a container pulls in, for the fallback branch
// where the kubelet message is unhelpful.
func describeConfigRefs(ctx *DiagnosticContext, containerName string) string {
	var refs []string
	for _, c := range ctx.Containers {
		if containerName != "" && c.Name != containerName {
			continue
		}
		for _, r := range c.EnvRefs {
			if r.Key != "" {
				refs = append(refs, fmt.Sprintf("%s/%s[%s]", r.From, r.Name, r.Key))
			} else {
				refs = append(refs, fmt.Sprintf("%s/%s", r.From, r.Name))
			}
		}
	}
	if len(refs) == 0 {
		return ""
	}
	return "\nIt references: " + strings.Join(refs, ", ")
}

// lastPathSegment turns "prod/app-config" into "app-config".
func lastPathSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}
