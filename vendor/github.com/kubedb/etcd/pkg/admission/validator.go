package admission

import (
	"fmt"
	"strings"
	"sync"

	"github.com/appscode/go/log"
	hookapi "github.com/appscode/kubernetes-webhook-util/admission/v1beta1"
	meta_util "github.com/appscode/kutil/meta"
	api "github.com/kubedb/apimachinery/apis/kubedb/v1alpha1"
	cs "github.com/kubedb/apimachinery/client/clientset/versioned"
	kubedbv1alpha1 "github.com/kubedb/apimachinery/client/clientset/versioned/typed/kubedb/v1alpha1"
	amv "github.com/kubedb/apimachinery/pkg/validator"
	"github.com/pkg/errors"
	admission "k8s.io/api/admission/v1beta1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type EtcdValidator struct {
	client      kubernetes.Interface
	extClient   cs.Interface
	lock        sync.RWMutex
	initialized bool
}

var _ hookapi.AdmissionHook = &EtcdValidator{}

func (a *EtcdValidator) Resource() (plural schema.GroupVersionResource, singular string) {
	return schema.GroupVersionResource{
			Group:    "validators.kubedb.com",
			Version:  "v1alpha1",
			Resource: "etcds",
		},
		"etcd"
}

func (a *EtcdValidator) Initialize(config *rest.Config, stopCh <-chan struct{}) error {
	a.lock.Lock()
	defer a.lock.Unlock()

	a.initialized = true

	var err error
	if a.client, err = kubernetes.NewForConfig(config); err != nil {
		return err
	}
	if a.extClient, err = cs.NewForConfig(config); err != nil {
		return err
	}
	return err
}

func (a *EtcdValidator) Admit(req *admission.AdmissionRequest) *admission.AdmissionResponse {
	status := &admission.AdmissionResponse{}

	if (req.Operation != admission.Create && req.Operation != admission.Update && req.Operation != admission.Delete) ||
		len(req.SubResource) != 0 ||
		req.Kind.Group != api.SchemeGroupVersion.Group ||
		req.Kind.Kind != api.ResourceKindEtcd {
		status.Allowed = true
		return status
	}

	a.lock.RLock()
	defer a.lock.RUnlock()
	if !a.initialized {
		return hookapi.StatusUninitialized()
	}

	switch req.Operation {
	case admission.Delete:
		// req.Object.Raw = nil, so read from kubernetes
		obj, err := a.extClient.KubedbV1alpha1().Etcds(req.Namespace).Get(req.Name, metav1.GetOptions{})
		if err != nil && !kerr.IsNotFound(err) {
			return hookapi.StatusInternalServerError(err)
		} else if err == nil && obj.Spec.DoNotPause {
			return hookapi.StatusBadRequest(fmt.Errorf(`etcd "%s" can't be paused. To continue delete, unset spec.doNotPause and retry`, req.Name))
		}
	default:
		obj, err := meta_util.UnmarshalFromJSON(req.Object.Raw, api.SchemeGroupVersion)
		if err != nil {
			return hookapi.StatusBadRequest(err)
		}
		if req.Operation == admission.Update {
			// validate changes made by user
			oldObject, err := meta_util.UnmarshalFromJSON(req.OldObject.Raw, api.SchemeGroupVersion)
			if err != nil {
				return hookapi.StatusBadRequest(err)
			}

			etcd := obj.(*api.Etcd).DeepCopy()
			oldEtcd := oldObject.(*api.Etcd).DeepCopy()
			// Allow changing Database Secret only if there was no secret have set up yet.
			if oldEtcd.Spec.DatabaseSecret == nil {
				oldEtcd.Spec.DatabaseSecret = etcd.Spec.DatabaseSecret
			}

			if err := validateUpdate(etcd, oldEtcd, req.Kind.Kind); err != nil {
				return hookapi.StatusBadRequest(fmt.Errorf("%v", err))
			}
		}
		// validate database specs
		if err = ValidateEtcd(a.client, a.extClient.KubedbV1alpha1(), obj.(*api.Etcd)); err != nil {
			return hookapi.StatusForbidden(err)
		}
	}
	status.Allowed = true
	return status
}

