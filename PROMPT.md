# Prompt à donner à Claude Code pour démarrer

Colle ce prompt dans Claude Code, depuis la racine du dossier `kubectl-whyfail`
(après avoir bien lu CLAUDE.md, qui est déjà dans le dossier).

---

Lis CLAUDE.md avant de commencer. Construis le squelette complet du projet
kubectl-whyfail en respectant strictement l'architecture décrite dedans.

Étapes, dans cet ordre :

1. Initialise le module Go (`go.mod`) avec le nom de module
   `github.com/<TON_USERNAME>/kubectl-whyfail`.

2. Crée `internal/collector/collector.go` : une fonction
   `Collect(clientset kubernetes.Interface, namespace, podName string) (*DiagnosticContext, error)`
   qui récupère describe, events, logs (courant + précédent), et spec du pod.
   Définis la struct `DiagnosticContext` avec tous les champs nécessaires aux 10 règles.

3. Crée `internal/rules/rule.go` avec les types `Rule` et `Diagnosis` exactement comme
   spécifié dans CLAUDE.md, plus une fonction `RunAll(ctx *DiagnosticContext) (*Diagnosis, bool)`
   qui boucle sur toutes les règles enregistrées et retourne la première qui matche.

4. Implémente les 10 règles listées dans CLAUDE.md, une par fichier, avec un test
   unitaire par règle dans le même dossier (`_test.go`), et les fixtures JSON/texte
   correspondantes dans `testdata/`. Chaque test doit vérifier un cas positif ET un cas
   négatif (pour éviter les faux positifs).

5. Crée `internal/llmfallback/openrouter.go` : un client minimal pour l'API OpenRouter
   (`https://openrouter.ai/api/v1/chat/completions`), qui lit `OPENROUTER_API_KEY` depuis
   l'environnement, essaie `openrouter/auto` en premier avec 2-3 modèles de secours codés
   en constantes (pas en dur dans la logique), et retourne un `Diagnosis` avec
   `Confidence: "medium"`.

6. Crée `internal/output/output.go` : formatage terminal coloré (utilise
   `github.com/fatih/color` ou équivalent), avec un badge visuel différent selon que le
   diagnostic vient d'une règle ou du LLM fallback.

7. Crée `cmd/whyfail/main.go` : point d'entrée du plugin kubectl, parsing des arguments
   (`pod <nom> [-n namespace] [--lang fr|en]`), connexion au cluster via kubeconfig
   standard, orchestration collector → rules → llmfallback → output.

8. Génère un `README.md` complet et engageant : description du problème résolu,
   GIF placeholder à remplacer, installation (`go install`), exemples d'usage avant/après,
   liste des 10 erreurs couvertes, section "comment ajouter une règle" pour encourager les
   contributions.

9. Ajoute `.gitignore` (Go standard + `whyfail` binaire), `LICENSE` (MIT), et un
   `.github/workflows/ci.yml` qui lance `go build`, `go vet` et `go test ./...` sur chaque
   push/PR.

Contraintes non négociables :
- Le programme doit compiler et tous les tests doivent passer avant de considérer une étape
  terminée.
- Ne mélange jamais logique de collecte et logique de diagnostic.
- Le LLM fallback ne doit jamais être appelé si une règle a matché — vérifie ce comportement
  avec un test d'intégration dans `cmd/whyfail`.

Travaille étape par étape, montre-moi le résultat de `go build` et `go test ./...` après
chaque étape majeure (collector, rules, llmfallback, main).
