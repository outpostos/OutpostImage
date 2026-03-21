package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/outpostos/outpostimage/pkg/oci"
)

// TestMultiStoreE2E tests that MultiStore correctly merges Docker and Containerd stores.
func TestMultiStoreE2E(t *testing.T) {
	ctx := context.Background()

	// Create two mock stores simulating Docker and Containerd
	dockerStore := newMockStore("docker")
	containerdStore := newMockStore("containerd")

	// Docker has nginx:latest
	nginxConfig := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:aaa"]}}`)
	nginxLayer := []byte("nginx-layer-data-from-docker-store")
	nginxConfigDigest, nginxLayerDigest := dockerStore.addImage("library/nginx:latest", nginxConfig, nginxLayer)

	// Containerd has alpine:3.19 AND nginx:latest with same digest (shared image)
	alpineConfig := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:bbb"]}}`)
	alpineLayer := []byte("alpine-layer-data-from-containerd-store")
	alpineConfigDigest, _ := containerdStore.addImage("docker.io/library/alpine:3.19", alpineConfig, alpineLayer)

	// Containerd also has nginx (same data, different tag format)
	containerdStore.addImage("docker.io/library/nginx:latest", nginxConfig, nginxLayer)

	// Create MultiStore
	multi := oci.NewMultiStore(dockerStore, containerdStore)

	// Test 1: ListImages should merge and dedup
	t.Run("ListImages_MergeDedup", func(t *testing.T) {
		images, err := multi.ListImages(ctx)
		if err != nil {
			t.Fatal(err)
		}

		// Should have 2 unique images (nginx deduped)
		if len(images) != 2 {
			t.Errorf("expected 2 unique images, got %d", len(images))
			for _, img := range images {
				t.Logf("  %s: %v", img.Digest, img.RepoTags)
			}
		}

		// Nginx should have merged tags from both stores
		for _, img := range images {
			if img.Digest == nginxConfigDigest {
				if len(img.RepoTags) < 2 {
					t.Errorf("expected nginx to have merged tags from both stores, got %v", img.RepoTags)
				}
				t.Logf("nginx tags merged: %v", img.RepoTags)
			}
		}
	})

	// Test 2: Resolve should prefer first store
	t.Run("Resolve_PriorityOrder", func(t *testing.T) {
		d, err := multi.Resolve(ctx, "library/nginx:latest")
		if err != nil {
			t.Fatal(err)
		}
		if d != nginxConfigDigest {
			t.Errorf("Resolve should return docker store's digest first")
		}

		// Containerd-only image
		d, err = multi.Resolve(ctx, "docker.io/library/alpine:3.19")
		if err != nil {
			t.Fatal(err)
		}
		if d != alpineConfigDigest {
			t.Errorf("alpine digest mismatch")
		}
	})

	// Test 3: GetBlob should find blobs from either store
	t.Run("GetBlob_AnyStore", func(t *testing.T) {
		// Docker-only blob
		rc, size, err := multi.GetBlob(ctx, nginxLayerDigest)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		if int64(len(data)) != size {
			t.Errorf("size mismatch: got %d, want %d", len(data), size)
		}

		// Verify digest integrity
		actualDigest := digest.FromBytes(data)
		if actualDigest != nginxLayerDigest {
			t.Errorf("blob digest mismatch: got %s, want %s", actualDigest, nginxLayerDigest)
		}
	})

	// Test 4: HasBlob across stores
	t.Run("HasBlob_CrossStore", func(t *testing.T) {
		if !multi.HasBlob(ctx, nginxLayerDigest) {
			t.Error("should find nginx layer from docker store")
		}
		if !multi.HasBlob(ctx, alpineConfigDigest) {
			t.Error("should find alpine config from containerd store")
		}
		fakeDigest := digest.FromBytes([]byte("nonexistent"))
		if multi.HasBlob(ctx, fakeDigest) {
			t.Error("should not find nonexistent blob")
		}
	})

	// Test 5: Subscribe fan-in
	t.Run("Subscribe_FanIn", func(t *testing.T) {
		subCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		ch, err := multi.Subscribe(subCtx)
		if err != nil {
			t.Fatal(err)
		}
		if ch == nil {
			t.Fatal("Subscribe returned nil channel")
		}
		t.Log("MultiStore Subscribe fan-in channel created successfully")
	})

	t.Logf("=== MultiStore E2E Report ===")
	t.Logf("Docker store: 1 image (nginx)")
	t.Logf("Containerd store: 2 images (alpine, nginx)")
	t.Logf("MultiStore: 2 unique images (deduped)")
}

