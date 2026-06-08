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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
)

// checksumMarker is the file written into a materialized blueprint directory holding
// the content checksum of the ConfigMap it was built from. It gates re-materialization
// and survives daemon restarts.
const checksumMarker = ".checksum"

// BlueprintReconciler materializes Spectrum-X blueprint ConfigMaps from the operator
// namespace onto the local filesystem, so the DMS planner can consume them via
// --blueprints-root. Each labeled ConfigMap <name> is written to <BaseDir>/<name>/.
//
// Materialization is checksum-gated: a directory is rewritten only when the ConfigMap
// content checksum differs from the on-disk marker, which also self-heals on-disk drift
// and partial writes.
type BlueprintReconciler struct {
	client.Client
	// Namespace is the operator namespace the reconciler watches for blueprint ConfigMaps.
	Namespace string
	// BaseDir is the on-disk base directory; defaults to consts.SpectrumXBlueprintsBaseDir.
	BaseDir string
}

//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile materializes (or removes) the blueprint directory for a ConfigMap.
func (r *BlueprintReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.Log.V(2).Info("BlueprintReconciler.Reconcile", "configmap", req.NamespacedName)

	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, req.NamespacedName, cm); err != nil {
		if apierrors.IsNotFound(err) {
			// ConfigMap deleted — drop its materialized blueprint.
			log.Log.V(2).Info("blueprint ConfigMap not found, removing materialized dir", "configmap", req.NamespacedName, "dir", filepath.Join(r.BaseDir, req.Name))
			if rmErr := removeBlueprint(r.BaseDir, req.Name); rmErr != nil {
				return ctrl.Result{}, rmErr
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	log.Log.V(2).Info("reconciling blueprint ConfigMap", "configmap", req.NamespacedName, "dataKeys", len(cm.Data))

	if err := materializeBlueprint(r.BaseDir, cm.Name, cm.Data); err != nil {
		log.Log.Error(err, "failed to materialize blueprint", "configmap", req.NamespacedName)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler to watch labeled ConfigMaps in the operator namespace.
func (r *BlueprintReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.BaseDir == "" {
		r.BaseDir = consts.SpectrumXBlueprintsBaseDir
	}
	namespace := r.Namespace
	pred := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == namespace && o.GetLabels()[consts.SpectrumXBlueprintLabel] == consts.LabelValueTrue
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(pred)).
		Named("spectrumx-blueprint").
		Complete(r)
}

// blueprintChecksum returns a content checksum over the ConfigMap data, independent of
// map iteration order and of object metadata (labels/annotations/resourceVersion).
func blueprintChecksum(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(data[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// decodeBlueprintKey reverses the ConfigMap-key encoding back to a relative file path.
// ConfigMap keys may only contain [-._a-zA-Z0-9], so the author encodes '/' as '__' and
// '@' as '_AT_'. The result is validated to be a clean relative path (no traversal).
func decodeBlueprintKey(key string) (string, error) {
	rel := strings.ReplaceAll(key, "_AT_", "@")
	rel = strings.ReplaceAll(rel, "__", "/")

	if rel == "" || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid blueprint key %q: empty or absolute path", key)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("invalid blueprint key %q: illegal path segment in %q", key, rel)
		}
	}
	return rel, nil
}

// validateBlueprintName guards the ConfigMap name used as a directory name.
func validateBlueprintName(name string) error {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return fmt.Errorf("invalid blueprint name %q", name)
	}
	return nil
}

// materializeBlueprint writes the ConfigMap data to <baseDir>/<name>/ as a decoded file
// tree. It is a no-op when the on-disk checksum marker already matches; otherwise it
// builds the tree in a temp dir and atomically swaps it in (so consumers never see a
// partial tree, and stale files are pruned).
func materializeBlueprint(baseDir, name string, data map[string]string) error {
	if err := validateBlueprintName(name); err != nil {
		return err
	}

	sum := blueprintChecksum(data)
	finalDir := filepath.Join(baseDir, name)
	if existing, err := os.ReadFile(filepath.Join(finalDir, checksumMarker)); err == nil && string(existing) == sum {
		log.Log.V(2).Info("blueprint up to date, skipping materialization", "name", name, "dir", finalDir, "checksum", sum)
		return nil // already materialized and current
	}
	log.Log.V(2).Info("materializing blueprint (checksum changed or new)", "name", name, "dir", finalDir, "checksum", sum, "files", len(data))

	tmpDir := filepath.Join(baseDir, ".tmp-"+name)
	if err := os.RemoveAll(tmpDir); err != nil {
		return err
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}

	for key, content := range data {
		rel, err := decodeBlueprintKey(key)
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return err
		}
		dest := filepath.Join(tmpDir, rel)
		if !strings.HasPrefix(dest, tmpDir+string(os.PathSeparator)) {
			_ = os.RemoveAll(tmpDir)
			return fmt.Errorf("blueprint key %q escapes the target directory", key)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			_ = os.RemoveAll(tmpDir)
			return err
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			_ = os.RemoveAll(tmpDir)
			return err
		}
	}

	if err := os.WriteFile(filepath.Join(tmpDir, checksumMarker), []byte(sum), 0o644); err != nil {
		_ = os.RemoveAll(tmpDir)
		return err
	}

	if err := os.RemoveAll(finalDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return err
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return err
	}

	log.Log.V(2).Info("materialized blueprint", "name", name, "dir", finalDir, "files", len(data))
	return nil
}

// removeBlueprint deletes a materialized blueprint directory.
func removeBlueprint(baseDir, name string) error {
	if err := validateBlueprintName(name); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(baseDir, name))
}
