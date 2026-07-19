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

// Package controller contains Kama's Kubernetes reconcilers.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	kamav1alpha1 "github.com/TannerBurns/kama/api/v1alpha1"
	"github.com/TannerBurns/kama/internal/artifact"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kamaName                = "kama"
	artifactImportComponent = "artifact-import"
	modelCacheComponent     = "model-cache"
	cacheName               = "cache"
	sourceName              = "source"
	tokenName               = "token"
	artifactName            = "artifact"
	metricNamespaceLabel    = "namespace"
	metricResultLabel       = "result"
	metricReasonLabel       = "reason"
	conditionNotPresent     = "condition is not present"
	failureNotPresent       = "failure is not present"
	probeSucceededReason    = "ProbeSucceeded"
	probeRunningReason      = "ProbeRunning"
	artifactClaimRefsReason = "ArtifactClaimReferencesRemain"
	managedByLabel          = "app.kubernetes.io/managed-by"
	componentLabel          = "app.kubernetes.io/component"
	cacheNameLabel          = "kama.tannerburns.github.io/model-cache"
	cacheUIDLabel           = "kama.tannerburns.github.io/model-cache-uid"
	artifactNameLabel       = "kama.tannerburns.github.io/model-artifact"
	artifactUIDLabel        = "kama.tannerburns.github.io/model-artifact-uid"
	operationIDLabel        = "kama.tannerburns.github.io/operation"
	leaseFingerprintLabel   = "kama.tannerburns.github.io/lease-fingerprint"
	importerContainer       = "importer"
	importSpecKey           = "spec.json"
	importSpecMount         = "/etc/kama/import"
	importSpecPath          = importSpecMount + "/" + importSpecKey
	terminationLogPath      = "/dev/termination-log"
	cacheMountPath          = "/cache"
	sourceMountPath         = "/source"
	tokenMountPath          = "/var/run/secrets/kama"
	defaultProbeInterval    = 5 * time.Minute
)

// ImporterOptions configure Jobs created by the controllers.
type ImporterOptions struct {
	Image            string
	PullPolicy       corev1.PullPolicy
	ImagePullSecrets []corev1.LocalObjectReference
	HubEndpoint      string
}

func (o ImporterOptions) validate() error {
	if o.Image == "" {
		return errors.New("importer image must not be empty")
	}
	switch o.PullPolicy {
	case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
	default:
		return fmt.Errorf("unsupported importer image pull policy %q", o.PullPolicy)
	}
	if o.HubEndpoint == "" {
		return errors.New("hub endpoint must not be empty")
	}
	return nil
}

type reconcilerBase struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     events.EventRecorder
	Clientset    kubernetes.Interface
	Importer     ImporterOptions
	ProbeTimeout time.Duration
}

func (b reconcilerBase) validate() error {
	if b.Client == nil || b.Scheme == nil || b.Recorder == nil || b.Clientset == nil {
		return errors.New("client, scheme, recorder, and clientset are required")
	}
	return b.Importer.validate()
}

func deterministicName(prefix string, values ...string) string {
	hasher := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(hasher, value)
		_, _ = hasher.Write([]byte{0})
	}
	suffix := hex.EncodeToString(hasher.Sum(nil))[:12]
	cleanPrefix := strings.Trim(strings.ToLower(prefix), "-")
	if problems := validation.IsDNS1123Label(cleanPrefix); len(problems) != 0 {
		cleanPrefix = kamaName
	}
	maximumPrefix := 63 - len(suffix) - 1
	if len(cleanPrefix) > maximumPrefix {
		cleanPrefix = strings.TrimRight(cleanPrefix[:maximumPrefix], "-")
	}
	return cleanPrefix + "-" + suffix
}

func boundedLabelValue(value string) string {
	if len(value) <= 63 && len(validation.IsValidLabelValue(value)) == 0 {
		return value
	}
	return deterministicName("object", value)
}

func operationID(value any, additional ...string) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode operation identity: %w", err)
	}
	hasher := sha256.New()
	_, _ = hasher.Write(payload)
	for _, item := range additional {
		_, _ = hasher.Write([]byte{0})
		_, _ = io.WriteString(hasher, item)
	}
	return hex.EncodeToString(hasher.Sum(nil))[:20], nil
}

