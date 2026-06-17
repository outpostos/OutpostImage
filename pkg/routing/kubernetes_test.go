package routing

import (
	"context"
	"os"
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
	go func() { _ = kd.Discover(ctx, pw) }()

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

func TestKubernetesDiscovery_WatchUpdates(t *testing.T) {
	initialEP := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "outpostimage",
			Namespace: "default",
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{IP: "10.0.0.1"},
					{IP: "10.0.0.2"},
				},
			},
		},
	}

	client := fake.NewSimpleClientset(initialEP)

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 1 * time.Hour,
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = kd.Discover(ctx, pw) }()
	time.Sleep(300 * time.Millisecond)

	// Initially: 10.0.0.2 only (self excluded)
	peers := pw.Peers()
	if len(peers) != 1 || peers[0] != "10.0.0.2:5000" {
		t.Fatalf("initial: expected [10.0.0.2:5000], got %v", peers)
	}

	// Update the Endpoints — add a new peer
	updatedEP := initialEP.DeepCopy()
	updatedEP.Subsets[0].Addresses = append(updatedEP.Subsets[0].Addresses,
		corev1.EndpointAddress{IP: "10.0.0.3"},
	)
	_, err := client.CoreV1().Endpoints("default").Update(ctx, updatedEP, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update endpoints: %v", err)
	}

	// Wait for the watch event to be processed
	time.Sleep(500 * time.Millisecond)

	peers = pw.Peers()
	if len(peers) != 2 {
		t.Fatalf("after update: expected 2 peers, got %d: %v", len(peers), peers)
	}
}

func TestKubernetesDiscovery_WatchPeerRemoval(t *testing.T) {
	initialEP := &corev1.Endpoints{
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
	}

	client := fake.NewSimpleClientset(initialEP)

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 1 * time.Hour,
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = kd.Discover(ctx, pw) }()
	time.Sleep(300 * time.Millisecond)

	if len(pw.Peers()) != 2 {
		t.Fatalf("initial: expected 2 peers, got %d", len(pw.Peers()))
	}

	// Remove a peer (10.0.0.3 leaves the cluster)
	updatedEP := initialEP.DeepCopy()
	updatedEP.Subsets[0].Addresses = []corev1.EndpointAddress{
		{IP: "10.0.0.1"},
		{IP: "10.0.0.2"},
	}
	_, err := client.CoreV1().Endpoints("default").Update(ctx, updatedEP, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update endpoints: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	peers := pw.Peers()
	if len(peers) != 1 {
		t.Fatalf("after removal: expected 1 peer, got %d: %v", len(peers), peers)
	}
	if peers[0] != "10.0.0.2:5000" {
		t.Errorf("expected 10.0.0.2:5000, got %s", peers[0])
	}
}


func TestKubernetesDiscovery_Resync(t *testing.T) {
	initialEP := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "outpostimage",
			Namespace: "default",
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{IP: "10.0.0.1"},
					{IP: "10.0.0.2"},
				},
			},
		},
	}

	client := fake.NewSimpleClientset(initialEP)

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 500 * time.Millisecond, // fast resync
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = kd.Discover(ctx, pw) }()
	time.Sleep(200 * time.Millisecond)

	// Verify initial state
	if len(pw.Peers()) != 1 {
		t.Fatalf("initial: expected 1 peer, got %d", len(pw.Peers()))
	}

	// Directly modify the Endpoints object in the fake client (simulating
	// a change that watch might miss but resync catches)
	updatedEP := initialEP.DeepCopy()
	updatedEP.Subsets[0].Addresses = append(updatedEP.Subsets[0].Addresses,
		corev1.EndpointAddress{IP: "10.0.0.4"},
	)
	_, err := client.CoreV1().Endpoints("default").Update(ctx, updatedEP, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// Wait for resync to fire (500ms interval + processing time)
	time.Sleep(1200 * time.Millisecond)

	peers := pw.Peers()
	if len(peers) != 2 {
		t.Fatalf("after resync: expected 2 peers, got %d: %v", len(peers), peers)
	}
}

func TestKubernetesDiscovery_DefaultConfig(t *testing.T) {
	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName: "outpostimage",
		Namespace:   "default",
		// RegistryPort and ResyncInterval left at zero
	}, fake.NewSimpleClientset(), "10.0.0.1")

	if kd.config.RegistryPort != 5000 {
		t.Errorf("expected default port 5000, got %d", kd.config.RegistryPort)
	}
	if kd.config.ResyncInterval != 30*time.Second {
		t.Errorf("expected default resync 30s, got %v", kd.config.ResyncInterval)
	}
}

func TestDetectSelfIP_POD_IP(t *testing.T) {
	os.Setenv("POD_IP", "192.168.1.100")
	defer os.Unsetenv("POD_IP")
	os.Unsetenv("HOST_IP")

	ip := detectSelfIP()
	if ip != "192.168.1.100" {
		t.Errorf("expected 192.168.1.100, got %s", ip)
	}
}

func TestDetectSelfIP_HOST_IP(t *testing.T) {
	os.Unsetenv("POD_IP")
	os.Setenv("HOST_IP", "192.168.1.200")
	defer os.Unsetenv("HOST_IP")

	ip := detectSelfIP()
	if ip != "192.168.1.200" {
		t.Errorf("expected 192.168.1.200, got %s", ip)
	}
}

func TestDetectSelfIP_POD_IP_Priority(t *testing.T) {
	os.Setenv("POD_IP", "10.0.0.50")
	os.Setenv("HOST_IP", "10.0.0.60")
	defer os.Unsetenv("POD_IP")
	defer os.Unsetenv("HOST_IP")

	ip := detectSelfIP()
	if ip != "10.0.0.50" {
		t.Errorf("POD_IP should take priority, got %s", ip)
	}
}

func TestDetectSelfIP_FallbackToInterface(t *testing.T) {
	os.Unsetenv("POD_IP")
	os.Unsetenv("HOST_IP")

	ip := detectSelfIP()
	// Should return some non-empty IP from a network interface (or empty if none)
	// We just verify it doesn't panic and returns a plausible result
	if ip != "" {
		// If we got an IP, verify it's not loopback
		if ip == "127.0.0.1" || ip == "::1" {
			t.Errorf("should not return loopback, got %s", ip)
		}
	}
}
