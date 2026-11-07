package aistudio

import (
	"fmt"
	"strings"
	"testing"
)

var generatedTextBenchmarkResult string

func BenchmarkGeneratedOutputText(b *testing.B) {
	for _, chunks := range []int{256, 512, 1024} {
		b.Run(fmt.Sprintf("%dKiB", chunks), func(b *testing.B) {
			chunk := strings.Repeat("x", 1024)
			b.ReportAllocs()
			b.SetBytes(int64(chunks * len(chunk)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var output generatedOutputParts
				for j := 0; j < chunks; j++ {
					output.observe(Event{Kind: EventText, Text: chunk})
				}
				generatedTextBenchmarkResult = output.visible[0].Text
			}
		})
	}
}