// TestContainerdStoreE2E tests ContainerdStore against a simulated containerd layout.
func TestContainerdStoreE2E(t *testing.T) {
	ctx := context.Background()

	// Create simulated containerd directory structure
	root := t.TempDir()
	blobDir := filepath.Join(root, "io.containerd.content.v1.content", "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write several blobs of different types
	// 1. Image config
	configContent := `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:aaaa"]},"config":{"Env":["PATH=/usr/local/bin"]}}`
	configDigest := digest.FromBytes([]byte(configContent))
	if err := os.WriteFile(filepath.Join(blobDir, configDigest.Encoded()), []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Manifest
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"digest":    configDigest.String(),
			"size":      len(configContent),
		},
		"layers": []map[string]any{
			{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": "sha256:fakeLayer", "size": 1024},
		},
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestDigest := digest.FromBytes(manifestBytes)
	if err := os.WriteFile(filepath.Join(blobDir, manifestDigest.Encoded()), manifestBytes, 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Layer blob (simulated tar.gz)
	layerData := make([]byte, 4096)
	for i := range layerData {
		layerData[i] = byte(i % 256)
	}
	layerDigest := digest.FromBytes(layerData)
	if err := os.WriteFile(filepath.Join(blobDir, layerDigest.Encoded()), layerData, 0644); err != nil {
		t.Fatal(err)
	}

	// Initialize ContainerdStore
	store := oci.NewContainerdStore(root)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	// Test: All blobs detected
	t.Run("BlobDiscovery", func(t *testing.T) {
		if !store.HasBlob(ctx, configDigest) {
			t.Error("config blob not found")
		}
		if !store.HasBlob(ctx, manifestDigest) {
			t.Error("manifest blob not found")
		}
		if !store.HasBlob(ctx, layerDigest) {
			t.Error("layer blob not found")
		}
	})

	// Test: GetBlob returns correct content
	t.Run("BlobIntegrity", func(t *testing.T) {
		rc, size, err := store.GetBlob(ctx, layerDigest)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()

		data, _ := io.ReadAll(rc)
		if int64(len(data)) != size {
			t.Errorf("size mismatch: got %d, want %d", len(data), size)
		}

		actualDigest := digest.FromBytes(data)
		if actualDigest != layerDigest {
			t.Errorf("blob integrity failed: got %s, want %s", actualDigest, layerDigest)
		}
	})

	// Test: GetManifest returns manifest with detected media type
	t.Run("ManifestMediaType", func(t *testing.T) {
		data, mediaType, err := store.GetManifest(ctx, manifestDigest)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			t.Error("empty manifest data")
		}
		if mediaType != "application/vnd.docker.distribution.manifest.v2+json" {
			t.Errorf("media type = %q, want docker manifest v2", mediaType)
		}
		t.Logf("Manifest media type correctly detected: %s", mediaType)
	})

	t.Logf("=== ContainerdStore E2E Report ===")
	t.Logf("Root: %s", root)
	t.Logf("Blobs discovered: 3 (config, manifest, layer)")
	t.Logf("Blob integrity: verified")
}

// TestStoreAutoDetection tests the DetectStores function with simulated filesystem.
func TestStoreAutoDetection(t *testing.T) {
	ctx := context.Background()

	t.Run("NoStoresDetected", func(t *testing.T) {
		root := t.TempDir()
		stores := oci.DetectStores(ctx, filepath.Join(root, "docker"), filepath.Join(root, "containerd"))
		if len(stores) != 0 {
			t.Errorf("expected 0 stores, got %d", len(stores))
		}
	})

	t.Run("ContainerdOnly", func(t *testing.T) {
		root := t.TempDir()
		// Create containerd content structure
		containerdBlobs := filepath.Join(root, "containerd", "io.containerd.content.v1.content", "blobs", "sha256")
		os.MkdirAll(containerdBlobs, 0755)

		stores := oci.DetectStores(ctx, filepath.Join(root, "docker"), filepath.Join(root, "containerd"))
		if len(stores) != 1 {
			t.Errorf("expected 1 store (containerd), got %d", len(stores))
			return
		}
		if stores[0].Name() != "containerd" {
			t.Errorf("expected containerd store, got %s", stores[0].Name())
		}
	})

	t.Run("DockerOnly", func(t *testing.T) {
		root := t.TempDir()
		// Create Docker imagedb structure
		dockerImageDB := filepath.Join(root, "docker", "image", "overlay2", "imagedb", "content", "sha256")
		os.MkdirAll(dockerImageDB, 0755)
		// Create repositories.json (required for init)
		reposPath := filepath.Join(root, "docker", "image", "overlay2", "repositories.json")
		os.WriteFile(reposPath, []byte(`{"Repositories":{}}`), 0644)

		stores := oci.DetectStores(ctx, filepath.Join(root, "docker"), filepath.Join(root, "containerd"))
		if len(stores) != 1 {
			t.Errorf("expected 1 store (docker), got %d", len(stores))
			return
		}
		if stores[0].Name() != "docker" {
			t.Errorf("expected docker store, got %s", stores[0].Name())
		}
	})
}

// TestMultiStoreNodeE2E tests a test node backed by MultiStore.
func TestMultiStoreNodeE2E(t *testing.T) {
	ctx := context.Background()

	// Create two mock stores for MultiStore
	s1 := newMockStore("docker")
	s2 := newMockStore("containerd")

	// Docker has nginx
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	layerData := []byte("nginx-layer-data-for-multistore-node-test")
	s1.addImage("library/nginx:latest", configData, layerData)

	// Containerd has alpine
	alpineConfig := []byte(`{"architecture":"arm64","os":"linux"}`)
	alpineLayer := []byte("alpine-layer-data-containerd-only")
	s2.addImage("library/alpine:3.19", alpineConfig, alpineLayer)

	// Create MultiStore and start node
	multi := oci.NewMultiStore(s1, s2)
	node := startTestNodeWithStore(t, "multi-node", multi)
	defer node.stop()
	node.initTracker(ctx, t)

	// Query digests API - should see blobs from both stores
	t.Run("DigestAPI_BothStores", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://%s/api/v1/digests", node.addr))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var body struct {
			Digests []string `json:"digests"`
			Count   int      `json:"count"`
		}
		json.NewDecoder(resp.Body).Decode(&body)

		// Should see digests from both stores
		if body.Count < 2 {
			t.Errorf("expected at least 2 digests from multi-store, got %d", body.Count)
		}
		t.Logf("MultiStore node has %d digests", body.Count)
	})

	// Pull manifests from both stores through the node
	t.Run("PullFromBothStores", func(t *testing.T) {
		// Pull nginx (from docker store)
		resp, err := http.Get(fmt.Sprintf("http://%s/v2/library/nginx/manifests/latest", node.addr))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("nginx manifest status=%d, want 200", resp.StatusCode)
		}

		// Pull alpine (from containerd store)
		resp, err = http.Get(fmt.Sprintf("http://%s/v2/library/alpine/manifests/3.19", node.addr))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("alpine manifest status=%d, want 200", resp.StatusCode)
		}
	})
}
