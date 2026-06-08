# Design: DMS Blueprints (do-SPCX) integration for Spectrum-X configuration

**Status:** Draft — for review
**Component:** nic-configuration-operator (`pkg/spectrumx`, `pkg/dms`, `internal/controller`)
**Related:** DMS T1/T2 `dms-cli` + `/nvidia/blueprints/plan`
(`dms/t1_t2/blueprints/arch/RA22_USER_GUIDE.md`, `dms/t1_t2/arch/transition/SPCX_TRANSITION.md`)

---

## 1. Summary

Spectrum-X NIC configuration is driven by the DMS **Blueprints / do-SPCX planner**. Given a
*profile* and a set of *params*, the planner renders a JSON **plan** of typed knob operations
against the `/nvidia/<domain>/…` YANG namespace, in two stages:

- `prepare` — persistent NVConfig requiring a reset (breakout + post-breakout nvconfig).
- `configure` — runtime config (link, eswitch, vf, cc, link-event).

For `deployment_mode=host-k8s` the plan exposes a **`semantic_groups`** view: typed operations
grouped by `order` and `scope`, designed for an external reconciler. NCO consumes those groups and
applies each operation through the new T1/T2 DMS client (`dms-cli`).

This replaces the previous in-tree Spectrum-X recipe flow entirely (no backward compatibility):

- **Blueprints are user-provided**, as a ConfigMap in the operator namespace, referenced by
  `spectrumXOptimized.version`. DMS does not ship blueprints; the ConfigMap is the source of truth.
- A small per-daemon controller materializes that ConfigMap to disk; the planner is pointed at it
  via `--blueprints-root`.
- Plans are rendered in **host-k8s** mode, which needs **no target-map** (the planner autodetects
  devices on-node, and the knob `path`/`values` are independent of device enumeration).

> **Placeholder:** our DMS build does not ship the planner action yet. Plan *generation* is stubbed
> to return committed example plans (`pkg/spectrumx/exampleplans/*.json`); **everything else —
> blueprint materialization, parsing, and apply via `dms-cli` — is real, working, unit-tested
> code.** When DMS ships `/nvidia/blueprints/plan`, only the stub body is swapped for a real
> `dms-cli` invocation.

---

## 2. End-to-end flow

```
User: ConfigMap <operator-ns>/<version>   (labeled, data = {encoded-path: content})
   │  watched in the operator namespace
   ▼  BlueprintReconciler (per daemon, k8s side)
materialize to <baseDir>/<version>/   (checksum-gated, atomic, prunes stale, .checksum marker)
   │
   ▼  NicDevice reconcile (SpectrumXOptimized.enabled)
spectrumx.generatePlan(device, stage):
   resolve <baseDir>/<version>  (error "not materialized" -> requeue until reconciler writes it)
   planner.RenderPlan(profile, stage, --blueprints-root=<baseDir>/<version>,
                      params=deployment_mode=host-k8s planes=N overlay=…)   [STUB: example plan]
   │
   ▼  ParsePlan -> semantic_groups -> []types.DMSConfigOp  (path + leaf values)
   │
   ├─ prepare:   breakout (order 10) -> reboot -> post-breakout-nvconfig (order 20) -> reboot
   └─ configure: link-runtime(30) -> eswitch(40) -> [launch doca_spcx_cc] cc(90) -> link-event(100)
        each op -> dms-cli -t pci/<BDF> <path> <leaf>=<value> …
```

The user-facing CRD (`SpectrumXOptimizedSpec`) is unchanged except that `version` now names the
blueprint ConfigMap.

---

## 3. Blueprint ConfigMap + materialization

### 3.1 ConfigMap format

One ConfigMap per blueprint, named by `spectrumXOptimized.version`, in the operator namespace,
carrying the label `configuration.net.nvidia.com/spectrumx-blueprint: "true"`. Its `data` is the
complete blueprints tree as a flat `{encoded-path: content}` map covering everything the profile's
`extends` chain + features + templates reference (`data/profiles/…`, `data/features/…`,
`data/templates/…`, and `data/{formulas,hca-types,phase-config,execution-groups}.yaml`). The whole
spcx tree is ~340 KB — well under the 1 MB ConfigMap limit.

ConfigMap keys may only contain `[-._a-zA-Z0-9]`, so paths are **encoded**: `/` → `__` and `@` →
`_AT_` (e.g. `data/templates/spcx/services/cc/doca_spcx_cc@.service.j2` →
`data__templates__spcx__services__cc__doca_spcx_cc_AT_.service.j2`). The daemon reverses `_AT_`→`@`
then `__`→`/`, rejecting path traversal. A reference ConfigMap is generated at
`tmp/ra2.2-swmp-cfgmap.yaml`.

### 3.2 BlueprintReconciler (`internal/controller/blueprint_controller.go`)

A per-daemon controller watching labeled ConfigMaps in the operator namespace. It materializes each
to `<baseDir>/<name>/` (`baseDir` = `consts.SpectrumXBlueprintsBaseDir`):

- **Checksum-gated.** sha256 over the sorted ConfigMap `data`, persisted as a `.checksum` marker.
  The tree is rebuilt only when the content checksum differs from the marker — which also self-heals
  on-disk drift / partial writes and ignores metadata churn (labels/resourceVersion). Generation is
  *not* used: it isn't reliably maintained for ConfigMaps, can't detect disk drift, and adds nothing
  the checksum + marker don't already cover.
- **Atomic.** Built in a temp dir and `rename`d in, so consumers never see a partial tree; stale
  files are pruned. ConfigMap delete removes the directory.

RBAC: `configmaps` get/list/watch in the operator namespace.

### 3.3 Optional lookup directory (library use case)