// ValidateEtcd checks if the object satisfies all the requirements.
// It is not method of Interface, because it is referenced from controller package too.
func ValidateEtcd(client kubernetes.Interface, extClient kubedbv1alpha1.KubedbV1alpha1Interface, etcd *api.Etcd) error {
	if etcd.Spec.Version == "" {
		return errors.New(`'spec.version' is missing`)
	}
	if _, err := extClient.EtcdVersions().Get(string(etcd.Spec.Version), metav1.GetOptions{}); err != nil {
		return err
	}

	if etcd.Spec.Replicas == nil {
		return fmt.Errorf(`spec.replicas "%v" invalid. Value must be one`, etcd.Spec.Replicas)
	}

	if etcd.Spec.StorageType == "" {
		return fmt.Errorf(`'spec.storageType' is missing`)
	}
	if etcd.Spec.Storage != nil {
		var err error
		if err = amv.ValidateStorage(client, etcd.Spec.StorageType, etcd.Spec.Storage); err != nil {
			return err
		}
	}

	if etcd.Spec.TerminationPolicy == "" {
		return fmt.Errorf(`'spec.terminationPolicy' is missing`)
	}

	databaseSecret := etcd.Spec.DatabaseSecret
	if databaseSecret != nil {
		if _, err := client.CoreV1().Secrets(etcd.Namespace).Get(databaseSecret.SecretName, metav1.GetOptions{}); err != nil {
			return err
		}
	}

	backupScheduleSpec := etcd.Spec.BackupSchedule
	if backupScheduleSpec != nil {
		if err := amv.ValidateBackupSchedule(client, backupScheduleSpec, etcd.Namespace); err != nil {
			return err
		}
	}

	monitorSpec := etcd.Spec.Monitor
	if monitorSpec != nil {
		if err := amv.ValidateMonitorSpec(monitorSpec); err != nil {
			return err
		}
	}

	if err := matchWithDormantDatabase(extClient, etcd); err != nil {
		return err
	}
	return nil
}

func matchWithDormantDatabase(extClient kubedbv1alpha1.KubedbV1alpha1Interface, etcd *api.Etcd) error {
	// Check if DormantDatabase exists or not
	dormantDb, err := extClient.DormantDatabases(etcd.Namespace).Get(etcd.Name, metav1.GetOptions{})
	if err != nil {
		if !kerr.IsNotFound(err) {
			return err
		}
		return nil
	}

	// Check DatabaseKind
	if value, _ := meta_util.GetStringValue(dormantDb.Labels, api.LabelDatabaseKind); value != api.ResourceKindEtcd {
		return errors.New(fmt.Sprintf(`invalid Etcd: "%v". Exists DormantDatabase "%v" of different Kind`, etcd.Name, dormantDb.Name))
	}

	// Check Origin Spec
	drmnOriginSpec := dormantDb.Spec.Origin.Spec.Etcd
	originalSpec := etcd.Spec

	// Skip checking doNotPause
	drmnOriginSpec.DoNotPause = originalSpec.DoNotPause

	// Skip checking Monitoring
	drmnOriginSpec.Monitor = originalSpec.Monitor

	// Skip Checking BackUP Scheduler
	drmnOriginSpec.BackupSchedule = originalSpec.BackupSchedule

	if !meta_util.Equal(drmnOriginSpec, &originalSpec) {
		diff := meta_util.Diff(drmnOriginSpec, &originalSpec)
		log.Errorf("etcd spec mismatches with OriginSpec in DormantDatabases. Diff: %v", diff)
		return errors.New(fmt.Sprintf("etcd spec mismatches with OriginSpec in DormantDatabases. Diff: %v", diff))
	}

	return nil
}

func validateUpdate(obj, oldObj runtime.Object, kind string) error {
	preconditions := getPreconditionFunc()
	_, err := meta_util.CreateStrategicPatch(oldObj, obj, preconditions...)
	if err != nil {
		if mergepatch.IsPreconditionFailed(err) {
			return fmt.Errorf("%v.%v", err, preconditionFailedError(kind))
		}
		return err
	}
	return nil
}

func getPreconditionFunc() []mergepatch.PreconditionFunc {
	preconditions := []mergepatch.PreconditionFunc{
		mergepatch.RequireKeyUnchanged("apiVersion"),
		mergepatch.RequireKeyUnchanged("kind"),
		mergepatch.RequireMetadataKeyUnchanged("name"),
		mergepatch.RequireMetadataKeyUnchanged("namespace"),
	}

	for _, field := range preconditionSpecFields {
		preconditions = append(preconditions,
			meta_util.RequireChainKeyUnchanged(field),
		)
	}
	return preconditions
}

var preconditionSpecFields = []string{
	// "spec.version",
	"spec.storageType",
	"spec.storage",
	"spec.databaseSecret",
	"spec.init",
	"spec.podTemplate.spec.nodeSelector",
	"spec.podTemplate.spec.env",
}

func preconditionFailedError(kind string) error {
	str := preconditionSpecFields
	strList := strings.Join(str, "\n\t")
	return fmt.Errorf(strings.Join([]string{`At least one of the following was changed:
	apiVersion
	kind
	name
	namespace
	status`, strList}, "\n\t"))
}
