/*
Copyright 2021 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/storage/names"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"
	"sigs.k8s.io/cluster-api/controllers/external"
	utilconversion "sigs.k8s.io/cluster-api/util/conversion"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// clusterTopologyClass holds all the objects required for computing the desired state of a managed Cluster topology.
type clusterTopologyClass struct {
	clusterClass                  *clusterv1.ClusterClass
	infrastructureClusterTemplate *unstructured.Unstructured
	controlPlane                  controlPlaneTopologyClass
	machineDeployments            map[string]machineDeploymentTopologyClass
}

// controlPlaneTopologyClass holds the templates required for computing the desired state of a managed control plane.
type controlPlaneTopologyClass struct {
	template                      *unstructured.Unstructured
	infrastructureMachineTemplate *unstructured.Unstructured
}

// machineDeploymentTopologyClass holds the templates required for computing the desired state of a managed deployment.
type machineDeploymentTopologyClass struct {
	bootstrapTemplate             *unstructured.Unstructured
	infrastructureMachineTemplate *unstructured.Unstructured
}

// clusterTopologyState holds all the objects representing the state of a managed Cluster topology.
// NOTE: please note that we are going to deal with two different type state, the current state as read from the API server,
// and the desired state resulting from processing the clusterTopologyClass.
type clusterTopologyState struct {
	cluster               *clusterv1.Cluster
	infrastructureCluster *unstructured.Unstructured
	controlPlane          controlPlaneTopologyState
	machineDeployments    []machineDeploymentTopologyState //nolint:structcheck
}

// controlPlaneTopologyState all the objects representing the state of a managed control plane.
type controlPlaneTopologyState struct {
	object                        *unstructured.Unstructured
	infrastructureMachineTemplate *unstructured.Unstructured
}

// machineDeploymentTopologyState all the objects representing the state of a managed deployment.
type machineDeploymentTopologyState struct {
	object                        *clusterv1.MachineDeployment //nolint:structcheck
	bootstrapTemplate             *unstructured.Unstructured   //nolint:structcheck
	infrastructureMachineTemplate *unstructured.Unstructured   //nolint:structcheck
}

// getClass gets the ClusterClass and the referenced templates to be used for a managed Cluster topology. It also converts
// and patches all ObjectReferences in ClusterClass and ControlPlane to the latest apiVersion of the current contract.
// NOTE: This function assumes that cluster.Spec.Topology.Class is set.
func (r *ClusterTopologyReconciler) getClass(ctx context.Context, cluster *clusterv1.Cluster) (_ *clusterTopologyClass, reterr error) {
	class := &clusterTopologyClass{
		clusterClass:       &clusterv1.ClusterClass{},
		machineDeployments: map[string]machineDeploymentTopologyClass{},
	}

	// Get ClusterClass.
	key := client.ObjectKey{Name: cluster.Spec.Topology.Class, Namespace: cluster.Namespace}
	if err := r.Client.Get(ctx, key, class.clusterClass); err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve ClusterClass %q in namespace %q", cluster.Spec.Topology.Class, cluster.Namespace)
	}

	// We use the patchHelper to patch potential changes to the ObjectReferences in ClusterClass.
	patchHelper, err := patch.NewHelper(class.clusterClass, r.Client)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err := patchHelper.Patch(ctx, class.clusterClass); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, errors.Wrapf(err, "failed to patch ClusterClass %q in namespace %q", class.clusterClass.Name, class.clusterClass.Namespace)})
		}
	}()

	// Get ClusterClass.spec.infrastructure.
	class.infrastructureClusterTemplate, err = r.getTemplate(ctx, class.clusterClass.Spec.Infrastructure.Ref)
	if err != nil {
		return nil, err
	}

	// Get ClusterClass.spec.controlPlane.
	class.controlPlane.template, err = r.getTemplate(ctx, class.clusterClass.Spec.ControlPlane.Ref)
	if err != nil {
		return nil, err
	}

	// Check if ClusterClass.spec.ControlPlane.MachineInfrastructure is set, as it's optional.
	if class.clusterClass.Spec.ControlPlane.MachineInfrastructure != nil && class.clusterClass.Spec.ControlPlane.MachineInfrastructure.Ref != nil {
		// Get ClusterClass.spec.controlPlane.machineInfrastructure.
		class.controlPlane.infrastructureMachineTemplate, err = r.getTemplate(ctx, class.clusterClass.Spec.ControlPlane.MachineInfrastructure.Ref)
		if err != nil {
			return nil, err
		}
	}

	for _, mdc := range class.clusterClass.Spec.Workers.MachineDeployments {
		mdTopologyClass := machineDeploymentTopologyClass{}

		mdTopologyClass.infrastructureMachineTemplate, err = r.getTemplate(ctx, mdc.Template.Infrastructure.Ref)
		if err != nil {
			return nil, err
		}

		mdTopologyClass.bootstrapTemplate, err = r.getTemplate(ctx, mdc.Template.Bootstrap.Ref)
		if err != nil {
			return nil, err
		}

		class.machineDeployments[mdc.Class] = mdTopologyClass
	}

	return class, nil
}

// getTemplate gets the object referenced in ref.
// If necessary, it updates the ref to the latest apiVersion of the current contract.
func (r *ClusterTopologyReconciler) getTemplate(ctx context.Context, ref *corev1.ObjectReference) (*unstructured.Unstructured, error) {
	if ref == nil {
		return nil, errors.New("reference is not set")
	}
	if err := utilconversion.ConvertReferenceAPIContract(ctx, r.Client, r.restConfig, ref); err != nil {
		return nil, err
	}

	obj, err := external.Get(ctx, r.UnstructuredCachingClient, ref, ref.Namespace)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve %s %q in namespace %q", ref.Kind, ref.Name, ref.Namespace)
	}
	return obj, nil
}

// Gets the current state of the Cluster topology.
func (r *ClusterTopologyReconciler) getCurrentState(ctx context.Context, cluster *clusterv1.Cluster) (*clusterTopologyState, error) {
	// TODO: add get class logic; also remove nolint exception from clusterTopologyState and machineDeploymentTopologyState
	return nil, nil
}

// computeDesiredState computes the desired state of the cluster topology.
// NOTE: We are assuming all the required objects are provided as input; also, in case of any error,
// the entire compute operation operation will fail. This might be improved in the future if support for reconciling
// subset of a topology will be implemented.
func (r *ClusterTopologyReconciler) computeDesiredState(_ context.Context, class *clusterTopologyClass, current *clusterTopologyState) (*clusterTopologyState, error) {
	var err error
	desiredState := &clusterTopologyState{}

	// Compute the desired state of the InfrastructureCluster object.
	if desiredState.infrastructureCluster, err = computeInfrastructureCluster(class, current); err != nil {
		return nil, err
	}

	// If the ControlPlane object requires it, compute the InfrastructureMachineTemplate for the ControlPlane.
	if class.clusterClass.Spec.ControlPlane.MachineInfrastructure != nil {
		if desiredState.controlPlane.infrastructureMachineTemplate, err = computeControlPlaneInfrastructureMachineTemplate(class, current); err != nil {
			return nil, err
		}
	}

	// Compute the desired state of the ControlPlane object, eventually adding a reference to the
	// InfrastructureMachineTemplate generated by the previous step.
	if desiredState.controlPlane.object, err = computeControlPlane(class, current, desiredState.controlPlane.infrastructureMachineTemplate); err != nil {
		return nil, err
	}

	// Compute the desired state for the Cluster object adding a reference to the
	// InfrastructureCluster and the ControlPlane objects generated by the previous step.
	desiredState.cluster = computeCluster(current, desiredState.infrastructureCluster, desiredState.controlPlane.object)

	// TODO: implement generate desired state for machine deployments

	return desiredState, nil
}

// computeInfrastructureCluster computes the desired state for the InfrastructureCluster object starting from the
// corresponding template defined in ClusterClass.
func computeInfrastructureCluster(class *clusterTopologyClass, current *clusterTopologyState) (*unstructured.Unstructured, error) {
	infrastructureCluster, err := templateToObject(templateToInput{
		template:              class.infrastructureClusterTemplate,
		templateClonedFromRef: class.clusterClass.Spec.Infrastructure.Ref,
		cluster:               current.cluster,
		namePrefix:            fmt.Sprintf("%s-", current.cluster.Name),
		currentObjectRef:      current.cluster.Spec.InfrastructureRef,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate the InfrastructureCluster object from the %s", class.infrastructureClusterTemplate.GetKind())
	}
	return infrastructureCluster, nil
}

// computeControlPlaneInfrastructureMachineTemplate computes the desired state for InfrastructureMachineTemplate
// that should be referenced by the ControlPlane object.
func computeControlPlaneInfrastructureMachineTemplate(class *clusterTopologyClass, current *clusterTopologyState) (*unstructured.Unstructured, error) {
	var currentInfrastructureMachineTemplate *corev1.ObjectReference
	if current.controlPlane.object != nil {
		var err error
		if currentInfrastructureMachineTemplate, err = getNestedRef(current.controlPlane.object, "spec", "machineTemplate", "infrastructureRef"); err != nil {
			return nil, errors.Wrap(err, "failed to get spec.machineTemplate.infrastructureRef for the current ControlPlane object")
		}
	}

	controlPlaneInfrastructureMachineTemplate := templateToTemplate(templateToInput{
		template:              class.controlPlane.infrastructureMachineTemplate,
		templateClonedFromRef: objToRef(class.controlPlane.infrastructureMachineTemplate),
		cluster:               current.cluster,
		namePrefix:            fmt.Sprintf("%s-controlplane-", current.cluster.Name),
		currentObjectRef:      currentInfrastructureMachineTemplate,
		labels:                mergeMap(current.cluster.Spec.Topology.ControlPlane.Metadata.Labels, class.clusterClass.Spec.ControlPlane.Metadata.Labels),
		annotations:           mergeMap(current.cluster.Spec.Topology.ControlPlane.Metadata.Annotations, class.clusterClass.Spec.ControlPlane.Metadata.Annotations),
	})
	return controlPlaneInfrastructureMachineTemplate, nil
}

// computeControlPlane computes the desired state for the ControlPlane object starting from the
// corresponding template defined in ClusterClass.
func computeControlPlane(class *clusterTopologyClass, current *clusterTopologyState, infrastructureMachineTemplate *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	controlPlane, err := templateToObject(templateToInput{
		template:              class.controlPlane.template,
		templateClonedFromRef: class.clusterClass.Spec.ControlPlane.Ref,
		cluster:               current.cluster,
		namePrefix:            fmt.Sprintf("%s-", current.cluster.Name),
		currentObjectRef:      current.cluster.Spec.ControlPlaneRef,
		labels:                mergeMap(current.cluster.Spec.Topology.ControlPlane.Metadata.Labels, class.clusterClass.Spec.ControlPlane.Metadata.Labels),
		annotations:           mergeMap(current.cluster.Spec.Topology.ControlPlane.Metadata.Annotations, class.clusterClass.Spec.ControlPlane.Metadata.Annotations),
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate the ControlPlane object from the %s", class.controlPlane.template.GetKind())
	}

	// If the ControlPlane object requires it, add a reference to InfrastructureMachine template to be used for the control plane machines.
	// NOTE: Once set for the first time, the reference name is not expected to changed in this step
	// (instead it could change later during the reconciliation if template rotation is triggered)
	if class.clusterClass.Spec.ControlPlane.MachineInfrastructure != nil {
		if err := setNestedRef(controlPlane, infrastructureMachineTemplate, "spec", "machineTemplate", "infrastructureRef"); err != nil {
			return nil, errors.Wrap(err, "failed to spec.machineTemplate.infrastructureRef in the ControlPlane object")
		}
	}

	// If it is required to manage the number of replicas for the control plane, set the corresponding field.
	// NOTE: If the Topology.controlPlane.replicas value is nil, it is assumed that the control plane controller
	// does not implement support for this field and the ControlPlane object is generated without the number of Replicas.
	if current.cluster.Spec.Topology.ControlPlane.Replicas != nil {
		if err := unstructured.SetNestedField(controlPlane.UnstructuredContent(), int64(*current.cluster.Spec.Topology.ControlPlane.Replicas), "spec", "replicas"); err != nil {
			return nil, errors.Wrap(err, "failed to set spec.replicas in the ControlPlane object")
		}
	}

	// Sets the desired Kubernetes version for the control plane.
	// TODO: improve this logic by adding support for version upgrade component by component
	if err := unstructured.SetNestedField(controlPlane.UnstructuredContent(), current.cluster.Spec.Topology.Version, "spec", "version"); err != nil {
		return nil, errors.Wrap(err, "failed to set spec.version in the ControlPlane object")
	}

	return controlPlane, nil
}

// computeCluster computes the desired state for the Cluster object.
// NOTE: Some fields of the Cluster’s fields contribute to defining how a Cluster should look like (e.g. Cluster.Spec.Topology),
// while some other fields should be managed as part of the actual Cluster (e.g. Cluster.Spec.ControlPlaneRef); in this func
// we are concerned only about the latest group of fields.
func computeCluster(current *clusterTopologyState, infrastructureCluster, controlPlane *unstructured.Unstructured) *clusterv1.Cluster {
	cluster := &clusterv1.Cluster{}
	current.cluster.DeepCopyInto(cluster)

	// Enforce the topology labels.
	// NOTE: The cluster label is added at creation time so this object could be read by the ClusterTopology
	// controller immediately after creation, even before other controllers are going to add the label (if missing).
	if cluster.Labels == nil {
		cluster.Labels = map[string]string{}
	}
	cluster.Labels[clusterv1.ClusterLabelName] = cluster.Name
	cluster.Labels[clusterv1.ClusterTopologyLabelName] = ""

	// Set the references to the infrastructureCluster and controlPlane objects.
	// NOTE: Once set for the first time, the references are not expected to change.
	cluster.Spec.InfrastructureRef = objToRef(infrastructureCluster)
	cluster.Spec.ControlPlaneRef = objToRef(controlPlane)

	return cluster
}

type templateToInput struct {
	template              *unstructured.Unstructured
	templateClonedFromRef *corev1.ObjectReference
	cluster               *clusterv1.Cluster
	namePrefix            string
	currentObjectRef      *corev1.ObjectReference
	labels                map[string]string
	annotations           map[string]string
}

// templateToObject generates an object from a template, taking care
// of adding required labels (cluster, topology), annotations (clonedFrom)
// and assigning a meaningful name (or reusing current reference name).
func templateToObject(in templateToInput) (*unstructured.Unstructured, error) {
	// Enforce the topology labels into the provided label set.
	// NOTE: The cluster label is added at creation time so this object could be read by the ClusterTopology
	// controller immediately after creation, even before other controllers are going to add the label (if missing).
	labels := in.labels
	if labels == nil {
		labels = map[string]string{}
	}
	labels[clusterv1.ClusterLabelName] = in.cluster.Name
	labels[clusterv1.ClusterTopologyLabelName] = ""

	// Generate the object from the template.
	// NOTE: OwnerRef can't be set at this stage; other controllers are going to add OwnerReferences when
	// the object is actually created.
	object, err := external.GenerateTemplate(&external.GenerateTemplateInput{
		Template:    in.template,
		TemplateRef: in.templateClonedFromRef,
		Namespace:   in.cluster.Namespace,
		Labels:      labels,
		Annotations: in.annotations,
		ClusterName: in.cluster.Name,
	})
	if err != nil {
		return nil, err
	}

	// Ensure the generated objects have a meaningful name.
	// NOTE: In case there is already a ref to this object in the Cluster, re-use the same name
	// in order to simplify compare at later stages of the reconcile process.
	object.SetName(names.SimpleNameGenerator.GenerateName(in.namePrefix))
	if in.currentObjectRef != nil && len(in.currentObjectRef.Name) > 0 {
		object.SetName(in.currentObjectRef.Name)
	}

	return object, nil
}

// templateToTemplate generates a template from an existing template, taking care
// of adding required labels (cluster, topology), annotations (clonedFrom)
// and assigning a meaningful name (or reusing current reference name).
// NOTE: We are creating a copy of the ClusterClass template for each cluster so
// it is possible to add cluster specific information without affecting the original object.
func templateToTemplate(in templateToInput) *unstructured.Unstructured {
	template := &unstructured.Unstructured{}
	in.template.DeepCopyInto(template)

	// Enforce the topology labels into the provided label set.
	// NOTE: The cluster label is added at creation time so this object could be read by the ClusterTopology
	// controller immediately after creation, even before other controllers are going to add the label (if missing).
	labels := template.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for key, value := range in.labels {
		labels[key] = value
	}
	labels[clusterv1.ClusterLabelName] = in.cluster.Name
	labels[clusterv1.ClusterTopologyLabelName] = ""
	template.SetLabels(labels)

	// Enforce cloned from annotations.
	annotations := template.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for key, value := range in.annotations {
		annotations[key] = value
	}
	annotations[clusterv1.TemplateClonedFromNameAnnotation] = in.templateClonedFromRef.Name
	annotations[clusterv1.TemplateClonedFromGroupKindAnnotation] = in.templateClonedFromRef.GroupVersionKind().GroupKind().String()
	template.SetAnnotations(annotations)

	// Ensure the generated template gets a meaningful name.
	// NOTE: In case there is already an object ref to this template, it is required to re-use the same name
	// in order to simplify compare at later stages of the reconcile process.
	template.SetName(names.SimpleNameGenerator.GenerateName(in.namePrefix))
	if in.currentObjectRef != nil && len(in.currentObjectRef.Name) > 0 {
		template.SetName(in.currentObjectRef.Name)
	}

	return template
}

// getNestedRef returns the ref value of a nested field.
func getNestedRef(obj *unstructured.Unstructured, fields ...string) (*corev1.ObjectReference, error) {
	ref := &corev1.ObjectReference{}
	if v, ok, err := unstructured.NestedString(obj.UnstructuredContent(), append(fields, "apiVersion")...); ok && err == nil {
		ref.APIVersion = v
	} else {
		return nil, errors.Errorf("failed to get %s.apiVersion from %s", strings.Join(fields, "."), obj.GetKind())
	}
	if v, ok, err := unstructured.NestedString(obj.UnstructuredContent(), append(fields, "kind")...); ok && err == nil {
		ref.Kind = v
	} else {
		return nil, errors.Errorf("failed to get %s.kind from %s", strings.Join(fields, "."), obj.GetKind())
	}
	if v, ok, err := unstructured.NestedString(obj.UnstructuredContent(), append(fields, "name")...); ok && err == nil {
		ref.Name = v
	} else {
		return nil, errors.Errorf("failed to get %s.name from %s", strings.Join(fields, "."), obj.GetKind())
	}
	if v, ok, err := unstructured.NestedString(obj.UnstructuredContent(), append(fields, "namespace")...); ok && err == nil {
		ref.Namespace = v
	} else {
		return nil, errors.Errorf("failed to get %s.namespace from %s", strings.Join(fields, "."), obj.GetKind())
	}
	return ref, nil
}

// setNestedRef sets the value of a nested field to a reference to the refObj provided.
func setNestedRef(obj, refObj *unstructured.Unstructured, fields ...string) error {
	ref := map[string]interface{}{
		"kind":       refObj.GetKind(),
		"namespace":  refObj.GetNamespace(),
		"name":       refObj.GetName(),
		"apiVersion": refObj.GetAPIVersion(),
	}
	return unstructured.SetNestedField(obj.UnstructuredContent(), ref, fields...)
}

func objToRef(obj client.Object) *corev1.ObjectReference {
	gvk := obj.GetObjectKind().GroupVersionKind()
	return &corev1.ObjectReference{
		Kind:       gvk.Kind,
		APIVersion: gvk.GroupVersion().String(),
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
	}
}

// mergeMap merges two maps into another one.
// NOTE: In case a key exists in both maps, the value in the first map is preserved.
func mergeMap(a, b map[string]string) map[string]string {
	m := make(map[string]string)
	for k, v := range b {
		m[k] = v
	}
	for k, v := range a {
		m[k] = v
	}
	return m
}
