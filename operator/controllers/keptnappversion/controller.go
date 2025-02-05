/*
Copyright 2022.

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

package keptnappversion

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/types"
	"time"

	"github.com/keptn/lifecycle-controller/operator/api/v1alpha1/semconv"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/go-logr/logr"
	"github.com/keptn/lifecycle-controller/operator/api/v1alpha1/common"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	klcv1alpha1 "github.com/keptn/lifecycle-controller/operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KeptnAppVersionReconciler reconciles a KeptnAppVersion object
type KeptnAppVersionReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Log         logr.Logger
	Recorder    record.EventRecorder
	Tracer      trace.Tracer
	Meters      common.KeptnMeters
	bindCRDSpan map[string]trace.Span
}

//+kubebuilder:rbac:groups=lifecycle.keptn.sh,resources=keptnappversions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lifecycle.keptn.sh,resources=keptnappversions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lifecycle.keptn.sh,resources=keptnappversions/finalizers,verbs=update
//+kubebuilder:rbac:groups=lifecycle.keptn.sh,resources=keptnworkloadinstances/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KeptnAppVersion object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *KeptnAppVersionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	r.Log.Info("Searching for Keptn App Version")

	appVersion := &klcv1alpha1.KeptnAppVersion{}
	err := r.Get(ctx, req.NamespacedName, appVersion)
	if errors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}

	if err != nil {
		r.Log.Error(err, "App Version not found")
		return reconcile.Result{}, fmt.Errorf("could not fetch KeptnappVersion: %+v", err)
	}

	appVersion.SetStartTime()

	traceContextCarrier := propagation.MapCarrier(appVersion.Annotations)
	ctx = otel.GetTextMapPropagator().Extract(ctx, traceContextCarrier)

	appTraceContextCarrier := propagation.MapCarrier(appVersion.Spec.TraceId)
	ctxAppTrace := otel.GetTextMapPropagator().Extract(context.TODO(), appTraceContextCarrier)

	ctx, span := r.Tracer.Start(ctx, "reconcile_app_version", trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()

	semconv.AddAttributeFromAppVersion(span, *appVersion)

	phase := common.PhaseAppPreDeployment

	if appVersion.Status.CurrentPhase == "" {
		r.unbindSpan(appVersion, phase.ShortName)
		var spanAppTrace trace.Span
		ctxAppTrace, spanAppTrace = r.getSpan(ctxAppTrace, appVersion, phase.ShortName)

		semconv.AddAttributeFromAppVersion(spanAppTrace, *appVersion)
		spanAppTrace.AddEvent("App Version Pre-Deployment Tasks started", trace.WithTimestamp(time.Now()))
		r.recordEvent(phase, "Normal", appVersion, "Started", "have started")
	}

	if !appVersion.IsPreDeploymentSucceeded() {
		reconcilePreDep := func() (common.KeptnState, error) {
			return r.reconcilePrePostDeployment(ctx, appVersion, common.PreDeploymentCheckType)
		}
		return r.handlePhase(ctx, ctxAppTrace, appVersion, phase, span, appVersion.IsPreDeploymentFailed, reconcilePreDep)
	}

	phase = common.PhaseAppPreEvaluation
	if !appVersion.IsPreDeploymentEvaluationSucceeded() {
		reconcilePreEval := func() (common.KeptnState, error) {
			return r.reconcilePrePostEvaluation(ctx, appVersion, common.PreDeploymentEvaluationCheckType)
		}
		return r.handlePhase(ctx, ctxAppTrace, appVersion, phase, span, appVersion.IsPreDeploymentEvaluationFailed, reconcilePreEval)
	}

	phase = common.PhaseAppDeployment
	if !appVersion.AreWorkloadsSucceeded() {
		reconcileAppDep := func() (common.KeptnState, error) {
			return r.reconcileWorkloads(ctx, appVersion)
		}
		return r.handlePhase(ctx, ctxAppTrace, appVersion, phase, span, appVersion.AreWorkloadsFailed, reconcileAppDep)

	}

	phase = common.PhaseAppPostDeployment
	if !appVersion.IsPostDeploymentSucceeded() {
		reconcilePostDep := func() (common.KeptnState, error) {
			return r.reconcilePrePostDeployment(ctx, appVersion, common.PostDeploymentCheckType)
		}
		return r.handlePhase(ctx, ctxAppTrace, appVersion, phase, span, appVersion.IsPostDeploymentFailed, reconcilePostDep)
	}

	phase = common.PhaseAppPostEvaluation
	if !appVersion.IsPostDeploymentEvaluationCompleted() {
		reconcilePostEval := func() (common.KeptnState, error) {
			return r.reconcilePrePostEvaluation(ctx, appVersion, common.PostDeploymentEvaluationCheckType)
		}
		return r.handlePhase(ctx, ctxAppTrace, appVersion, phase, span, appVersion.IsPostDeploymentEvaluationFailed, reconcilePostEval)
	}

	r.recordEvent(phase, "Normal", appVersion, "Finished", "is finished")
	err = r.Client.Status().Update(ctx, appVersion)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return ctrl.Result{Requeue: true}, err
	}

	// AppVersion is completed at this place

	if !appVersion.IsEndTimeSet() {
		appVersion.Status.CurrentPhase = common.PhaseCompleted.ShortName
		appVersion.SetEndTime()
	}

	err = r.Client.Status().Update(ctx, appVersion)
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	attrs := appVersion.GetMetricsAttributes()

	r.Log.Info("Increasing app count")

	// metrics: increment app counter
	r.Meters.AppCount.Add(ctx, 1, attrs...)

	// metrics: add app duration
	duration := appVersion.Status.EndTime.Time.Sub(appVersion.Status.StartTime.Time)
	r.Meters.AppDuration.Record(ctx, duration.Seconds(), attrs...)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KeptnAppVersionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&klcv1alpha1.KeptnAppVersion{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

func (r *KeptnAppVersionReconciler) recordEvent(phase common.KeptnPhaseType, eventType string, appVersion *klcv1alpha1.KeptnAppVersion, shortReason string, longReason string) {
	r.Recorder.Event(appVersion, eventType, fmt.Sprintf("%s%s", phase.ShortName, shortReason), fmt.Sprintf("%s %s / Namespace: %s, Name: %s, Version: %s ", phase.LongName, longReason, appVersion.Namespace, appVersion.Name, appVersion.Spec.Version))
}

func (r *KeptnAppVersionReconciler) handlePhase(ctx context.Context, ctxAppTrace context.Context, appVersion *klcv1alpha1.KeptnAppVersion, phase common.KeptnPhaseType, span trace.Span, phaseFailed func() bool, reconcilePhase func() (common.KeptnState, error)) (ctrl.Result, error) {

	oldStatus := appVersion.Status.Status
	newStatus := oldStatus
	statusUpdated := false

	r.Log.Info(phase.LongName + " not finished")
	ctxAppTrace, spanAppTrace := r.getSpan(ctxAppTrace, appVersion, phase.ShortName)

	oldPhase := appVersion.Status.CurrentPhase
	appVersion.Status.CurrentPhase = phase.ShortName
	if phaseFailed() { //TODO eventually we should decide whether a task returns FAILED, currently we never have this status set
		r.recordEvent(phase, "Warning", appVersion, "Failed", "has failed")
		return ctrl.Result{Requeue: true, RequeueAfter: 60 * time.Second}, nil
	}
	state, err := reconcilePhase()
	if err != nil {
		spanAppTrace.AddEvent(phase.LongName + " could not get reconciled")
		r.recordEvent(phase, "Warning", appVersion, "ReconcileErrored", "could not get reconciled")
		span.SetStatus(codes.Error, err.Error())
		return ctrl.Result{Requeue: true}, err
	}
	if state.IsSucceeded() {
		newStatus = common.StateSucceeded
		spanAppTrace.AddEvent(phase.LongName + " has succeeded")
		spanAppTrace.SetStatus(codes.Ok, "Succeeded")
		spanAppTrace.End()
		r.unbindSpan(appVersion, phase.ShortName)
		r.recordEvent(phase, "Normal", appVersion, "Succeeded", "has succeeded")
	} else if state.IsFailed() {

		appVersion.SetEndTime()
		attrs := appVersion.GetMetricsAttributes()
		r.Meters.AppCount.Add(ctx, 1, attrs...)

		newStatus = common.StateFailed

		spanAppTrace.AddEvent(phase.LongName + " has failed")
		spanAppTrace.SetStatus(codes.Error, "Failed")
		spanAppTrace.End()
		r.unbindSpan(appVersion, phase.ShortName)

		r.recordEvent(phase, "Warning", appVersion, "Failed", "has failed")
	} else {
		newStatus = common.StateProgressing
		r.recordEvent(phase, "Warning", appVersion, "NotFinished", "has not finished")
	}

	// check if status changed
	if oldPhase != appVersion.Status.CurrentPhase {
		ctx, spanAppTrace = r.getSpan(ctxAppTrace, appVersion, appVersion.Status.CurrentPhase)
		semconv.AddAttributeFromAppVersion(spanAppTrace, *appVersion)
		statusUpdated = true
	}
	if oldStatus != newStatus {
		appVersion.Status.Status = newStatus
		statusUpdated = true
	}

	if statusUpdated {
		if err := r.Status().Update(ctx, appVersion); err != nil {
			r.Log.Error(err, "could not update status")
		}
	}
	return ctrl.Result{Requeue: true, RequeueAfter: 5 * time.Second}, nil
}

func (r *KeptnAppVersionReconciler) getSpanName(appv *klcv1alpha1.KeptnAppVersion, phase string) string {
	return fmt.Sprintf("%s.%s.%s.%s", appv.Spec.TraceId, appv.Spec.AppName, appv.Spec.Version, phase)
}

func (r *KeptnAppVersionReconciler) getSpan(ctx context.Context, appv *klcv1alpha1.KeptnAppVersion, phase string) (context.Context, trace.Span) {
	appvName := r.getSpanName(appv, phase)
	if r.bindCRDSpan == nil {
		r.bindCRDSpan = make(map[string]trace.Span)
	}
	if span, ok := r.bindCRDSpan[appvName]; ok {
		return ctx, span
	}
	ctx, span := r.Tracer.Start(ctx, phase, trace.WithSpanKind(trace.SpanKindConsumer))
	r.Log.Info("DEBUG: Created span " + appvName)
	r.bindCRDSpan[appvName] = span
	return ctx, span
}

func (r *KeptnAppVersionReconciler) unbindSpan(appv *klcv1alpha1.KeptnAppVersion, phase string) {
	delete(r.bindCRDSpan, r.getSpanName(appv, phase))
}

func (r *KeptnAppVersionReconciler) GetActiveApps(ctx context.Context) ([]common.GaugeValue, error) {
	appInstances := &klcv1alpha1.KeptnAppVersionList{}
	err := r.List(ctx, appInstances)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve app versions: %w", err)
	}

	res := []common.GaugeValue{}

	for _, appInstance := range appInstances.Items {
		gaugeValue := int64(0)
		if !appInstance.IsEndTimeSet() {
			gaugeValue = int64(1)
		}
		res = append(res, common.GaugeValue{
			Value:      gaugeValue,
			Attributes: appInstance.GetActiveMetricsAttributes(),
		})
	}

	return res, nil
}

func (r *KeptnAppVersionReconciler) GetDeploymentInterval(ctx context.Context) ([]common.GaugeFloatValue, error) {
	appInstances := &klcv1alpha1.KeptnAppVersionList{}
	err := r.List(ctx, appInstances)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve app versions: %w", err)
	}

	res := []common.GaugeFloatValue{}

	for _, appInstance := range appInstances.Items {

		if appInstance.Spec.PreviousVersion != "" {
			previousAppVersion := &klcv1alpha1.KeptnAppVersion{}
			appName := fmt.Sprintf("%s-%s", appInstance.Spec.AppName, appInstance.Spec.PreviousVersion)
			err := r.Get(ctx, types.NamespacedName{Name: appName, Namespace: appInstance.Namespace}, previousAppVersion)
			if err != nil {
				r.Log.Error(err, "Previous App Version not found")
			} else {
				previousInterval := appInstance.Status.StartTime.Time.Sub(previousAppVersion.Status.EndTime.Time)
				res = append(res, common.GaugeFloatValue{
					Value:      previousInterval.Seconds(),
					Attributes: appInstance.GetDurationMetricsAttributes(),
				})
			}
		}
	}

	return res, nil
}

func (r *KeptnAppVersionReconciler) GetDeploymentDuration(ctx context.Context) ([]common.GaugeFloatValue, error) {
	appInstances := &klcv1alpha1.KeptnAppVersionList{}
	err := r.List(ctx, appInstances)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve app versions: %w", err)
	}

	res := []common.GaugeFloatValue{}

	for _, appInstance := range appInstances.Items {
		if appInstance.IsEndTimeSet() {
			duration := appInstance.Status.EndTime.Time.Sub(appInstance.Status.StartTime.Time)
			res = append(res, common.GaugeFloatValue{
				Value:      duration.Seconds(),
				Attributes: appInstance.GetDurationMetricsAttributes(),
			})
		}
	}

	return res, nil
}