func ownerReference(owner client.Object, scheme *runtime.Scheme) ([]metav1.OwnerReference, error) {
	gvks, _, err := scheme.ObjectKinds(owner)
	if err != nil {
		return nil, fmt.Errorf("resolve owner kind: %w", err)
	}
	if len(gvks) == 0 {
		return nil, errors.New("owner kind is not registered")
	}
	reference := metav1.NewControllerRef(owner, gvks[0])
	return []metav1.OwnerReference{*reference}, nil
}

func normalizeMountScope(modes []corev1.PersistentVolumeAccessMode) (kamav1alpha1.MountScope, error) {
	if slices.Contains(modes, corev1.ReadWriteOncePod) {
		return kamav1alpha1.MountScopeSinglePod, nil
	}
	if slices.Contains(modes, corev1.ReadWriteOnce) {
		return kamav1alpha1.MountScopeSingleNode, nil
	}
	for _, mode := range modes {
		if mode == corev1.ReadWriteMany || mode == corev1.ReadOnlyMany {
			return kamav1alpha1.MountScopeMultiNode, nil
		}
	}
	return "", fmt.Errorf("unsupported access modes %v", modes)
}

func hasWritableMode(modes []corev1.PersistentVolumeAccessMode) bool {
	for _, mode := range modes {
		if mode == corev1.ReadWriteMany || mode == corev1.ReadWriteOnce || mode == corev1.ReadWriteOncePod {
			return true
		}
	}
	return false
}

type volumeIdentity struct {
	AccessModes      []corev1.PersistentVolumeAccessMode
	VolumeMode       corev1.PersistentVolumeMode
	MountScope       kamav1alpha1.MountScope
	StorageClassName string
	VolumeName       string
	VolumeUID        types.UID
	NodeAffinity     *corev1.VolumeNodeAffinity
	Capacity         *resource.Quantity
}

func (b reconcilerBase) resolveVolume(ctx context.Context, claim *corev1.PersistentVolumeClaim) (volumeIdentity, error) {
	return b.resolveVolumeWithReader(ctx, b.Client, claim)
}

func (b reconcilerBase) resolveVolumeWithReader(
	ctx context.Context,
	reader client.Reader,
	claim *corev1.PersistentVolumeClaim,
) (volumeIdentity, error) {
	mode := corev1.PersistentVolumeFilesystem
	if claim.Spec.VolumeMode != nil {
		mode = *claim.Spec.VolumeMode
	}
	if mode != corev1.PersistentVolumeFilesystem {
		return volumeIdentity{}, errors.New("PVC volumeMode must be Filesystem")
	}
	accessModes := claim.Status.AccessModes
	if len(accessModes) == 0 {
		// PVC status normally echoes the bound modes, but a newly-bound claim or
		// lightweight API implementation may only expose the requested modes.
		accessModes = claim.Spec.AccessModes
	}
	accessModes = normalizeAccessModes(accessModes)
	scope, err := normalizeMountScope(accessModes)
	if err != nil {
		return volumeIdentity{}, err
	}
	identity := volumeIdentity{
		AccessModes: append([]corev1.PersistentVolumeAccessMode(nil), accessModes...),
		VolumeMode:  mode,
		MountScope:  scope,
		VolumeName:  claim.Spec.VolumeName,
	}
	if claim.Spec.StorageClassName != nil {
		identity.StorageClassName = *claim.Spec.StorageClassName
	}
	if capacity, found := claim.Status.Capacity[corev1.ResourceStorage]; found {
		copy := capacity.DeepCopy()
		identity.Capacity = &copy
	}
	if claim.Spec.VolumeName == "" {
		return identity, nil
	}
	var volume corev1.PersistentVolume
	if err := reader.Get(ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, &volume); err != nil {
		return volumeIdentity{}, fmt.Errorf("get bound PersistentVolume: %w", err)
	}
	identity.VolumeUID = volume.UID
	identity.NodeAffinity = normalizeVolumeNodeAffinity(volume.Spec.NodeAffinity)
	return identity, nil
}

func normalizeAccessModes(modes []corev1.PersistentVolumeAccessMode) []corev1.PersistentVolumeAccessMode {
	normalized := append([]corev1.PersistentVolumeAccessMode(nil), modes...)
	slices.Sort(normalized)
	result := normalized[:0]
	for _, mode := range normalized {
		if len(result) == 0 || result[len(result)-1] != mode {
			result = append(result, mode)
		}
	}
	return result
}

