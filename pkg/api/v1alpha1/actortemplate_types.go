// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PhaseType string

// Define your phases as constants
const (
	PhaseInitial           PhaseType = ""
	PhaseResumeGoldenActor PhaseType = "ResumeGoldenActor"
	PhaseWaitGoldenActor   PhaseType = "WaitGoldenActor"
	PhaseReady             PhaseType = "Ready"
	PhaseFailed            PhaseType = "Failed"
)

// A single application container that you want to run within a WorkerPool.
type Container struct {
	// Name of the container.
	//
	// +required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="!format.dns1123Label().validate(self).hasValue()",message="Name must be a valid DNS label"
	Name string `json:"name"`

	// Image to use for the worker replicas.
	//
	// +required
	// +kubebuilder:validation:XValidation:rule="self.contains('@')",message="All images must be pinned (changing the image invalidates snapshots)"
	Image string `json:"image,omitempty"`

	// Entrypoint array. Not executed within a shell.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	Command []string `json:"command,omitempty"`

	// Environment variables to set in the worker replicas.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Env []EnvVar `json:"env,omitempty"`
}

// EnvVar represents an environment variable supplied to a container in an
// ActorTemplate. It models only a subset of Kubernetes Pod env behavior:
// literal values are not expanded with Kubernetes-style $(VAR) references,
// envFrom is not supported, and valueFrom currently supports only secretKeyRef.
//
// +kubebuilder:validation:ExactlyOneOf={value, valueFrom}
type EnvVar struct {
	// Name is the name of the environment variable. May be any printable ASCII
	// character except '='.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[ -<>-~]+$`
	Name string `json:"name"`

	// Exactly one of the following must be specified.

	// Variable value. Mutually exclusive with ValueFrom.
	// Value is the literal value of the environment variable. Unlike in
	// Kubernetes pods, this value is not interpolated, and $(VAR)
	// references are not expanded.
	//
	// +optional
	// +kubebuilder:validation:MinLength=0
	Value *string `json:"value,omitempty"`

	// Source for the environment variable's value. Mutually exclusive with
	// Value.
	//
	// +optional
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

// EnvVarSource represents a source for the value of an EnvVar. Exactly one of
// its fields must be set.
//
// +kubebuilder:validation:MinProperties=1
// +kubebuilder:validation:MaxProperties=1
type EnvVarSource struct {
	// Selects a key of a Secret in the ActorTemplate's namespace.
	//
	// +optional
	SecretKeyRef *SecretKeySelector `json:"secretKeyRef,omitempty"`
}

// SecretKeySelector selects a key from a Secret.
type SecretKeySelector struct {
	// Name of the referent Secret.
	//
	// +required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="!format.dns1123Subdomain().validate(self).hasValue()",message="Name must be a valid DNS subdomain"
	Name string `json:"name"`

	// Key to select within the Secret.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`

	// Specify whether the Secret or its key must be defined.
	//
	// +optional
	Optional *bool `json:"optional,omitempty"`
}

type SnapshotsConfig struct {
	// Location to store snapshots in.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	Location string `json:"location"`
}

type EgressPolicyAction string

const (
	EgressPolicyActionAllow EgressPolicyAction = "Allow"
	EgressPolicyActionDeny  EgressPolicyAction = "Deny"
)

type EgressTLSMode string

const (
	EgressTLSModeRequire   EgressTLSMode = "Require"
	EgressTLSModeOriginate EgressTLSMode = "Originate"
	EgressTLSModeIntercept EgressTLSMode = "Intercept"
	EgressTLSModeDisable   EgressTLSMode = "Disable"
)

type EgressTLSPolicy struct {
	// Mode controls how TLS is handled for matching egress traffic.
	//
	// +kubebuilder:validation:Enum=Require;Originate;Intercept;Disable
	// +optional
	Mode EgressTLSMode `json:"mode,omitempty"`

	// Required controls whether matching egress traffic must use TLS.
	// +optional
	Required bool `json:"required,omitempty"`

	// Intercept configures explicit TLS interception for matching egress traffic.
	// +optional
	Intercept *EgressTLSInterceptPolicy `json:"intercept,omitempty"`
}

type EgressTLSInterceptPolicy struct {
	// IssuerSecretRef references the CA material used by the egress gateway to
	// issue certificates for intercepted TLS traffic.
	// +optional
	IssuerSecretRef *corev1.SecretReference `json:"issuerSecretRef,omitempty"`

	// ValidateUpstream controls whether the egress gateway validates the
	// upstream service certificate before proxying intercepted traffic.
	// +optional
	ValidateUpstream bool `json:"validateUpstream,omitempty"`
}

type EgressIPBlock struct {
	// CIDR is an IP address range in CIDR notation.
	// +required
	CIDR string `json:"cidr"`
}

type EgressPolicyDestination struct {
	// Host is the DNS name to match for egress traffic.
	// +optional
	Host string `json:"host,omitempty"`

	// IPBlock is the IP range to match for egress traffic.
	// +optional
	IPBlock *EgressIPBlock `json:"ipBlock,omitempty"`
}

type EgressPort struct {
	// Port is the destination port number.
	// +required
	Port int32 `json:"port"`

	// Protocol is the transport protocol for this port.
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

type EgressCredentialPolicy struct {
	// Inject configures credentials that the egress gateway injects into
	// matching outbound requests. Values are referenced from Kubernetes Secrets;
	// the policy does not contain credential material.
	// +optional
	Inject []EgressCredentialInjection `json:"inject,omitempty"`
}

type EgressCredentialInjection struct {
	// Header is the outbound HTTP header name to set.
	// +required
	Header string `json:"header"`

	// ValueFrom selects the source of the injected credential value.
	// +required
	ValueFrom EgressCredentialValueFrom `json:"valueFrom"`
}

type EgressCredentialValueFrom struct {
	// SecretKeyRef selects a key in a Kubernetes Secret.
	// +optional
	SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
}

type EgressPolicyRule struct {
	// Name is an optional human-readable identifier for this rule.
	// +optional
	Name string `json:"name,omitempty"`

	// To lists the destinations matched by this rule.
	// +optional
	To []EgressPolicyDestination `json:"to,omitempty"`

	// Ports is the list of destination ports matched by this rule.
	// If empty, the rule applies to all destination ports.
	// +optional
	Ports []EgressPort `json:"ports,omitempty"`

	// TLS defines transport security requirements for this destination.
	// +optional
	TLS *EgressTLSPolicy `json:"tls,omitempty"`

	// Credentials configures explicit egress gateway credential injection for
	// matching outbound requests.
	// +optional
	Credentials *EgressCredentialPolicy `json:"credentials,omitempty"`

	// TODO: Add L7 policy fields here when they are needed, such as path
	// matches, rate limits, or header handling.
}

type EgressAuditPolicy struct {
	// Logs enables egress access logs for actors created from this template.
	// +optional
	Logs bool `json:"logs,omitempty"`

	// Traces enables egress tracing for actors created from this template.
	// +optional
	Traces bool `json:"traces,omitempty"`

	// RedactHeaders is the list of headers that must be redacted from audit output.
	// +optional
	RedactHeaders []string `json:"redactHeaders,omitempty"`
}

type EgressPolicy struct {
	// DefaultAction is applied when no allow rule matches.
	//
	// +kubebuilder:validation:Enum=Allow;Deny
	// +optional
	DefaultAction EgressPolicyAction `json:"defaultAction,omitempty"`

	// Allow contains destination rules actors created from this template may reach.
	// +optional
	Allow []EgressPolicyRule `json:"allow,omitempty"`

	// Deny contains destination rules actors created from this template may not reach.
	// +optional
	Deny []EgressPolicyRule `json:"deny,omitempty"`

	// Audit configures egress logging and tracing for actors created from this template.
	// +optional
	Audit *EgressAuditPolicy `json:"audit,omitempty"`
}

// ActorTemplateSpec defined desired spec of an actor.
type ActorTemplateSpec struct {
	// PauseImage is the container to use as the root sandbox container.
	//
	// Typically, set it to [1] for on-gcp, and [2] for off-gcp
	//
	//   - [1] gcr.io/gke-release/pause@sha256:bcbd57ba5653580ec647b16d8163cdd1112df3609129b01f912a8032e48265da
	//   - [2] registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
	//
	// +required
	// +kubebuilder:validation:XValidation:rule="self.contains('@')",message="All images must be pinned (changing the image invalidates snapshots)"
	PauseImage string `json:"pauseImage,omitempty"`

	// Containers is the workload definition.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Containers []Container `json:"containers,omitempty"`

	// Snapshots configuration for the actor.
	//
	// +required
	SnapshotsConfig SnapshotsConfig `json:"snapshotsConfig"`

	// Name of the worker pool to use for the actor.
	//
	// +required
	// TODO: clone this type locally and add validation
	WorkerPoolRef corev1.ObjectReference `json:"workerPoolRef"`

	// Parameters for fetching the runsc binary to use.
	//
	// +required
	Runsc RunscConfig `json:"runsc,omitempty"`

	// EgressPolicy defines the default outbound network policy for actors
	// created from this template.
	// +optional
	EgressPolicy *EgressPolicy `json:"egressPolicy,omitempty"`
}

type GCPAuthenticationConfig struct {
}

// Authentication configuration for atelet to download static files.
//
// If no members are set, then atelet will use anonymous authentication.
type AuthenticationConfig struct {
	// Use GCP application-default credentials.
	//
	// +optional
	GCP *GCPAuthenticationConfig `json:"gcp,omitempty"`
}

type RunscPlatformConfig struct {
	// The SHA256 hash of the binary to download.  Used both to name the
	// downloaded file (for preventing conflicts), and to check the integrity of
	// the downloaded file.
	//
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+$`
	SHA256Hash string `json:"sha256Hash,omitempty"`

	// A gs:// URL pointing to a runsc binary that can be downloaded (possibly
	// with atelet's credentials).
	//
	// +required
	// TODO: add real format checking
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url,omitempty"`
}

type RunscConfig struct {
	// Configuration for the amd64 binary.
	//
	// +optional
	AMD64 *RunscPlatformConfig `json:"amd64,omitempty"`

	// Configuration for the arm64 binary.
	//
	// +optional
	ARM64 *RunscPlatformConfig `json:"arm64,omitempty"`

	// How should atelet authenticate to download the runsc binary?
	Authentication AuthenticationConfig `json:"authentication,omitempty"`
}

// TODO: add validation
type ActorTemplateStatus struct {
	// Phase of the actor template.
	// +optional
	Phase PhaseType `json:"phase,omitempty"`

	GoldenActorID        string      `json:"goldenActorID,omitempty"`
	TakeGoldenSnapshotAt metav1.Time `json:"takeGoldenSnapshotAt,omitempty"`
	GoldenSnapshot       string      `json:"goldenSnapshot,omitempty"`

	// conditions defines the status conditions array
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +kubebuilder:object:generate=true
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=actortemplate
// +kubebuilder:subresource:status
type ActorTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of ActorTemplate
	// +required
	Spec ActorTemplateSpec `json:"spec"`

	// status is the observed state of ActorTemplate
	// +optional
	Status ActorTemplateStatus `json:"status,omitempty"`
}

// ActorTemplateList contains a list of ActorTemplates.
// +kubebuilder:object:generate=true
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=actortemplate
type ActorTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ActorTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ActorTemplate{}, &ActorTemplateList{})
}
