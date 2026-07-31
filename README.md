# kubectl-whyfail

> Un plugin kubectl qui explique en langage humain pourquoi ton pod est en échec.

**Statut : en développement.** Ce README sera complété par Claude Code à l'étape 8 du
prompt dans `PROMPT.md`.

```
kubectl whyfail pod my-app-7d9f8b6c-x2kpl -n production
```

## Pourquoi ce projet

Tout ingénieur Kubernetes connaît ce moment : un pod en `CrashLoopBackOff`, et 30-45
minutes perdues à recouper `kubectl describe`, `kubectl logs` et les events pour
comprendre ce qui se passe. `whyfail` automatise ce recoupement et donne une réponse
claire en quelques secondes.

## Comment ça marche

1. Collecte du contexte (describe, events, logs) — toujours, quel que soit le cas.
2. Vérification contre 10 causes fréquentes et bien testées, réponse instantanée si
   ça matche.
3. Si aucune règle ne matche, appel optionnel à un LLM via OpenRouter (clé API gratuite
   fournie par l'utilisateur, jamais par le mainteneur).

## Installation

```bash
go install github.com/<TON_USERNAME>/kubectl-whyfail/cmd/whyfail@latest
```

## Licence

MIT
# kubectl-whyfail
