# CLAUDE.md — kubectl-whyfail

Ce fichier donne à Claude Code tout le contexte nécessaire pour travailler sur ce projet.
Le lire avant toute modification.

## Ce que fait ce projet

`kubectl-whyfail` est un plugin `kubectl` qui diagnostique pourquoi un pod Kubernetes est en échec
(`CrashLoopBackOff`, `OOMKilled`, `ImagePullBackOff`, `Pending`, etc.) et explique la cause en
langage humain, pas en JSON brut.

Usage cible :
```
kubectl whyfail pod my-app-7d9f8b6c-x2kpl -n production
```

## Architecture (ne pas dévier sans discussion)

Pipeline en 3 étapes, dans cet ordre strict :

1. **Collector** (`internal/collector`) — récupère via `client-go` : describe du pod (status,
   conditions), events du namespace liés au pod, logs du conteneur courant + précédent si
   crash-loop, spec du pod (probes, limits, image, volumes). Cette couche est générique et ne
   contient AUCUNE logique de diagnostic.

2. **Rules engine** (`internal/rules`) — 10 règles déterministes, une par fichier, qui matchent
   sur le contexte collecté. Zéro appel réseau. Zéro dépendance externe. Chaque règle doit être
   testable à 100% avec des fixtures dans `testdata/`.

3. **LLM fallback** (`internal/llmfallback`) — appelé UNIQUEMENT si aucune règle ne matche.
   Utilise l'API OpenRouter (compatible OpenAI) avec la clé fournie par l'utilisateur via
   `OPENROUTER_API_KEY`. Ne jamais coder un modèle en dur — utiliser `openrouter/auto` avec une
   liste de modèles de secours, car le catalogue gratuit d'OpenRouter change souvent.

## Les 10 règles à implémenter (v1, dans cet ordre de priorité)

1. `crashloop_probe.go` — CrashLoopBackOff causé par une liveness probe trop stricte
2. `oomkilled.go` — OOMKilled, limite mémoire trop basse
3. `imagepull.go` — ImagePullBackOff / ErrImagePull (typo, tag inexistant, credentials manquants)
4. `pending_resources.go` — Pending, ressources insuffisantes sur les nodes
5. `pending_pvc.go` — Pending, PVC non bound (StorageClass manquant ou zone incompatible)
6. `configerror.go` — CreateContainerConfigError (ConfigMap/Secret manquant ou mauvaise clé)
7. `evicted.go` — Evicted, pression mémoire/disque sur le node
8. `crashloop_command.go` — CrashLoopBackOff causé par un mauvais CMD/ENTRYPOINT
9. `readiness_never_ready.go` — Readiness probe qui ne passe jamais (mauvais port/path)
10. `imagepull_arch.go` — ImagePullBackOff par mismatch d'architecture (amd64 vs arm64)

Chaque règle respecte cette interface (`internal/rules/rule.go`) :
```go
type Rule struct {
    Name       string
    Match      func(ctx *DiagnosticContext) bool
    Explain    func(ctx *DiagnosticContext) Diagnosis
}

type Diagnosis struct {
    Cause      string
    Suggestion string
    Confidence string // "high" ou "medium"
}
```

## Conventions de code

- Go idiomatique, `gofmt` obligatoire, pas de dépendances inutiles.
- Chaque règle a son fichier de test associé avec au moins 2 fixtures réalistes dans `testdata/`
  (un cas qui matche, un cas qui ne doit PAS matcher pour éviter les faux positifs).
- Pas de logique de diagnostic dans `collector/` — il ne fait QUE de la lecture.
- Les messages affichés à l'utilisateur sont en français OU en anglais selon un flag `--lang`
  (par défaut : anglais, pour l'audience GitHub internationale).
- Aucune clé API ne doit jamais être loggée ou committée.

## Ce qu'il ne faut PAS faire

- Ne pas ajouter de règle au-delà des 10 listées sans qu'on en discute — le scope v1 est
  volontairement fermé pour garantir 100% de fiabilité sur ce périmètre.
- Ne pas appeler le LLM fallback si une règle a déjà matché.
- Ne pas exiger Ollama ou une installation locale de modèle — l'IA doit rester 100% optionnelle
  et basée sur une clé API que l'utilisateur fournit lui-même.

## Commandes utiles

```bash
go build -o whyfail ./cmd/whyfail
go test ./...
gofmt -l .
```
