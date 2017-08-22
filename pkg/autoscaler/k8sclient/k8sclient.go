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

package k8sclient

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/golang/glog"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// K8sClient - Wraps all needed client functionalities for autoscaler
type K8sClient interface {
	// GetClusterSize counts schedulable nodes and cores in the cluster
	GetClusterSize() (*ClusterSize, error)
	// UpdateResources updates the resource needs for the containers in the target
	UpdateResources(resources map[string]apiv1.ResourceRequirements) error
}

// k8sClient - Wraps all Kubernetes API client functionality.
type k8sClient struct {
	target        *targetSpec
	clientset     kubernetes.Interface
	clusterStatus *ClusterSize
}

// NewK8sClient gives a k8sClient with the given dependencies.
func NewK8sClient(namespace, target, kubeconfig string) (K8sClient, error) {
	var config *rest.Config
	var err error
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	// Use protobufs for communication with apiserver.
	config.ContentType = "application/vnd.kubernetes.protobuf"
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	tgt, err := makeTarget(clientset, target, namespace)
	if err != nil {
		return nil, err
	}

	return &k8sClient{
		clientset: clientset,
		target:    tgt,
	}, nil
}

func makeTarget(client kubernetes.Interface, target, namespace string) (*targetSpec, error) {
	splits := strings.Split(target, "/")
	if len(splits) != 2 {
		return &targetSpec{}, fmt.Errorf("target format error: %v", target)
	}
	kind := splits[0]
	name := splits[1]

	kind, groupVersion, err := discoverAPI(client, kind)
	if err != nil {
		return &targetSpec{}, err
	}
	glog.V(4).Infof("discovered target %s = %s.%s", target, groupVersion, kind)
	return &targetSpec{kind, groupVersion, name, namespace}, nil
}

func discoverAPI(client kubernetes.Interface, kindArg string) (kind, groupVersion string, err error) {
	var plural string
	switch strings.ToLower(kindArg) {
	case "deployment":
		kind = "Deployment"
		plural = "Deployments"
	case "daemonset":
		kind = "DaemonSet"
		plural = "DaemonSets"
	case "replicaset":
		kind = "ReplicaSet"
		plural = "ReplicaSets"
	default:
		return "", "", fmt.Errorf("unknown kind %q", kindArg)
	}

	resourceLists, err := client.Discovery().ServerPreferredNamespacedResources()
	if err != nil {
		return "", "", fmt.Errorf("failed to discover apigroup for kind %q: %v", kind, err)
	}

	for _, resourceList := range resourceLists {
		groupVersion = resourceList.GroupVersion
		for _, res := range resourceList.APIResources {
			if res.Name == plural {
				kind = res.Kind
				groupVersion = resourceList.GroupVersion
			}
		}
	}

	return kind, groupVersion, nil
}

// targetSpec stores the scalable target resource.
type targetSpec struct {
	kind         string
	groupVersion string
	name         string
	namespace    string
}

// ClusterSize defines the cluster status.
type ClusterSize struct {
	Nodes int
	Cores int
}

func (k *k8sClient) GetClusterSize() (clusterStatus *ClusterSize, err error) {
	opt := metav1.ListOptions{Watch: false}

	nodes, err := k.clientset.Core().Nodes().List(opt)
	if err != nil || nodes == nil {
		return nil, err
	}
	clusterStatus = &ClusterSize{}
	clusterStatus.Nodes = len(nodes.Items)
	var tc resource.Quantity
	// All nodes are considered, even those that are marked as unshedulable,
	// this includes the master.
	for _, node := range nodes.Items {
		tc.Add(node.Status.Capacity[apiv1.ResourceCPU])
	}

	tcInt64, tcOk := tc.AsInt64()
	if !tcOk {
		return nil, fmt.Errorf("unable to compute integer values of cores in the cluster")
	}
	clusterStatus.Cores = int(tcInt64)
	k.clusterStatus = clusterStatus
	return clusterStatus, nil
}

func (k *k8sClient) UpdateResources(resources map[string]apiv1.ResourceRequirements) error {
	ctrs := []interface{}{}
	for ctrName, res := range resources {
		ctrs = append(ctrs, map[string]interface{}{
			"name":      ctrName,
			"resources": res,
		})
	}
	patch := map[string]interface{}{
		"apiVersion": fmt.Sprintf("%s", k.target.groupVersion),
		"kind":       k.target.kind,
		"metadata": map[string]interface{}{
			"name": k.target.name,
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": ctrs,
				},
			},
		},
	}
	jb, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("can't marshal patch to JSON: %v", err)
	}
	kind := strings.ToLower(k.target.kind)
	switch kind {
	case "deployment":
		if _, err := k.clientset.Extensions().Deployments(k.target.namespace).Patch(k.target.name, types.StrategicMergePatchType, jb); err != nil {
			return fmt.Errorf("patch failed: %v", err)
		}
	case "daemonset":
		if _, err := k.clientset.Extensions().DaemonSets(k.target.namespace).Patch(k.target.name, types.StrategicMergePatchType, jb); err != nil {
			return fmt.Errorf("patch failed: %v", err)
		}
	case "replicaset":
		if _, err := k.clientset.Extensions().ReplicaSets(k.target.namespace).Patch(k.target.name, types.StrategicMergePatchType, jb); err != nil {
			return fmt.Errorf("patch failed: %v", err)
		}
	default:
		return fmt.Errorf("Unknown target format: must be one of deployment/*, daemonset/*, or replicaset/* (not case sensitive).")
	}

	return nil
}
