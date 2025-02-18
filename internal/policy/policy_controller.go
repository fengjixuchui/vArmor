// Copyright 2021-2023 vArmor Authors
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

package policy

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	apicorev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	// informers "k8s.io/client-go/informers/core/v1"

	varmor "github.com/bytedance/vArmor/apis/varmor/v1beta1"
	varmorprofile "github.com/bytedance/vArmor/internal/profile"
	statusmanager "github.com/bytedance/vArmor/internal/status/api/v1"
	varmortypes "github.com/bytedance/vArmor/internal/types"
	varmorutils "github.com/bytedance/vArmor/internal/utils"
	varmorinterface "github.com/bytedance/vArmor/pkg/client/clientset/versioned/typed/varmor/v1beta1"
	varmorinformer "github.com/bytedance/vArmor/pkg/client/informers/externalversions/varmor/v1beta1"
	varmorlister "github.com/bytedance/vArmor/pkg/client/listers/varmor/v1beta1"
)

const (
	// maxRetries used for setting the retry times of sync failed
	maxRetries = 5
)

type PolicyController struct {
	podInterface          corev1.PodInterface
	appsInterface         appsv1.AppsV1Interface
	varmorInterface       varmorinterface.CrdV1beta1Interface
	vpInformer            varmorinformer.VarmorPolicyInformer
	vpLister              varmorlister.VarmorPolicyLister
	vpInformerSynced      cache.InformerSynced
	queue                 workqueue.RateLimitingInterface
	statusManager         *statusmanager.StatusManager
	restartExistWorkloads bool
	enableDefenseInDepth  bool
	bpfExclusiveMode      bool
	debug                 bool
	log                   logr.Logger
}

// NewPolicyController create a new PolicyController
func NewPolicyController(
	podInterface corev1.PodInterface,
	appsInterface appsv1.AppsV1Interface,
	varmorInterface varmorinterface.CrdV1beta1Interface,
	vpInformer varmorinformer.VarmorPolicyInformer,
	statusManager *statusmanager.StatusManager,
	restartExistWorkloads bool,
	enableDefenseInDepth bool,
	bpfExclusiveMode bool,
	debug bool,
	log logr.Logger) (*PolicyController, error) {

	c := PolicyController{
		podInterface:          podInterface,
		appsInterface:         appsInterface,
		varmorInterface:       varmorInterface,
		vpInformer:            vpInformer,
		vpLister:              vpInformer.Lister(),
		vpInformerSynced:      vpInformer.Informer().HasSynced,
		queue:                 workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "policy"),
		statusManager:         statusManager,
		restartExistWorkloads: restartExistWorkloads,
		enableDefenseInDepth:  enableDefenseInDepth,
		bpfExclusiveMode:      bpfExclusiveMode,
		debug:                 debug,
		log:                   log,
	}

	return &c, nil
}

func (c *PolicyController) enqueuePolicy(vp *varmor.VarmorPolicy, logger logr.Logger) {
	key, err := cache.MetaNamespaceKeyFunc(vp)
	if err != nil {
		logger.Error(err, "cache.MetaNamespaceKeyFunc()")
		return
	}
	c.queue.Add(key)
}

func (c *PolicyController) addVarmorPolicy(obj interface{}) {
	logger := c.log.WithName("AddFunc()")

	vp := obj.(*varmor.VarmorPolicy)

	logger.V(3).Info("enqueue VarmorPolicy")
	c.enqueuePolicy(vp, logger)
}

func (c *PolicyController) deleteVarmorPolicy(obj interface{}) {
	logger := c.log.WithName("DeleteFunc()")

	vp := obj.(*varmor.VarmorPolicy)

	logger.V(3).Info("enqueue VarmorPolicy")
	c.enqueuePolicy(vp, logger)
}

func (c *PolicyController) updateVarmorPolicy(oldObj, newObj interface{}) {
	logger := c.log.WithName("UpdateFunc()")

	oldVp := oldObj.(*varmor.VarmorPolicy)
	newVp := newObj.(*varmor.VarmorPolicy)

	if newVp.ResourceVersion == oldVp.ResourceVersion ||
		reflect.DeepEqual(newVp.Spec, oldVp.Spec) ||
		!reflect.DeepEqual(newVp.Status, oldVp.Status) {
		logger.V(3).Info("nothing need to be updated")
	} else {
		logger.V(3).Info("enqueue VarmorPolicy")
		c.enqueuePolicy(newVp, logger)
	}
}

