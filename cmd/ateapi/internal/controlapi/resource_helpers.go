// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controlapi

import (
	"fmt"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

func labelSelector(in *ateapipb.LabelSelector) (labels.Selector, error) {
	if in == nil {
		return labels.Everything(), nil
	}
	selector := labels.SelectorFromSet(labels.Set(in.GetMatchLabels()))
	for _, expr := range in.GetMatchExpressions() {
		op, err := selectionOperator(expr.GetOperator())
		if err != nil {
			return nil, err
		}
		req, err := labels.NewRequirement(expr.GetKey(), op, expr.GetValues())
		if err != nil {
			return nil, err
		}
		selector = selector.Add(*req)
	}
	return selector, nil
}

func selectionOperator(op string) (selection.Operator, error) {
	switch op {
	case "In":
		return selection.In, nil
	case "NotIn":
		return selection.NotIn, nil
	case "Exists":
		return selection.Exists, nil
	case "DoesNotExist":
		return selection.DoesNotExist, nil
	default:
		return "", fmt.Errorf("unsupported selector operator %q", op)
	}
}
