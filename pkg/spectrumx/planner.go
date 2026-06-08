/*
2026 NVIDIA CORPORATION & AFFILIATES
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package spectrumx

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
)

// PlanStage identifies which do-SPCX plan to render.
type PlanStage string

const (
	// StagePrepare is the persistent NVConfig stage (breakout + post-breakout nvconfig); requires a reset.
	StagePrepare PlanStage = "prepare"
	// StageConfigure is the runtime stage (link, eswitch, vf, cc, link-event).
	StageConfigure PlanStage = "configure"

	// plannerFamily is the do-SPCX blueprint family.
	plannerFamily = "spcx"
	// plannerDeploymentMode selects the host-k8s plan projection (semantic_groups, no systemd).
	plannerDeploymentMode = "host-k8s"
)

// multiplaneModeToDMSMode maps the CRD multiplaneMode enum onto the DMS planner's
// mode name. DMS uses "hwmp"/"swmp" (hardware/software multiplane) where the CRD
// uses "hwplb"/"swplb". This and the profile naming derived from it are best-effort
// until the planner ships — see open item O10 in docs/design-dms-blueprints-spcx.md.
var multiplaneModeToDMSMode = map[string]string{
	consts.MultiplaneModeHwplb:    "hwmp",
	consts.MultiplaneModeSwplb:    "swmp",
	consts.MultiplaneModeUniplane: "uniplane",
	consts.MultiplaneModeNone:     "swmp",
}

// PlannerArgs are the arguments NCO passes to the do-SPCX planner.
type PlannerArgs struct {
	Family  string
	Profile string
	Params  map[string]string
}

// spectrumXSpecToPlannerArgs is the single place the operator decides which profile
// and params to render for a SpectrumXOptimizedSpec. If upstream profile naming
// changes, this is the only knob to update.
func spectrumXSpecToPlannerArgs(spec *v1alpha1.SpectrumXOptimizedSpec) PlannerArgs {
	family := strings.ToLower(spec.Version) // "RA2.2" -> "ra2.2"

	mode, ok := multiplaneModeToDMSMode[spec.MultiplaneMode]
	if !ok {
		mode = multiplaneModeToDMSMode[consts.MultiplaneModeNone]
	}

	overlay := spec.Overlay
	if overlay == "" {
		overlay = consts.OverlayNone
	}

	planes := spec.NumberOfPlanes
	if planes <= 0 {
		planes = 1
	}

	return PlannerArgs{
		Family:  plannerFamily,
		Profile: fmt.Sprintf("%s-%s", family, mode),
		Params: map[string]string{
			"deployment_mode": plannerDeploymentMode,
			"multiplane_mode": mode,
			"planes":          strconv.Itoa(planes),
			"overlay":         overlay,
		},
	}
}

// Planner renders the do-SPCX plan for a device and stage. blueprintsRoot is the
// materialized blueprint directory the planner resolves profiles/features under
// (passed to the real planner as --blueprints-root).
type Planner interface {
	RenderPlan(device *v1alpha1.NicDevice, stage PlanStage, blueprintsRoot string) ([]byte, error)
}

// Committed example plans (CX8, ra2.2-hwmp, 2 planes) sourced verbatim from the
// do-SPCX planner output. Until DMS ships /nvidia/blueprints/plan, the stub planner
// returns these so the parsing/apply path can be exercised end-to-end.
//
//go:embed exampleplans/plan-prepare-ra22-hwmp.json
var examplePreparePlan []byte

//go:embed exampleplans/plan-configure-ra22-hwmp.json
var exampleConfigurePlan []byte

// stubPlanner is the placeholder Planner used until DMS ships the planner action.
// It returns the committed example plan for the requested stage, ignoring the
// rendered args beyond logging them. See docs/design-dms-blueprints-spcx.md (Phase 2).
type stubPlanner struct{}

func newStubPlanner() Planner {
	return &stubPlanner{}
}

func (p *stubPlanner) RenderPlan(device *v1alpha1.NicDevice, stage PlanStage, blueprintsRoot string) ([]byte, error) {
	if device == nil {
		return nil, fmt.Errorf("cannot render plan for a nil device")
	}

	if spec := spectrumXSpec(device); spec != nil {
		args := spectrumXSpecToPlannerArgs(spec)
		// TODO(dms-planner): replace with a real `dms-cli /nvidia/blueprints/plan family=spcx
		// profile=<args.Profile> stage=<stage> --blueprints-root <blueprintsRoot>
		// params=deployment_mode=host-k8s params=planes=.. params=overlay=..` invocation.
		// Until then return the committed example plan.
		log.Log.V(2).Info("stubPlanner.RenderPlan(): returning example plan",
			"device", device.Name, "stage", stage, "profile", args.Profile, "params", args.Params,
			"blueprintsRoot", blueprintsRoot)
	}

	switch stage {
	case StagePrepare:
		return examplePreparePlan, nil
	case StageConfigure:
		return exampleConfigurePlan, nil
	default:
		return nil, fmt.Errorf("unknown plan stage %q", stage)
	}
}

// spectrumXSpec returns the device's SpectrumXOptimized spec, or nil if unset.
func spectrumXSpec(device *v1alpha1.NicDevice) *v1alpha1.SpectrumXOptimizedSpec {
	cfg := device.Spec.Configuration
	if cfg == nil || cfg.Template == nil {
		return nil
	}
	return cfg.Template.SpectrumXOptimized
}

// generatePlan resolves the device's materialized blueprint directory and renders the
// do-SPCX plan for the stage. This is the generation entry point consumed by the
// parse/apply phases. host-k8s deployment mode is used, so no target-map is needed.
func (m *spectrumXConfigManager) generatePlan(device *v1alpha1.NicDevice, stage PlanStage) ([]byte, error) {
	root, err := m.blueprintsRoot(device)
	if err != nil {
		return nil, err
	}
	log.Log.V(2).Info("generating do-SPCX plan", "device", device.Name, "stage", stage, "blueprintsRoot", root)
	return m.planner.RenderPlan(device, stage, root)
}

// blueprintsRoot returns the materialized blueprint directory for the device's
// spectrumXOptimized.version. It errors if the version is unset or the blueprint has
// not been materialized on disk yet — the BlueprintReconciler writes it from the
// ConfigMap, and the NicDevice reconcile requeues until it is present.
func (m *spectrumXConfigManager) blueprintsRoot(device *v1alpha1.NicDevice) (string, error) {
	spec := spectrumXSpec(device)
	if spec == nil || spec.Version == "" {
		return "", fmt.Errorf("device %s has no spectrumXOptimized.version", device.Name)
	}
	root := filepath.Join(m.blueprintsBaseDir, spec.Version)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			log.Log.V(2).Info("blueprint not materialized yet, will requeue", "device", device.Name, "version", spec.Version, "expectedDir", root)
			return "", fmt.Errorf("blueprint %q not materialized yet at %s", spec.Version, root)
		}
		return "", err
	}
	log.Log.V(2).Info("resolved blueprint root", "device", device.Name, "version", spec.Version, "root", root)
	return root, nil
}
