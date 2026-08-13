# storage-foundation — project rules

## Comments and descriptions are self-contained (MUST)

This repository is public. A reader must never be sent to something they cannot open, so in comments,
CRD descriptions, `docs/**`, and test/spec names reference only what is in this repository or publicly
reachable: a symbol, a path, a test name, a public URL. Not a document that is not here, not its path,
not its section number, and not internal process shorthand (phase, block or decision ids).

- **State the rule instead of its address.** If an invariant must match a document outside this
  repository verbatim, the link is a **guard test** here — name it — not a pointer in a comment.
- **Doc comments on API types are user documentation.** `hack/generate_code.sh` runs controller-gen
  over `api/v1alpha1` and writes the CRD descriptions under `crds/internal`; the DataExport and
  DataImport CRDs in `crds/` are hand-curated. Both reach users through `kubectl explain`, and
  `docs/CR*.md` is published as module documentation.
- Commit messages and PR descriptions are public as well, and unlike a comment they cannot be
  corrected without rewriting history. Check before push.
- **The greppable half of this rule is a check:** `hack/check-public-self-containment.sh`. Run it
  before a push. It searches the tracked texts by word for the mechanical forms of such a pointer —
  ids of components, phases and rollout waves, references to a numbered section, ids of process steps
  and lettered alternatives — and its own header is the specification of what it matches, what it
  skips and why. That header also names the three forms it CANNOT see: a pointer written out in
  prose, the name of a repository or host the reader cannot reach, and a retelling of an inaccessible
  document. Those three stay with the reviewer, so a green run is not a certificate. The rule itself
  is stated here and nowhere else — the check does not restate it, so the two cannot drift apart.
  It is not wired into CI yet on purpose: the rule holds for every repository of this stack, so the
  enforcement is going into one shared action rather than a workflow step copied per repository.
