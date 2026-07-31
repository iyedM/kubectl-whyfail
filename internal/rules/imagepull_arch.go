package rules

import (
	"fmt"
	"regexp"
	"strings"
)

// platformRE pulls "linux/amd64" out of a manifest error such as
// "no matching manifest for linux/amd64 in the manifest list entries".
var platformRE = regexp.MustCompile(`(?i)\b(linux|windows)/(amd64|arm64|arm|386|ppc64le|s390x)(/v\d)?\b`)

// imagePullArchRule explains the mistake that has become common since Apple
// Silicon: an image built on an arm64 laptop, pushed as a single-arch image,
// and deployed to an amd64 cluster (or the reverse).
//
// It shows up in two shapes — the registry refuses to serve a manifest for the
// node's platform, or the pull succeeds and the binary dies instantly with
// "exec format error". Both are the same root cause, so both live here.
var imagePullArchRule = Rule{
	Name: "imagepull_arch",

	Match: func(ctx *DiagnosticContext) bool {
		return hasArchMismatchSignal(ctx)
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		msg, c := archEvidence(ctx)

		nodeArch := ""
		if ctx.Node != nil {
			nodeArch = ctx.Node.Architecture
		}
		wanted := nodeArch
		if m := platformRE.FindString(msg); m != "" {
			wanted = m
		} else if nodeArch != "" {
			wanted = "linux/" + nodeArch
		}
		if wanted == "" {
			wanted = "the node's platform"
		}

		nodeDesc := ""
		nodeDescFR := ""
		if ctx.Node != nil && ctx.Node.Architecture != "" {
			nodeDesc = fmt.Sprintf(" Node %s runs %s/%s.", ctx.Node.Name, orNA(ctx.Node.OS), ctx.Node.Architecture)
			nodeDescFR = fmt.Sprintf(" Le node %s tourne en %s/%s.", ctx.Node.Name, orNA(ctx.Node.OS), ctx.Node.Architecture)
		}

		image := c.Image
		if image == "" {
			image = "<unknown>"
		}

		var shape, shapeFR string
		if containsFold(msg, "exec format error") {
			shape = "The image was pulled successfully, but its binary is compiled for a different CPU architecture, so the kernel refuses to execute it (\"exec format error\")."
			shapeFR = "L'image a bien été récupérée, mais son binaire est compilé pour une autre architecture CPU : le noyau refuse de l'exécuter (« exec format error »)."
		} else {
			shape = fmt.Sprintf("The registry has no variant of this image for %s, so the pull fails before the container is ever created.", wanted)
			shapeFR = fmt.Sprintf("Le registre n'a aucune variante de cette image pour %s : le pull échoue avant même la création du conteneur.", wanted)
		}

		cause := fmt.Sprintf(
			"Image %q (container %q) does not match the architecture of the node it was scheduled on. %s%s\n"+
				"This is the classic Apple Silicon trap: `docker build` on an arm64 machine produces an arm64-only image, "+
				"which an amd64 cluster cannot run.\nRuntime said: %s",
			image, c.Name, shape, nodeDesc, orNA(strings.TrimSpace(msg)))
		causeFR := fmt.Sprintf(
			"L'image %q (conteneur %q) ne correspond pas à l'architecture du node sur lequel elle a été planifiée. %s%s\n"+
				"C'est le piège classique d'Apple Silicon : `docker build` sur une machine arm64 produit une image arm64 uniquement, "+
				"qu'un cluster amd64 ne peut pas exécuter.\nRéponse du runtime : %s",
			image, c.Name, shapeFR, nodeDescFR, orNA(strings.TrimSpace(msg)))

		target := wanted
		if !strings.Contains(target, "/") {
			target = "linux/amd64"
		}

		suggestion := fmt.Sprintf(
			"Check what the image actually contains, then rebuild it multi-arch:\n"+
				"  docker manifest inspect %s | grep architecture\n"+
				"  docker buildx build --platform linux/amd64,linux/arm64 -t %s --push .\n"+
				"A single-platform build for this cluster also works:  docker build --platform %s ...\n"+
				"Beware: `docker build --platform` alone still needs the base image and any downloaded binaries to exist for that platform.",
			image, image, target)
		suggestionFR := fmt.Sprintf(
			"Vérifiez ce que contient réellement l'image, puis reconstruisez-la en multi-arch :\n"+
				"  docker manifest inspect %s | grep architecture\n"+
				"  docker buildx build --platform linux/amd64,linux/arm64 -t %s --push .\n"+
				"Un build mono-plateforme pour ce cluster fonctionne aussi :  docker build --platform %s ...\n"+
				"Attention : `docker build --platform` seul exige quand même que l'image de base et les binaires téléchargés existent pour cette plateforme.",
			image, image, target)

		return Diagnosis{
			Cause:      ctx.L(cause, causeFR),
			Suggestion: ctx.L(suggestion, suggestionFR),
			Confidence: ConfidenceHigh,
		}
	},
}

// archEvidence returns the message proving the mismatch and the container it
// belongs to.
func archEvidence(ctx *DiagnosticContext) (string, container) {
	for _, c := range ctx.Containers {
		if containsAnyFold(c.State.Message, archMismatchSignals...) {
			return c.State.Message, c
		}
		if c.LastState != nil && containsAnyFold(c.LastState.Message, archMismatchSignals...) {
			return c.LastState.Message, c
		}
		if logs := allLogs(c); containsFold(logs, "exec format error") {
			return firstLineContaining(logs, []string{"exec format error"}), c
		}
	}
	if e, ok := lastEventMessageContaining(ctx, archMismatchSignals...); ok {
		if cs := appContainers(ctx); len(cs) > 0 {
			return e.Message, cs[0]
		}
		return e.Message, container{}
	}
	if cs := appContainers(ctx); len(cs) > 0 {
		return "", cs[0]
	}
	return "", container{}
}