func normalizeVolumeNodeAffinity(affinity *corev1.VolumeNodeAffinity) *corev1.VolumeNodeAffinity {
	if affinity == nil {
		return nil
	}
	normalized := affinity.DeepCopy()
	if normalized.Required == nil {
		return normalized
	}
	for termIndex := range normalized.Required.NodeSelectorTerms {
		term := &normalized.Required.NodeSelectorTerms[termIndex]
		term.MatchExpressions = normalizeRequirements(term.MatchExpressions)
		term.MatchFields = normalizeRequirements(term.MatchFields)
	}
	slices.SortFunc(normalized.Required.NodeSelectorTerms, func(left, right corev1.NodeSelectorTerm) int {
		leftPayload, _ := json.Marshal(left)
		rightPayload, _ := json.Marshal(right)

		return strings.Compare(string(leftPayload), string(rightPayload))
	})
	terms := normalized.Required.NodeSelectorTerms[:0]
	previous := ""
	for _, term := range normalized.Required.NodeSelectorTerms {
		payload, _ := json.Marshal(term)
		key := string(payload)
		if len(terms) == 0 || key != previous {
			terms = append(terms, term)
			previous = key
		}
	}
	normalized.Required.NodeSelectorTerms = terms
	return normalized
}

func normalizeRequirements(requirements []corev1.NodeSelectorRequirement) []corev1.NodeSelectorRequirement {
	for index := range requirements {
		slices.Sort(requirements[index].Values)
		values := requirements[index].Values[:0]
		for _, value := range requirements[index].Values {
			if len(values) == 0 || values[len(values)-1] != value {
				values = append(values, value)
			}
		}
		requirements[index].Values = values
	}
	slices.SortFunc(requirements, func(left, right corev1.NodeSelectorRequirement) int {
		if left.Key != right.Key {
			return strings.Compare(left.Key, right.Key)
		}
		if left.Operator != right.Operator {
			return strings.Compare(string(left.Operator), string(right.Operator))
		}

		return strings.Compare(strings.Join(left.Values, "\x00"), strings.Join(right.Values, "\x00"))
	})
	result := requirements[:0]
	for _, requirement := range requirements {
		if len(result) == 0 || !equality.Semantic.DeepEqual(result[len(result)-1], requirement) {
			result = append(result, requirement)
		}
	}
	return result
}

// volumeNodeAffinitiesCompatible proves that at least one node can satisfy
// both PV required affinities. It mirrors the Kubernetes OR-of-terms,
// AND-of-requirements semantics and is used before a Copy Job mounts both
// source and cache claims.
func volumeNodeAffinitiesCompatible(left, right *corev1.VolumeNodeAffinity) bool {
	leftTerms, leftUnconstrained := requiredVolumeTerms(left)
	rightTerms, rightUnconstrained := requiredVolumeTerms(right)
	if leftUnconstrained && rightUnconstrained {
		return true
	}
	if leftUnconstrained {
		for _, term := range rightTerms {
			if nodeSelectorTermNonempty(term) && nodeSelectorTermsCompatible(corev1.NodeSelectorTerm{}, term) {
				return true
			}
		}
		return false
	}
	if rightUnconstrained {
		for _, term := range leftTerms {
			if nodeSelectorTermNonempty(term) && nodeSelectorTermsCompatible(term, corev1.NodeSelectorTerm{}) {
				return true
			}
		}
		return false
	}
	for _, leftTerm := range leftTerms {
		if !nodeSelectorTermNonempty(leftTerm) {
			continue
		}
		for _, rightTerm := range rightTerms {
			if nodeSelectorTermNonempty(rightTerm) && nodeSelectorTermsCompatible(leftTerm, rightTerm) {
				return true
			}
		}
	}
	return false
}

func nodeSelectorTermNonempty(term corev1.NodeSelectorTerm) bool {
	return len(term.MatchExpressions) > 0 || len(term.MatchFields) > 0
}

func requiredVolumeTerms(affinity *corev1.VolumeNodeAffinity) ([]corev1.NodeSelectorTerm, bool) {
	if affinity == nil || affinity.Required == nil {
		return nil, true
	}
	return affinity.Required.NodeSelectorTerms, false
}

type selectorConstraint struct {
	existence int
	allowed   map[string]struct{}
	excluded  map[string]struct{}
	lower     *int64
	upper     *int64
}

