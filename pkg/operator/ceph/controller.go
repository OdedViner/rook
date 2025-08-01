/*
Copyright 2021 The Rook Authors. All rights reserved.

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

package operator

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/discover"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"github.com/rook/rook/pkg/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	controllerName = "rook-ceph-operator-config-controller"
)

// ReconcileConfig reconciles a Ceph Operator config
type ReconcileConfig struct {
	client           client.Client
	context          *clusterd.Context
	config           opcontroller.OperatorConfig
	opManagerContext context.Context
}

// Add creates a new Operator configuration Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, context *clusterd.Context, opManagerContext context.Context, opConfig opcontroller.OperatorConfig) error {
	return add(opManagerContext, context, mgr, newReconciler(mgr, context, opManagerContext, opConfig))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, context *clusterd.Context, opManagerContext context.Context, config opcontroller.OperatorConfig) reconcile.Reconciler {
	return &ReconcileConfig{
		client:           mgr.GetClient(),
		context:          context,
		config:           config,
		opManagerContext: opManagerContext,
	}
}

func add(ctx context.Context, context *clusterd.Context, mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}
	logger.Infof("%s successfully started", controllerName)

	// Watch for ConfigMap (operator config)
	err = c.Watch(
		source.Kind(
			mgr.GetCache(),
			&v1.ConfigMap{TypeMeta: metav1.TypeMeta{Kind: "ConfigMap", APIVersion: v1.SchemeGroupVersion.String()}},
			&handler.TypedEnqueueRequestForObject[*v1.ConfigMap]{},
			operatorSettingConfigMapPredicate(),
		),
	)
	if err != nil {
		return err
	}

	return nil
}

// Reconcile reads that state of the cluster for a CephClient object and makes changes based on the state read
// and what is in the CephClient.Spec
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileConfig) Reconcile(context context.Context, request reconcile.Request) (reconcile.Result, error) {
	defer opcontroller.RecoverAndLogException()
	// workaround because the rook logging mechanism is not compatible with the controller-runtime logging interface
	reconcileResponse, err := r.reconcile(request)
	if err != nil {
		logger.Errorf("failed to reconcile %v", err)
	}

	return reconcileResponse, err
}

func (r *ReconcileConfig) reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the operator's configmap
	logger.Debugf("reconciling %s", request.NamespacedName)
	if request.Name == opcontroller.OperatorSettingConfigMapName {
		if err := k8sutil.ApplyOperatorSettingsConfigmap(r.opManagerContext, r.context.Clientset); err != nil {
			return opcontroller.ImmediateRetryResult, errors.Wrap(err, "failed to get operator setting configmap")
		}
	}
	// Reconcile Ceph CLI timeout, since the clusterd context is passed to by pointer to all CRD
	// controllers they will receive the update
	opcontroller.SetCephCommandsTimeout()

	// Reconcile Operator's logging level
	reconcileOperatorLogLevel()

	// Reconcile discovery daemon
	err := r.reconcileDiscoveryDaemon()
	if err != nil {
		return opcontroller.ImmediateRetryResult, err
	}

	opcontroller.SetAllowLoopDevices()
	opcontroller.SetEnforceHostNetwork()
	opcontroller.SetRevisionHistoryLimit()
	opcontroller.SetObcAllowAdditionalConfigFields()

	logger.Infof("%s done reconciling", controllerName)
	return reconcile.Result{}, nil
}

func reconcileOperatorLogLevel() {
	rookLogLevel := k8sutil.GetOperatorSetting("ROOK_LOG_LEVEL", util.DefaultLogLevel.String())
	util.SetGlobalLogLevel(rookLogLevel, logger)
}

func (r *ReconcileConfig) reconcileDiscoveryDaemon() error {
	rookDiscover := discover.New(r.context.Clientset)
	if opcontroller.DiscoveryDaemonEnabled() {
		if err := rookDiscover.Start(r.opManagerContext, r.config.OperatorNamespace, r.config.Image, r.config.ServiceAccount, true); err != nil {
			return errors.Wrap(err, "failed to start device discovery daemonset")
		}
	} else {
		if err := rookDiscover.Stop(r.opManagerContext, r.config.OperatorNamespace); err != nil {
			return errors.Wrap(err, "failed to stop device discovery daemonset")
		}
	}

	return nil
}
