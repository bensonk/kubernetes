/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package e2e

import (
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/fields"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/wait"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	// Interval to poll /runningpods on a node
	pollInterval = 1 * time.Second
	// Interval used compute cpu usage of a container
	cpuIntervalInSeconds = 60
)

// getPodMatches returns a set of pod names on the given node that matches the
// podNamePrefix and namespace.
func getPodMatches(c *client.Client, nodeName string, podNamePrefix string, namespace string) util.StringSet {
	matches := util.NewStringSet()
	Logf("Checking pods on node %v via /runningpods endpoint", nodeName)
	runningPods, err := GetKubeletPods(c, nodeName)
	if err != nil {
		Logf("Error checking running pods on %v: %v", nodeName, err)
		return matches
	}
	for _, pod := range runningPods.Items {
		if pod.Namespace == namespace && strings.HasPrefix(pod.Name, podNamePrefix) {
			matches.Insert(pod.Name)
		}
	}
	return matches
}

// waitTillNPodsRunningOnNodes polls the /runningpods endpoint on kubelet until
// it finds targetNumPods pods that match the given criteria (namespace and
// podNamePrefix). Note that we usually use label selector to filter pods that
// belong to the same RC. However, we use podNamePrefix with namespace here
// because pods returned from /runningpods do not contain the original label
// information; they are reconstructed by examining the container runtime. In
// the scope of this test, we do not expect pod naming conflicts so
// podNamePrefix should be sufficient to identify the pods.
func waitTillNPodsRunningOnNodes(c *client.Client, nodeNames util.StringSet, podNamePrefix string, namespace string, targetNumPods int, timeout time.Duration) error {
	return wait.Poll(pollInterval, timeout, func() (bool, error) {
		matchCh := make(chan util.StringSet, len(nodeNames))
		for _, item := range nodeNames.List() {
			// Launch a goroutine per node to check the pods running on the nodes.
			nodeName := item
			go func() {
				matchCh <- getPodMatches(c, nodeName, podNamePrefix, namespace)
			}()
		}

		seen := util.NewStringSet()
		for i := 0; i < len(nodeNames.List()); i++ {
			seen = seen.Union(<-matchCh)
		}
		if seen.Len() == targetNumPods {
			return true, nil
		}
		Logf("Waiting for %d pods to be running on the node; %d are currently running;", targetNumPods, seen.Len())
		return false, nil
	})
}

var _ = Describe("Clean up pods on node", func() {
	var numNodes int
	var nodeNames util.StringSet
	framework := NewFramework("kubelet-delete")

	BeforeEach(func() {
		nodes, err := framework.Client.Nodes().List(labels.Everything(), fields.Everything())
		expectNoError(err)
		numNodes = len(nodes.Items)
		nodeNames = util.NewStringSet()
		for _, node := range nodes.Items {
			nodeNames.Insert(node.Name)
		}
		logOneTimeResourceUsageSummary(framework.Client, nodeNames.List(), cpuIntervalInSeconds)
	})

	type DeleteTest struct {
		podsPerNode int
		timeout     time.Duration
	}

	deleteTests := []DeleteTest{
		{podsPerNode: 10, timeout: 1 * time.Minute},
	}

	for _, itArg := range deleteTests {
		name := fmt.Sprintf(
			"kubelet should be able to delete %d pods per node in %v.", itArg.podsPerNode, itArg.timeout)
		It(name, func() {
			totalPods := itArg.podsPerNode * numNodes

			By(fmt.Sprintf("Creating a RC of %d pods and wait until all pods of this RC are running", totalPods))
			rcName := fmt.Sprintf("cleanup%d-%s", totalPods, string(util.NewUUID()))

			Expect(RunRC(RCConfig{
				Client:    framework.Client,
				Name:      rcName,
				Namespace: framework.Namespace.Name,
				Image:     "gcr.io/google_containers/pause:go",
				Replicas:  totalPods,
			})).NotTo(HaveOccurred())
			// Perform a sanity check so that we know all desired pods are
			// running on the nodes according to kubelet. The timeout is set to
			// only 30 seconds here because RunRC already waited for all pods to
			// transition to the running status.
			Expect(waitTillNPodsRunningOnNodes(framework.Client, nodeNames, rcName, framework.Namespace.Name, totalPods,
				time.Second*30)).NotTo(HaveOccurred())
			logOneTimeResourceUsageSummary(framework.Client, nodeNames.List(), cpuIntervalInSeconds)

			By("Deleting the RC")
			DeleteRC(framework.Client, framework.Namespace.Name, rcName)
			// Check that the pods really are gone by querying /runningpods on the
			// node. The /runningpods handler checks the container runtime (or its
			// cache) and  returns a list of running pods. Some possible causes of
			// failures are:
			//   - kubelet deadlock
			//   - a bug in graceful termination (if it is enabled)
			//   - docker slow to delete pods (or resource problems causing slowness)
			start := time.Now()
			Expect(waitTillNPodsRunningOnNodes(framework.Client, nodeNames, rcName, framework.Namespace.Name, 0,
				itArg.timeout)).NotTo(HaveOccurred())
			Logf("Deleting %d pods on %d nodes completed in %v after the RC was deleted", totalPods, len(nodeNames),
				time.Since(start))
		})
	}
})
