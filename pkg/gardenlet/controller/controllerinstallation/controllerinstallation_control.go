// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllerinstallation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	gardencorev1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/utils"
	gutil "github.com/gardener/gardener/pkg/utils/gardener"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/managedresources"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	reconcilerName       = "controllerinstallation"
	installationTypeHelm = "helm"
)

func (c *Controller) controllerInstallationAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		c.log.Error(err, "Could not get key", "obj", obj)
		return
	}
	c.controllerInstallationQueue.Add(key)
}

func (c *Controller) controllerInstallationUpdate(oldObj, newObj interface{}) {
	oldCtrlInst, ok1 := oldObj.(*gardencorev1beta1.ControllerInstallation)
	newCtrlInst, ok2 := newObj.(*gardencorev1beta1.ControllerInstallation)
	if !ok1 || !ok2 {
		return
	}

	if newCtrlInst.DeletionTimestamp == nil &&
		reflect.DeepEqual(oldCtrlInst.Spec.DeploymentRef, newCtrlInst.Spec.DeploymentRef) &&
		oldCtrlInst.Spec.RegistrationRef.ResourceVersion == newCtrlInst.Spec.RegistrationRef.ResourceVersion &&
		oldCtrlInst.Spec.SeedRef.ResourceVersion == newCtrlInst.Spec.SeedRef.ResourceVersion {
		return
	}

	c.controllerInstallationAdd(newObj)
}

func (c *Controller) controllerInstallationDelete(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		c.log.Error(err, "Could not get key", "obj", obj)
		return
	}
	c.controllerInstallationQueue.Add(key)
}

func newReconciler(
	gardenClient client.Client,
	seedClientSet kubernetes.Interface,
	identity *gardencorev1beta1.Gardener,
	gardenNamespace *corev1.Namespace,
	gardenClusterIdentity string,
) reconcile.Reconciler {
	return &reconciler{
		gardenClient:          gardenClient,
		seedClientSet:         seedClientSet,
		identity:              identity,
		gardenNamespace:       gardenNamespace,
		gardenClusterIdentity: gardenClusterIdentity,
	}
}

type reconciler struct {
	gardenClient          client.Client
	seedClientSet         kubernetes.Interface
	identity              *gardencorev1beta1.Gardener
	gardenNamespace       *corev1.Namespace
	gardenClusterIdentity string
}

func (r *reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log := logf.FromContext(ctx)

	controllerInstallation := &gardencorev1beta1.ControllerInstallation{}
	if err := r.gardenClient.Get(ctx, request.NamespacedName, controllerInstallation); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("Object is gone, stop reconciling")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("error retrieving object from store: %w", err)
	}

	if isResponsible, err := r.isResponsible(ctx, controllerInstallation); !isResponsible || err != nil {
		return reconcile.Result{}, err
	}

	if controllerInstallation.DeletionTimestamp != nil {
		return r.delete(ctx, log, controllerInstallation)
	}
	return r.reconcile(ctx, log, controllerInstallation)
}