func nodeSelectorTermsCompatible(left, right corev1.NodeSelectorTerm) bool {
	// An empty term supplied by this function represents an unconstrained side;
	// an actual empty required term is filtered by the caller's nonempty peer.
	requirements := make([]struct {
		domain string
		item   corev1.NodeSelectorRequirement
	}, 0, len(left.MatchExpressions)+len(right.MatchExpressions)+len(left.MatchFields)+len(right.MatchFields))
	for _, pair := range []struct {
		domain string
		items  []corev1.NodeSelectorRequirement
	}{
		{"label:", left.MatchExpressions}, {"label:", right.MatchExpressions},
		{"field:", left.MatchFields}, {"field:", right.MatchFields},
	} {
		for _, item := range pair.items {
			requirements = append(requirements, struct {
				domain string
				item   corev1.NodeSelectorRequirement
			}{pair.domain, item})
		}
	}
	if len(requirements) == 0 {
		return true
	}
	constraints := map[string]*selectorConstraint{}
	for _, requirement := range requirements {
		key := requirement.domain + requirement.item.Key
		constraint := constraints[key]
		if constraint == nil {
			constraint = &selectorConstraint{excluded: map[string]struct{}{}}
			constraints[key] = constraint
		}
		if !applySelectorRequirement(constraint, requirement.item) {
			return false
		}
	}
	for _, constraint := range constraints {
		if !selectorConstraintSatisfiable(constraint) {
			return false
		}
	}
	return true
}

func applySelectorRequirement(constraint *selectorConstraint, requirement corev1.NodeSelectorRequirement) bool {
	requireExists := func(value bool) bool {
		wanted := -1
		if value {
			wanted = 1
		}
		if constraint.existence != 0 && constraint.existence != wanted {
			return false
		}
		constraint.existence = wanted
		return true
	}
	switch requirement.Operator {
	case corev1.NodeSelectorOpIn:
		if !requireExists(true) || len(requirement.Values) == 0 {
			return false
		}
		incoming := make(map[string]struct{}, len(requirement.Values))
		for _, value := range requirement.Values {
			incoming[value] = struct{}{}
		}
		if constraint.allowed == nil {
			constraint.allowed = incoming
		} else {
			for value := range constraint.allowed {
				if _, found := incoming[value]; !found {
					delete(constraint.allowed, value)
				}
			}
		}
	case corev1.NodeSelectorOpNotIn:
		if !requireExists(true) {
			return false
		}
		for _, value := range requirement.Values {
			constraint.excluded[value] = struct{}{}
		}
	case corev1.NodeSelectorOpExists:
		return requireExists(true)
	case corev1.NodeSelectorOpDoesNotExist:
		return requireExists(false)
	case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
		if !requireExists(true) || len(requirement.Values) != 1 {
			return false
		}
		value, err := strconv.ParseInt(requirement.Values[0], 10, 64)
		if err != nil {
			return false
		}
		if requirement.Operator == corev1.NodeSelectorOpGt && (constraint.lower == nil || value > *constraint.lower) {
			constraint.lower = &value
		}
		if requirement.Operator == corev1.NodeSelectorOpLt && (constraint.upper == nil || value < *constraint.upper) {
			constraint.upper = &value
		}
	default:
		// Unknown future operators cannot safely prove incompatibility.
		return true
	}
	return true
}

func selectorConstraintSatisfiable(constraint *selectorConstraint) bool {
	if constraint.existence < 0 {
		return constraint.allowed == nil && constraint.lower == nil && constraint.upper == nil
	}
	withinBounds := func(value string) bool {
		if _, excluded := constraint.excluded[value]; excluded {
			return false
		}
		if constraint.lower == nil && constraint.upper == nil {
			return true
		}
		numeric, err := strconv.ParseInt(value, 10, 64)
		if err != nil || constraint.lower != nil && numeric <= *constraint.lower ||
			constraint.upper != nil && numeric >= *constraint.upper {
			return false
		}
		return true
	}
	if constraint.allowed != nil {
		for value := range constraint.allowed {
			if withinBounds(value) {
				return true
			}
		}
		return false
	}
	if constraint.lower != nil && constraint.upper != nil {
		if *constraint.lower == int64(^uint64(0)>>1) || *constraint.lower+1 >= *constraint.upper {
			return false
		}
		if *constraint.lower+2 == *constraint.upper {
			_, excluded := constraint.excluded[strconv.FormatInt(*constraint.lower+1, 10)]
			return !excluded
		}
	}
	return true
}

