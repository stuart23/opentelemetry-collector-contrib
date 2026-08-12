// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package k8sclusterreceiver

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// mockPreferredResourcesDiscovery overrides ServerPreferredResources, which
// fakediscovery.FakeDiscovery always stubs out to (nil, nil) regardless of the
// fake clientset's configured Resources.
type mockPreferredResourcesDiscovery struct {
	fakediscovery.FakeDiscovery
	preferred []*metav1.APIResourceList
	err       error
}

func (m *mockPreferredResourcesDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return m.preferred, m.err
}

// fakeClientWithDiscovery wraps a fake.Clientset to substitute a custom
// discovery.DiscoveryInterface, since fake.Clientset.Discovery() always returns
// its own *fakediscovery.FakeDiscovery.
type fakeClientWithDiscovery struct {
	*fake.Clientset
	discovery discovery.DiscoveryInterface
}

func (f *fakeClientWithDiscovery) Discovery() discovery.DiscoveryInterface {
	return f.discovery
}