func (r *reconciler) reconcile(
	ctx context.Context,
	log logr.Logger,
	controllerInstallation *gardencorev1beta1.ControllerInstallation,
) (
	reconcile.Result,
	error,
) {
	if !controllerutil.ContainsFinalizer(controllerInstallation, finalizerName) {
		log.Info("Adding finalizer")
		if err := controllerutils.AddFinalizers(ctx, r.gardenClient, controllerInstallation, finalizerName); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	var (
		conditionValid     = gardencorev1beta1helper.GetOrInitCondition(controllerInstallation.Status.Conditions, gardencorev1beta1.ControllerInstallationValid)
		conditionInstalled = gardencorev1beta1helper.GetOrInitCondition(controllerInstallation.Status.Conditions, gardencorev1beta1.ControllerInstallationInstalled)
	)

	defer func() {
		if err := patchConditions(ctx, r.gardenClient, controllerInstallation, conditionValid, conditionInstalled); err != nil {
			log.Error(err, "Failed to patch conditions")
		}
	}()

	controllerRegistration := &gardencorev1beta1.ControllerRegistration{}
	if err := r.gardenClient.Get(ctx, client.ObjectKey{Name: controllerInstallation.Spec.RegistrationRef.Name}, controllerRegistration); err != nil {
		if apierrors.IsNotFound(err) {
			conditionValid = gardencorev1beta1helper.UpdatedCondition(conditionValid, gardencorev1beta1.ConditionFalse, "RegistrationNotFound", fmt.Sprintf("Referenced ControllerRegistration does not exist: %+v", err))
		} else {
			conditionValid = gardencorev1beta1helper.UpdatedCondition(conditionValid, gardencorev1beta1.ConditionUnknown, "RegistrationReadError", fmt.Sprintf("Referenced ControllerRegistration cannot be read: %+v", err))
		}
		return reconcile.Result{}, err
	}

	seed := &gardencorev1beta1.Seed{}
	if err := r.gardenClient.Get(ctx, client.ObjectKey{Name: controllerInstallation.Spec.SeedRef.Name}, seed); err != nil {
		if apierrors.IsNotFound(err) {
			conditionValid = gardencorev1beta1helper.UpdatedCondition(conditionValid, gardencorev1beta1.ConditionFalse, "SeedNotFound", fmt.Sprintf("Referenced Seed does not exist: %+v", err))
		} else {
			conditionValid = gardencorev1beta1helper.UpdatedCondition(conditionValid, gardencorev1beta1.ConditionUnknown, "SeedReadError", fmt.Sprintf("Referenced Seed cannot be read: %+v", err))
		}
		return reconcile.Result{}, err
	}

	var providerConfig *runtime.RawExtension
	if deploymentRef := controllerInstallation.Spec.DeploymentRef; deploymentRef != nil {
		controllerDeployment := &gardencorev1beta1.ControllerDeployment{}
		if err := r.gardenClient.Get(ctx, kutil.Key(deploymentRef.Name), controllerDeployment); err != nil {
			return reconcile.Result{}, err
		}
		providerConfig = &controllerDeployment.ProviderConfig
	}

	var helmDeployment HelmDeployment

	if err := json.Unmarshal(providerConfig.Raw, &helmDeployment); err != nil {
		conditionValid = gardencorev1beta1helper.UpdatedCondition(conditionValid, gardencorev1beta1.ConditionFalse, "ChartInformationInvalid", fmt.Sprintf("Chart Information cannot be unmarshalled: %+v", err))
		return reconcile.Result{}, err
	}

	namespace := getNamespaceForControllerInstallation(controllerInstallation)
	if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, r.seedClientSet.Client(), namespace, func() error {
		metav1.SetMetaDataLabel(&namespace.ObjectMeta, v1beta1constants.GardenRole, v1beta1constants.GardenRoleExtension)
		metav1.SetMetaDataLabel(&namespace.ObjectMeta, v1beta1constants.LabelControllerRegistrationName, controllerRegistration.Name)
		return nil
	}); err != nil {
		return reconcile.Result{}, err
	}

	var (
		volumeProvider  string
		volumeProviders []gardencorev1beta1.SeedVolumeProvider
	)

	if seed.Spec.Volume != nil {
		volumeProviders = seed.Spec.Volume.Providers
		if len(seed.Spec.Volume.Providers) > 0 {
			volumeProvider = seed.Spec.Volume.Providers[0].Name
		}
	}

	if seed.Status.ClusterIdentity == nil {
		return reconcile.Result{}, fmt.Errorf("cluster-identity of seed '%s' not set", seed.Name)
	}
	seedClusterIdentity := *seed.Status.ClusterIdentity

	ingressDomain := seed.Spec.DNS.IngressDomain
	if ingressDomain == nil {
		ingressDomain = &seed.Spec.Ingress.Domain
	}

	// Mix-in some standard values for garden and seed.
	gardenerValues := map[string]interface{}{
		"gardener": map[string]interface{}{
			"version": r.identity.Version,
			"garden": map[string]interface{}{
				"identity":        r.gardenNamespace.UID, // 'identity' value is deprecated to be replaced by 'clusterIdentity'. Should be removed in a future version.
				"clusterIdentity": r.gardenClusterIdentity,
			},
			"seed": map[string]interface{}{
				"identity":        seed.Name, // 'identity' value is deprecated to be replaced by 'clusterIdentity'. Should be removed in a future version.
				"clusterIdentity": seedClusterIdentity,
				"annotations":     seed.Annotations,
				"labels":          seed.Labels,
				"provider":        seed.Spec.Provider.Type,
				"region":          seed.Spec.Provider.Region,
				"volumeProvider":  volumeProvider,
				"volumeProviders": volumeProviders,
				"ingressDomain":   ingressDomain,
				"protected":       gardencorev1beta1helper.TaintsHave(seed.Spec.Taints, gardencorev1beta1.SeedTaintProtected),
				"visible":         seed.Spec.Settings.Scheduling.Visible,
				"taints":          seed.Spec.Taints,
				"networks":        seed.Spec.Networks,
				"blockCIDRs":      seed.Spec.Networks.BlockCIDRs,
				"spec":            seed.Spec,
			},
		},
	}

	release, err := r.seedClientSet.ChartRenderer().RenderArchive(helmDeployment.Chart, controllerRegistration.Name, namespace.Name, utils.MergeMaps(helmDeployment.Values, gardenerValues))
	if err != nil {
		conditionValid = gardencorev1beta1helper.UpdatedCondition(conditionValid, gardencorev1beta1.ConditionFalse, "ChartCannotBeRendered", fmt.Sprintf("Chart rendering process failed: %+v", err))
		return reconcile.Result{}, err
	}
	conditionValid = gardencorev1beta1helper.UpdatedCondition(conditionValid, gardencorev1beta1.ConditionTrue, "RegistrationValid", "Chart could be rendered successfully.")

	if err := managedresources.Create(ctx, r.seedClientSet.Client(), v1beta1constants.GardenNamespace, controllerInstallation.Name, false, v1beta1constants.SeedResourceManagerClass, release.AsSecretData(), nil, nil, nil); err != nil {
		conditionInstalled = gardencorev1beta1helper.UpdatedCondition(conditionInstalled, gardencorev1beta1.ConditionFalse, "InstallationFailed", fmt.Sprintf("Creation of ManagedResource %q failed: %+v", controllerInstallation.Name, err))
		return reconcile.Result{}, err
	}

	if conditionInstalled.Status == gardencorev1beta1.ConditionUnknown {
		// initially set condition to Pending
		// care controller will update condition based on 'ResourcesApplied' condition of ManagedResource
		conditionInstalled = gardencorev1beta1helper.UpdatedCondition(conditionInstalled, gardencorev1beta1.ConditionFalse, "InstallationPending", fmt.Sprintf("Installation of ManagedResource %q is still pending.", controllerInstallation.Name))
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) delete(
	ctx context.Context,
	log logr.Logger,
	controllerInstallation *gardencorev1beta1.ControllerInstallation,
) (
	reconcile.Result,
	error,
) {
	var (
		newConditions      = gardencorev1beta1helper.MergeConditions(controllerInstallation.Status.Conditions, gardencorev1beta1helper.InitCondition(gardencorev1beta1.ControllerInstallationValid), gardencorev1beta1helper.InitCondition(gardencorev1beta1.ControllerInstallationInstalled))
		conditionValid     = newConditions[0]
		conditionInstalled = newConditions[1]
	)

	defer func() {
		if err := patchConditions(ctx, r.gardenClient, controllerInstallation, conditionValid, conditionInstalled); client.IgnoreNotFound(err) != nil {
			log.Error(err, "Failed to patch conditions")
		}
	}()

	seed := &gardencorev1beta1.Seed{}
	if err := r.gardenClient.Get(ctx, client.ObjectKey{Name: controllerInstallation.Spec.SeedRef.Name}, seed); err != nil {
		if apierrors.IsNotFound(err) {
			conditionValid = gardencorev1beta1helper.UpdatedCondition(conditionValid, gardencorev1beta1.ConditionFalse, "SeedNotFound", fmt.Sprintf("Referenced Seed does not exist: %+v", err))
		} else {
			conditionValid = gardencorev1beta1helper.UpdatedCondition(conditionValid, gardencorev1beta1.ConditionUnknown, "SeedReadError", fmt.Sprintf("Referenced Seed cannot be read: %+v", err))
		}
		return reconcile.Result{}, err
	}

	mr := &resourcesv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controllerInstallation.Name,
			Namespace: v1beta1constants.GardenNamespace,
		},
	}
	if err := r.seedClientSet.Client().Delete(ctx, mr); err == nil {
		log.Info("Deletion of ManagedResource is still pending", "managedResource", client.ObjectKeyFromObject(mr))

		msg := fmt.Sprintf("Deletion of ManagedResource %q is still pending.", controllerInstallation.Name)
		conditionInstalled = gardencorev1beta1helper.UpdatedCondition(conditionInstalled, gardencorev1beta1.ConditionFalse, "DeletionPending", msg)
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	} else if !apierrors.IsNotFound(err) {
		conditionInstalled = gardencorev1beta1helper.UpdatedCondition(conditionInstalled, gardencorev1beta1.ConditionFalse, "DeletionFailed", fmt.Sprintf("Deletion of ManagedResource %q failed: %+v", controllerInstallation.Name, err))
		return reconcile.Result{}, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controllerInstallation.Name,
			Namespace: v1beta1constants.GardenNamespace,
		},
	}
	if err := r.seedClientSet.Client().Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
		conditionInstalled = gardencorev1beta1helper.UpdatedCondition(conditionInstalled, gardencorev1beta1.ConditionFalse, "DeletionFailed", fmt.Sprintf("Deletion of ManagedResource secret %q failed: %+v", controllerInstallation.Name, err))
	}

	namespace := getNamespaceForControllerInstallation(controllerInstallation)
	if err := r.seedClientSet.Client().Delete(ctx, namespace); err == nil || apierrors.IsConflict(err) {
		log.Info("Deletion of Namespace is still pending", "namespace", client.ObjectKeyFromObject(namespace))

		msg := fmt.Sprintf("Deletion of Namespace %q is still pending.", namespace.Name)
		conditionInstalled = gardencorev1beta1helper.UpdatedCondition(conditionInstalled, gardencorev1beta1.ConditionFalse, "DeletionPending", msg)
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	} else if !apierrors.IsNotFound(err) {
		conditionInstalled = gardencorev1beta1helper.UpdatedCondition(conditionInstalled, gardencorev1beta1.ConditionFalse, "DeletionFailed", fmt.Sprintf("Deletion of Namespace %q failed: %+v", namespace.Name, err))
		return reconcile.Result{}, err
	}

	conditionInstalled = gardencorev1beta1helper.UpdatedCondition(conditionInstalled, gardencorev1beta1.ConditionFalse, "DeletionSuccessful", "Deletion of old resources succeeded.")

	if controllerutil.ContainsFinalizer(controllerInstallation, finalizerName) {
		log.Info("Removing finalizer")
		if err := controllerutils.RemoveFinalizers(ctx, r.gardenClient, controllerInstallation, finalizerName); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
		}
	}

	return reconcile.Result{}, nil
}

