package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"

	"github.com/outpostos/outpostimage/pkg/routing"
)

const (
	// DefaultChunkSize is the default size of each chunk for parallel fetch (2MB).
	DefaultChunkSize = 2 * 1024 * 1024

	// DefaultParallelism is the default number of concurrent chunk fetches.
	DefaultParallelism = 4
)

// ParallelFetcher fetches a blob from multiple peers in parallel using Range requests.
type ParallelFetcher struct {
	client      *http.Client
	chunkSize   int64
	parallelism int
}

// NewParallelFetcher creates a new parallel blob fetcher.
func NewParallelFetcher(chunkSize int64, parallelism int) *ParallelFetcher {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	if parallelism <= 0 {
		parallelism = DefaultParallelism
	}
	return &ParallelFetcher{
		client:      &http.Client{},
		chunkSize:   chunkSize,
		parallelism: parallelism,
	}
}

// chunk represents a byte range to fetch.
type chunk struct {
	index int
	start int64
	end   int64 // inclusive
}

// chunkResult holds the result of fetching a single chunk.
type chunkResult struct {
	index int
	data  []byte
	err   error
}

// Fetch downloads a blob from the given peers in parallel chunks.
// It uses Range requests to split the blob across available peers.
// If a peer fails, the chunk is retried on another peer.
func (pf *ParallelFetcher) Fetch(ctx context.Context, balancer *routing.Balancer, blobPath string, totalSize int64) ([]byte, error) {
	peers := balancer.Peers()
	if len(peers) == 0 {
		return nil, fmt.Errorf("no peers available")
	}

	// If blob is small enough, just fetch from one peer
	if totalSize <= pf.chunkSize || len(peers) == 1 {
		return pf.fetchSingle(ctx, peers[0], blobPath)
	}

	// Calculate chunks
	chunks := pf.calculateChunks(totalSize)

	// Create result buffer
	results := make([]chunkResult, len(chunks))
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Semaphore for parallelism
	sem := make(chan struct{}, pf.parallelism)

	for _, c := range chunks {
		wg.Add(1)
		sem <- struct{}{} // acquire

		go func(ch chunk) {
			defer wg.Done()
			defer func() { <-sem }() // release

			// Try peers in round-robin order
			var lastErr error
			for attempt := 0; attempt < len(peers); attempt++ {
				peer, ok := balancer.Next()
				if !ok {
					break
				}

				data, err := pf.fetchChunk(ctx, peer, blobPath, ch.start, ch.end)
				if err != nil {
					lastErr = err
					slog.Debug("chunk fetch failed, trying next peer",
						"chunk", ch.index, "peer", peer, "error", err)
					continue
				}

				mu.Lock()
				results[ch.index] = chunkResult{index: ch.index, data: data}
				mu.Unlock()
				return
			}

			mu.Lock()
			results[ch.index] = chunkResult{index: ch.index, err: lastErr}
			mu.Unlock()
		}(c)
	}

	wg.Wait()

	// Sort by index and reassemble
	sort.Slice(results, func(i, j int) bool {
		return results[i].index < results[j].index
	})

	var assembled []byte
	for _, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("chunk %d failed: %w", r.index, r.err)
		}
		assembled = append(assembled, r.data...)
	}

	return assembled, nil
}

// calculateChunks divides the total size into chunks.
func (pf *ParallelFetcher) calculateChunks(totalSize int64) []chunk {
	var chunks []chunk
	for i, offset := int(0), int64(0); offset < totalSize; i, offset = i+1, offset+pf.chunkSize {
		end := offset + pf.chunkSize - 1
		if end >= totalSize {
			end = totalSize - 1
		}
		chunks = append(chunks, chunk{index: i, start: offset, end: end})
	}
	return chunks
}

// fetchSingle fetches an entire blob from a single peer.
func (pf *ParallelFetcher) fetchSingle(ctx context.Context, peer, blobPath string) ([]byte, error) {
	url := fmt.Sprintf("http://%s%s", peer, blobPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := pf.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, peer)
	}

	return io.ReadAll(resp.Body)
}

// fetchChunk fetches a byte range from a peer using Range request.
func (pf *ParallelFetcher) fetchChunk(ctx context.Context, peer, blobPath string, start, end int64) ([]byte, error) {
	url := fmt.Sprintf("http://%s%s", peer, blobPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := pf.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s (chunk %d-%d)", resp.StatusCode, peer, start, end)
	}

	return io.ReadAll(resp.Body)
}

// StreamFetch downloads a blob in parallel and streams it to the http.ResponseWriter.
func (pf *ParallelFetcher) StreamFetch(ctx context.Context, w http.ResponseWriter, balancer *routing.Balancer, blobPath string, totalSize int64) error {
	data, err := pf.Fetch(ctx, balancer, blobPath, totalSize)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(data)
	return err
}
