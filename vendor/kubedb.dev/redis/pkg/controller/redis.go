/*
Copyright AppsCode Inc. and Contributors

Licensed under the AppsCode Community License 1.0.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://github.com/appscode/licenses/raw/1.0.0/AppsCode-Community-1.0.0.md

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha2"
	"kubedb.dev/apimachinery/client/clientset/versioned/typed/kubedb/v1alpha2/util"
	"kubedb.dev/apimachinery/pkg/eventer"
	validator "kubedb.dev/redis/pkg/admission"

	"github.com/appscode/go/log"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kutil "kmodules.xyz/client-go"
	kmapi "kmodules.xyz/client-go/api/v1"
	dynamic_util "kmodules.xyz/client-go/dynamic"
)

func (c *Controller) create(redis *api.Redis) error {
	if err := validator.ValidateRedis(c.Client, c.DBClient, redis, true); err != nil {
		c.Recorder.Event(
			redis,
			core.EventTypeWarning,
			eventer.EventReasonInvalid,
			err.Error(),
		)
		log.Errorln(err)
		return nil // user error so just record error and don't retry.
	}

	if redis.Status.Phase == "" {
		rd, err := util.UpdateRedisStatus(context.TODO(), c.DBClient.KubedbV1alpha2(), redis.ObjectMeta, func(in *api.RedisStatus) *api.RedisStatus {
			in.Phase = api.DatabasePhaseProvisioning
			return in
		}, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		redis.Status = rd.Status
	}

	// create Governing Service
	governingService := c.GoverningService
	if err := c.CreateGoverningService(governingService, redis.Namespace); err != nil {
		return err
	}

	// ensure ConfigMap for redis configuration file (i.e. redis.conf)
	if redis.Spec.Mode == api.RedisModeCluster {
		if err := c.ensureRedisConfig(redis); err != nil {
			return err
		}
	}

	// Ensure ClusterRoles for statefulsets
	if err := c.ensureRBACStuff(redis); err != nil {
		return err
	}

	// ensure database Service
	vt1, err := c.ensureService(redis)
	if err != nil {
		return err
	}

	// wait for  Certificates secrets
	if redis.Spec.TLS != nil {
		ok, err := dynamic_util.ResourcesExists(
			c.DynamicClient,
			core.SchemeGroupVersion.WithResource("secrets"),
			redis.Namespace,
			redis.MustCertSecretName(api.RedisServerCert),
			redis.MustCertSecretName(api.RedisClientCert),
			redis.MustCertSecretName(api.RedisMetricsExporterCert),
		)
		if err != nil {
			return err
		}
		if !ok {
			log.Infof("wait for all certificate secrets for Redis %s/%s", redis.Namespace, redis.Name)
			return nil
		}
	}

	// ensure database StatefulSet
	vt2, err := c.ensureRedisNodes(redis)
	if err != nil {
		return err
	}

	if vt1 == kutil.VerbCreated && vt2 == kutil.VerbCreated {
		c.Recorder.Event(
			redis,
			core.EventTypeNormal,
			eventer.EventReasonSuccessful,
			"Successfully created Redis",
		)
	} else if vt1 == kutil.VerbPatched || vt2 == kutil.VerbPatched {
		c.Recorder.Event(
			redis,
			core.EventTypeNormal,
			eventer.EventReasonSuccessful,
			"Successfully patched Redis",
		)
	}

	_, err = c.ensureAppBinding(redis)
	if err != nil {
		log.Errorln(err)
		return err
	}

	//======================== Wait for the initial restore =====================================
	if redis.Spec.Init != nil && redis.Spec.Init.WaitForInitialRestore {
		// Only wait for the first restore.
		// For initial restore, "Provisioned" condition won't exist and "DataRestored" condition either won't exist or will be "False".
		if !kmapi.HasCondition(redis.Status.Conditions, api.DatabaseProvisioned) &&
			!kmapi.IsConditionTrue(redis.Status.Conditions, api.DatabaseDataRestored) {
			// write log indicating that the database is waiting for the data to be restored by external initializer
			log.Infof("Database %s %s/%s is waiting for data to be restored by external initializer",
				redis.Kind,
				redis.Namespace,
				redis.Name,
			)
			// Rest of the processing will execute after the the restore process completed. So, just return for now.
			return nil
		}
	}

	rd, err := util.UpdateRedisStatus(context.TODO(), c.DBClient.KubedbV1alpha2(), redis.ObjectMeta, func(in *api.RedisStatus) *api.RedisStatus {
		in.Phase = api.DatabasePhaseReady
		in.ObservedGeneration = redis.Generation
		return in
	}, metav1.UpdateOptions{})
	if err != nil {
		c.Recorder.Eventf(
			redis,
			core.EventTypeWarning,
			eventer.EventReasonFailedToUpdate,
			err.Error(),
		)
		return err
	}
	redis.Status = rd.Status

	// ensure StatsService for desired monitoring
	if _, err := c.ensureStatsService(redis); err != nil {
		c.Recorder.Eventf(
			redis,
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			"Failed to manage monitoring system. Reason: %v",
			err,
		)
		log.Errorf("failed to manage monitoring system. Reason: %v", err)
		return nil
	}

	if err := c.manageMonitor(redis); err != nil {
		c.Recorder.Eventf(
			redis,
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			"Failed to manage monitoring system. Reason: %v",
			err,
		)
		log.Errorf("failed to manage monitoring system. Reason: %v", err)
		return nil
	}

	return nil
}

func (c *Controller) halt(db *api.Redis) error {
	if db.Spec.Halted && db.Spec.TerminationPolicy != api.TerminationPolicyHalt {
		return errors.New("can't halt db. 'spec.terminationPolicy' is not 'Halt'")
	}
	log.Infof("Halting Redis %v/%v", db.Namespace, db.Name)
	if err := c.haltDatabase(db); err != nil {
		return err
	}
	if err := c.waitUntilPaused(db); err != nil {
		return err
	}
	log.Infof("update status of Redis %v/%v to Halted.", db.Namespace, db.Name)
	if _, err := util.UpdateRedisStatus(context.TODO(), c.DBClient.KubedbV1alpha2(), db.ObjectMeta, func(in *api.RedisStatus) *api.RedisStatus {
		in.Phase = api.DatabasePhaseHalted
		in.ObservedGeneration = db.Generation
		return in
	}, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

func (c *Controller) terminate(redis *api.Redis) error {
	// If TerminationPolicy is "halt", keep PVCs,Secrets intact.
	if redis.Spec.TerminationPolicy == api.TerminationPolicyHalt {
		if err := c.removeOwnerReferenceFromOffshoots(redis); err != nil {
			return err
		}
	} else {
		// If TerminationPolicy is "wipeOut", delete everything (ie, PVCs,Secrets,Snapshots).
		// If TerminationPolicy is "delete", delete PVCs and keep snapshots,secrets intact.
		// In both these cases, don't create dormantdatabase
		if err := c.setOwnerReferenceToOffshoots(redis); err != nil {
			return err
		}
	}

	if redis.Spec.Monitor != nil {
		if err := c.deleteMonitor(redis); err != nil {
			log.Errorln(err)
			return nil
		}
	}
	return nil
}

func (c *Controller) setOwnerReferenceToOffshoots(redis *api.Redis) error {
	owner := metav1.NewControllerRef(redis, api.SchemeGroupVersion.WithKind(api.ResourceKindRedis))
	selector := labels.SelectorFromSet(redis.OffshootSelectors())

	// If TerminationPolicy is "wipeOut", delete snapshots and secrets,
	// else, keep it intact.
	if redis.Spec.TerminationPolicy == api.TerminationPolicyWipeOut {
		if err := c.wipeOutDatabase(redis.ObjectMeta, c.GetRedisSecrets(redis), owner); err != nil {
			return errors.Wrap(err, "error in wiping out database.")
		}
	} else {
		// Make sure secret's ownerreference is removed.
		if err := dynamic_util.RemoveOwnerReferenceForItems(
			context.TODO(),
			c.DynamicClient,
			core.SchemeGroupVersion.WithResource("secrets"),
			redis.Namespace,
			c.GetRedisSecrets(redis),
			redis); err != nil {
			return err
		}
	}

	// delete PVC for both "wipeOut" and "delete" TerminationPolicy.
	return dynamic_util.EnsureOwnerReferenceForSelector(
		context.TODO(),
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("persistentvolumeclaims"),
		redis.Namespace,
		selector,
		owner)
}

func (c *Controller) removeOwnerReferenceFromOffshoots(redis *api.Redis) error {
	// First, Get LabelSelector for Other Components
	labelSelector := labels.SelectorFromSet(redis.OffshootSelectors())
	if err := dynamic_util.RemoveOwnerReferenceForItems(
		context.TODO(),
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("secrets"),
		redis.Namespace,
		c.GetRedisSecrets(redis),
		redis); err != nil {
		return err
	}
	if err := dynamic_util.RemoveOwnerReferenceForSelector(
		context.TODO(),
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("persistentvolumeclaims"),
		redis.Namespace,
		labelSelector,
		redis); err != nil {
		return err
	}
	return nil
}