func (c *PolicyController) handleDeleteVarmorPolicy(namespace, name string) error {
	logger := c.log.WithName("handleDeleteVarmorPolicy()")

	logger.Info("VarmorPolicy", "namespace", namespace, "name", name)

	apName := varmorprofile.GenerateArmorProfileName(namespace, name, false)

	logger.Info("retrieve ArmorProfile")
	ap, err := c.varmorInterface.ArmorProfiles(namespace).Get(context.Background(), apName, metav1.GetOptions{})
	if err != nil {
		if k8errors.IsNotFound(err) {
			return nil
		}
		logger.Error(err, "c.varmorInterface.ArmorProfiles().Get()")
		return err
	}

	logger.Info("delete ArmorProfile")
	err = c.varmorInterface.ArmorProfiles(namespace).Delete(context.Background(), apName, metav1.DeleteOptions{})
	if err != nil {
		logger.Error(err, "ArmorProfile().Delete()")
		return err
	}

	if c.restartExistWorkloads {
		// This will trigger the rolling upgrade of the target workload
		logger.Info("delete annotations of target workloads asynchronously")
		go varmorutils.UpdateWorkloadAnnotationsAndEnv(
			c.appsInterface,
			namespace,
			ap.Spec.Profile.Enforcer,
			ap.Spec.Target,
			"", "", false, logger)
	}

	// Cleanup the PolicyStatus and ModelingStatus of status manager for the deleted VarmorPolicy/ArmorProfile object
	logger.Info("cleanup the policy status (and if any modeling status) of statusmanager.policystatuses")
	policyStatusKey := namespace + "/" + name
	c.statusManager.DeleteCh <- policyStatusKey

	return nil
}

func (c *PolicyController) updateVarmorPolicyStatus(vp *varmor.VarmorPolicy, profileName string, resetReady bool, phase varmor.VarmorPolicyPhase, condType varmor.VarmorPolicyConditionType,
	status apicorev1.ConditionStatus, reason, message string) error {

	var exist bool = false

	if condType == varmortypes.VarmorPolicyUpdated {

		for i, c := range vp.Status.Conditions {
			if c.Type == varmortypes.VarmorPolicyUpdated {
				vp.Status.Conditions[i].Status = status
				vp.Status.Conditions[i].LastTransitionTime = metav1.Now()
				vp.Status.Conditions[i].Reason = reason
				vp.Status.Conditions[i].Message = message
				exist = true
				break
			}
		}
	}

	if !exist {
		condition := varmor.VarmorPolicyCondition{
			Type:               condType,
			Status:             status,
			LastTransitionTime: metav1.Now(),
			Reason:             reason,
			Message:            message,
		}
		vp.Status.Conditions = append(vp.Status.Conditions, condition)
	}

	if profileName != "" {
		vp.Status.ProfileName = profileName
	}
	if resetReady {
		vp.Status.Ready = false
	}
	if phase != varmortypes.VarmorPolicyUnchanged {
		vp.Status.Phase = phase
	}

	_, err := c.varmorInterface.VarmorPolicies(vp.Namespace).UpdateStatus(context.Background(), vp, metav1.UpdateOptions{})

	return err
}

