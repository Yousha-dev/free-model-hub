package auth

// This file provides a general concurrent executor for account batch processing.
// Admin quota/keepalive shares the tasks queue to avoid duplicating the semaphore/done pattern.

import "sync"

// RunBatch executes fn with a fixed concurrency and returns results maintaining the input order.
func RunBatch[T any, R any](items []T, concurrency int, fn func(T) R) []R {
	results := make([]R, len(items))
	if len(items) == 0 {
		return results
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > len(items) {
		concurrency = len(items)
	}
	semaphore := make(chan struct{}, concurrency)
	var waitGroup sync.WaitGroup
	for index := range items {
		waitGroup.Add(1)
		go func(slot int) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			results[slot] = fn(items[slot])
		}(index)
	}
	waitGroup.Wait()
	return results
}
