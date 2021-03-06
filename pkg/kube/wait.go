/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package kube // import "k8s.io/helm/pkg/kube"

import (
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/pkg/apis/apps"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/apis/extensions/v1beta1"
	"k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	core "k8s.io/kubernetes/pkg/client/clientset_generated/clientset/typed/core/v1"
	extensions "k8s.io/kubernetes/pkg/client/clientset_generated/clientset/typed/extensions/v1beta1"
	internalclientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	deploymentutil "k8s.io/kubernetes/pkg/controller/deployment/util"
)

// deployment holds associated replicaSets for a deployment
type deployment struct {
	replicaSets *v1beta1.ReplicaSet
	deployment  *v1beta1.Deployment
}

// waitForResources polls to get the current status of all pods, PVCs, and Services
// until all are ready or a timeout is reached
func (c *Client) waitForResources(timeout time.Duration, created Result) error {
	log.Printf("beginning wait for resources with timeout of %v", timeout)
	cs, _ := c.ClientSet()
	client := versionedClientsetForDeployment(cs)
	return wait.Poll(2*time.Second, timeout, func() (bool, error) {
		pods := []v1.Pod{}
		services := []v1.Service{}
		pvc := []v1.PersistentVolumeClaim{}
		replicaSets := []*v1beta1.ReplicaSet{}
		deployments := []deployment{}
		for _, v := range created {
			obj, err := c.AsVersionedObject(v.Object)
			if err != nil && !runtime.IsNotRegisteredError(err) {
				return false, err
			}
			switch value := obj.(type) {
			case (*v1.ReplicationController):
				list, err := getPods(client, value.Namespace, value.Spec.Selector)
				if err != nil {
					return false, err
				}
				pods = append(pods, list...)
			case (*v1.Pod):
				pod, err := client.Core().Pods(value.Namespace).Get(value.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				pods = append(pods, *pod)
			case (*v1beta1.Deployment):
				// Get the RS children first
				rs, err := client.Extensions().ReplicaSets(value.Namespace).List(metav1.ListOptions{
					FieldSelector: fields.Everything().String(),
					LabelSelector: labels.Set(value.Spec.Selector.MatchLabels).AsSelector().String(),
				})
				if err != nil {
					return false, err
				}

				for _, i := range rs.Items {
					replicaSets = append(replicaSets, &i)
				}

				currentDeployment, err := client.Extensions().Deployments(value.Namespace).Get(value.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				// Find RS associated with deployment
				newReplicaSet, err := deploymentutil.FindNewReplicaSet(currentDeployment, replicaSets)
				if err != nil {
					return false, err
				}
				newDeployment := deployment{
					newReplicaSet,
					currentDeployment,
				}
				deployments = append(deployments, newDeployment)
			case (*v1beta1.DaemonSet):
				list, err := getPods(client, value.Namespace, value.Spec.Selector.MatchLabels)
				if err != nil {
					return false, err
				}
				pods = append(pods, list...)
			case (*apps.StatefulSet):
				list, err := getPods(client, value.Namespace, value.Spec.Selector.MatchLabels)
				if err != nil {
					return false, err
				}
				pods = append(pods, list...)
			case (*v1beta1.ReplicaSet):
				list, err := getPods(client, value.Namespace, value.Spec.Selector.MatchLabels)
				if err != nil {
					return false, err
				}
				pods = append(pods, list...)
			case (*v1.PersistentVolumeClaim):
				claim, err := client.Core().PersistentVolumeClaims(value.Namespace).Get(value.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				pvc = append(pvc, *claim)
			case (*v1.Service):
				svc, err := client.Core().Services(value.Namespace).Get(value.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				services = append(services, *svc)
			}
		}
		return podsReady(pods) && servicesReady(services) && volumesReady(pvc) && deploymentsReady(deployments), nil
	})
}

func podsReady(pods []v1.Pod) bool {
	for _, pod := range pods {
		if !v1.IsPodReady(&pod) {
			return false
		}
	}
	return true
}

func servicesReady(svc []v1.Service) bool {
	for _, s := range svc {
		// Make sure the service is not explicitly set to "None" before checking the IP
		if s.Spec.ClusterIP != v1.ClusterIPNone && !v1.IsServiceIPSet(&s) {
			return false
		}
		// This checks if the service has a LoadBalancer and that balancer has an Ingress defined
		if s.Spec.Type == v1.ServiceTypeLoadBalancer && s.Status.LoadBalancer.Ingress == nil {
			return false
		}
	}
	return true
}

func volumesReady(vols []v1.PersistentVolumeClaim) bool {
	for _, v := range vols {
		if v.Status.Phase != v1.ClaimBound {
			return false
		}
	}
	return true
}

func deploymentsReady(deployments []deployment) bool {
	for _, v := range deployments {
		if !(v.replicaSets.Status.ReadyReplicas >= *v.deployment.Spec.Replicas-deploymentutil.MaxUnavailable(*v.deployment)) {
			return false
		}
	}
	return true
}

func getPods(client clientset.Interface, namespace string, selector map[string]string) ([]v1.Pod, error) {
	list, err := client.Core().Pods(namespace).List(metav1.ListOptions{
		FieldSelector: fields.Everything().String(),
		LabelSelector: labels.Set(selector).AsSelector().String(),
	})
	return list.Items, err
}

func versionedClientsetForDeployment(internalClient internalclientset.Interface) clientset.Interface {
	if internalClient == nil {
		return &clientset.Clientset{}
	}
	return &clientset.Clientset{
		CoreV1Client:            core.New(internalClient.Core().RESTClient()),
		ExtensionsV1beta1Client: extensions.New(internalClient.Extensions().RESTClient()),
	}
}
