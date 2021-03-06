// Copyright (c) 2017, Oracle and/or its affiliates. All rights reserved.
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
package loadbalancer

import (
	"fmt"
	"testing"

	"github.com/golang/glog"
	"github.com/oracle/oci-cloud-controller-manager/pkg/oci"
	"github.com/oracle/oci-cloud-controller-manager/pkg/oci/client"
	api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func TestPublicLoadBalancer(t *testing.T) {
	testLoadBalancer(t, false)
}

func TestInternalLoadBalancer(t *testing.T) {
	testLoadBalancer(t, true)
}

func testLoadBalancer(t *testing.T, internal bool) {
	cp, err := oci.NewCloudProvider(fw.Config)
	if err != nil {
		t.Fatal(err)
	}

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	cp.(*oci.CloudProvider).NodeLister = listersv1.NewNodeLister(indexer)

	service := &api.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "kube-system",
			Name:        "testservice",
			UID:         "integration-test-uid",
			Annotations: map[string]string{},
		},
		Spec: api.ServiceSpec{
			Type: api.ServiceTypeLoadBalancer,
			Ports: []api.ServicePort{
				{
					Name:       "http",
					Protocol:   api.ProtocolTCP,
					Port:       80,
					NodePort:   8080,
					TargetPort: intstr.FromInt(9090),
				},
			},
			SessionAffinity:          api.ServiceAffinityNone,
			LoadBalancerSourceRanges: []string{"0.0.0.0/0"},
		},
	}

	if internal {
		service.Annotations[oci.ServiceAnnotationLoadBalancerInternal] = ""
	}

	loadbalancers, enabled := cp.LoadBalancer()
	if !enabled {
		t.Fatal("the LoadBalancer interface is not enabled on the CCM")
	}

	// Always call cleanup before any api calls are made since then otherwise we may
	// get to an error state and some objects won't be cleaned up.
	defer func() {
		fw.Cleanup()

		err := loadbalancers.EnsureLoadBalancerDeleted("foo", service)
		if err != nil {
			t.Fatalf("Unable to delete the load balancer during cleanup: %v", err)
		}
	}()

	nodes := []*api.Node{}
	for _, subnetID := range fw.NodeSubnets() {

		subnet, err := fw.Client.GetSubnet(subnetID)
		if err != nil {
			t.Fatal(err)
		}

		instance, err := fw.CreateInstance(subnet.AvailabilityDomain, subnetID)
		if err != nil {
			t.Fatal(err)
		}

		err = fw.WaitForInstance(instance.ID)
		if err != nil {
			t.Fatal(err)
		}

		addresses, err := fw.Client.GetNodeAddressesForInstance(instance.ID)
		if err != nil {
			t.Fatal(err)
		}

		node := &api.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: instance.ID,
			},
			Spec: api.NodeSpec{
				ProviderID: instance.ID,
			},
			Status: api.NodeStatus{
				Addresses: addresses,
			},
		}
		indexer.Add(node)
		nodes = append(nodes, node)
	}

	glog.Info("Stating test on creating initial load balancer")

	status, err := loadbalancers.EnsureLoadBalancer("foo", service, nodes)
	if err != nil {
		t.Fatalf("Unable to ensure the load balancer: %v", err)
	}

	glog.Infof("Load Balancer Status: %+v", status)

	err = validateLoadBalancer(fw.Client, service, nodes)
	if err != nil {
		t.Fatalf("validation error: %v", err)
	}

	glog.Info("Stating test on decreasing node count to 1")

	// Decrease the number of backends to 1
	lessNodes := []*api.Node{nodes[0]}
	status, err = loadbalancers.EnsureLoadBalancer("foo", service, lessNodes)
	if err != nil {
		t.Fatalf("Unable to ensure load balancer: %v", err)
	}

	err = validateLoadBalancer(fw.Client, service, lessNodes)
	if err != nil {
		t.Fatalf("validation error: %v", err)
	}

	glog.Info("Stating test on increasing node count back to 2")

	// Go back to 2 nodes
	status, err = loadbalancers.EnsureLoadBalancer("foo", service, nodes)
	if err != nil {
		t.Fatalf("Unable to ensure the load balancer: %v", err)
	}

	err = validateLoadBalancer(fw.Client, service, nodes)
	if err != nil {
		t.Fatalf("validation error: %v", err)
	}

	glog.Info("Stating test on changing service port")

	// Validate changing the service port.
	service.Spec.Ports[0].Port = 81
	status, err = loadbalancers.EnsureLoadBalancer("foo", service, nodes)
	if err != nil {
		t.Fatalf("Unable to ensure the load balancer: %v", err)
	}

	err = validateLoadBalancer(fw.Client, service, nodes)
	if err != nil {
		t.Fatalf("validation error: %v", err)
	}

	glog.Info("Stating test on changing node port")
	// Validate changing the node port.
	service.Spec.Ports[0].NodePort = 8081
	status, err = loadbalancers.EnsureLoadBalancer("foo", service, nodes)
	if err != nil {
		t.Fatalf("Unable to ensure the load balancer: %v", err)
	}

	err = validateLoadBalancer(fw.Client, service, nodes)
	if err != nil {
		t.Fatalf("validation error: %v", err)
	}
}

func validateLoadBalancer(client client.Interface, service *api.Service, nodes []*api.Node) error {
	// TODO: make this better :)
	// Generate expected listeners / backends based on service / nodes.

	lb, err := client.GetLoadBalancerByName(oci.GetLoadBalancerName(service))
	if err != nil {
		return err
	}

	if len(lb.Listeners) != 1 {
		return fmt.Errorf("expected 1 Listener but got %d", len(lb.Listeners))
	}

	if len(lb.BackendSets) != 1 {
		return fmt.Errorf("expected 1 BackendSet but got %d", len(lb.BackendSets))
	}

	name := fmt.Sprintf("TCP-%d", service.Spec.Ports[0].Port)
	backendSet, ok := lb.BackendSets[name]
	if !ok {
		return fmt.Errorf("expected BackendSet with name %q to exist but it doesn't", name)
	}

	if len(backendSet.Backends) != len(nodes) {
		return fmt.Errorf("expected %d backends but got %d", len(nodes), len(backendSet.Backends))
	}

	expectedBackendPort := service.Spec.Ports[0].NodePort
	actualBackendPort := backendSet.Backends[0].Port
	if int(expectedBackendPort) != int(actualBackendPort) {
		return fmt.Errorf("expected backend port %d but got %d", expectedBackendPort, actualBackendPort)
	}

	return nil
}
