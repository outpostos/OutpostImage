package routing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
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

// TestNewKubernetesDiscovery_NoNamespace verifies that NewKubernetesDiscovery
// returns an error when no namespace is configured and the service-account file
// doesn't exist (i.e. outside a real Kubernetes cluster).
func TestNewKubernetesDiscovery_NoNamespace(t *testing.T) {
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		t.Skip("running inside a Kubernetes cluster — skipping out-of-cluster error test")
	}

	_, err := NewKubernetesDiscovery(KubernetesConfig{
		ServiceName: "outpostimage",
		// Namespace intentionally empty so the code tries to read the SA file
	})
	if err == nil {
		t.Fatal("expected error when not in a Kubernetes cluster")
	}
	if !strings.Contains(err.Error(), "cannot detect namespace") {
		t.Errorf("expected 'cannot detect namespace' in error, got: %v", err)
	}
}

// TestNewKubernetesDiscovery_NoInClusterConfig verifies that NewKubernetesDiscovery
// fails at the in-cluster config step when namespace is provided but no cluster
// credentials are available.
func TestNewKubernetesDiscovery_NoInClusterConfig(t *testing.T) {
	// Ensure the in-cluster env vars are absent.
	orig1, set1 := os.LookupEnv("KUBERNETES_SERVICE_HOST")
	orig2, set2 := os.LookupEnv("KUBERNETES_SERVICE_PORT")
	os.Unsetenv("KUBERNETES_SERVICE_HOST")
	os.Unsetenv("KUBERNETES_SERVICE_PORT")
	defer func() {
		if set1 {
			os.Setenv("KUBERNETES_SERVICE_HOST", orig1)
		}
		if set2 {
			os.Setenv("KUBERNETES_SERVICE_PORT", orig2)
		}
	}()

	_, err := NewKubernetesDiscovery(KubernetesConfig{
		ServiceName: "outpostimage",
		Namespace:   "default", // skip the SA file read
	})
	if err == nil {
		t.Skip("NewKubernetesDiscovery succeeded unexpectedly (in-cluster creds present)")
	}
	if !strings.Contains(err.Error(), "in-cluster config") {
		t.Errorf("expected 'in-cluster config' in error, got: %v", err)
	}
}

// TestKubernetesDiscovery_WatchCallError exercises the error path in watchEndpoints
// when the Watch API call itself returns an error (line 127: "watch endpoints: %w").
func TestKubernetesDiscovery_WatchCallError(t *testing.T) {
	initialEP := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "outpostimage", Namespace: "default"},
		Subsets: []corev1.EndpointSubset{
			{Addresses: []corev1.EndpointAddress{{IP: "10.0.0.2"}}},
		},
	}

	client := fake.NewSimpleClientset(initialEP)
	// Make the Watch call return an error; Get still succeeds (prepended = first match wins).
	client.PrependWatchReactor("*", func(_ ktesting.Action) (bool, watch.Interface, error) {
		return true, nil, fmt.Errorf("simulated watch API error")
	})

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 1 * time.Hour,
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)
	err := kd.watchEndpoints(context.Background(), pw)
	if err == nil {
		t.Error("expected error when Watch API call fails")
	}
}

// TestKubernetesDiscovery_WatchChannelClosed exercises the "watch channel closed"
// error path in watchEndpoints and the subsequent retry logic in Discover.
func TestKubernetesDiscovery_WatchChannelClosed(t *testing.T) {
	initialEP := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "outpostimage", Namespace: "default"},
		Subsets: []corev1.EndpointSubset{
			{Addresses: []corev1.EndpointAddress{{IP: "10.0.0.2"}}},
		},
	}

	client := fake.NewSimpleClientset(initialEP)
	fw := watch.NewFake()
	client.PrependWatchReactor("*", func(_ ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 1 * time.Hour,
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)

	errCh := make(chan error, 1)
	go func() {
		errCh <- kd.watchEndpoints(context.Background(), pw)
	}()

	// Give watchEndpoints time to call Watch and enter the event loop.
	time.Sleep(50 * time.Millisecond)

	// Closing the fake watcher closes its result channel → "watch channel closed".
	fw.Stop()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected an error when watch channel closes, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Error("watchEndpoints did not return after watch channel closed")
	}
}

// TestKubernetesDiscovery_WatchErrorEvent exercises the watch.Error event path
// in watchEndpoints.
func TestKubernetesDiscovery_WatchErrorEvent(t *testing.T) {
	initialEP := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "outpostimage", Namespace: "default"},
	}

	client := fake.NewSimpleClientset(initialEP)
	fw := watch.NewFake()
	client.PrependWatchReactor("*", func(_ ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 1 * time.Hour,
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)

	errCh := make(chan error, 1)
	go func() {
		errCh <- kd.watchEndpoints(context.Background(), pw)
	}()

	time.Sleep(50 * time.Millisecond)

	// Send an error event through the fake watcher.
	fw.Action(watch.Error, &metav1.Status{Message: "simulated watch error"})

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected error from watch error event, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Error("watchEndpoints did not return after watch error event")
	}
}

