package storage

import (
	"context"
	"strings"
	"sync"

	"vozkot/domain/media"
)

// Memory keeps objects in a map instead of a bucket.
//
// It is the double tests compose with: a use case that stores artwork can be
// exercised end to end, and what it stored asserted on, without a Cloudflare
// account, a network call or bytes left on the machine afterwards. It is never
// wired into a running application, which is why the composition root has no
// switch for it.
type Memory struct {
	mutex         sync.RWMutex
	objects       map[string]MemoryObject
	publicBaseURL string
}

// MemoryObject is one stored object, kept with the Content-Type R2 would have
// written, so a test can catch an upload that would have been served as a
// download.
type MemoryObject struct {
	Data        []byte
	ContentType string
}

var _ media.FileStorage = (*Memory)(nil)

func NewMemory(publicBaseURL string) *Memory {
	return &Memory{
		objects:       make(map[string]MemoryObject),
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
	}
}

func (s *Memory) Upload(_ context.Context, key string, data []byte, contentType string) error {
	stored := make([]byte, len(data))
	copy(stored, data)

	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.objects[s.object(key)] = MemoryObject{Data: stored, ContentType: resolveContentType(key, data, contentType)}
	return nil
}

func (s *Memory) Delete(_ context.Context, key string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	delete(s.objects, s.object(key))
	return nil
}

func (s *Memory) URL(key string) string {
	return s.publicBaseURL + "/" + s.object(key)
}

// Object returns what was stored under a key, for assertions.
func (s *Memory) Object(key string) (MemoryObject, bool) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	object, ok := s.objects[s.object(key)]
	return object, ok
}

// Len is how many objects are held, for asserting that a failed upload left
// nothing behind.
func (s *Memory) Len() int {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return len(s.objects)
}

func (s *Memory) object(key string) string {
	return strings.TrimLeft(key, "/")
}
