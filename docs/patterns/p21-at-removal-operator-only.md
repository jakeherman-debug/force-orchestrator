---
audience: agent
scope: AT removal/deprecation is operator-UI-only — LLM proposal schemas may not declare a remove intent.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P21 — AT removal is operator-only
type: pattern-doc
pattern: P21
---

# Pattern P21 — AT removal is operator-only

## Rationale

Spec deprecation is operator-UI-only. LLMs cannot propose
`REMOVE` / `DEPRECATE` on AT references. Removal moves an AT from
`verification_spec_json.ats[]` to
`verification_spec_json.deprecated[]` with operator-supplied
rationale; that mutation must be reachable only from operator-routed
handlers. Roadmap reference: D3 § "Spec deprecation (concern #9)" /
exit criterion 14d.

P21 graduates to a BoS commit-time rule when D4 ships.

## What it checks

`TestPattern_P21_ATRemovalIsOperatorOnly` walks production Go (cmd/,
internal/, excluding `internal/audittools/` and
`internal/store/schema.go`) and inspects every string literal:

1. **Schema-definition prong.** A literal matching
   `p21RemovalIntentRe` (`(remove|deprecate|delete)[_-]?at(s|_id|_ids)?`,
   case-insensitive) anywhere is an offender — that's an LLM-prompt /
   JSON-schema declaration of removal intent on AT references.
2. **Write-path prong.** A literal matching `p21DeprecatedWriteRe`
   (json_insert/json_set/json_replace on
   `verification_spec_json.deprecated`, OR a Go-side
   `verification_spec_json.deprecated = ...` assignment) is an
   offender unless the file is in `p21OperatorWriteAllowlist` (empty
   at landing — slice γ populates it with the operator dashboard
   handler).

`TestPattern_P21_AllowlistReasonsTruthful` asserts entries in the
allowlist mention `operator / dashboard / handler / endpoint /
ratify / approve / ui-routed / operator-action`.

## How it fails

```
Pattern P21 (D3 14d): N production site(s) violate the operator-only AT-removal invariant:
  internal/agents/captain_proposal.go:42 — removal-intent keyword on AT references in a non-operator file
      preview: {"action": "remove_ats", "ids": [...]}
...
Fix: spec deprecation is operator-UI-only. LLM proposal schemas MUST NOT declare a remove/deprecate intent on AT references. Writes to verification_spec_json.deprecated[] route through the operator dashboard handler ONLY.
```

## How to fix

Remove the LLM-side removal intent. Move deprecation writes into the
operator dashboard handler and add the handler's file path to
`p21OperatorWriteAllowlist` with a reason naming the operator
endpoint.

## Test reference

- File: `internal/audittools/audit_pattern_p21_at_removal_operator_only_test.go`
- Core assertions:
  - `TestPattern_P21_ATRemovalIsOperatorOnly` (lines 91–211)
  - `TestPattern_P21_AllowlistReasonsTruthful` (lines 217–249)
- Regexes: `p21RemovalIntentRe`, `p21DeprecatedWriteRe` (lines 74, 82).

## See also

- [P20 — AT-id scope integrity](p20-at-id-scope-integrity.md)
- [P-StagingPromotionConfirm](p-staging-promotion-confirm.md)
