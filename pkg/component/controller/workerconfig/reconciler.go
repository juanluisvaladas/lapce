/*
Copyright 2022 k0s authors

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

package workerconfig

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/k0sproject/k0s/pkg/apis/k0s.k0sproject.io/v1beta1"
	"github.com/k0sproject/k0s/pkg/applier"
	"github.com/k0sproject/k0s/pkg/component/controller/leaderelector"
	"github.com/k0sproject/k0s/pkg/component/manager"
	workerconfig "github.com/k0sproject/k0s/pkg/component/worker/config"
	"github.com/k0sproject/k0s/pkg/constant"
	kubeutil "github.com/k0sproject/k0s/pkg/kubernetes"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubeletv1beta1 "k8s.io/kubelet/config/v1beta1"
	"k8s.io/utils/pointer"

	"github.com/sirupsen/logrus"
	"golang.org/x/exp/slices"
	"sigs.k8s.io/yaml"
)

type resources = []*unstructured.Unstructured

// Reconciler maintains ConfigMaps that hold configuration to be
// used on k0s worker nodes, depending on their selected worker profile.
type Reconciler struct {
	log logrus.FieldLogger

	clusterDomain string
	clusterDNSIP  net.IP
	clientFactory kubeutil.ClientFactoryInterface
	leaderElector leaderelector.Interface

	mu    sync.Mutex
	state reconcilerState

	// valid when initialized
	apply func(context.Context, resources) error

	// valid when started
	updates     chan<- updateFunc
	requestStop func()
	stopped     <-chan struct{}
}

var (
	_ manager.Component  = (*Reconciler)(nil)
	_ manager.Reconciler = (*Reconciler)(nil)
)

type reconcilerState string

var (
	reconcilerCreated     reconcilerState = "created"
	reconcilerInitialized reconcilerState = "initialized"
	reconcilerStarted     reconcilerState = "started"
	reconcilerStopped     reconcilerState = "stopped"
)

// NewReconciler creates a new reconciler for worker configurations.
func NewReconciler(k0sVars constant.CfgVars, nodeSpec *v1beta1.ClusterSpec, clientFactory kubeutil.ClientFactoryInterface, leaderElector leaderelector.Interface) (*Reconciler, error) {
	log := logrus.WithFields(logrus.Fields{"component": "workerconfig.Reconciler"})

	clusterDNSIPString, err := nodeSpec.Network.DNSAddress()
	if err != nil {
		return nil, err
	}
	clusterDNSIP := net.ParseIP(clusterDNSIPString)
	if clusterDNSIP == nil {
		return nil, fmt.Errorf("not an IP address: %q", clusterDNSIPString)
	}

	reconciler := &Reconciler{
		log: log,

		clusterDomain: nodeSpec.Network.ClusterDomain,
		clusterDNSIP:  clusterDNSIP,
		clientFactory: clientFactory,
		leaderElector: leaderElector,

		state: reconcilerCreated,
	}

	return reconciler, nil
}

// Init implements [manager.Component].
func (r *Reconciler) Init(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != reconcilerCreated {
		return fmt.Errorf("cannot initialize, not created: %s", r.state)
	}

	clientFactory := r.clientFactory
	apply := func(ctx context.Context, resources resources) error {
		dynamicClient, err := clientFactory.GetDynamicClient()
		if err != nil {
			return err
		}
		discoveryClient, err := clientFactory.GetDiscoveryClient()
		if err != nil {
			return err
		}

		return (&applier.Stack{
			Name:      "k0s-" + constant.WorkerConfigComponentName,
			Client:    dynamicClient,
			Discovery: discoveryClient,
			Resources: resources,
		}).Apply(ctx, true)
	}

	r.apply = apply
	r.state = reconcilerInitialized

	return nil
}

type updateFunc = func(*snapshot) chan<- error

// Start implements [manager.Component].
func (r *Reconciler) Start(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != reconcilerInitialized {
		return fmt.Errorf("cannot start, not initialized: %s", r.state)
	}

	// Setup the updates channel. Updates may be sent via the reconcile()
	// method. The reconciliation goroutine will pick them up for processing.
	updates := make(chan updateFunc, 1)

	// Setup the reconciliation goroutine. It will read the state changes from
	// the update channel and apply those to the desired state. Changes will be
	// applied whenever the last reconciled state differs from the desired
	// state.
	reconcilerCtx, cancelReconciler := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	apply := r.apply
	go func() {
		defer close(stopped)
		defer r.log.Info("Reconciliation loop done")
		r.log.Info("Starting reconciliation loop")
		r.runReconcileLoop(reconcilerCtx, updates, apply)
	}()

	// React to leader elector changes. Enforce a reconciliation whenever the
	// lease is acquired.
	r.leaderElector.AddAcquiredLeaseCallback(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
			defer cancel()

			err := reconcile(ctx, updates, stopped, func(s *snapshot) {
				s.serial++
			})

			// Log any reconciliation errors, but only if they don't indicate
			// that the reconciler has been stopped concurrently.
			if err != nil && !errors.Is(err, errStoppedConcurrently) {
				r.log.WithError(err).Error("Failed to reconcile after having acquired the leader lease")
			}
		}()
	})

	// Store the started state
	r.apply = nil
	r.updates = updates
	r.requestStop = cancelReconciler
	r.stopped = stopped
	r.state = reconcilerStarted

	return nil
}

// runReconcileLoop executes the main reconciliation loop. The loop will exit
// when the context is done.
//
// The loop works as follows:
//   - Receive any updates from the channel
//   - Apply the updates to the desired state
//   - Compare the desired state to the last reconciled state
//   - Reconcile the desired state
//   - Send back the outcome of the reconciliation to the channel provided by
//     the update function
//
// Reconciliation may be skipped if:
//   - The desired state hasn't been fully collected yet
//   - The leader lease isn't acquired
//   - The last applied state is identical to the desired state
//
// Any failed reconciliations will be retried roughly every minute, until they
// succeed.
func (r *Reconciler) runReconcileLoop(ctx context.Context, updates <-chan updateFunc, apply func(context.Context, resources) error) {
	var desiredState, reconciledState snapshot

	runReconciliation := func() error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w while processing reconciliation", errStoppedConcurrently)
		}

		if !r.leaderElector.IsLeader() {
			r.log.Debug("Skipping reconciliation, not the leader")
			return nil
		}

		if desiredState.configSnapshot == nil {
			r.log.Debug("Skipping reconciliation, snapshot not yet complete")
			return nil
		}

		if reflect.DeepEqual(&reconciledState, &desiredState) {
			r.log.Debug("Skipping reconciliation, nothing changed")
			return nil
		}

		stateToReconcile := desiredState.DeepCopy()
		resources, err := r.generateResources(stateToReconcile)
		if err != nil {
			return fmt.Errorf("failed to generate resources for worker configuration: %w", err)
		}

		r.log.Debug("Updating worker configuration ...")

		err = apply(ctx, resources)
		if err != nil {
			return fmt.Errorf("failed to apply resources for worker configuration: %w", err)
		}

		stateToReconcile.DeepCopyInto(&reconciledState)

		r.log.Info("Worker configuration updated")
		return nil
	}

	retryTicker := time.NewTicker(60 * time.Second)
	defer retryTicker.Stop()

	var lastRecoFailed bool

	for {
		select {
		case update := <-updates:
			done := update(&desiredState)
			func() {
				defer close(done)
				err := runReconciliation()
				done <- err
				lastRecoFailed = err != nil
			}()

		case <-ctx.Done():
			return // stop requested

		case <-retryTicker.C: // Retry failed reconciliations every minute
			if lastRecoFailed {
				if err := runReconciliation(); err != nil {
					r.log.WithError(err).Error("Failed to recover from previously failed reconciliation")
					continue
				}

				lastRecoFailed = false
			}
		}
	}
}

// Reconcile implements [manager.Reconciler].
func (r *Reconciler) Reconcile(ctx context.Context, cluster *v1beta1.ClusterConfig) error {
	updates, stopped, err := func() (chan<- updateFunc, <-chan struct{}, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.state != reconcilerStarted {
			return nil, nil, fmt.Errorf("cannot reconcile, not started: %s", r.state)
		}
		return r.updates, r.stopped, nil
	}()
	if err != nil {
		return err
	}

	configSnapshot := takeConfigSnapshot(cluster.Spec)

	return reconcile(ctx, updates, stopped, func(s *snapshot) {
		s.configSnapshot = &configSnapshot
	})
}

var errStoppedConcurrently = errors.New("stopped concurrently")

// reconcile enqueues the given update and awaits its reconciliation.
func reconcile(ctx context.Context, updates chan<- updateFunc, stopped <-chan struct{}, update func(*snapshot)) error {
	recoDone := make(chan error, 1)

	select {
	case updates <- func(s *snapshot) chan<- error { update(s); return recoDone }:
		break
	case <-stopped:
		return fmt.Errorf("%w while trying to enqueue state update", errStoppedConcurrently)
	case <-ctx.Done():
		return fmt.Errorf("%w while trying to enqueue state update", ctx.Err())
	}

	select {
	case err := <-recoDone:
		return err
	case <-stopped:
		return fmt.Errorf("%w while waiting for reconciliation to finish", errStoppedConcurrently)
	case <-ctx.Done():
		return fmt.Errorf("%w while waiting for reconciliation to finish", ctx.Err())
	}
}

// Stop implements [manager.Component].
func (r *Reconciler) Stop() error {
	r.log.Debug("Stopping")

	stopped, err := func() (<-chan struct{}, error) {
		r.mu.Lock()
		defer r.mu.Unlock()

		switch r.state {
		case reconcilerStarted:
			go r.requestStop()
			r.updates = nil
			r.requestStop = nil
			r.state = reconcilerStopped
			return r.stopped, nil

		case reconcilerStopped:
			return r.stopped, nil

		default:
			return nil, fmt.Errorf("cannot stop: %s", r.state)
		}
	}()
	if err != nil {
		return err
	}

	<-stopped
	r.log.Info("Stopped")
	return nil
}

type resource interface {
	runtime.Object
	metav1.Object
}

func (r *Reconciler) generateResources(snapshot *snapshot) (resources, error) {
	configMaps, err := r.buildConfigMaps(snapshot)
	if err != nil {
		return nil, err
	}

	objects := buildRBACResources(configMaps)
	for _, configMap := range configMaps {
		objects = append(objects, configMap)
	}

	// Ensure a stable order, so that reflect.DeepEqual on slices will work.
	slices.SortFunc(objects, func(l, r resource) bool {
		x := strings.Join([]string{l.GetObjectKind().GroupVersionKind().Kind, l.GetNamespace(), l.GetName()}, "/")
		y := strings.Join([]string{r.GetObjectKind().GroupVersionKind().Kind, r.GetNamespace(), r.GetName()}, "/")
		return x < y
	})

	resources, err := applier.ToUnstructuredSlice(nil, objects...)
	if err != nil {
		return nil, err
	}

	return resources, nil
}

func (r *Reconciler) buildConfigMaps(snapshot *snapshot) ([]*corev1.ConfigMap, error) {
	workerProfiles := make(map[string]*workerconfig.Profile)

	workerProfile := r.buildProfile(snapshot)
	workerProfile.KubeletConfiguration.CgroupsPerQOS = pointer.Bool(true)
	workerProfiles["default"] = workerProfile

	workerProfile = r.buildProfile(snapshot)
	workerProfile.KubeletConfiguration.CgroupsPerQOS = pointer.Bool(false)
	workerProfiles["default-windows"] = workerProfile

	for _, profile := range snapshot.profiles {
		workerProfile, ok := workerProfiles[profile.Name]
		if !ok {
			workerProfile = r.buildProfile(snapshot)
		}
		if err := yaml.Unmarshal(profile.Config, &workerProfile.KubeletConfiguration); err != nil {
			return nil, fmt.Errorf("failed to decode worker profile %q: %w", profile.Name, err)
		}
		workerProfiles[profile.Name] = workerProfile
	}

	var configMaps []*corev1.ConfigMap
	for name, workerProfile := range workerProfiles {
		configMap, err := toConfigMap(name, workerProfile)
		if err != nil {
			return nil, fmt.Errorf("failed to generate ConfigMap for worker profile %q: %w", name, err)
		}
		configMaps = append(configMaps, configMap)
	}

	return configMaps, nil
}

func buildRBACResources(configMaps []*corev1.ConfigMap) []resource {
	configMapNames := make([]string, len(configMaps))
	for i, configMap := range configMaps {
		configMapNames[i] = configMap.ObjectMeta.Name
	}

	// Not strictly necessary, but it guarantees a stable ordering.
	sort.Strings(configMapNames)

	meta := metav1.ObjectMeta{
		Name:      fmt.Sprintf("system:bootstrappers:%s", constant.WorkerConfigComponentName),
		Namespace: "kube-system",
		Labels:    applier.CommonLabels(constant.WorkerConfigComponentName),
	}

	var objects []resource
	objects = append(objects, &rbacv1.Role{
		ObjectMeta: meta,
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"configmaps"},
			Verbs:         []string{"get"},
			ResourceNames: configMapNames,
		}},
	})

	objects = append(objects, &rbacv1.RoleBinding{
		ObjectMeta: meta,
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     meta.Name,
		},
		Subjects: []rbacv1.Subject{{
			APIGroup: rbacv1.GroupName,
			Kind:     rbacv1.GroupKind,
			Name:     "system:bootstrappers",
		}, {
			APIGroup: rbacv1.GroupName,
			Kind:     rbacv1.GroupKind,
			Name:     "system:nodes",
		}},
	})

	return objects
}

func (r *Reconciler) buildProfile(snapshot *snapshot) *workerconfig.Profile {
	workerProfile := &workerconfig.Profile{
		KubeletConfiguration: kubeletv1beta1.KubeletConfiguration{
			TypeMeta: metav1.TypeMeta{
				APIVersion: kubeletv1beta1.SchemeGroupVersion.String(),
				Kind:       "KubeletConfiguration",
			},
			ClusterDNS:    []string{r.clusterDNSIP.String()},
			ClusterDomain: r.clusterDomain,
			TLSCipherSuites: []string{
				"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
				"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305",
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
				"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305",
				"TLS_RSA_WITH_AES_128_GCM_SHA256",
				"TLS_RSA_WITH_AES_256_GCM_SHA384",
			},
			FailSwapOn:         pointer.Bool(false),
			RotateCertificates: true,
			ServerTLSBootstrap: true,
			EventRecordQPS:     pointer.Int32(0),
		},
	}

	return workerProfile
}

func toConfigMap(profileName string, profile *workerconfig.Profile) (*corev1.ConfigMap, error) {
	data, err := workerconfig.ToConfigMapData(profile)
	if err != nil {
		return nil, err
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s", constant.WorkerConfigComponentName, profileName, constant.KubernetesMajorMinorVersion),
			Namespace: "kube-system",
			Labels: applier.
				CommonLabels(constant.WorkerConfigComponentName).
				With("k0s.k0sproject.io/worker-profile", profileName),
		},
		Data: data,
	}, nil
}
