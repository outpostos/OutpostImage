package routing

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKubernetesDiscovery_ExtractPeers(t *testing.T) {
	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:  "outpostimage",
		Namespace:    "default",
		RegistryPort: 5000,
	}, fake.NewSimpleClientset(), "10.0.0.1")

	ep := &corev1.Endpoints{
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{IP: "10.0.0.1"}, // self — should be excluded
					{IP: "10.0.0.2"},
					{IP: "10.0.0.3"},
				},
			},
		},
	}

	peers := kd.extractPeers(ep)
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d: %v", len(peers), peers)
	}
	if peers[0] != "10.0.0.2:5000" {
		t.Errorf("expected 10.0.0.2:5000, got %s", peers[0])
	}
	if peers[1] != "10.0.0.3:5000" {
		t.Errorf("expected 10.0.0.3:5000, got %s", peers[1])
	}
}

func TestKubernetesDiscovery_EmptyEndpoints(t *testing.T) {
	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:  "outpostimage",
		Namespace:    "default",
		RegistryPort: 5000,
	}, fake.NewSimpleClientset(), "10.0.0.1")

	ep := &corev1.Endpoints{}
	peers := kd.extractPeers(ep)
	if len(peers) != 0 {
		t.Fatalf("expected 0 peers, got %d", len(peers))
	}
}

func TestKubernetesDiscovery_OnlySelf(t *testing.T) {
	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:  "outpostimage",
		Namespace:    "default",
		RegistryPort: 5000,
	}, fake.NewSimpleClientset(), "10.0.0.1")

	ep := &corev1.Endpoints{
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{IP: "10.0.0.1"},
				},
			},
		},
	}
	peers := kd.extractPeers(ep)
	if len(peers) != 0 {
		t.Fatalf("expected 0 peers (self excluded), got %d", len(peers))
	}
}

func TestKubernetesDiscovery_DiscoverInitialLoad(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "outpostimage",
			Namespace: "default",
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{IP: "10.0.0.1"},
					{IP: "10.0.0.2"},
					{IP: "10.0.0.3"},
				},
			},
		},
	})

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 1 * time.Hour, // don't resync during test
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Run discovery in background — it will load initial endpoints then block on watch
	go kd.Discover(ctx, pw)

	// Give it a moment to process
	time.Sleep(200 * time.Millisecond)

	peers := pw.Peers()
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d: %v", len(peers), peers)
	}
}

func TestKubernetesDiscovery_CustomPort(t *testing.T) {
	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:  "outpostimage",
		Namespace:    "default",
		RegistryPort: 8000,
	}, fake.NewSimpleClientset(), "10.0.0.1")

	ep := &corev1.Endpoints{
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{IP: "10.0.0.2"},
				},
			},
		},
	}

	peers := kd.extractPeers(ep)
	if len(peers) != 1 || peers[0] != "10.0.0.2:8000" {
		t.Fatalf("expected [10.0.0.2:8000], got %v", peers)
	}
}

func TestKubernetesDiscovery_MultipleSubsets(t *testing.T) {
	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:  "outpostimage",
		Namespace:    "default",
		RegistryPort: 5000,
	}, fake.NewSimpleClientset(), "10.0.0.1")

	ep := &corev1.Endpoints{
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{IP: "10.0.0.2"},
				},
			},
			{
				Addresses: []corev1.EndpointAddress{
					{IP: "10.0.0.3"},
					{IP: "10.0.0.4"},
				},
			},
		},
	}

	peers := kd.extractPeers(ep)
	if len(peers) != 3 {
		t.Fatalf("expected 3 peers from 2 subsets, got %d: %v", len(peers), peers)
	}
}