func newSpecConfigMap(
	owner client.Object,
	scheme *runtime.Scheme,
	name string,
	spec artifact.Spec,
) (*corev1.ConfigMap, error) {
	payload, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("encode importer spec: %w", err)
	}
	references, err := ownerReference(owner, scheme)
	if err != nil {
		return nil, err
	}
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       owner.GetNamespace(),
			OwnerReferences: references,
			Labels: map[string]string{
				managedByLabel: kamaName,
				componentLabel: artifactImportComponent,
			},
		},
		Data:      map[string]string{importSpecKey: string(payload)},
		Immutable: &immutable,
	}, nil
}

type importJobOptions struct {
	Owner         client.Object
	Name          string
	ConfigMapName string
	CacheClaim    string
	SourceClaim   string
	TokenSecret   *kamav1alpha1.SecretKeyReference
	OperationID   string
	ActiveTimeout time.Duration
}

func newImportJob(scheme *runtime.Scheme, importer ImporterOptions, options importJobOptions) (*batchv1.Job, error) {
	references, err := ownerReference(options.Owner, scheme)
	if err != nil {
		return nil, err
	}
	if options.ActiveTimeout <= 0 {
		options.ActiveTimeout = 24 * time.Hour
	}
	deadline := int64(options.ActiveTimeout.Seconds())
	// Structured importer reasons let the controller retry transient failures
	// deliberately; the Job must not repeat permanent auth/checksum/GGUF work.
	backoff := int32(0)
	nonRoot := true
	readOnlyRoot := true
	allowPrivilegeEscalation := false
	runAsUser := int64(65532)
	runAsGroup := int64(65532)
	fsGroup := int64(65532)
	fsGroupChangePolicy := corev1.FSGroupChangeOnRootMismatch
	terminationGrace := int64(30)
	configMapMode := int32(0o644)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            options.Name,
			Namespace:       options.Owner.GetNamespace(),
			OwnerReferences: references,
			Labels: map[string]string{
				managedByLabel:   kamaName,
				componentLabel:   artifactImportComponent,
				operationIDLabel: options.OperationID,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					managedByLabel:   kamaName,
					componentLabel:   artifactImportComponent,
					operationIDLabel: options.OperationID,
				}},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken:  ptr(false),
					EnableServiceLinks:            ptr(false),
					RestartPolicy:                 corev1.RestartPolicyNever,
					ImagePullSecrets:              append([]corev1.LocalObjectReference(nil), importer.ImagePullSecrets...),
					TerminationGracePeriodSeconds: &terminationGrace,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(nonRoot),
						RunAsUser:    &runAsUser,
						RunAsGroup:   &runAsGroup,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:            importerContainer,
						Image:           importer.Image,
						ImagePullPolicy: importer.PullPolicy,
						Args: []string{
							"--spec-file=" + importSpecPath,
							"--result-file=" + terminationLogPath,
						},
						TerminationMessagePath:   terminationLogPath,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPrivilegeEscalation,
							ReadOnlyRootFilesystem:   &readOnlyRoot,
							RunAsNonRoot:             ptr(nonRoot),
							RunAsUser:                &runAsUser,
							RunAsGroup:               &runAsGroup,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("25m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "spec", MountPath: importSpecMount, ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{
						Name: "spec",
						VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: options.ConfigMapName},
							DefaultMode:          &configMapMode,
						}},
					}},
				},
			},
		},
	}
	container := &job.Spec.Template.Spec.Containers[0]
	if options.CacheClaim != "" {
		// Cache-only Jobs may establish the importer group on cache content. A
		// Copy Job also mounts an adopted source claim, so a Pod-wide fsGroup
		// would let kubelet recursively mutate user-owned source permissions.
		if options.SourceClaim == "" {
			job.Spec.Template.Spec.SecurityContext.FSGroup = &fsGroup
			job.Spec.Template.Spec.SecurityContext.FSGroupChangePolicy = &fsGroupChangePolicy
		}
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: cacheName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: options.CacheClaim,
			}},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: cacheName, MountPath: cacheMountPath})
	}
	if options.SourceClaim != "" {
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: sourceName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: options.SourceClaim,
				ReadOnly:  true,
			}},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: sourceName, MountPath: sourceMountPath, ReadOnly: true,
		})
	}
	if options.TokenSecret != nil {
		mode := int32(0o440)
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: tokenName,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  options.TokenSecret.Name,
				DefaultMode: &mode,
				Items: []corev1.KeyToPath{{
					Key: options.TokenSecret.Key, Path: tokenName, Mode: &mode,
				}},
			}},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: tokenName, MountPath: tokenMountPath, ReadOnly: true,
		})
	}
	return job, nil
}

