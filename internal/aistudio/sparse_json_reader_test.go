package aistudio

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type sparseChunkReader struct {
	source io.Reader
	size   int
	reads  int
}

func (r *sparseChunkReader) Read(destination []byte) (int, error) {
	r.reads++
	return r.source.Read(destination[:min(len(destination), r.size)])
}

func TestSparseJSONReaderChunkBoundaries(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"empty_source", "", ""},
		{"empty_array", "[]", "[]"},
		{"all_holes", "[,,]", "[null,null,null]"},
		{"leading_middle_trailing", "[,1,,2,]", "[null,1,null,2,null]"},
		{"nested", "[,[,],[],[1,,2,],]", "[null,[null,null],[],[1,null,2,null],null]"},
		{"whitespace", "[ \n,\t, 3, \r]", "[ \nnull,\tnull, 3, \rnull]"},
		{"strings", `[",]","a\"[,]","\\",,"中文🙂",]`, `[",]","a\"[,]","\\",null,"中文🙂",null]`},
		{"object", `{"parts":[,"x",],"count":3}`, `{"parts":[null,"x",null],"count":3}`},
	}
	for _, test := range cases {
		for _, sourceSize := range []int{1, 2, 5, 4096} {
			for _, destinationSize := range []int{1, 2, 3, 4, 5, 4096} {
				t.Run(fmt.Sprintf("%s/source=%d/destination=%d", test.name, sourceSize, destinationSize), func(t *testing.T) {
					source := &sparseChunkReader{source: strings.NewReader(test.input), size: sourceSize}
					reader := newSparseJSONReader(source)
					buffer := make([]byte, destinationSize)
					var output strings.Builder
					for {
						if n, err := reader.Read(nil); n != 0 || err != nil {
							t.Fatalf("zero-length read = %d, %v", n, err)
						}
						n, err := reader.Read(buffer)
						output.Write(buffer[:n])
						if err == io.EOF {
							break
						}
						if err != nil || n == 0 {
							t.Fatalf("read = %d, %v", n, err)
						}
					}
					if got := output.String(); got != test.want {
						t.Fatalf("normalization = %q, want %q", got, test.want)
					}
				})
			}
		}
	}
}

type sparseFailingReader struct {
	data string
	err  error
}

func (r *sparseFailingReader) Read(destination []byte) (int, error) {
	n := copy(destination, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestSparseJSONReaderPreservesSourceFailure(t *testing.T) {
	failure := errors.New("fixture source failure")
	reader := newSparseJSONReader(&sparseFailingReader{data: "[,]", err: failure})
	got, err := io.ReadAll(reader)
	if string(got) != "[null,null]" || !errors.Is(err, failure) {
		t.Fatalf("read = %q, %v", got, err)
	}
}

func TestSparseJSONReaderZeroLengthDoesNotReadSource(t *testing.T) {
	source := &sparseChunkReader{source: strings.NewReader("[,]"), size: 1}
	reader := newSparseJSONReader(source)
	if n, err := reader.Read(nil); n != 0 || err != nil || source.reads != 0 {
		t.Fatalf("zero-length read = %d, %v; source reads = %d", n, err, source.reads)
	}
}

func drainSparseJSON(input string, buffer []byte) error {
	reader := newSparseJSONReader(strings.NewReader(input))
	for {
		if _, err := reader.Read(buffer); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func TestSparseJSONReaderAllocationsDoNotScaleWithInput(t *testing.T) {
	buffer := make([]byte, 4096)
	for _, test := range []struct{ name, unit string }{
		{"text", "ordinary text"},
		{"holes", ","},
	} {
		t.Run(test.name, func(t *testing.T) {
			wrap := func(value string) string { return "[" + value + "]" }
			if test.name == "text" {
				wrap = func(value string) string { return `["` + value + `"]` }
			}
			short := wrap(test.unit)
			long := wrap(strings.Repeat(test.unit, 4096))
			allocations := func(input string) float64 {
				return testing.AllocsPerRun(10, func() {
					if err := drainSparseJSON(input, buffer); err != nil {
						t.Fatal(err)
					}
				})
			}
			small, large := allocations(short), allocations(long)
			t.Logf("allocations short=%.0f long=%.0f", small, large)
			if large > small+1 {
				t.Fatalf("allocations scale with input: short=%.0f long=%.0f", small, large)
			}
		})
	}
}

func BenchmarkSparseJSONReader(b *testing.B) {
	for _, test := range []struct{ name, input string }{
		{"text", `["` + strings.Repeat("ordinary text", 4096) + `"]`},
		{"holes", "[" + strings.Repeat(",", 4096) + "]"},
	} {
		b.Run(test.name, func(b *testing.B) {
			buffer := make([]byte, 4096)
			b.SetBytes(int64(len(test.input)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := drainSparseJSON(test.input, buffer); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
