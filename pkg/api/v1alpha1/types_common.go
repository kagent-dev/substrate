//  Copyright 2026 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package v1alpha1

// EnvVar represents an environment variable supplied to a container in an
// ActorTemplate. It mirrors the shape of corev1.EnvVar but only models the
// subset of sources that the substrate control plane resolves: literal values,
// and Secret keys.
type EnvVar struct {
	// Name of the environment variable. Must be a C_IDENTIFIER.
	// +required
	Name string `json:"name"`

	// Variable value. Defaults to "". Mutually exclusive with ValueFrom.
	// +optional
	Value string `json:"value,omitempty"`

	// Source for the environment variable's value. Mutually exclusive with
	// Value.
	// +optional
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

// EnvVarSource represents a source for the value of an EnvVar. Exactly one of
// its fields must be set.
type EnvVarSource struct {
	// Selects a key of a Secret in the ActorTemplate's namespace.
	// +optional
	SecretKeyRef *SecretKeySelector `json:"secretKeyRef,omitempty"`
}

// SecretKeySelector selects a key from a Secret.
type SecretKeySelector struct {
	// Name of the referent Secret.
	// +required
	Name string `json:"name"`

	// Key to select within the Secret.
	// +required
	Key string `json:"key"`

	// Specify whether the Secret or its key must be defined.
	// +optional
	Optional *bool `json:"optional,omitempty"`
}
