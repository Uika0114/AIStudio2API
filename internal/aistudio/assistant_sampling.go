package aistudio

import (
	"fmt"
	"math"
)

// ValidateAssistantSamplingParameters checks only the normalized sampling values
// accepted for compatibility on the Assistant text route. It does not clean the
// config or validate an entire request. PG/Build keep their existing validators.
// HTTP integer syntax/overflow is handled by the existing typed decoders; Seed
// intentionally accepts the full int64 domain, including negative values and 0.
func ValidateAssistantSamplingParameters(config GenerationConfig) error {
	if value := config.Temperature; value != nil && (!finiteSamplingValue(*value) || *value < 0 || *value > 2) {
		return fmt.Errorf("%w: assistant temperature must be finite and between 0 and 2", ErrInvalidArgument)
	}
	if value := config.TopP; value != nil && (!finiteSamplingValue(*value) || *value < 0 || *value > 1) {
		return fmt.Errorf("%w: assistant topP must be finite and between 0 and 1", ErrInvalidArgument)
	}
	if value := config.TopK; value != nil && *value < 0 {
		return fmt.Errorf("%w: assistant topK must be nonnegative", ErrInvalidArgument)
	}
	return nil
}

func finiteSamplingValue(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