`pkg/spectrumx` stays k8s-free. `NewSpectrumXConfigManager(dmsManager, blueprintsBaseDir)` takes the
base directory (`""` → `consts.SpectrumXBlueprintsBaseDir`), so a library consumer can point it at
its own materialized tree.

---

## 4. Plan generation

`spectrumXSpecToPlannerArgs(spec)` is the single place mapping the CRD onto planner args:

| spec | planner |
|---|---|
| `version` | names the blueprint ConfigMap → `--blueprints-root <baseDir>/<version>` |
| `multiplaneMode` | selects `profile` (`ra2.2-hwmp` for hwplb, `ra2.2-swmp` for swplb/none) |
| `numberOfPlanes` | `params=planes=<N>` |
| `overlay` | `params=overlay=<l3\|none>` |
| (constant) | `params=family=spcx params=deployment_mode=host-k8s` |

`multiplane_mode` is **pinned by the profile** and `nic_type` is **derived from device_id**, so NCO
does not pass them (per the RA22 user guide / profile catalog). The stub planner ignores the args
beyond logging them and returns the committed example plan for the stage; a `// TODO(dms-planner)`
marks where the real `dms-cli /nvidia/blueprints/plan …` call goes.

`generatePlan` errors with "blueprint not materialized yet" if `<baseDir>/<version>` is absent, so
the NicDevice reconcile requeues until the BlueprintReconciler has written it.

---

## 5. Parsing + apply (via `dms-cli`)

### 5.1 The op model

A `semantic_groups` operation is `{path, values:{leaf: typed-value}, target_class, scope, condition}`.
`ParsePlan` + `SemanticGroup.Ops` turn each group's operations (after evaluating `condition` against
`plan.params`) into `[]types.DMSConfigOp{Path, Values}`. There is **no** intermediate
`ConfigurationParameter` wrapper — an op maps 1:1 onto a `dms-cli` call.

### 5.2 DMS client (`pkg/dms`, `dms-cli`)

`SPCX_TRANSITION.md` §4 retires the old `dmsc` client for `dms-cli`/`libdms`. The client speaks the
new form (no backward compatibility):

- target `-t pci/<BDF>`
- set: `dms-cli … <path> <leaf>=<value> [<leaf>=<value> …]` (one invocation per op; the YANG type is
  inferred, so no explicit type tag)
- get: `dms-cli … <path> <leaf> --plain` → `key: value`, parsed back per leaf

`Set/GetParameters([]types.DMSConfigOp)` are the apply/check primitives. QoS uses the flat
target-based paths (`/nvidia/qos/trust-mode`, `/nvidia/qos/pfc/enabled-priorities`,
`/nvidia/roce/tos/value`).

### 5.3 Prepare → NVConfig apply

The `SpectrumXManager` only *exposes* the prepare-stage ops: `GetPrepareOps(device)` returns the
breakout and post-breakout-nvconfig ops divided (with `rawNvConfig` overrides applied — a raw entry
overrides a matching `<path>/<leaf>` op or appends a new op, open item O11). The
**`ConfigurationManager` owns the check and apply** (`spectrumXNVConfigApplied` /
`applySpectrumXNVConfiguration`), so the Spectrum-X NVConfig sequences with the other NVConfig
options it manages (e.g. NetworkBay `system-conf`) rather than being a black box. It applies the
`breakout` ops, signals `RebootRequired`, then (next reconcile) the `post-breakout-nvconfig` ops +
reboot — the same breakout→reboot→post-breakout cadence — checking each via the shared
`dms.OpsApplied` and applying via `dms-cli`.

### 5.4 Configure → runtime apply

`ApplyRuntimeConfig` walks the configure groups in ascending order (`link-runtime`, `eswitch`, `cc`,
`link-event`). Before the `cc` group it launches `doca_spcx_cc` and waits for its startup window
(unchanged; in hwplb once on the first port's RDMA device). `RuntimeConfigApplied` mirrors the walk
and additionally requires `doca_spcx_cc` to be running before the `cc` group is considered applied.
Repeated leaves across ops (e.g. link-runtime's admin-status down→up toggle) are deduped to their
final value for the *check* only; apply executes the full ordered sequence.

---

## 6. Debug logging

The integration-relevant actions log at `V(2)` (enable with `LOG_LEVEL=debug`): blueprint
materialize/skip/remove decisions, resolved/awaited blueprint root, plan stage, parsed op counts,
per-group apply progress (order + op counts), `doca_spcx_cc` launches, the exact `dms-cli` set/get
command lines (`runSet`/`runGetLeaf`), and resolved get values.

---

## 7. Open items (not assumed)

| # | Item | Handling |
|---|---|---|
| O1 | DMS persists prepare-stage YANG writes so a reset realizes them | applied via DMS + reboot; flagged |
| O4 | host-k8s plans carry `condition`s | evaluated against `plan.params` (`@params.X == literal`, ` and `); unsupported grammar errors |
| O6 | list-valued knobs (`lanes`) `dms-cli` encoding | rendered as `[a,b,c]`; confirm on HW |
| O10 | profile names for none/uniplane and RA<2.2 | `ra2.2-{hwmp,swmp}` only; flagged |
| O11 | rawNvConfig names interpreted as `<path>/<leaf>` in the plan flow | override-by-leaf else append op |
| O12 | plan carries no `doca_spcx_cc` binary version | `GetDocaCCTargetVersion` returns ""; binary still launched |
| O13 | ordered toggles (admin down→up) aren't idempotent state | check dedupes to final value; apply runs the sequence |
| O14 | `dms-cli` QoS PFC `enabled-priorities` encoding, per-port selectors, `os install/activate` action form | best-effort; confirm on HW |