// TestKubernetesDiscovery_WatchNonEndpointsObject exercises the type-assertion
// guard `ep, ok := event.Object.(*corev1.Endpoints); if !ok { continue }`.
func TestKubernetesDiscovery_WatchNonEndpointsObject(t *testing.T) {
	initialEP := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "outpostimage", Namespace: "default"},
		Subsets: []corev1.EndpointSubset{
			{Addresses: []corev1.EndpointAddress{{IP: "10.0.0.2"}}},
		},
	}

	client := fake.NewSimpleClientset(initialEP)
	fw := watch.NewFake()
	client.PrependWatchReactor("*", func(_ ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 1 * time.Hour,
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = kd.watchEndpoints(ctx, pw) }()
	time.Sleep(50 * time.Millisecond)

	// Send a non-Endpoints object — should be skipped (continue).
	fw.Modify(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "some-pod"}})

	// A real Endpoints update after the non-Endpoints one should still be processed.
	updatedEP := initialEP.DeepCopy()
	updatedEP.Subsets[0].Addresses = append(updatedEP.Subsets[0].Addresses,
		corev1.EndpointAddress{IP: "10.0.0.3"},
	)
	fw.Modify(updatedEP)

	time.Sleep(300 * time.Millisecond)

	peers := pw.Peers()
	if len(peers) != 2 {
		t.Errorf("expected 2 peers after non-endpoints + valid update, got %d: %v", len(peers), peers)
	}
}

// TestKubernetesDiscovery_Discover_RetryOnError verifies that Discover retries
// after watchEndpoints returns an error and that context cancellation during the
// retry wait causes Discover to return nil.
func TestKubernetesDiscovery_Discover_RetryOnError(t *testing.T) {
	initialEP := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "outpostimage", Namespace: "default"},
	}

	client := fake.NewSimpleClientset(initialEP)
	fw := watch.NewFake()
	client.PrependWatchReactor("*", func(_ ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 1 * time.Hour,
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- kd.Discover(ctx, pw) }()

	// Wait for Discover to start watching.
	time.Sleep(100 * time.Millisecond)

	// Close the fake watcher — watchEndpoints returns an error.
	fw.Stop()

	// Give Discover time to process the error and enter the retry select.
	time.Sleep(100 * time.Millisecond)

	// Cancel context — should trigger <-ctx.Done() inside the retry select.
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil from Discover after retry+cancel, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("Discover did not return after context cancel in retry path")
	}
}

// TestKubernetesDiscovery_Discover_ContextAlreadyDoneOnError verifies the
// `if ctx.Err() != nil { return nil }` guard in Discover when the context is
// cancelled before watchEndpoints returns an error.  A custom Get reactor
// returns context.Canceled so that the fake client honours cancellation.
func TestKubernetesDiscovery_Discover_ContextAlreadyDoneOnError(t *testing.T) {
	client := fake.NewSimpleClientset()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the custom reactor sees ctx done

	// Make Get return an error when the context is already done.
	client.PrependReactor("get", "endpoints", func(_ ktesting.Action) (bool, runtime.Object, error) {
		if ctx.Err() != nil {
			return true, nil, context.Canceled
		}
		return false, nil, nil
	})

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 1 * time.Hour,
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)

	done := make(chan error, 1)
	go func() { done <- kd.Discover(ctx, pw) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil when ctx already done, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Discover did not return with pre-cancelled context")
	}
}

// TestKubernetesDiscovery_WatchEndpoints_ResyncError exercises the error path
// in the resync branch of watchEndpoints (`if err != nil { slog.Warn; continue }`).
// The reactor returns success only for the initial Get, and an error for
// subsequent Gets (resync calls).
func TestKubernetesDiscovery_WatchEndpoints_ResyncError(t *testing.T) {
	initialEP := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "outpostimage", Namespace: "default"},
		Subsets: []corev1.EndpointSubset{
			{Addresses: []corev1.EndpointAddress{{IP: "10.0.0.2"}}},
		},
	}

	client := fake.NewSimpleClientset(initialEP)
	fw := watch.NewFake()
	client.PrependWatchReactor("*", func(_ ktesting.Action) (bool, watch.Interface, error) {
		return true, fw, nil
	})

	var getCount int
	client.PrependReactor("get", "endpoints", func(_ ktesting.Action) (bool, runtime.Object, error) {
		getCount++
		if getCount == 1 {
			// First call: let the default handler respond (initial list succeeds).
			return false, nil, nil
		}
		// Subsequent calls (resync): return an error.
		return true, nil, fmt.Errorf("simulated resync error")
	})

	kd := NewKubernetesDiscoveryWithClient(KubernetesConfig{
		ServiceName:    "outpostimage",
		Namespace:      "default",
		RegistryPort:   5000,
		ResyncInterval: 50 * time.Millisecond, // trigger resync quickly
	}, client, "10.0.0.1")

	pw := NewPeerWatcher(nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = kd.watchEndpoints(ctx, pw) }()

	// Wait for the resync to fire and fail at least once.
	time.Sleep(300 * time.Millisecond)

	// Verify peers are still intact (resync error should just log and continue).
	peers := pw.Peers()
	if len(peers) != 1 || peers[0] != "10.0.0.2:5000" {
		t.Errorf("expected [10.0.0.2:5000] after resync error, got %v", peers)
	}

	cancel()
}
