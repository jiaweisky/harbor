package store

import (
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// validateTask rejects task metadata that cannot be stored: values must be nil,
// a bool, a signed integer, a float, a string, []any, or map[string]any, with no
// reference cycles. It mirrors the constraint the A2A task store applies.
func validateTask(task *a2a.Task) error {
	if task == nil {
		return nil
	}

	if err := validateMessage(task.Status.Message); err != nil {
		return err
	}

	for _, msg := range task.History {
		if err := validateMessage(msg); err != nil {
			return err
		}
	}

	for _, artifact := range task.Artifacts {
		if err := validateArtifact(artifact); err != nil {
			return err
		}
	}

	return validateMeta(task.Metadata)
}

func validateArtifact(artifact *a2a.Artifact) error {
	if artifact == nil {
		return nil
	}

	if err := validateParts(artifact.Parts); err != nil {
		return err
	}

	return validateMeta(artifact.Metadata)
}

func validateMessage(msg *a2a.Message) error {
	if msg == nil {
		return nil
	}

	if err := validateParts(msg.Parts); err != nil {
		return err
	}

	return validateMeta(msg.Metadata)
}

func validateParts(parts a2a.ContentParts) error {
	for _, part := range parts {
		if err := validateMeta(part.Meta()); err != nil {
			return err
		}
	}

	return nil
}

func validateMeta(meta map[string]any) error {
	return validateMetaValue(meta, map[string]bool{})
}

func validateMetaValue(value any, seen map[string]bool) error {
	switch value.(type) {
	case nil, bool, int, int8, int16, int32, int64, float32, float64, string:
		return nil
	}

	var elems []any
	switch v := value.(type) {
	case []any:
		elems = v
	case map[string]any:
		for _, e := range v {
			elems = append(elems, e)
		}
	default:
		return fmt.Errorf("%T is not permitted in metadata", value)
	}

	key := fmt.Sprintf("%p", value)
	if seen[key] {
		return fmt.Errorf("circular reference in metadata")
	}

	seen[key] = true
	defer delete(seen, key)

	for _, e := range elems {
		if err := validateMetaValue(e, seen); err != nil {
			return err
		}
	}

	return nil
}
