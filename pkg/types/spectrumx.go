/*
2025 NVIDIA CORPORATION & AFFILIATES
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

package types

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// DMSConfigOp is a single do-SPCX semantic-group operation: a YANG container path plus
// one or more leaf -> typed-value assignments. It maps 1:1 onto a dms-cli invocation:
//
//	dms-cli -t pci/<BDF> <Path> <leaf>=<value> [<leaf>=<value> ...]
//
// (the new T1/T2 DMS client; the old `dmsc … set --update path:::type:::value` form is gone).
type DMSConfigOp struct {
	// Path is the YANG container path, e.g. /nvidia/roce.
	Path string
	// Values maps a leaf name to its JSON-decoded typed value (bool/float64/string/[]any).
	Values map[string]any
}

// StringifyDMSValue renders a JSON-decoded value as a dms-cli assignment value:
// bool -> "true"/"false", integral float64 -> bare integer, string -> as-is,
// []any -> "[a,b,c]". dms-cli infers the YANG type, so no explicit type tag is emitted.
func StringifyDMSValue(v any) (string, error) {
	switch val := v.(type) {
	case bool:
		return strconv.FormatBool(val), nil
	case float64:
		if val == math.Trunc(val) && !math.IsInf(val, 0) {
			return strconv.FormatInt(int64(val), 10), nil
		}
		return strconv.FormatFloat(val, 'f', -1, 64), nil
	case string:
		return val, nil
	case []any:
		parts := make([]string, 0, len(val))
		for _, e := range val {
			s, err := StringifyDMSValue(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	default:
		return "", fmt.Errorf("unsupported value type %T", v)
	}
}
