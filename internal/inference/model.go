package inference

import (
	"fmt"

	ort "github.com/yalue/onnxruntime_go"
)

// Model represents an ONNX model for inference.
type Model struct {
	session *ort.DynamicAdvancedSession
	inputs  []string
	outputs []string
}

// NewModel loads an ONNX model from file.
func NewModel(path string, inputNames, outputNames []string) (*Model, error) {
	session, err := ort.NewDynamicAdvancedSession(path, inputNames, outputNames, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	return &Model{
		session: session,
		inputs:  inputNames,
		outputs: outputNames,
	}, nil
}

// Predict runs inference on the model.
func (m *Model) Predict(input map[string]*ort.Tensor[float32]) (map[string]*ort.Tensor[float32], error) {
	inputValues := make([]ort.Value, len(m.inputs))
	for i, name := range m.inputs {
		tensor, ok := input[name]
		if !ok {
			return nil, fmt.Errorf("missing input: %s", name)
		}
		inputValues[i] = tensor
	}

	outputValues := make([]ort.Value, len(m.outputs))
	err := m.session.Run(inputValues, outputValues)
	if err != nil {
		return nil, fmt.Errorf("inference failed: %w", err)
	}

	result := make(map[string]*ort.Tensor[float32])
	for i, name := range m.outputs {
		if t, ok := outputValues[i].(*ort.Tensor[float32]); ok {
			result[name] = t
		} else {
			return nil, fmt.Errorf("unexpected output type for %s", name)
		}
	}

	return result, nil
}

// Close releases resources.
func (m *Model) Close() {
	if m.session != nil {
		m.session.Destroy()
	}
}
