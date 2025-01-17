/*
Copyright 2018 The Knative Authors

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

package route

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/knative/serving/pkg/apis/networking"
	netv1alpha1 "github.com/knative/serving/pkg/apis/networking/v1alpha1"
	"github.com/knative/serving/pkg/apis/serving/v1alpha1"
	"github.com/knative/serving/pkg/apis/serving/v1beta1"
	networkinglisters "github.com/knative/serving/pkg/client/listers/networking/v1alpha1"
	listers "github.com/knative/serving/pkg/client/listers/serving/v1alpha1"
	"github.com/knative/serving/pkg/reconciler"
	"github.com/knative/serving/pkg/reconciler/route/config"
	"github.com/knative/serving/pkg/reconciler/route/domains"
	"github.com/knative/serving/pkg/reconciler/route/resources"
	resourcenames "github.com/knative/serving/pkg/reconciler/route/resources/names"
	"github.com/knative/serving/pkg/reconciler/route/traffic"
	tr "github.com/knative/serving/pkg/reconciler/route/traffic"
	"knative.dev/pkg/apis"
	duckv1alpha1 "knative.dev/pkg/apis/duck/v1alpha1"
	duckv1beta1 "knative.dev/pkg/apis/duck/v1beta1"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/system"
	"knative.dev/pkg/tracker"
)

// routeFinalizer is the name that we put into the resource finalizer list, e.g.
//  metadata:
//    finalizers:
//    - routes.serving.knative.dev
var (
	routeResource  = v1alpha1.Resource("routes")
	routeFinalizer = routeResource.String()
)

// Reconciler implements controller.Reconciler for Route resources.
type Reconciler struct {
	*reconciler.Base

	// Listers index properties about resources
	routeLister          listers.RouteLister
	configurationLister  listers.ConfigurationLister
	revisionLister       listers.RevisionLister
	serviceLister        corev1listers.ServiceLister
	clusterIngressLister networkinglisters.ClusterIngressLister
	certificateLister    networkinglisters.CertificateLister
	configStore          reconciler.ConfigStore
	tracker              tracker.Interface

	clock system.Clock
}

// Check that our Reconciler implements controller.Reconciler
var _ controller.Reconciler = (*Reconciler)(nil)

// Reconcile compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Route resource
// with the current status of the resource.
func (c *Reconciler) Reconcile(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.Logger.Errorf("invalid resource key: %s", key)
		return nil
	}
	logger := logging.FromContext(ctx)

	ctx = c.configStore.ToContext(ctx)

	// Get the Route resource with this namespace/name.
	original, err := c.routeLister.Routes(namespace).Get(name)
	if apierrs.IsNotFound(err) {
		// The resource may no longer exist, in which case we stop processing.
		logger.Errorf("route %q in work queue no longer exists", key)
		return nil
	} else if err != nil {
		return err
	}
	// Don't modify the informers copy.
	route := original.DeepCopy()

	// Reconcile this copy of the route and then write back any status
	// updates regardless of whether the reconciliation errored out.
	reconcileErr := c.reconcile(ctx, route)
	if equality.Semantic.DeepEqual(original.Status, route.Status) {
		// If we didn't change anything then don't call updateStatus.
		// This is important because the copy we loaded from the informer's
		// cache may be stale and we don't want to overwrite a prior update
		// to status with this stale state.
	} else if _, err = c.updateStatus(route); err != nil {
		logger.Warnw("Failed to update route status", zap.Error(err))
		c.Recorder.Eventf(route, corev1.EventTypeWarning, "UpdateFailed",
			"Failed to update status for Route %q: %v", route.Name, err)
		return err
	}
	if reconcileErr != nil {
		c.Recorder.Event(route, corev1.EventTypeWarning, "InternalError", reconcileErr.Error())
		return reconcileErr
	}
	// TODO(mattmoor): Remove this after 0.7 cuts.
	// If the spec has changed, then assume we need an upgrade and issue a patch to trigger
	// the webhook to upgrade via defaulting.  Status updates do not trigger this due to the
	// use of the /status resource.
	if !equality.Semantic.DeepEqual(original.Spec, route.Spec) {
		routes := v1alpha1.SchemeGroupVersion.WithResource("routes")
		if err := c.MarkNeedsUpgrade(routes, route.Namespace, route.Name); err != nil {
			return err
		}
	}
	return nil
}

func ingressClassForRoute(ctx context.Context, r *v1alpha1.Route) string {
	if ingressClass := r.Annotations[networking.IngressClassAnnotationKey]; ingressClass != "" {
		return ingressClass
	}
	return config.FromContext(ctx).Network.DefaultClusterIngressClass
}

func (c *Reconciler) reconcile(ctx context.Context, r *v1alpha1.Route) error {
	logger := logging.FromContext(ctx)
	if r.GetDeletionTimestamp() != nil {
		// Check for a DeletionTimestamp.  If present, elide the normal reconcile logic.
		return c.reconcileDeletion(ctx, r)
	}

	// We may be reading a version of the object that was stored at an older version
	// and may not have had all of the assumed defaults specified.  This won't result
	// in this getting written back to the API Server, but lets downstream logic make
	// assumptions about defaulting.
	r.SetDefaults(v1beta1.WithUpgradeViaDefaulting(ctx))
	r.Status.InitializeConditions()

	if err := r.ConvertUp(ctx, &v1beta1.Route{}); err != nil {
		return err
	}

	logger.Infof("Reconciling route: %#v", r)

	// Update the information that makes us Addressable. This is needed to configure traffic and
	// make the cluster ingress.
	host, err := domains.DomainNameFromTemplate(ctx, r, r.Name)
	if err != nil {
		return err
	}

	r.Status.URL = &apis.URL{
		Scheme: "http",
		Host:   host,
	}
	// TODO(mattmoor): Remove completely after 0.7 cuts.
	r.Status.DeprecatedDomain = ""

	// Configure traffic based on the RouteSpec.
	traffic, err := c.configureTraffic(ctx, r)
	if traffic == nil || err != nil {
		// Traffic targets aren't ready, no need to configure child resources.
		return err
	}

	logger.Info("Updating targeted revisions.")
	// In all cases we will add annotations to the referred targets.  This is so that when they become
	// routable we can know (through a listener) and attempt traffic configuration again.
	if err := c.reconcileTargetRevisions(ctx, traffic, r); err != nil {
		return err
	}

	// TODO(mattmoor): Remove completely after 0.7 cuts.
	r.Status.DeprecatedDomainInternal = ""

	r.Status.Address = &duckv1alpha1.Addressable{
		Addressable: duckv1beta1.Addressable{
			URL: &apis.URL{
				Scheme: "http",
				Host:   resourcenames.K8sServiceFullname(r),
			},
		},
		// TODO(mattmoor): Remove completely after 0.7 cuts.
		Hostname: "",
	}

	// Add the finalizer before creating the ClusterIngress so that we can be sure it gets cleaned up.
	if err := c.ensureFinalizer(r); err != nil {
		return err
	}

	logger.Info("Creating placeholder k8s services")
	services, err := c.reconcilePlaceholderServices(ctx, r, traffic.Targets)
	if err != nil {
		return err
	}

	tls, err := c.tls(ctx, host, r, traffic)
	if err != nil {
		return err
	}

	logger.Info("Creating ClusterIngress.")
	desired, err := resources.MakeClusterIngress(ctx, r, traffic, tls, ingressClassForRoute(ctx, r))
	if err != nil {
		return err
	}
	clusterIngress, err := c.reconcileClusterIngress(ctx, r, desired)
	if err != nil {
		return err
	}
	r.Status.PropagateClusterIngressStatus(clusterIngress.Status)

	logger.Info("Updating placeholder k8s services with clusterIngress information")
	if err := c.updatePlaceholderServices(ctx, r, services, clusterIngress); err != nil {
		return err
	}

	r.Status.ObservedGeneration = r.Generation
	logger.Info("Route successfully synced")
	return nil
}

func (c *Reconciler) tls(ctx context.Context, host string, r *v1alpha1.Route, traffic *traffic.Config) ([]netv1alpha1.IngressTLS, error) {
	tls := []netv1alpha1.IngressTLS{}
	if !config.FromContext(ctx).Network.AutoTLS || resources.IsClusterLocal(r) {
		return tls, nil
	}
	allDomainTagMap, err := domains.GetAllDomainsAndTags(ctx, r, getTrafficNames(traffic.Targets))
	if err != nil {
		return nil, err
	}
	desiredCerts := resources.MakeCertificates(r, allDomainTagMap)
	for _, desiredCert := range desiredCerts {

		cert, err := c.reconcileCertificate(ctx, r, desiredCert)
		if err != nil {
			r.Status.MarkCertificateProvisionFailed(desiredCert.Name)
			return nil, err
		}

		dnsNames := sets.NewString(cert.Spec.DNSNames...)
		if cert.Status.IsReady() {
			r.Status.MarkCertificateReady(cert.Name)
			// r.Status.URL is for the major domain, so only change if the cert is for
			// the major domain
			if dnsNames.Has(host) {
				r.Status.URL.Scheme = "https"
			}
			// TODO: we should only mark https for the public visible targets when
			// we are able to configure visibility per target.
			setTargetsScheme(&r.Status, cert.Spec.DNSNames, "https")
		} else {
			r.Status.MarkCertificateNotReady(cert.Name)
			if dnsNames.Has(host) {
				r.Status.URL = &apis.URL{
					Scheme: "http",
					Host:   host,
				}
			}
			setTargetsScheme(&r.Status, cert.Spec.DNSNames, "http")
		}
		tls = append(tls, resources.MakeIngressTLS(cert, cert.Spec.DNSNames))
	}
	return tls, nil
}

func (c *Reconciler) reconcileDeletion(ctx context.Context, r *v1alpha1.Route) error {
	logger := logging.FromContext(ctx)

	// If our Finalizer is first, delete the ClusterIngress for this Route
	// and remove the finalizer.
	if len(r.Finalizers) == 0 || r.Finalizers[0] != routeFinalizer {
		return nil
	}

	// Delete the ClusterIngress resources for this Route.
	logger.Info("Cleaning up ClusterIngress")
	if err := c.deleteClusterIngressesForRoute(r); err != nil {
		return err
	}

	// Update the Route to remove the Finalizer.
	logger.Info("Removing Finalizer")
	r.Finalizers = r.Finalizers[1:]
	_, err := c.ServingClientSet.ServingV1alpha1().Routes(r.Namespace).Update(r)
	return err
}

// configureTraffic attempts to configure traffic based on the RouteSpec.  If there are missing
// targets (e.g. Configurations without a Ready Revision, or Revision that isn't Ready or Inactive),
// no traffic will be configured.
//
// If traffic is configured we update the RouteStatus with AllTrafficAssigned = True.  Otherwise we
// mark AllTrafficAssigned = False, with a message referring to one of the missing target.
func (c *Reconciler) configureTraffic(ctx context.Context, r *v1alpha1.Route) (*tr.Config, error) {
	logger := logging.FromContext(ctx)
	t, err := tr.BuildTrafficConfiguration(c.configurationLister, c.revisionLister, r)

	if t != nil {
		// Tell our trackers to reconcile Route whenever the things referred to by our
		// Traffic stanza change.
		for _, configuration := range t.Configurations {
			if err := c.tracker.Track(objectRef(configuration), r); err != nil {
				return nil, err
			}
		}
		for _, revision := range t.Revisions {
			if revision.Status.IsActivationRequired() {
				logger.Infof("Revision %s/%s is inactive", revision.Namespace, revision.Name)
			}
			if err := c.tracker.Track(objectRef(revision), r); err != nil {
				return nil, err
			}
		}
	}

	badTarget, isTargetError := err.(tr.TargetError)
	if err != nil && !isTargetError {
		// An error that's not due to missing traffic target should
		// make us fail fast.
		r.Status.MarkUnknownTrafficError(err.Error())
		return nil, err
	}
	if badTarget != nil && isTargetError {
		badTarget.MarkBadTrafficTarget(&r.Status)

		// Traffic targets aren't ready, no need to configure Route.
		return nil, nil
	}

	logger.Info("All referred targets are routable, marking AllTrafficAssigned with traffic information.")
	// Domain should already be present
	r.Status.Traffic, err = t.GetRevisionTrafficTargets(ctx, r)
	if err != nil {
		return nil, err
	}

	r.Status.MarkTrafficAssigned()

	return t, nil
}

func (c *Reconciler) ensureFinalizer(route *v1alpha1.Route) error {
	finalizers := sets.NewString(route.Finalizers...)
	if finalizers.Has(routeFinalizer) {
		return nil
	}
	mergePatch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers":      append(route.Finalizers, routeFinalizer),
			"resourceVersion": route.ResourceVersion,
		},
	}

	patch, err := json.Marshal(mergePatch)
	if err != nil {
		return err
	}

	_, err = c.ServingClientSet.ServingV1alpha1().Routes(route.Namespace).Patch(route.Name, types.MergePatchType, patch)
	return err
}

/////////////////////////////////////////
// Misc helpers.
/////////////////////////////////////////

type accessor interface {
	GetGroupVersionKind() schema.GroupVersionKind
	GetNamespace() string
	GetName() string
}

func objectRef(a accessor) corev1.ObjectReference {
	gvk := a.GetGroupVersionKind()
	apiVersion, kind := gvk.ToAPIVersionAndKind()
	return corev1.ObjectReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Namespace:  a.GetNamespace(),
		Name:       a.GetName(),
	}
}

func getTrafficNames(targets map[string]traffic.RevisionTargets) []string {
	names := []string{}
	for name := range targets {
		names = append(names, name)
	}
	return names
}

// Sets the traffic URL scheme to scheme if the URL matches the dnsNames.
// dnsNames are DNS names under a certificate for a particular domain, and so only change
// the corresponding traffic under the route, rather than all traffic
func setTargetsScheme(rs *v1alpha1.RouteStatus, dnsNames []string, scheme string) {
	for i := range rs.Traffic {
		if rs.Traffic[i].URL == nil {
			continue
		}
		for _, dnsName := range dnsNames {
			if rs.Traffic[i].URL.Host == dnsName {
				rs.Traffic[i].URL.Scheme = scheme
				break
			}
		}
	}
}