func ptr[T any](value T) *T { return &value }

func jobComplete(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (b reconcilerBase) readJobResult(ctx context.Context, job *batchv1.Job) (artifact.Result, error) {
	return b.readJobResultWithReader(ctx, b.Client, job)
}

func (b reconcilerBase) readJobResultWithReader(
	ctx context.Context,
	reader client.Reader,
	job *batchv1.Job,
) (artifact.Result, error) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return artifact.Result{}, fmt.Errorf("list importer Pods: %w", err)
	}
	slices.SortFunc(pods.Items, func(left, right corev1.Pod) int {
		if left.CreationTimestamp.Before(&right.CreationTimestamp) {
			return -1
		}
		if right.CreationTimestamp.Before(&left.CreationTimestamp) {
			return 1
		}

		return 0
	})
	var lastError error
	for index := range slices.Backward(pods.Items) {
		pod := &pods.Items[index]
		if !hasControllerOwner(pod.OwnerReferences, job.UID, "Job") ||
			pod.Labels[operationIDLabel] != job.Labels[operationIDLabel] {
			continue
		}
		payload, err := b.Clientset.CoreV1().Pods(job.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container:  importerContainer,
			TailLines:  ptr(int64(1)),
			LimitBytes: ptr(int64(artifact.MaxResultBytes)),
		}).DoRaw(ctx)
		if err == nil {
			result, parseErr := artifact.ParseResult(strings.NewReader(string(payload)))
			if parseErr == nil {
				return result, nil
			}
			lastError = parseErr
		} else {
			lastError = err
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != importerContainer || status.State.Terminated == nil || status.State.Terminated.Message == "" {
				continue
			}
			var summary struct {
				SchemaVersion  int             `json:"schemaVersion"`
				Mode           artifact.Mode   `json:"mode"`
				OperationID    string          `json:"operationID"`
				Success        bool            `json:"success"`
				Reason         artifact.Reason `json:"reason"`
				Message        string          `json:"message"`
				ArtifactDigest string          `json:"artifactDigest"`
			}
			if err := json.Unmarshal([]byte(status.State.Terminated.Message), &summary); err == nil &&
				summary.SchemaVersion == artifact.SchemaVersion && artifact.ValidMode(summary.Mode) &&
				(summary.Success || artifact.ValidReason(summary.Reason)) {
				return artifact.Result{
					SchemaVersion:  artifact.SchemaVersion,
					Mode:           summary.Mode,
					OperationID:    summary.OperationID,
					Success:        summary.Success,
					Reason:         summary.Reason,
					Message:        summary.Message,
					ArtifactDigest: summary.ArtifactDigest,
				}, nil
			}
		}
	}
	if lastError == nil {
		lastError = errors.New("importer result is unavailable")
	}
	return artifact.Result{}, lastError
}

func ensureObject(ctx context.Context, kubeClient client.Client, object client.Object) error {
	return ensureObjectWithReader(ctx, kubeClient, kubeClient, object)
}

func ensureObjectWithReader(
	ctx context.Context,
	kubeClient client.Client,
	reader client.Reader,
	object client.Object,
) error {
	err := kubeClient.Create(ctx, object)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	switch desired := object.(type) {
	case *corev1.ConfigMap:
		var existing corev1.ConfigMap
		if err := reader.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
			return err
		}
		if err := validateExistingConfigMap(desired, &existing); err != nil {
			return err
		}
		*desired = existing
		return nil
	case *batchv1.Job:
		var existing batchv1.Job
		if err := reader.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
			return err
		}
		if err := validateExistingJob(desired, &existing); err != nil {
			return err
		}
		*desired = existing
		return nil
	default:
		return fmt.Errorf("refusing unvalidated collision for %T %s/%s", object, object.GetNamespace(), object.GetName())
	}
}

func validateExistingConfigMap(desired, existing *corev1.ConfigMap) error {
	if !ownerIdentityMatches(desired.OwnerReferences, existing.OwnerReferences) {
		return fmt.Errorf("refusing ConfigMap %s/%s with mismatched owner identity", existing.Namespace, existing.Name)
	}
	if !requiredLabelsMatch(desired.Labels, existing.Labels) ||
		!equality.Semantic.DeepEqual(desired.Data, existing.Data) ||
		!equality.Semantic.DeepEqual(desired.BinaryData, existing.BinaryData) ||
		existing.Immutable == nil || !*existing.Immutable {
		return fmt.Errorf("refusing ConfigMap %s/%s with mismatched immutable importer input", existing.Namespace, existing.Name)
	}
	return nil
}

