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

	"kubedb.dev/apimachinery/apis/kubedb"
	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha2"
	"kubedb.dev/apimachinery/client/clientset/versioned/typed/kubedb/v1alpha2/util"

	"gomodules.xyz/x/log"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	kmapi "kmodules.xyz/client-go/api/v1"
	core_util "kmodules.xyz/client-go/core/v1"
	"kmodules.xyz/client-go/tools/queue"
)

func (c *Controller) initWatcher() {
	c.rdInformer = c.KubedbInformerFactory.Kubedb().V1alpha2().Redises().Informer()
	c.rdQueue = queue.New("Redis", c.MaxNumRequeues, c.NumThreads, c.runRedis)
	c.rdLister = c.KubedbInformerFactory.Kubedb().V1alpha2().Redises().Lister()
	c.rdInformer.AddEventHandler(queue.NewChangeHandler(c.rdQueue.GetQueue()))
}

func (c *Controller) runRedis(key string) error {
	log.Debugln("started processing, key:", key)
	obj, exists, err := c.rdInformer.GetIndexer().GetByKey(key)
	if err != nil {
		log.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		log.Debugf("Redis %s does not exist anymore", key)
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Redis was recreated with the same name
		redis := obj.(*api.Redis).DeepCopy()
		if redis.DeletionTimestamp != nil {
			if core_util.HasFinalizer(redis.ObjectMeta, kubedb.GroupName) {
				if err := c.terminate(redis); err != nil {
					log.Errorln(err)
					return err
				}
				_, _, err = util.PatchRedis(context.TODO(), c.DBClient.KubedbV1alpha2(), redis, func(in *api.Redis) *api.Redis {
					in.ObjectMeta = core_util.RemoveFinalizer(in.ObjectMeta, kubedb.GroupName)
					return in
				}, metav1.PatchOptions{})
				return err
			}
		} else {
			redis, _, err = util.PatchRedis(context.TODO(), c.DBClient.KubedbV1alpha2(), redis, func(in *api.Redis) *api.Redis {
				in.ObjectMeta = core_util.AddFinalizer(in.ObjectMeta, kubedb.GroupName)
				return in
			}, metav1.PatchOptions{})
			if err != nil {
				return err
			}

			if kmapi.IsConditionTrue(redis.Status.Conditions, api.DatabasePaused) {
				return nil
			}

			if redis.Spec.Halted {
				if err := c.halt(redis); err != nil {
					log.Errorln(err)
					c.pushFailureEvent(redis, err.Error())
					return err
				}
			} else {
				if err := c.create(redis); err != nil {
					log.Errorln(err)
					c.pushFailureEvent(redis, err.Error())
					return err
				}
			}
		}
	}
	return nil
}

func (c *Controller) initSecretWatcher() {
	c.SecretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if secret, ok := obj.(*core.Secret); ok {
				if key := c.RedisForSecret(secret); key != "" {
					queue.Enqueue(c.rdQueue.GetQueue(), key)
				}
			}
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			if secret, ok := newObj.(*core.Secret); ok {
				if key := c.RedisForSecret(secret); key != "" {
					queue.Enqueue(c.rdQueue.GetQueue(), key)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
		},
	})
}
