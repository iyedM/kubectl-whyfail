package rules

import (
	"fmt"
	"strings"
)

// badCommandSignals are what a container runtime prints when the entrypoint
// itself cannot be executed.
var badCommandSignals = []string{
	"executable file not found",
	"no such file or directory",
	"exec: \"",
	"command not found",
	"oci runtime create failed",
	"starting container process caused",
	"permission denied",
	"not a directory",
}

// crashLoopCommandRule catches a container whose CMD/ENTRYPOINT is simply
// wrong: a binary that is not in the image, a typo in the path, or a script
// without the executable bit.
//
// Exit codes 127 (command not found) and 126 (found but not executable) are
// the shell's own way of saying exactly this, and they are decisive. The
// runtime's error message is the other decisive signal.
var crashLoopCommandRule = Rule{
	Name: "crashloop_command",

	Match: func(ctx *DiagnosticContext) bool {
		// "exec format error" is the same failure mode but a different cause;
		// rule 10 explains the architecture mismatch properly.
		if hasArchMismatchSignal(ctx) {
			return false
		}
		_, ok := badCommandContainer(ctx)
		return ok
	},

	Explain: func(ctx *DiagnosticContext) Diagnosis {
		c, _ := badCommandContainer(ctx)
		st, _ := lastExit(c)

		cmd := quote(append(append([]string{}, c.Command...), c.Args...))
		var cmdLine, cmdLineFR string
		if cmd != "" {
			cmdLine = fmt.Sprintf("The pod spec overrides the entrypoint with: %s\n", cmd)
			cmdLineFR = fmt.Sprintf("La spec du pod remplace l'entrypoint par : %s\n", cmd)
		} else {
			cmdLine = "The pod spec sets no command/args, so this is the image's own ENTRYPOINT/CMD.\n"
			cmdLineFR = "La spec du pod ne définit ni command ni args : c'est donc l'ENTRYPOINT/CMD de l'image elle-même.\n"
		}

		detail := badCommandEvidence(ctx, c)

		var why, whyFR string
		switch st.ExitCode {
		case 127:
			why = "Exit code 127 means \"command not found\": the binary does not exist at that path inside the image."
			whyFR = "Le code de sortie 127 signifie « commande introuvable » : le binaire n'existe pas à ce chemin dans l'image."
		case 126:
			why = "Exit code 126 means the file was found but could not be executed — usually a missing +x bit, or a script whose shebang points at an interpreter the image does not have."
			whyFR = "Le code de sortie 126 signifie que le fichier a été trouvé mais n'a pas pu être exécuté — généralement un bit +x manquant, ou un shebang pointant vers un interpréteur absent de l'image."
		default:
			why = "The container runtime could not start the process at all."
			whyFR = "Le runtime de conteneur n'a pas réussi à démarrer le processus du tout."
		}

		cause := fmt.Sprintf(
			"Container %q is in CrashLoopBackOff because its command never runs. %s\n%s%s"+
				"The container exits immediately (%d restarts), so there are no application logs to read — the app never started.",
			c.Name, why, cmdLine, detail, c.RestartCount)
		causeFR := fmt.Sprintf(
			"Le conteneur %q est en CrashLoopBackOff parce que sa commande ne s'exécute jamais. %s\n%s%s"+
				"Le conteneur sort immédiatement (%d redémarrages), il n'y a donc aucun log applicatif à lire — l'application n'a jamais démarré.",
			c.Name, whyFR, cmdLineFR, detail, c.RestartCount)

		suggestion := fmt.Sprintf(
			"Check what the image actually ships before guessing:\n"+
				"  docker run --rm --entrypoint sh %s -c 'ls -l /; command -v <your-binary>'\n"+
				"  docker inspect %s --format '{{.Config.Entrypoint}} {{.Config.Cmd}}'\n"+
				"Then fix the command in the pod spec, or drop the override entirely and let the image's own entrypoint run. "+
				"Remember that `command:` replaces ENTRYPOINT (not CMD) and that it is exec'd directly — shell syntax like pipes or $VAR needs an explicit [\"sh\", \"-c\", \"...\"].",
			c.Image, c.Image)
		suggestionFR := fmt.Sprintf(
			"Vérifiez ce que l'image contient réellement avant de deviner :\n"+
				"  docker run --rm --entrypoint sh %s -c 'ls -l /; command -v <votre-binaire>'\n"+
				"  docker inspect %s --format '{{.Config.Entrypoint}} {{.Config.Cmd}}'\n"+
				"Puis corrigez la commande dans la spec du pod, ou supprimez complètement la surcharge pour laisser l'entrypoint de l'image s'exécuter. "+
				"Rappel : `command:` remplace ENTRYPOINT (pas CMD) et est exécuté directement — une syntaxe shell (pipes, $VAR) exige un [\"sh\", \"-c\", \"...\"] explicite.",
			c.Image, c.Image)

		return Diagnosis{
			Cause:      ctx.L(cause, causeFR),
			Suggestion: ctx.L(suggestion, suggestionFR),
			Confidence: ConfidenceHigh,
		}
	},
}

// badCommandContainer returns a container whose entrypoint cannot be executed.
func badCommandContainer(ctx *DiagnosticContext) (container, bool) {
	return findContainer(ctx, func(c container) bool {
		if wasOOMKilled(c) {
			return false
		}
		st, ok := lastExit(c)
		if !ok && c.State.Type != "Waiting" {
			return false
		}
		// A shell reports 127 for "not found" and 126 for "not executable";
		// both are conclusive on their own.
		if ok && (st.ExitCode == 127 || st.ExitCode == 126) {
			return true
		}
		// Otherwise require the runtime to have said so explicitly, and require
		// the container to actually be failing.
		if !isCrashLooping(c) && !(ok && st.ExitCode != 0) && c.State.Reason != "CreateContainerError" {
			return false
		}
		if containsAnyFold(c.State.Message, badCommandSignals...) {
			return true
		}
		if ok && containsAnyFold(st.Message, badCommandSignals...) {
			return true
		}
		return containsAnyFold(allLogs(c), badCommandSignals...)
	})
}

// badCommandEvidence quotes the runtime's own error, wherever it landed.
func badCommandEvidence(ctx *DiagnosticContext, c container) string {
	candidates := []string{c.State.Message}
	if st, ok := lastExit(c); ok {
		candidates = append(candidates, st.Message)
	}
	candidates = append(candidates, c.PreviousLogs, c.Logs)
	for _, e := range ctx.Events {
		candidates = append(candidates, e.Message)
	}
	for _, s := range candidates {
		if containsAnyFold(s, badCommandSignals...) {
			return "Runtime said: " + strings.TrimSpace(firstLineContaining(s, badCommandSignals)) + "\n"
		}
	}
	return ""
}

// firstLineContaining returns the first line of s holding one of the markers,
// so a 100-line log tail does not end up in the diagnosis.
func firstLineContaining(s string, markers []string) string {
	for _, line := range strings.Split(s, "\n") {
		if containsAnyFold(line, markers...) {
			return line
		}
	}
	return s
}
