package controller

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Minimal local types for the KEDA CRDs (keda.sh/v1alpha1). Only the fields
// the operator renders are modelled; JSON tags must match the KEDA CRD
// schema exactly. Defining them locally keeps the operator free of a
// compile-time keda dependency while staying typed for the fake-client
// tests — the objects are plain API resources on the wire.
const (
	kedaAPIVersion = "keda.sh/v1alpha1"
	wakeDBSecret   = "de-wake-db"
	wakeAuthName   = "de-wake-db-auth"
)

// kedaGroupVersion is registered wholesale: besides the modelled kinds, the
// informer path needs the metav1 request/watch kinds (ListOptions,
// WatchEvent, …) present in the group version, or reflector calls fail with
// "v1.ListOptions is not suitable for converting to keda.sh/v1alpha1".
var kedaGroupVersion = schema.GroupVersion{Group: "keda.sh", Version: "v1alpha1"}

// AddKedaTypesToScheme registers the locally-modelled KEDA kinds plus their
// metav1 companions so the controller-runtime cache can inform on them.
func AddKedaTypesToScheme(scheme *runtime.Scheme) {
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("ScaledObject"), &KedaScaledObject{})
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("ScaledObjectList"), &KedaScaledObjectList{})
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("TriggerAuthentication"), &KedaTriggerAuthentication{})
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("TriggerAuthenticationList"), &KedaTriggerAuthenticationList{})
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("ListOptions"), &metav1.ListOptions{})
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("GetOptions"), &metav1.GetOptions{})
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("DeleteOptions"), &metav1.DeleteOptions{})
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("CreateOptions"), &metav1.CreateOptions{})
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("UpdateOptions"), &metav1.UpdateOptions{})
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("PatchOptions"), &metav1.PatchOptions{})
	scheme.AddKnownTypeWithName(kedaGroupVersion.WithKind("WatchEvent"), &metav1.WatchEvent{})
}

// KedaScaledObject scales one employee's Deployment on the portal
// database's activity signal.
// +kubebuilder:object:generate=false
type KedaScaledObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec KedaScaledObjectSpec `json:"spec"`
}

type KedaScaledObjectSpec struct {
	ScaleTargetRef  KedaScaleTargetRef `json:"scaleTargetRef"`
	PollingInterval *int32             `json:"pollingInterval,omitempty"`
	CooldownPeriod  *int32             `json:"cooldownPeriod,omitempty"`
	MinReplicaCount *int32             `json:"minReplicaCount,omitempty"`
	MaxReplicaCount *int32             `json:"maxReplicaCount,omitempty"`
	Triggers        []KedaScaleTrigger `json:"triggers"`
}

type KedaScaleTargetRef struct {
	Name string `json:"name"`
}

type KedaScaleTrigger struct {
	Type              string                   `json:"type"`
	Metadata          map[string]string        `json:"metadata"`
	AuthenticationRef *KedaScaledObjectAuthRef `json:"authenticationRef,omitempty"`
}

type KedaScaledObjectAuthRef struct {
	Name string `json:"name"`
}

// KedaTriggerAuthentication points the postgresql trigger's password at the
// shared wake-db Secret.
// +kubebuilder:object:generate=false
type KedaTriggerAuthentication struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec KedaTriggerAuthenticationSpec `json:"spec"`
}

type KedaTriggerAuthenticationSpec struct {
	SecretTargetRef []KedaSecretTargetRef `json:"secretTargetRef"`
}

type KedaSecretTargetRef struct {
	Parameter string `json:"parameter"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

// DeepCopyBoilerplate satisfies runtime.Object for the controller-runtime
// client and fake client (JSON round-trip: these objects are small).
func (in *KedaScaledObject) DeepCopyObject() runtime.Object {
	return deepCopyKeda[KedaScaledObject](in)
}

func (in *KedaTriggerAuthentication) DeepCopyObject() runtime.Object {
	return deepCopyKeda[KedaTriggerAuthentication](in)
}

func deepCopyKeda[T any](in *T) *T {
	raw, err := json.Marshal(in)
	if err != nil {
		panic("keda type marshal: " + err.Error())
	}
	out := new(T)
	if err := json.Unmarshal(raw, out); err != nil {
		panic("keda type unmarshal: " + err.Error())
	}
	return out
}

// List variants: the controller-runtime client's typed path resolves them
// even for single-object Gets — register them alongside the single kinds.
type KedaScaledObjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KedaScaledObject `json:"items"`
}

type KedaTriggerAuthenticationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KedaTriggerAuthentication `json:"items"`
}

func (in *KedaScaledObjectList) DeepCopyObject() runtime.Object {
	return deepCopyKeda[KedaScaledObjectList](in)
}

func (in *KedaTriggerAuthenticationList) DeepCopyObject() runtime.Object {
	return deepCopyKeda[KedaTriggerAuthenticationList](in)
}

var _ runtime.Object = (*KedaScaledObject)(nil)
var _ runtime.Object = (*KedaTriggerAuthentication)(nil)
var _ runtime.Object = (*KedaScaledObjectList)(nil)
var _ runtime.Object = (*KedaTriggerAuthenticationList)(nil)