func (c *PolicyController) resetArmorProfileModelStatus(namespace, name string, logger logr.Logger) error {
	apm, err := c.varmorInterface.ArmorProfileModels(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err == nil {
		apm.Status.CompletedNumber = 0
		apm.Status.Conditions = nil
		apm.Status.Ready = false
		_, err = c.varmorInterface.ArmorProfileModels(namespace).UpdateStatus(context.Background(), apm, metav1.UpdateOptions{})
		if err != nil {
			logger.Error(err, "resetArmorProfileModelStatus()")
		}
		return err
	}
	return nil
}

func (c *PolicyController) ignoreAdd(vp *varmor.VarmorPolicy, logger logr.Logger) bool {
	if vp.Spec.Target.Kind != "Deployment" && vp.Spec.Target.Kind != "StatefulSet" && vp.Spec.Target.Kind != "DaemonSet" && vp.Spec.Target.Kind != "Pod" {
		err := fmt.Errorf("Target.Kind is not supported")
		logger.Error(err, "update VarmorPolicy/status with forbidden info")
		err = c.updateVarmorPolicyStatus(vp, "", true, varmortypes.VarmorPolicyError, varmortypes.VarmorPolicyCreated, apicorev1.ConditionFalse,
			"Forbidden",
			"This kind of target is not supported.")
		if err != nil {
			logger.Error(err, "updateVarmorPolicyStatus()")
		}
		return true
	}

	if vp.Spec.Target.Name == "" && vp.Spec.Target.Selector == nil {
		err := fmt.Errorf("target.Name and target.Selector are empty")
		logger.Error(err, "update VarmorPolicy/status with forbidden info")
		err = c.updateVarmorPolicyStatus(vp, "", true, varmortypes.VarmorPolicyError, varmortypes.VarmorPolicyCreated, apicorev1.ConditionFalse,
			"Forbidden",
			"You should specify the target workload by name or selector.")
		if err != nil {
			logger.Error(err, "updateVarmorPolicyStatus()")
		}
		return true
	}

	if vp.Spec.Target.Name != "" && vp.Spec.Target.Selector != nil {
		err := fmt.Errorf("target.Name and target.Selector are exclusive")
		logger.Error(err, "update VarmorPolicy/status with forbidden info")
		err = c.updateVarmorPolicyStatus(vp, "", true, varmortypes.VarmorPolicyError, varmortypes.VarmorPolicyCreated, apicorev1.ConditionFalse,
			"Forbidden",
			"You shouldn't specify the target workload by both both name and selector.")
		if err != nil {
			logger.Error(err, "updateVarmorPolicyStatus()")
		}
		return true
	}

	if !c.enableDefenseInDepth && vp.Spec.Policy.Mode == varmortypes.DefenseInDepthMode {
		err := fmt.Errorf("the DefenseInDepth mode is not enabled")
		logger.Error(err, "update VarmorPolicy/status with forbidden info")
		err = c.updateVarmorPolicyStatus(vp, "", true, varmortypes.VarmorPolicyError, varmortypes.VarmorPolicyCreated, apicorev1.ConditionFalse,
			"Forbidden",
			"The DefenseInDepth feature is not enabled.")
		if err != nil {
			logger.Error(err, "updateVarmorPolicyStatus()")
		}
		return true
	}

	// Do not exceed the length of a standard Kubernetes name (63 characters)
	// Note: The advisory length of AppArmor profile name is 100 (See https://bugs.launchpad.net/apparmor/+bug/1499544).
	profileName := varmorprofile.GenerateArmorProfileName(vp.Namespace, vp.Name, false)
	if len(profileName) > 63 {
		err := fmt.Errorf("the length of ArmorProfile name is exceed 63. name: %s, length: %d", profileName, len(profileName))
		logger.Error(err, "update VarmorPolicy/status with forbidden info")
		msg := fmt.Sprintf("The length of VarmorProfile object name is too long, please limit it to %d bytes", 63-len(varmorprofile.ProfileNameTemplate)+4-len(vp.Namespace))
		err = c.updateVarmorPolicyStatus(vp, "", true, varmortypes.VarmorPolicyError, varmortypes.VarmorPolicyCreated, apicorev1.ConditionFalse,
			"Forbidden",
			msg)
		if err != nil {
			logger.Error(err, "updateVarmorPolicyStatus()")
		}
		return true
	}

	return false
}

func (c *PolicyController) handleAddVarmorPolicy(vp *varmor.VarmorPolicy) error {
	logger := c.log.WithName("handleAddVarmorPolicy()")

	logger.Info("VarmorPolicy created", "namespace", vp.Namespace, "name", vp.Name, "labels", vp.Labels, "target", vp.Spec.Target)

	if c.ignoreAdd(vp, logger) {
		return nil
	}

	ap, err := varmorprofile.NewArmorProfile(vp, false)
	if err != nil {
		logger.Error(err, "NewArmorProfile() failed")
		err = c.updateVarmorPolicyStatus(vp, "", true, varmortypes.VarmorPolicyError, varmortypes.VarmorPolicyCreated, apicorev1.ConditionFalse,
			"Error",
			err.Error())
		if err != nil {
			logger.Error(err, "updateVarmorPolicyStatus()")
			return err
		}
		return nil
	}

	logger.Info("update VarmorPolicy/status (created=true)")
	err = c.updateVarmorPolicyStatus(vp, ap.Spec.Profile.Name, true, varmortypes.VarmorPolicyPending, varmortypes.VarmorPolicyCreated, apicorev1.ConditionTrue, "", "")
	if err != nil {
		logger.Error(err, "updateVarmorPolicyStatus()")
		return err
	}

	if vp.Spec.Policy.Mode == varmortypes.DefenseInDepthMode {
		c.resetArmorProfileModelStatus(ap.Namespace, ap.Name, logger)
	}

	c.statusManager.UpdateDesiredNumber = true

	logger.Info("create ArmorProfile")
	ap, err = c.varmorInterface.ArmorProfiles(vp.Namespace).Create(context.Background(), ap, metav1.CreateOptions{})
	if err != nil {
		logger.Error(err, "ArmorProfile().Create()")
		return err
	}

	if c.restartExistWorkloads {
		// This will trigger the rolling upgrade of the target workload.
		logger.Info("add annotations to target workload asynchronously")
		go varmorutils.UpdateWorkloadAnnotationsAndEnv(
			c.appsInterface,
			vp.Namespace,
			vp.Spec.Policy.Enforcer,
			vp.Spec.Target,
			ap.Name,
			ap.Spec.BehaviorModeling.UniqueID,
			c.bpfExclusiveMode,
			logger)
	}

	return nil
}

func (c *PolicyController) ignoreUpdate(newVp *varmor.VarmorPolicy, oldAp *varmor.ArmorProfile, logger logr.Logger) (bool, error) {
	// Disallow modify the target of VarmorPolicy.
	if !reflect.DeepEqual(newVp.Spec.Target, oldAp.Spec.Target) {
		err := fmt.Errorf("modify spec.target is forbidden")
		logger.Error(err, "update VarmorPolicy/status with forbidden info")
		err = c.updateVarmorPolicyStatus(newVp, "", true, varmortypes.VarmorPolicyUnchanged, varmortypes.VarmorPolicyUpdated, apicorev1.ConditionFalse,
			"Forbidden",
			"Modify the target of VarmorPolicy is not allowed. You need to recreate the VarmorPolicy object.")
		return true, err
	}

	// Disallow switch mode from others to DefenseInDepth.
	if newVp.Spec.Policy.Mode == varmortypes.DefenseInDepthMode &&
		oldAp.Spec.BehaviorModeling.ModelingDuration == 0 {
		err := fmt.Errorf("disallow switch spec.policy.mode from others to DefenseInDepth")
		logger.Error(err, "update VarmorPolicy/status with forbidden info")
		err = c.updateVarmorPolicyStatus(newVp, "", true, varmortypes.VarmorPolicyUnchanged, varmortypes.VarmorPolicyUpdated, apicorev1.ConditionFalse,
			"Forbidden",
			"Switch the mode from others to DefenseInDepth is not allowed. You need to recreate the VarmorPolicy object.")
		return true, err
	}

	// Disallow switch mode from DefenseInDepth to others.
	if newVp.Spec.Policy.Mode != varmortypes.DefenseInDepthMode &&
		oldAp.Spec.BehaviorModeling.ModelingDuration != 0 {
		err := fmt.Errorf("disallow switch spec.policy.mode from DefenseInDepth to others")
		logger.Error(err, "update VarmorPolicy/status with forbidden info")
		err = c.updateVarmorPolicyStatus(newVp, "", true, varmortypes.VarmorPolicyUnchanged, varmortypes.VarmorPolicyUpdated, apicorev1.ConditionFalse,
			"Forbidden",
			"Switch the mode from DefenseInDepth to others is not allowed. You need to recreate the VarmorPolicy object.")
		return true, err
	}

	// Disallow modify the VarmorPolicy that run as DefenseInDepth mode and already completed.
	if newVp.Spec.Policy.Mode == varmortypes.DefenseInDepthMode &&
		(newVp.Status.Phase == varmortypes.VarmorPolicyCompleted || newVp.Status.Phase == varmortypes.VarmorPolicyProtecting) {
		err := fmt.Errorf("disallow modify the VarmorPolicy that run as DefenseInDepth mode and already completed")
		logger.Error(err, "update VarmorPolicy/status with forbidden info")
		err = c.updateVarmorPolicyStatus(newVp, "", false, varmortypes.VarmorPolicyUnchanged, varmortypes.VarmorPolicyUpdated, apicorev1.ConditionFalse,
			"Forbidden",
			"Modify the VarmorPolicy that run as DefenseInDepth mode and already completed is not allowed. You need to recreate the VarmorPolicy object.")
		return true, err
	}

	// Nothing need to be updated if VarmorPolicy is in the modeling phase and its duration is not changed.
	if newVp.Spec.Policy.Mode == varmortypes.DefenseInDepthMode &&
		newVp.Status.Phase == varmortypes.VarmorPolicyModeling &&
		newVp.Spec.Policy.DefenseInDepth.ModelingDuration == oldAp.Spec.BehaviorModeling.ModelingDuration {
		logger.Info("nothing need to be updated (duration is not changed)")
		return true, nil
	}

	// Disallow switch the enforcer.
	if newVp.Spec.Policy.Enforcer != oldAp.Spec.Profile.Enforcer {
		err := fmt.Errorf("disallow switch the enforcer")
		logger.Error(err, "update VarmorPolicy/status with forbidden info")
		err = c.updateVarmorPolicyStatus(newVp, "", true, varmortypes.VarmorPolicyUnchanged, varmortypes.VarmorPolicyUpdated, apicorev1.ConditionFalse,
			"Forbidden",
			"Switch the enforcer is not allowed. You need to recreate the VarmorPolicy object.")
		return true, err
	}

	return false, nil
}

func (c *PolicyController) handleUpdateVarmorPolicy(newVp *varmor.VarmorPolicy, oldAp *varmor.ArmorProfile) error {
	logger := c.log.WithName("handleUpdateVarmorPolicy()")

	logger.Info("VarmorPolicy updated", "namespace", newVp.Namespace, "name", newVp.Name, "labels", newVp.Labels, "target", newVp.Spec.Target)

	if ignore, err := c.ignoreUpdate(newVp, oldAp, logger); ignore {
		if err != nil {
			logger.Error(err, "ignoreUpdate()")
		}
		return err
	}

	// First, reset VarmorPolicy/status
	logger.Info("1. reset VarmorPolicy/status (updated=true)", "namesapce", newVp.Namespace, "name", newVp.Name)
	err := c.updateVarmorPolicyStatus(newVp, "", true, varmortypes.VarmorPolicyPending, varmortypes.VarmorPolicyUpdated, apicorev1.ConditionTrue, "", "")
	if err != nil {
		logger.Error(err, "updateVarmorPolicyStatus()")
		return err
	}

	// Second, build a new ArmorProfileSpec
	newApSpec := oldAp.Spec.DeepCopy()
	newProfile, err := varmorprofile.GenerateProfile(newVp.Spec.Policy, oldAp.Spec.Profile.Name, false, "")
	if err != nil {
		logger.Error(err, "GenerateProfile() failed")
		err = c.updateVarmorPolicyStatus(newVp, "", true, varmortypes.VarmorPolicyError, varmortypes.VarmorPolicyCreated, apicorev1.ConditionFalse,
			"Error",
			err.Error())
		if err != nil {
			logger.Error(err, "updateVarmorPolicyStatus()")
			return err
		}
		return nil
	}
	newApSpec.Profile = *newProfile
	if newVp.Spec.Policy.Mode == varmortypes.DefenseInDepthMode {
		newApSpec.BehaviorModeling.ModelingDuration = newVp.Spec.Policy.DefenseInDepth.ModelingDuration
	}

	// Last, do update
	statusKey := newVp.Namespace + "/" + newVp.Name
	c.statusManager.UpdateDesiredNumber = true
	if !reflect.DeepEqual(oldAp.Spec, *newApSpec) {
		// Update object
		logger.Info("2. update the object and its status")

		logger.Info("2.1. reset ArmorProfile/status and ArmorProfileModel/Status", "namespace", oldAp.Namespace, "name", oldAp.Name)
		oldAp.Status.CurrentNumberLoaded = 0
		oldAp.Status.Conditions = nil
		oldAp, err = c.varmorInterface.ArmorProfiles(newVp.Namespace).UpdateStatus(context.Background(), oldAp, metav1.UpdateOptions{})
		if err != nil {
			logger.Error(err, "ArmorProfile().UpdateStatus()")
			return err
		}

		if newVp.Spec.Policy.Mode == varmortypes.DefenseInDepthMode {
			c.resetArmorProfileModelStatus(newVp.Namespace, oldAp.Name, logger)
		}

		logger.Info("2.2. reset the status cache", "status key", statusKey)
		c.statusManager.ResetCh <- statusKey

		logger.Info("2.3. update ArmorProfile")
		oldAp.Spec = *newApSpec
		_, err = c.varmorInterface.ArmorProfiles(newVp.Namespace).Update(context.Background(), oldAp, metav1.UpdateOptions{})
		if err != nil {
			logger.Error(err, "ArmorProfile().Update()")
			return err
		}
	} else {
		// Update status
		logger.Info("2. update the object' status")

		logger.Info("2.1. update VarmorPolicy/status and ArmorProfile/status", "status key", statusKey)
		c.statusManager.UpdateStatusCh <- statusKey
	}
	return nil
}

func (c *PolicyController) syncPolicy(key string) error {
	logger := c.log.WithName("syncPolicy()")

	startTime := time.Now()
	logger.V(3).Info("started syncing policy", "key", key, "startTime", startTime)
	defer func() {
		logger.V(3).Info("finished syncing policy", "key", key, "processingTime", time.Since(startTime).String())
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.Error(err, "cache.SplitMetaNamespaceKey()")
		return err
	}

	vp, err := c.varmorInterface.VarmorPolicies(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		if k8errors.IsNotFound(err) {
			// VarmorPolicy delete event
			logger.V(3).Info("processing VarmorPolicy delete event")
			return c.handleDeleteVarmorPolicy(namespace, name)
		} else {
			logger.Error(err, "c.varmorInterface.VarmorPolicies().Get()")
			return err
		}
	}

	apName := varmorprofile.GenerateArmorProfileName(vp.Namespace, vp.Name, false)
	ap, err := c.varmorInterface.ArmorProfiles(vp.Namespace).Get(context.Background(), apName, metav1.GetOptions{})
	if err != nil {
		if k8errors.IsNotFound(err) {
			// VarmorPolicy create event
			logger.V(3).Info("processing VarmorPolicy create event")
			return c.handleAddVarmorPolicy(vp)
		} else {
			logger.Error(err, "c.varmorInterface.ArmorProfiles().Get()")
			return err
		}
	} else {
		// VarmorPolicy update event
		logger.V(3).Info("processing VarmorPolicy update event")
		return c.handleUpdateVarmorPolicy(vp, ap)
	}
}

func (c *PolicyController) handleErr(err error, key interface{}) {
	logger := c.log
	if err == nil {
		c.queue.Forget(key)
		return
	}

	if c.queue.NumRequeues(key) < maxRetries {
		logger.Error(err, "failed to sync policy", "key", key)
		c.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	logger.V(3).Info("dropping policy out of queue", "key", key)
	c.queue.Forget(key)
}

func (c *PolicyController) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)
	err := c.syncPolicy(key.(string))
	c.handleErr(err, key)

	return true
}

func (c *PolicyController) worker() {
	for c.processNextWorkItem() {
	}
}

// Run begins watching and syncing.
func (c *PolicyController) Run(workers int, stopCh <-chan struct{}) {
	logger := c.log
	logger.Info("starting")

	defer utilruntime.HandleCrash()

	if !cache.WaitForCacheSync(stopCh, c.vpInformerSynced) {
		logger.Error(fmt.Errorf("failed to sync informer cache"), "cache.WaitForCacheSync()")
		return
	}

	c.vpInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addVarmorPolicy,
		UpdateFunc: c.updateVarmorPolicy,
		DeleteFunc: c.deleteVarmorPolicy,
	})

	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (c *PolicyController) CleanUp() {
	c.log.Info("cleaning up")
	c.queue.ShutDown()
}
