package api

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Inputs are local to this function and are gone before the retained-heap sample.
// Exercise both legacy and routed caches with fixed-size, distinct signatures.
func thoughtSignatureMemorySample(count, payloadBytes int) *thoughtSignatureStore {
	store := newThoughtSignatureStore()
	arguments := json.RawMessage(fmt.Sprintf("{\"payload\":%q}", strings.Repeat("x", payloadBytes)))
	selection := aistudio.ModelSelection{BaseModel: "memory-model"}
	for i := 0; i < count; i++ {
		call := aistudio.FunctionCall{
			ID: fmt.Sprintf("call_%06d", i), Name: "lookup", Arguments: arguments,
			ThoughtSignature: fmt.Sprintf("signature_%06d", i),
		}
		store.Remember([]aistudio.FunctionCall{call}, selection, aistudio.ChannelBuild, "memory-model")
	}
	return store
}

// Run with -benchtime=1x -count=3. retained-B is post-GC heap growth, not B/op
// or production RSS; key-B counts legacy/routed key bytes once per map entry.
func BenchmarkThoughtSignatureCacheRetention(b *testing.B) {
	for _, size := range []int{1 << 10, 16 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("payload_%dKiB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			var retained, keyBytes int64
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				runtime.GC()
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				b.StartTimer()
				store := thoughtSignatureMemorySample(1024, size)
				b.StopTimer()
				runtime.GC()
				runtime.GC() // Clear normalization buffers held by sync.Pool.
				runtime.ReadMemStats(&after)
				retained += int64(after.HeapAlloc) - int64(before.HeapAlloc)
				if len(store.legacy) != 1024 || len(store.routed) != 1024 || len(store.origins) != 1024 {
					b.Fatal("incomplete memory sample")
				}
				for key := range store.legacy {
					keyBytes += int64(len(key))
				}
				for key := range store.routed {
					keyBytes += int64(len(key))
				}
				runtime.KeepAlive(store)
			}
			b.ReportMetric(float64(retained)/float64(b.N), "retained-B")
			b.ReportMetric(float64(keyBytes)/float64(b.N), "key-B")
		})
	}
}