func patchConditions(ctx context.Context, c client.StatusClient, controllerInstallation *gardencorev1beta1.ControllerInstallation, conditions ...gardencorev1beta1.Condition) error {
	patch := client.StrategicMergeFrom(controllerInstallation.DeepCopy())
	controllerInstallation.Status.Conditions = gardencorev1beta1helper.MergeConditions(controllerInstallation.Status.Conditions, conditions...)
	return c.Status().Patch(ctx, controllerInstallation, patch)
}

func (r *reconciler) isResponsible(ctx context.Context, controllerInstallation *gardencorev1beta1.ControllerInstallation) (bool, error) {
	// First check if a ControllerDeployment is used for the affected installation.
	if deploymentName := controllerInstallation.Spec.DeploymentRef; deploymentName != nil {
		controllerDeployment := &gardencorev1beta1.ControllerDeployment{}
		if err := r.gardenClient.Get(ctx, kutil.Key(deploymentName.Name), controllerDeployment); err != nil {
			return false, err
		}
		return controllerDeployment.Type == installationTypeHelm, nil
	}

	return false, nil
}

func getNamespaceForControllerInstallation(controllerInstallation *gardencorev1beta1.ControllerInstallation) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: gutil.NamespaceNameForControllerInstallation(controllerInstallation),
		},
	}
}
