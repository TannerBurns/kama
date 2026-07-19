/*
Copyright 2026 Kama Authors.

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

// Package v1alpha1 contains API Schema definitions for the kama v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=kama.tannerburns.github.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion identifies the v1alpha1 Kama API.
	GroupVersion = schema.GroupVersion{Group: "kama.tannerburns.github.io", Version: "v1alpha1"}

	// SchemeBuilder registers the v1alpha1 Kama API with a runtime scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds all v1alpha1 Kama API types to a runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(
		GroupVersion,
		&ModelCache{},
		&ModelCacheList{},
		&ModelArtifact{},
		&ModelArtifactList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