// validateExistingJob deliberately enumerates every immutable Job execution
// field so a deterministic-name collision cannot weaken the importer contract.
//
//nolint:gocyclo // Keeping the checks together makes this trust boundary auditable.
func validateExistingJob(desired, existing *batchv1.Job) error {
	if !ownerIdentityMatches(desired.OwnerReferences, existing.OwnerReferences) {
		return fmt.Errorf("refusing Job %s/%s with mismatched owner identity", existing.Namespace, existing.Name)
	}
	if !requiredLabelsMatch(desired.Labels, existing.Labels) ||
		!requiredLabelsMatch(desired.Spec.Template.Labels, existing.Spec.Template.Labels) {
		return fmt.Errorf("refusing Job %s/%s with mismatched operation labels", existing.Namespace, existing.Name)
	}
	if desired.Spec.BackoffLimit == nil || existing.Spec.BackoffLimit == nil ||
		*desired.Spec.BackoffLimit != *existing.Spec.BackoffLimit ||
		desired.Spec.ActiveDeadlineSeconds == nil || existing.Spec.ActiveDeadlineSeconds == nil ||
		*desired.Spec.ActiveDeadlineSeconds != *existing.Spec.ActiveDeadlineSeconds ||
		!nilOrInt32(existing.Spec.Parallelism, 1) || !nilOrInt32(existing.Spec.Completions, 1) ||
		existing.Spec.BackoffLimitPerIndex != nil || existing.Spec.MaxFailedIndexes != nil ||
		existing.Spec.TTLSecondsAfterFinished != nil ||
		(existing.Spec.ManualSelector != nil && *existing.Spec.ManualSelector) ||
		(existing.Spec.CompletionMode != nil && *existing.Spec.CompletionMode != batchv1.NonIndexedCompletion) ||
		(existing.Spec.PodReplacementPolicy != nil &&
			*existing.Spec.PodReplacementPolicy != batchv1.TerminatingOrFailed) ||
		(existing.Spec.ManagedBy != nil && *existing.Spec.ManagedBy != "kubernetes.io/job-controller") ||
		existing.Spec.Suspend != nil && *existing.Spec.Suspend || existing.Spec.PodFailurePolicy != nil ||
		existing.Spec.SuccessPolicy != nil {
		return fmt.Errorf("refusing Job %s/%s with mismatched execution policy", existing.Namespace, existing.Name)
	}
	if existing.Spec.Selector == nil ||
		existing.Spec.Selector.MatchLabels[batchv1.ControllerUidLabel] != string(existing.UID) ||
		existing.Spec.Template.Labels[batchv1.ControllerUidLabel] != string(existing.UID) {
		return fmt.Errorf("refusing Job %s/%s with mismatched controller selector", existing.Namespace, existing.Name)
	}
	if err := validateExistingPodSpec(&desired.Spec.Template.Spec, &existing.Spec.Template.Spec); err != nil {
		return fmt.Errorf("refusing Job %s/%s: %w", existing.Namespace, existing.Name, err)
	}
	return nil
}

func nilOrInt32(value *int32, expected int32) bool {
	return value == nil || *value == expected
}

