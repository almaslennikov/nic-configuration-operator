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
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
)

func TestDecodeBlueprintKey(t *testing.T) {
	cases := []struct {
		key     string
		want    string
		wantErr bool
	}{
		{"data__profiles__spcx__ra22__ra2.2-swmp.yaml", "data/profiles/spcx/ra22/ra2.2-swmp.yaml", false},
		{"data__templates__spcx__services__cc__doca_spcx_cc_AT_.service.j2", "data/templates/spcx/services/cc/doca_spcx_cc@.service.j2", false},
		{"data__formulas.yaml", "data/formulas.yaml", false},
		{"__etc__passwd", "", true},             // absolute
		{"data__..__..__etc__passwd", "", true}, // traversal
		{"", "", true},                          // empty
	}
	for _, c := range cases {
		got, err := decodeBlueprintKey(c.key)
		if c.wantErr {
			if err == nil {
				t.Errorf("decodeBlueprintKey(%q): expected error, got %q", c.key, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("decodeBlueprintKey(%q): unexpected error %v", c.key, err)
		}
		if got != c.want {
			t.Errorf("decodeBlueprintKey(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}

func TestBlueprintChecksumStableAndOrderIndependent(t *testing.T) {
	a := map[string]string{"k1": "v1", "k2": "v2", "k3": "v3"}
	b := map[string]string{"k3": "v3", "k1": "v1", "k2": "v2"} // same content, different insert order
	if blueprintChecksum(a) != blueprintChecksum(b) {
		t.Fatal("checksum should be independent of map order")
	}
	c := map[string]string{"k1": "v1", "k2": "CHANGED", "k3": "v3"}
	if blueprintChecksum(a) == blueprintChecksum(c) {
		t.Fatal("checksum should change when content changes")
	}
}

func TestMaterializeBlueprint(t *testing.T) {
	base := t.TempDir()
	data := map[string]string{
		"data__profiles__spcx__ra22__ra2.2-swmp.yaml": "name: ra2.2-swmp\n",
		"data__formulas.yaml":                         "formulas: {}\n",
	}

	// First materialize writes the tree + marker.
	if err := materializeBlueprint(base, "ra2.2-swmp", data); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	profile := filepath.Join(base, "ra2.2-swmp", "data", "profiles", "spcx", "ra22", "ra2.2-swmp.yaml")
	if content, err := os.ReadFile(profile); err != nil || string(content) != "name: ra2.2-swmp\n" {
		t.Fatalf("profile not materialized correctly: content=%q err=%v", content, err)
	}
	marker, err := os.ReadFile(filepath.Join(base, "ra2.2-swmp", checksumMarker))
	if err != nil || string(marker) != blueprintChecksum(data) {
		t.Fatalf("marker missing or wrong: %q err=%v", marker, err)
	}

	// Second materialize with identical data is a no-op (marker unchanged).
	mInfoBefore, _ := os.Stat(filepath.Join(base, "ra2.2-swmp", checksumMarker))
	if err := materializeBlueprint(base, "ra2.2-swmp", data); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	mInfoAfter, _ := os.Stat(filepath.Join(base, "ra2.2-swmp", checksumMarker))
	if !mInfoBefore.ModTime().Equal(mInfoAfter.ModTime()) {
		t.Fatal("identical data should not rewrite the blueprint")
	}

	// Changed data prunes the removed file and rewrites.
	data2 := map[string]string{"data__formulas.yaml": "formulas: {changed: true}\n"}
	if err := materializeBlueprint(base, "ra2.2-swmp", data2); err != nil {
		t.Fatalf("materialize changed: %v", err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("stale file should have been pruned")
	}
}

func TestMaterializeBlueprintRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	if err := materializeBlueprint(base, "bad", map[string]string{"data__..__..__escape": "x"}); err == nil {
		t.Fatal("expected traversal key to be rejected")
	}
}

func TestBlueprintReconcile(t *testing.T) {
	base := t.TempDir()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ra2.2-swmp",
			Namespace: "nvidia-network-operator",
			Labels:    map[string]string{consts.SpectrumXBlueprintLabel: consts.LabelValueTrue},
		},
		Data: map[string]string{"data__formulas.yaml": "formulas: {}\n"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	r := &BlueprintReconciler{Client: cl, Namespace: "nvidia-network-operator", BaseDir: base}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "ra2.2-swmp", Namespace: "nvidia-network-operator"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "ra2.2-swmp", "data", "formulas.yaml")); err != nil {
		t.Fatalf("blueprint not materialized: %v", err)
	}

	// Deleting the ConfigMap removes the materialized dir.
	clEmpty := fake.NewClientBuilder().WithScheme(scheme).Build()
	r.Client = clEmpty
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "ra2.2-swmp")); !os.IsNotExist(err) {
		t.Fatal("blueprint dir should have been removed on ConfigMap delete")
	}
}
