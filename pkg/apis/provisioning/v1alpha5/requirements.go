/*
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

package v1alpha5

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	stringsets "k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/aws/karpenter/pkg/utils/sets"
)

// Requirements are an alias type that wrap []v1.NodeSelectorRequirement and
// include an efficient set representation under the hood. Since its underlying
// types are slices and maps, this type should not be used as a pointer.
type Requirements struct {
	// Requirements are layered with Labels and applied to every node.
	Requirements []v1.NodeSelectorRequirement `json:"requirements,omitempty"`
	requirements map[string]sets.Set          `json:"-"`
}

// NewRequirements constructs requirements from NodeSelectorRequirements
func NewRequirements(requirements ...v1.NodeSelectorRequirement) Requirements {
	return Requirements{requirements: map[string]sets.Set{}}.Add(requirements...)
}

// NewLabelRequirements constructs requirements from labels
func NewLabelRequirements(labels map[string]string) Requirements {
	var requirements []v1.NodeSelectorRequirement
	for key, value := range labels {
		requirements = append(requirements, v1.NodeSelectorRequirement{Key: key, Operator: v1.NodeSelectorOpIn, Values: []string{value}})
	}
	return NewRequirements(requirements...)
}

// NewPodRequirements constructs requirements from a pod
func NewPodRequirements(pod *v1.Pod) Requirements {
	var requirements []v1.NodeSelectorRequirement
	for key, value := range pod.Spec.NodeSelector {
		requirements = append(requirements, v1.NodeSelectorRequirement{Key: key, Operator: v1.NodeSelectorOpIn, Values: []string{value}})
	}
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return NewRequirements(requirements...)
	}
	// The legal operators for pod affinity and anti-affinity are In, NotIn, Exists, DoesNotExist.
	// Select heaviest preference and treat as a requirement. An outer loop will iteratively unconstrain them if unsatisfiable.
	if preferred := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution; len(preferred) > 0 {
		sort.Slice(preferred, func(i int, j int) bool { return preferred[i].Weight > preferred[j].Weight })
		requirements = append(requirements, preferred[0].Preference.MatchExpressions...)
	}
	// Select first requirement. An outer loop will iteratively remove OR requirements if unsatisfiable
	if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil &&
		len(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) > 0 {
		requirements = append(requirements, pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions...)
	}
	return NewRequirements(requirements...)
}

// Add function returns a new Requirements object with new requirements inserted.
func (r Requirements) Add(requirements ...v1.NodeSelectorRequirement) Requirements {
	// Deep copy to avoid mutating existing requirements
	cp := *r.DeepCopy()
	// This fail-safe measurement can be removed later when we implement test webhook.
	if cp.requirements == nil {
		cp.requirements = map[string]sets.Set{}
	}
	for _, requirement := range requirements {
		if normalized, ok := NormalizedLabels[requirement.Key]; ok {
			requirement.Key = normalized
		}
		if IgnoredLabels.Has(requirement.Key) {
			continue
		}
		cp.Requirements = append(cp.Requirements, requirement)
		var values sets.Set
		switch requirement.Operator {
		case v1.NodeSelectorOpIn:
			values = sets.NewSet(requirement.Values...)
		case v1.NodeSelectorOpNotIn:
			values = sets.NewComplementSet(requirement.Values...)
		case v1.NodeSelectorOpExists:
			values = sets.NewComplementSet()
		case v1.NodeSelectorOpDoesNotExist:
			values = sets.NewSet()
		}
		if existing, ok := cp.requirements[requirement.Key]; ok {
			values = values.Intersection(existing)
		}
		cp.requirements[requirement.Key] = values
	}
	cp.rebuild()
	return cp
}

// rebuild re-generates the node selector requirements based on the set based versions.  This improves scheduling speed
// as it causes us to not carry around redundant requirements (e.g. 20x copies of instance-type in [it0, it1, ..., it400])
func (r *Requirements) rebuild() {
	existing := r.Requirements
	r.Requirements = nil
	for key, values := range r.requirements {
		req := v1.NodeSelectorRequirement{
			Key: key,
		}
		if values.IsComplement() {
			req.Operator = v1.NodeSelectorOpNotIn
			req.Values = values.ComplementValues().List()
		} else {
			req.Operator = v1.NodeSelectorOpIn
			req.Values = values.Values().List()
		}
		r.Requirements = append(r.Requirements, req)
	}
	// re-add any requirements that the set based versions don't handle so we can properly validate
	for _, req := range existing {
		switch req.Operator {
		case v1.NodeSelectorOpIn, v1.NodeSelectorOpNotIn, v1.NodeSelectorOpExists, v1.NodeSelectorOpDoesNotExist:
		default:
			r.Requirements = append(r.Requirements, req)
		}
	}

	sort.Slice(r.Requirements, func(a, b int) bool {
		lhs := r.Requirements[a]
		rhs := r.Requirements[b]
		if lhs.Key != rhs.Key {
			return lhs.Key < rhs.Key
		}
		if lhs.Operator != rhs.Operator {
			return lhs.Operator < rhs.Operator
		}
		if len(lhs.Values) != len(rhs.Values) {
			return len(lhs.Values) < len(rhs.Values)
		}

		// lengths are the same now
		for i := 0; i < len(lhs.Values); i++ {
			if lhs.Values[i] != rhs.Values[i] {
				return lhs.Values[i] < rhs.Values[i]
			}
		}
		return false
	})
}

// Keys returns unique set of the label keys from the requirements
func (r Requirements) Keys() stringsets.String {
	keys := stringsets.NewString()
	for _, requirement := range r.Requirements {
		keys.Insert(requirement.Key)
	}
	return keys
}

func (r Requirements) Has(key string) bool {
	_, ok := r.requirements[key]
	return ok
}

func (r Requirements) Get(key string) sets.Set {
	return r.requirements[key]
}

func (r Requirements) Zones() stringsets.String {
	return r.Get(v1.LabelTopologyZone).Values()
}

func (r Requirements) InstanceTypes() stringsets.String {
	return r.Get(v1.LabelInstanceTypeStable).Values()
}

func (r Requirements) Architectures() stringsets.String {
	return r.Get(v1.LabelArchStable).Values()
}

func (r Requirements) OperatingSystems() stringsets.String {
	return r.Get(v1.LabelOSStable).Values()
}

func (r Requirements) CapacityTypes() stringsets.String {
	return r.Get(LabelCapacityType).Values()
}

// Validate validates the feasibility of the requirements.
// Do not apply validation to requirements after merging with other requirements.
//gocyclo:ignore
func (r Requirements) Validate() (errs error) {
	for _, requirement := range r.Requirements {
		for _, err := range validation.IsQualifiedName(requirement.Key) {
			errs = multierr.Append(errs, fmt.Errorf("key %s is not a qualified name, %s", requirement.Key, err))
		}
		for _, value := range requirement.Values {
			for _, err := range validation.IsValidLabelValue(value) {
				errs = multierr.Append(errs, fmt.Errorf("invalid value %s for key %s, %s", value, requirement.Key, err))
			}
		}
		if !SupportedNodeSelectorOps.Has(string(requirement.Operator)) {
			errs = multierr.Append(errs, fmt.Errorf("operator %s not in %s for key %s", requirement.Operator, SupportedNodeSelectorOps.UnsortedList(), requirement.Key))
		}
	}
	return errs
}

// Compatible ensures the provided requirements can be met.
func (r Requirements) Compatible(requirements Requirements) (errs error) {
	for key, requirement := range requirements.requirements {
		intersection := requirement.Intersection(r.Get(key))
		// There must be some value, except in these cases
		if intersection.Len() == 0 {
			// Where incoming requirement has operator { NotIn, DoesNotExist }
			if requirement.Type() == v1.NodeSelectorOpNotIn || requirement.Type() == v1.NodeSelectorOpDoesNotExist {
				// And existing requirement has operator { NotIn, DoesNotExist }
				if r.Get(key).Type() == v1.NodeSelectorOpNotIn || r.Get(key).Type() == v1.NodeSelectorOpDoesNotExist {
					continue
				}
			}
			errs = multierr.Append(errs, fmt.Errorf("%s not in %s, key %s", requirement, r.Get(key), key))
		}
	}
	return errs
}

func (r *Requirements) MarshalJSON() ([]byte, error) {
	if r.Requirements == nil {
		r.Requirements = []v1.NodeSelectorRequirement{}
	}
	return json.Marshal(r.Requirements)
}

func (r *Requirements) UnmarshalJSON(b []byte) error {
	var requirements []v1.NodeSelectorRequirement
	if err := json.Unmarshal(b, &requirements); err != nil {
		return err
	}
	*r = NewRequirements(requirements...)
	return nil
}

func (r Requirements) String() string {
	var sb strings.Builder
	for key, req := range r.requirements {
		var values []string
		if !req.IsComplement() {
			values = req.Values().List()
		} else {
			values = req.ComplementValues().List()
		}
		if sb.Len() > 0 {
			sb.WriteString(", ")
		}
		if len(values) > 5 {
			values[5] = fmt.Sprintf("and %d others", len(values)-5)
			values = values[0:6]
		}
		fmt.Fprintf(&sb, "%s %s %v", key, req.Type(), values)
	}
	return sb.String()
}