// validateExistingPodSpec deliberately enumerates the complete Pod security,
// scheduling, volume, and container contract for an existing deterministic Job.
//
//nolint:gocyclo // Keeping the checks together makes this trust boundary auditable.
func validateExistingPodSpec(desired, existing *corev1.PodSpec) error {
	if len(existing.Containers) != 1 || len(desired.Containers) != 1 ||
		len(existing.InitContainers) != 0 || len(existing.EphemeralContainers) != 0 ||
		existing.HostNetwork || existing.HostPID || existing.HostIPC || existing.ShareProcessNamespace != nil ||
		existing.NodeName != "" || existing.Hostname != "" || existing.Subdomain != "" ||
		len(existing.HostAliases) != 0 || len(existing.NodeSelector) != 0 || existing.Affinity != nil ||
		len(existing.Tolerations) != 0 || len(existing.TopologySpreadConstraints) != 0 ||
		existing.RuntimeClassName != nil || existing.PriorityClassName != "" || existing.Overhead != nil ||
		existing.AutomountServiceAccountToken == nil || *existing.AutomountServiceAccountToken ||
		existing.EnableServiceLinks == nil || *existing.EnableServiceLinks ||
		existing.RestartPolicy != desired.RestartPolicy ||
		!equality.Semantic.DeepEqual(existing.SecurityContext, desired.SecurityContext) ||
		!equality.Semantic.DeepEqual(existing.ImagePullSecrets, desired.ImagePullSecrets) ||
		!equality.Semantic.DeepEqual(existing.Volumes, desired.Volumes) {
		return errors.New("pod security, scheduling, or volume contract differs")
	}
	if existing.ServiceAccountName != "" && existing.ServiceAccountName != "default" {
		return errors.New("pod uses an unexpected service account")
	}
	desiredContainer := desired.Containers[0]
	existingContainer := existing.Containers[0]
	if existingContainer.Name != desiredContainer.Name || existingContainer.Image != desiredContainer.Image ||
		existingContainer.ImagePullPolicy != desiredContainer.ImagePullPolicy ||
		!equality.Semantic.DeepEqual(existingContainer.Command, desiredContainer.Command) ||
		!equality.Semantic.DeepEqual(existingContainer.Args, desiredContainer.Args) ||
		existingContainer.WorkingDir != desiredContainer.WorkingDir || len(existingContainer.Ports) != 0 ||
		len(existingContainer.EnvFrom) != 0 || len(existingContainer.Env) != 0 ||
		!equality.Semantic.DeepEqual(existingContainer.Resources, desiredContainer.Resources) ||
		!equality.Semantic.DeepEqual(existingContainer.VolumeMounts, desiredContainer.VolumeMounts) ||
		len(existingContainer.VolumeDevices) != 0 || existingContainer.LivenessProbe != nil ||
		existingContainer.ReadinessProbe != nil || existingContainer.StartupProbe != nil ||
		existingContainer.Lifecycle != nil ||
		existingContainer.TerminationMessagePath != desiredContainer.TerminationMessagePath ||
		existingContainer.TerminationMessagePolicy != desiredContainer.TerminationMessagePolicy ||
		!equality.Semantic.DeepEqual(existingContainer.SecurityContext, desiredContainer.SecurityContext) ||
		existingContainer.Stdin || existingContainer.StdinOnce || existingContainer.TTY {
		return errors.New("importer container contract differs")
	}
	return nil
}

func requiredControllerOwnerUID(references []metav1.OwnerReference) (types.UID, error) {
	for _, reference := range references {
		if reference.Controller != nil && *reference.Controller {
			return reference.UID, nil
		}
	}
	return "", errors.New("controller owner reference is missing")
}

// ownerIdentityMatches permits deliberately detached cleanup resources while
// retaining the controller-owner identity check for ordinary generated objects.
// Detached resources must remain completely ownerless so garbage collection
// cannot remove them while their ModelArtifact is held by a deletion finalizer.
func ownerIdentityMatches(desired, existing []metav1.OwnerReference) bool {
	ownerUID, err := requiredControllerOwnerUID(desired)
	if err == nil {
		return hasControllerOwner(existing, ownerUID, "")
	}
	return len(desired) == 0 && len(existing) == 0
}

func hasControllerOwner(references []metav1.OwnerReference, uid types.UID, kind string) bool {
	for _, reference := range references {
		if reference.Controller != nil && *reference.Controller && reference.UID == uid &&
			(kind == "" || reference.Kind == kind) {
			return true
		}
	}
	return false
}

func requiredLabelsMatch(required, actual map[string]string) bool {
	for key, value := range required {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func deleteIfPresent(ctx context.Context, kubeClient client.Client, object client.Object) error {
	return deleteWithPropagation(ctx, kubeClient, object, metav1.DeletePropagationBackground)
}

func deleteForeground(ctx context.Context, kubeClient client.Client, object client.Object) error {
	return deleteWithPropagation(ctx, kubeClient, object, metav1.DeletePropagationForeground)
}

func deleteWithPropagation(
	ctx context.Context,
	kubeClient client.Client,
	object client.Object,
	propagation metav1.DeletionPropagation,
) error {
	options := &client.DeleteOptions{PropagationPolicy: &propagation}
	uid := object.GetUID()
	resourceVersion := object.GetResourceVersion()
	if uid != "" || resourceVersion != "" {
		options.Preconditions = &metav1.Preconditions{}
		if uid != "" {
			options.Preconditions.UID = &uid
		}
		if resourceVersion != "" {
			options.Preconditions.ResourceVersion = &resourceVersion
		}
	}
	err := kubeClient.Delete(ctx, object, options)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
